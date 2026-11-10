package taskstore

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestCreateAndCompleteText(t *testing.T) {
	t.Parallel()
	store := New()
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != StatusPending || !ValidID(view.ID) {
		t.Fatalf("view = %+v", view)
	}
	done, err := store.Complete(view.ID, Result{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusDone || done.Content != "hello" || done.HasFile {
		t.Fatalf("done = %+v", done)
	}
	select {
	case <-done.Done:
	default:
		t.Fatal("done channel still open")
	}
}

func TestCompleteFileAndDuplicate(t *testing.T) {
	t.Parallel()
	store := New()
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
	file, err := store.File(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Bytes) != string([]byte{1, 2, 3}) || file.Name != "a.png" || file.MIME != "image/png" {
		t.Fatalf("file = %+v", file)
	}
	again, err := store.Complete(view.ID, Result{Content: "other"})
	if !errors.Is(err, ErrNotPending) || again.Content != "pic" {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
	select {
	case <-again.Done:
	default:
		t.Fatal("channel closed twice or left open")
	}
}

func TestFailRequiresReasonAndPending(t *testing.T) {
	t.Parallel()
	store := New()
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
}

func TestMissingAndDelete(t *testing.T) {
	t.Parallel()
	store := New()
	if _, err := store.View("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(view.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(view.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestSweepRemovesExpiredAndWakes(t *testing.T) {
	t.Parallel()
	store := New()
	oldView, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.tasks[oldView.ID].createdAt = time.Now().Add(-2 * time.Hour)
	store.mu.Unlock()
	woken := make(chan struct{})
	go func() {
		<-oldView.Done
		close(woken)
	}()
	ids, err := store.Sweep(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != oldView.ID {
		t.Fatalf("ids = %v", ids)
	}
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("expired waiter was not woken")
	}
	if _, err := store.View(fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sweep(time.Now(), 0); err == nil {
		t.Fatal("expected ttl error")
	}
}

func TestCompleteAndFailRaceClosesOnce(t *testing.T) {
	t.Parallel()
	store := New()
	view, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = store.Fail(view.ID, "forward failed")
	}()
	go func() {
		defer wg.Done()
		_, _ = store.Complete(view.ID, Result{Content: "ok"})
	}()
	wg.Wait()
	got, err := store.View(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone && got.Status != StatusFailed {
		t.Fatalf("status = %s", got.Status)
	}
	select {
	case <-got.Done:
	default:
		t.Fatal("done channel still open")
	}
}

func TestStartSweepRejectsBadArgs(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := StartSweep(nil, New(), time.Second, time.Second, logger); err == nil {
		t.Fatal("expected context error")
	}
	if err := StartSweep(t.Context(), New(), 0, time.Second, logger); err == nil {
		t.Fatal("expected ttl error")
	}
}

func TestRejectsInconsistentFileResult(t *testing.T) {
	t.Parallel()
	store := New()
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
