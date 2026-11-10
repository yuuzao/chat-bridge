// Package taskstore 在进程内存中保存任务，并在状态变化时唤醒等待方。
package taskstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Status 是任务在内存中的状态。
type Status string

const (
	// StatusPending 表示已创建，还没有收到回调，也还没有确认转发失败。
	StatusPending Status = "PENDING"
	// StatusDone 表示已收到一次有效回调。
	StatusDone Status = "DONE"
	// StatusFailed 表示向 Bot 转发失败。
	StatusFailed Status = "FAILED"
)

var (
	// ErrNotFound 表示任务不在内存中。
	ErrNotFound = errors.New("task not found")
	// ErrNotPending 表示任务已经离开 PENDING，不能再次写入结果。
	ErrNotPending = errors.New("task is not pending")
	// ErrFileNotAvailable 表示任务还不能提供可下载的文件。
	ErrFileNotAvailable = errors.New("file is not available")
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidID 判断 id 是否为本服务生成的小写 UUID 版本 4。
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// View 是任务的可读快照，不包含文件字节。
type View struct {
	ID       string
	Status   Status
	Content  string
	Failure  string
	HasFile  bool
	FileName string
	FileMIME string
	Done     <-chan struct{}
}

// File 是可写回客户端的文件内容。Bytes 与内存中的切片共享底层数组，调用方不得修改。
type File struct {
	Name  string
	MIME  string
	Bytes []byte
}

// Result 是回调写入的结果。
type Result struct {
	Content   string
	HasFile   bool
	FileName  string
	FileMIME  string
	FileBytes []byte
}

type task struct {
	id        string
	status    Status
	content   string
	failure   string
	hasFile   bool
	fileName  string
	fileMIME  string
	fileBytes []byte
	done      chan struct{}
	closeOnce sync.Once
	createdAt time.Time
}

// Store 是单进程内的任务表。
type Store struct {
	mu    sync.RWMutex
	tasks map[string]*task
}

// New 返回空任务表。
func New() *Store {
	return &Store{tasks: make(map[string]*task)}
}

// Create 生成任务 ID，并以 PENDING 放入任务表。
func (s *Store) Create() (View, error) {
	if s == nil {
		return View{}, errors.New("store is required")
	}
	id, err := newID()
	if err != nil {
		return View{}, err
	}
	item := &task{
		id:        id,
		status:    StatusPending,
		done:      make(chan struct{}),
		createdAt: time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[id] = item
	return item.view(), nil
}

// View 返回任务快照。
func (s *Store) View(id string) (View, error) {
	if s == nil {
		return View{}, errors.New("store is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.tasks[id]
	if !ok {
		return View{}, ErrNotFound
	}
	return item.view(), nil
}

// File 返回已完成且带文件的任务内容。
func (s *Store) File(id string) (File, error) {
	if s == nil {
		return File{}, errors.New("store is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.tasks[id]
	if !ok {
		return File{}, ErrNotFound
	}
	if item.status != StatusDone || !item.hasFile {
		return File{}, ErrFileNotAvailable
	}
	return File{Name: item.fileName, MIME: item.fileMIME, Bytes: item.fileBytes}, nil
}

// Complete 把 PENDING 任务写成 DONE，并关闭等待通道。
// 任务已不在 PENDING 时返回当前快照和 ErrNotPending，不修改任务，也不再次关闭通道。
func (s *Store) Complete(id string, result Result) (View, error) {
	if s == nil {
		return View{}, errors.New("store is required")
	}
	if err := validateResult(result); err != nil {
		return View{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.tasks[id]
	if !ok {
		return View{}, ErrNotFound
	}
	if item.status != StatusPending {
		return item.view(), ErrNotPending
	}
	item.status = StatusDone
	item.content = result.Content
	item.hasFile = result.HasFile
	item.fileName = result.FileName
	item.fileMIME = result.FileMIME
	item.fileBytes = result.FileBytes
	item.closeDone()
	return item.view(), nil
}

// Fail 把 PENDING 任务写成 FAILED，并关闭等待通道。reason 会原样保存。
func (s *Store) Fail(id string, reason string) error {
	if s == nil {
		return errors.New("store is required")
	}
	if reason == "" {
		return errors.New("failure reason is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if item.status != StatusPending {
		return ErrNotPending
	}
	item.status = StatusFailed
	item.failure = reason
	item.closeDone()
	return nil
}

// Delete 从任务表删除任务。若通道尚未关闭，会先关闭它以唤醒等待方。
func (s *Store) Delete(id string) error {
	if s == nil {
		return errors.New("store is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.tasks[id]
	if !ok {
		return ErrNotFound
	}
	item.closeDone()
	item.fileBytes = nil
	delete(s.tasks, id)
	return nil
}

// Sweep 删除创建时间距 now 超过 ttl 的任务，并返回被删除的 ID。
func (s *Store) Sweep(now time.Time, ttl time.Duration) ([]string, error) {
	if s == nil {
		return nil, errors.New("store is required")
	}
	if ttl <= 0 {
		return nil, errors.New("ttl must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id, item := range s.tasks {
		if now.Sub(item.createdAt) > ttl {
			item.closeDone()
			item.fileBytes = nil
			delete(s.tasks, id)
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (item *task) view() View {
	return View{
		ID:       item.id,
		Status:   item.status,
		Content:  item.content,
		Failure:  item.failure,
		HasFile:  item.hasFile,
		FileName: item.fileName,
		FileMIME: item.fileMIME,
		Done:     item.done,
	}
}

func (item *task) closeDone() {
	item.closeOnce.Do(func() {
		close(item.done)
	})
}

func validateResult(result Result) error {
	if result.HasFile {
		if result.FileName == "" || result.FileMIME == "" || result.FileBytes == nil {
			return errors.New("file name, mime, and bytes are required when has_file is true")
		}
		return nil
	}
	if result.FileName != "" || result.FileMIME != "" || result.FileBytes != nil {
		return errors.New("file fields must be empty when has_file is false")
	}
	return nil
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
