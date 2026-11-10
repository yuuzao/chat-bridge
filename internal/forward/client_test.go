package forward

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	secretCalled := make(chan struct{}, 1)
	secret := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secretCalled <- struct{}{}
	}))
	t.Cleanup(secret.Close)
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "secret" {
			t.Errorf("token = %q", r.Header.Get("X-Auth-Token"))
		}
		http.Redirect(w, r, secret.URL, http.StatusFound)
	}))
	t.Cleanup(bot.Close)
	client, err := New(bot.URL, "secret", time.Second, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	err = client.Send(context.Background(), Message{RequestID: "req", Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("error = %v", err)
	}
	select {
	case <-secretCalled:
		t.Fatal("redirect was followed")
	default:
	}
}

func TestSendPostsJSONWithoutHTMLEscape(t *testing.T) {
	t.Parallel()
	var got []byte
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		got = body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(bot.Close)
	client, err := New(bot.URL, "secret", time.Second, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(context.Background(), Message{RequestID: "req", Content: "<b>"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"<b>"`)) {
		t.Fatalf("body = %s", got)
	}
}

func TestNewRejectsInvalidArguments(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New("", "secret", time.Second, 1, logger); err == nil {
		t.Fatal("expected url error")
	}
	if _, err := New("http://127.0.0.1/bot", "", time.Second, 1, logger); err == nil {
		t.Fatal("expected token error")
	}
	if _, err := New("http://127.0.0.1/bot", "secret", 0, 1, logger); err == nil {
		t.Fatal("expected timeout error")
	}
	if _, err := New("http://user:pass@127.0.0.1/bot", "secret", time.Second, 1, logger); err == nil {
		t.Fatal("expected userinfo error")
	}
	if _, err := New("http://127.0.0.1/bot", "secret", time.Second, 1, nil); err == nil {
		t.Fatal("expected logger error")
	}
}
