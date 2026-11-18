// Package taskstore 把任务持久化到 SQLite。任务不会因为被读取或下载而删除，进程重启后仍可读取。
package taskstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status 是任务在数据库中的状态。
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
	// ErrNotFound 表示任务不在数据库中。
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
	ID        string
	Status    Status
	Content   string
	Failure   string
	HasFile   bool
	FileName  string
	FileMIME  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// File 是可写回客户端的文件内容。
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

// Store 是 SQLite 中的任务表。
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	request_id TEXT PRIMARY KEY,
	status     TEXT NOT NULL,
	content    TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT '',
	has_file   INTEGER NOT NULL DEFAULT 0,
	file_name  TEXT NOT NULL DEFAULT '',
	file_mime  TEXT NOT NULL DEFAULT '',
	file_bytes BLOB,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
)`

// 连接池上限。写入由 SQLite 串行化，busy_timeout 负责等待，避免并发下直接返回 SQLITE_BUSY。
const (
	maxOpenConns = 8
	maxIdleConns = 8
	busyTimeout  = "5000"
)

// Open 打开 path 指向的 SQLite 数据库，必要时创建父目录，并确保任务表存在。
// path 不能为空，也不能包含 ?（会被驱动当作 DSN 参数分隔符）。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("db path is required")
	}
	if strings.ContainsRune(path, '?') {
		return nil, errors.New("db path must not contain '?'")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close sqlite: %w", closeErr))
		}
		return nil, err
	}
	return store, nil
}

// dataSourceName 组装 DSN。PRAGMA 由驱动在每条新连接上执行，因此连接池里的连接都带同样的设置。
// temp_store=MEMORY 让临时文件不进磁盘，scratch 镜像里没有 /tmp。
func dataSourceName(path string) string {
	params := []string{
		"_pragma=busy_timeout(" + busyTimeout + ")",
		"_pragma=temp_store(MEMORY)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
	}
	return path + "?" + strings.Join(params, "&")
}

// init 建立连接并确保表结构存在。
func (s *Store) init() error {
	if err := s.db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create tasks table: %w", err)
	}
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return errors.New("store is required")
	}
	return s.db.Close()
}

// Create 生成任务 ID，并以 PENDING 写入数据库。
func (s *Store) Create() (View, error) {
	if s == nil || s.db == nil {
		return View{}, errors.New("store is required")
	}
	id, err := newID()
	if err != nil {
		return View{}, err
	}
	now := time.Now()
	_, err = s.db.Exec(
		`INSERT INTO tasks (request_id, status, content, error, has_file, file_name, file_mime, file_bytes, created_at, updated_at)
		 VALUES (?, ?, '', '', 0, '', '', NULL, ?, ?)`,
		id, string(StatusPending), now.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return View{}, fmt.Errorf("insert task: %w", err)
	}
	return s.View(id)
}

// View 返回任务快照。
func (s *Store) View(id string) (View, error) {
	if s == nil || s.db == nil {
		return View{}, errors.New("store is required")
	}
	row := s.db.QueryRow(
		`SELECT request_id, status, content, error, has_file, file_name, file_mime, created_at, updated_at
		 FROM tasks WHERE request_id = ?`, id,
	)
	return scanView(row)
}

// File 返回已完成且带文件的任务内容。
func (s *Store) File(id string) (File, error) {
	if s == nil || s.db == nil {
		return File{}, errors.New("store is required")
	}
	var status string
	var hasFile int
	var file File
	err := s.db.QueryRow(
		`SELECT status, has_file, file_name, file_mime, file_bytes FROM tasks WHERE request_id = ?`, id,
	).Scan(&status, &hasFile, &file.Name, &file.MIME, &file.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("query task file: %w", err)
	}
	if Status(status) != StatusDone || hasFile != 1 {
		return File{}, ErrFileNotAvailable
	}
	if file.Name == "" || file.MIME == "" || file.Bytes == nil {
		return File{}, fmt.Errorf("task %s is DONE with has_file but its file is incomplete", id)
	}
	return file, nil
}

// Complete 用一条带 status = 'PENDING' 条件的 UPDATE 把任务写成 DONE。
// 任务已不在 PENDING 时返回当前快照和 ErrNotPending，不覆写已有结果。
func (s *Store) Complete(id string, result Result) (View, error) {
	if s == nil || s.db == nil {
		return View{}, errors.New("store is required")
	}
	if err := validateResult(result); err != nil {
		return View{}, err
	}
	var fileBytes any
	if result.HasFile {
		fileBytes = result.FileBytes
	}
	done, err := s.updatePending(id, `status = ?, content = ?, error = '', has_file = ?, file_name = ?, file_mime = ?, file_bytes = ?`,
		string(StatusDone), result.Content, boolToInt(result.HasFile), result.FileName, result.FileMIME, fileBytes)
	if err != nil {
		return View{}, err
	}
	if done {
		return s.View(id)
	}
	view, err := s.View(id)
	if err != nil {
		return View{}, err
	}
	return view, ErrNotPending
}

// Fail 把 PENDING 任务写成 FAILED。reason 会原样保存。
func (s *Store) Fail(id string, reason string) error {
	if s == nil || s.db == nil {
		return errors.New("store is required")
	}
	if reason == "" {
		return errors.New("failure reason is required")
	}
	done, err := s.updatePending(id, `status = ?, error = ?, has_file = 0, file_name = '', file_mime = '', file_bytes = NULL`,
		string(StatusFailed), reason)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if _, err := s.View(id); err != nil {
		return err
	}
	return ErrNotPending
}

// updatePending 以 status = 'PENDING' 为条件更新任务，返回是否命中。
// 未命中时调用方需要再查一次任务是否存在，区分 ErrNotFound 与 ErrNotPending。
func (s *Store) updatePending(id, assignments string, args ...any) (bool, error) {
	now := time.Now()
	args = append(args, now.UnixNano(), id, string(StatusPending))
	res, err := s.db.Exec(
		`UPDATE tasks SET `+assignments+`, updated_at = ? WHERE request_id = ? AND status = ?`,
		args...,
	)
	if err != nil {
		return false, fmt.Errorf("update task: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update task rows: %w", err)
	}
	return rows == 1, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanView(row rowScanner) (View, error) {
	var view View
	var status string
	var hasFile int
	var createdAt, updatedAt int64
	err := row.Scan(
		&view.ID, &status, &view.Content, &view.Failure, &hasFile,
		&view.FileName, &view.FileMIME, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("query task: %w", err)
	}
	switch Status(status) {
	case StatusPending, StatusDone, StatusFailed:
		view.Status = Status(status)
	default:
		return View{}, fmt.Errorf("task %s has unknown status %q", view.ID, status)
	}
	view.HasFile = hasFile == 1
	view.CreatedAt = time.Unix(0, createdAt)
	view.UpdatedAt = time.Unix(0, updatedAt)
	return view, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
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
