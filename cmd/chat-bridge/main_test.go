package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chat_bridge/internal/taskstore"
)

func TestRunRequiresConfigFlag(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := run(ctx, nil, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "-config") {
		t.Fatal(err)
	}
}

func TestRunRejectsExtraArgsAndBadLogLevel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path, _ := writeConfig(t)
	err := run(ctx, []string{"-config", path, "extra"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatal(err)
	}
	err = run(ctx, []string{"-config", path}, func(key string) string {
		if key == "LOG_LEVEL" {
			return "LOUD"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatal(err)
	}
}

func TestRunOpensAndClosesStore(t *testing.T) {
	t.Parallel()
	configPath, dbPath := writeConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-config", configPath}, func(key string) string {
			if key == "LOG_LEVEL" {
				return "ERROR"
			}
			return ""
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(dbPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("sqlite database was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	store, err := taskstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen database after shutdown: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeStopsWhenContextIsCanceled(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), logger)
	}()
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for {
		resp, err = http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.StatusCode)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func writeConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	content := []byte(`listen_addr: "127.0.0.1:0"
auth_token: "secret"
bot_url: "http://127.0.0.1:9/bot"
bot_bearer_token: "bot-secret"
max_body_bytes: 1024
db_path: "` + filepath.Join(dir, "chat-bridge.db") + `"
forward_timeout_seconds: 5
`)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "chat-bridge.db")
}
