// Package config 从 YAML 文件加载 Chat Bridge 的全部业务配置。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是启动时已经校验过的配置。时长字段由秒数换算而来。
type Config struct {
	ListenAddr      string
	AuthToken       string
	BotURL          string
	BotBearerToken  string
	MaxBodyBytes    int64
	TaskTTL         time.Duration
	ForwardTimeout  time.Duration
	CleanupInterval time.Duration
}

type fileConfig struct {
	ListenAddr             string `yaml:"listen_addr"`
	AuthToken              string `yaml:"auth_token"`
	BotURL                 string `yaml:"bot_url"`
	BotBearerToken         string `yaml:"bot_bearer_token"`
	MaxBodyBytes           int64  `yaml:"max_body_bytes"`
	TaskTTLSeconds         int64  `yaml:"task_ttl_seconds"`
	ForwardTimeoutSeconds  int64  `yaml:"forward_timeout_seconds"`
	CleanupIntervalSeconds int64  `yaml:"cleanup_interval_seconds"`
}

// Load 读取 path 指向的 YAML。缺字段、未知字段、重复字段或非法取值都会返回错误。
func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Config{}, err
	}
	var raw fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("config must contain a single document")
		}
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return raw.config()
}

func (raw fileConfig) config() (Config, error) {
	if raw.ListenAddr == "" {
		return Config{}, errors.New("listen_addr is required")
	}
	if raw.AuthToken == "" {
		return Config{}, errors.New("auth_token is required")
	}
	botURL, err := parseBotURL(raw.BotURL)
	if err != nil {
		return Config{}, err
	}
	bearer, err := parseBotBearer(raw.BotBearerToken)
	if err != nil {
		return Config{}, err
	}
	if raw.MaxBodyBytes <= 0 {
		return Config{}, errors.New("max_body_bytes must be a positive integer")
	}
	ttl, err := positiveSeconds("task_ttl_seconds", raw.TaskTTLSeconds)
	if err != nil {
		return Config{}, err
	}
	forwardTimeout, err := positiveSeconds("forward_timeout_seconds", raw.ForwardTimeoutSeconds)
	if err != nil {
		return Config{}, err
	}
	cleanupInterval, err := positiveSeconds("cleanup_interval_seconds", raw.CleanupIntervalSeconds)
	if err != nil {
		return Config{}, err
	}
	return Config{
		ListenAddr:      raw.ListenAddr,
		AuthToken:       raw.AuthToken,
		BotURL:          botURL,
		BotBearerToken:  bearer,
		MaxBodyBytes:    raw.MaxBodyBytes,
		TaskTTL:         ttl,
		ForwardTimeout:  forwardTimeout,
		CleanupInterval: cleanupInterval,
	}, nil
}

func parseBotURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("bot_url is required")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return "", errors.New("bot_url must not contain whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bot_url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("bot_url scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("bot_url host is required")
	}
	if parsed.User != nil {
		return "", errors.New("bot_url must not contain user info")
	}
	if parsed.Fragment != "" {
		return "", errors.New("bot_url must not contain a fragment")
	}
	return raw, nil
}

func parseBotBearer(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("bot_bearer_token is required")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return "", errors.New("bot_bearer_token must not contain whitespace")
	}
	return raw, nil
}

func positiveSeconds(field string, seconds int64) (time.Duration, error) {
	if seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", field)
	}
	maxSeconds := int64(math.MaxInt64 / int64(time.Second))
	if seconds > maxSeconds {
		return 0, fmt.Errorf("%s is too large", field)
	}
	return time.Duration(seconds) * time.Second, nil
}

func rejectDuplicateKeys(data []byte) error {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return errors.New("config root must be a mapping")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return errors.New("config root must be a mapping")
	}
	seen := make(map[string]struct{}, len(root.Content)/2)
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode || key.Value == "" {
			return errors.New("config key must be a non-empty string")
		}
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("duplicate config key %q", key.Value)
		}
		seen[key.Value] = struct{}{}
	}
	return nil
}
