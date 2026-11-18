package taskstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newStore 在 t.TempDir() 里打开真实 SQLite 文件，测试结束后关闭。
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "chat-bridge.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store, path
}

func TestOpenCreatesSchemaAndPragmas(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	if _, err := store.db.Exec(`SELECT count(*) FROM tasks`); err != nil {
		t.Fatalf("tasks table missing: %v", err)
	}
	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q", journalMode)
	}
	var tempStore int
	if err := store.db.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil {
		t.Fatal(err)
	}
	if tempStore != 2 {
		t.Fatalf("temp_store = %d, want 2 (MEMORY)", tempStore)
	}
	var busyTimeout int
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(""); err == nil {
		t.Fatal("expected empty path error")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "a?b.db")); err == nil {
		t.Fatal("expected '?' rejection")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "db.sqlite")); err == nil {
		t.Fatal("expected directory creation error")
	}
}

func TestCreateAndCompleteText(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusPending || !ValidID(view.ID) {
		t.Fatalf("view = %+v", view)
	}
	if view.CreatedAt.IsZero() || view.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %+v", view)
	}
	done, err := store.Complete(view.ID, Result{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusDone || done.Content != "hello" || done.HasFile || done.Failure != "" {
		t.Fatalf("done = %+v", done)
	}
}

func TestCompleteFileAndRepeatReads(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Complete(view.ID, Result{
		Content:   "pic",
		HasFile:   true,
		FileName:  "a.png",
		FileMIME:  "image/png",
		FileBytes: []byte{1, 2, 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.HasFile || first.FileName != "a.png" || first.FileMIME != "image/png" {
		t.Fatalf("first = %+v", first)
	}
	for i := 0; i < 3; i++ {
		file, err := store.File(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(file.Bytes, []byte{1, 2, 3}) || file.Name != "a.png" || file.MIME != "image/png" {
			t.Fatalf("read %d = %+v", i, file)
		}
	}
	again, err := store.Complete(view.ID, Result{Content: "other"})
	if !errors.Is(err, ErrNotPending) || again.Content != "pic" {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
}

func TestFileNotAvailable(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.File(view.ID); !errors.Is(err, ErrFileNotAvailable) {
		t.Fatalf("pending file err = %v", err)
	}
	if _, err := store.Complete(view.ID, Result{Content: "text only"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.File(view.ID); !errors.Is(err, ErrFileNotAvailable) {
		t.Fatalf("text file err = %v", err)
	}
	if _, err := store.File("11111111-1111-4111-8111-111111111111"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestFailRequiresReasonAndPending(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(view.ID, ""); err == nil {
		t.Fatal("expected empty reason error")
	}
	if err := store.Fail(view.ID, "bot down"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.Failure != "bot down" {
		t.Fatalf("failed = %+v", failed)
	}
	if err := store.Fail(view.ID, "again"); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
	if err := store.Fail("11111111-1111-4111-8111-111111111111", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestMissingTask(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	missing := "11111111-1111-4111-8111-111111111111"
	if _, err := store.View(missing); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := store.Complete(missing, Result{Content: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRejectsInconsistentFileResult(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(view.ID, Result{HasFile: true}); err == nil {
		t.Fatal("expected file validation error")
	}
	got, err := store.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestCompleteAndFailRaceAppliesOnce(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	completed := 0
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := store.Complete(view.ID, Result{Content: "ok"}); err == nil {
				mu.Lock()
				completed++
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			_ = store.Fail(view.ID, "forward failed")
		}()
	}
	wg.Wait()
	got, err := store.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone && got.Status != StatusFailed {
		t.Fatalf("status = %s", got.Status)
	}
	if got.Status == StatusFailed && completed != 0 {
		t.Fatalf("completed %d tasks but status is FAILED", completed)
	}
	if got.Status == StatusDone && completed != 1 {
		t.Fatalf("completed = %d, want 1", completed)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "chat-bridge.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := first.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Complete(view.ID, Result{
		Content:   "persisted",
		HasFile:   true,
		FileName:  "b.bin",
		FileMIME:  "application/octet-stream",
		FileBytes: []byte{9, 8, 7},
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	got, err := second.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.Content != "persisted" || got.FileName != "b.bin" {
		t.Fatalf("view after reopen = %+v", got)
	}
	file, err := second.File(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(file.Bytes, []byte{9, 8, 7}) || file.MIME != "application/octet-stream" {
		t.Fatalf("file after reopen = %+v", file)
	}
}

func TestConcurrentWebhookUpdates(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Complete(view.ID, Result{Content: "same"})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 16; i++ {
			if _, err := store.View(view.ID); err != nil {
				t.Errorf("view: %v", err)
			}
		}
	}()
	wg.Wait()
	got, err := store.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.Content != "same" {
		t.Fatalf("view = %+v", got)
	}
}

func TestClosedStoreRejectsQueries(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "chat-bridge.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(); err == nil {
		t.Fatal("expected error after close")
	}
}

func TestValidID(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(view.ID) {
		t.Fatalf("generated id %q is not valid", view.ID)
	}
	for _, bad := range []string{"", "missing", "11111111-1111-1111-8111-111111111111", "11111111-1111-4111-8111-11111111111z"} {
		if ValidID(bad) {
			t.Fatalf("accepted invalid id %q", bad)
		}
	}
}
