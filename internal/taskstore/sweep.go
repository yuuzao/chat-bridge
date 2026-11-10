package taskstore

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// StartSweep 按 interval 周期删除过期任务。参数不合法时返回错误，不启动循环。
func StartSweep(ctx context.Context, store *Store, ttl, interval time.Duration, logger *slog.Logger) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if store == nil {
		return errors.New("store is required")
	}
	if logger == nil {
		return errors.New("logger is required")
	}
	if ttl <= 0 || interval <= 0 {
		return errors.New("ttl and cleanup interval must be positive")
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				ids, err := store.Sweep(now, ttl)
				if err != nil {
					logger.Error("sweep tasks", "error", err.Error())
					continue
				}
				for _, id := range ids {
					logger.Warn("task expired", "request_id", id)
				}
			}
		}
	}()
	return nil
}
