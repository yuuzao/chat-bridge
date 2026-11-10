package applog

import (
	"context"
	"log/slog"
	"testing"
)

func TestNewUsesInfoWhenUnset(t *testing.T) {
	t.Parallel()
	logger, err := New(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !logger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info disabled")
	}
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled")
	}
}

func TestNewAcceptsDebug(t *testing.T) {
	t.Parallel()
	logger, err := New(func(key string) string {
		if key == "LOG_LEVEL" {
			return "DEBUG"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug disabled")
	}
}

func TestNewRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	_, err := New(func(key string) string {
		if key == "LOG_LEVEL" {
			return "LOUD"
		}
		return ""
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNewRequiresGetenv(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatal("expected error")
	}
}
