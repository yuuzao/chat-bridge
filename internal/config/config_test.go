package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAcceptsCompleteFile(t *testing.T) {
	t.Parallel()
	cfg, err := Load(writeConfig(t, validYAML()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" || cfg.AuthToken != "secret" || cfg.BotURL != "http://127.0.0.1:9/bot?x=1" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.MaxBodyBytes != 1024 || cfg.TaskTTL != 10*time.Second || cfg.ForwardTimeout != 5*time.Second || cfg.CleanupInterval != 2*time.Second {
		t.Fatalf("numeric config = %+v", cfg)
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "missing listen", yaml: strings.Replace(validYAML(), "listen_addr: \"127.0.0.1:8080\"\n", "", 1), wantErr: "listen_addr"},
		{name: "empty token", yaml: strings.Replace(validYAML(), "auth_token: \"secret\"", "auth_token: \"\"", 1), wantErr: "auth_token"},
		{name: "zero body", yaml: strings.Replace(validYAML(), "max_body_bytes: 1024", "max_body_bytes: 0", 1), wantErr: "max_body_bytes"},
		{name: "negative ttl", yaml: strings.Replace(validYAML(), "task_ttl_seconds: 10", "task_ttl_seconds: -1", 1), wantErr: "task_ttl_seconds"},
		{name: "unknown field", yaml: validYAML() + "extra: 1\n", wantErr: "extra"},
		{name: "duplicate key", yaml: "listen_addr: \"127.0.0.1:1\"\n" + validYAML(), wantErr: "duplicate"},
		{name: "userinfo", yaml: strings.Replace(validYAML(), "http://127.0.0.1:9/bot?x=1", "http://user:pass@127.0.0.1:9/bot", 1), wantErr: "user info"},
		{name: "fragment", yaml: strings.Replace(validYAML(), "http://127.0.0.1:9/bot?x=1", "http://127.0.0.1:9/bot#frag", 1), wantErr: "fragment"},
		{name: "ftp", yaml: strings.Replace(validYAML(), "http://127.0.0.1:9/bot?x=1", "ftp://127.0.0.1:9/bot", 1), wantErr: "scheme"},
		{name: "whitespace", yaml: strings.Replace(validYAML(), "http://127.0.0.1:9/bot?x=1", "http://127.0.0.1:9/bot path", 1), wantErr: "whitespace"},
		{name: "wrong type", yaml: strings.Replace(validYAML(), "max_body_bytes: 1024", "max_body_bytes: big", 1), wantErr: "parse config"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeConfig(t, test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadMissingPath(t *testing.T) {
	t.Parallel()
	if _, err := Load(""); err == nil {
		t.Fatal("expected error")
	}
}

func validYAML() string {
	return `listen_addr: "127.0.0.1:8080"
auth_token: "secret"
bot_url: "http://127.0.0.1:9/bot?x=1"
max_body_bytes: 1024
task_ttl_seconds: 10
forward_timeout_seconds: 5
cleanup_interval_seconds: 2
`
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
