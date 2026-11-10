// Package applog 创建进程日志。业务配置不从环境变量读取；日志级别除外。
package applog

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// New 按 LOG_LEVEL 创建 JSON 日志。
// getenv 由调用方传入。LOG_LEVEL 为空时使用 Info；这是日志级别规则，不是业务参数。
// LOG_LEVEL 有值但无法识别时返回错误。
func New(getenv func(string) string) (*slog.Logger, error) {
	if getenv == nil {
		return nil, errors.New("getenv is required")
	}
	level := slog.LevelInfo
	raw := getenv("LOG_LEVEL")
	if raw != "" {
		if err := level.UnmarshalText([]byte(raw)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}
