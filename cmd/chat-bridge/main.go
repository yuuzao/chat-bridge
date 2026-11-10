package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"chat_bridge/internal/applog"
	"chat_bridge/internal/config"
	"chat_bridge/internal/forward"
	"chat_bridge/internal/httpserver"
	"chat_bridge/internal/taskstore"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		slog.Error("process stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if getenv == nil {
		return errors.New("getenv is required")
	}
	configPath, err := parseArgs(args, os.Stderr)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger, err := applog.New(getenv)
	if err != nil {
		return err
	}
	store := taskstore.New()
	fwd, err := forward.New(cfg.BotURL, cfg.AuthToken, cfg.ForwardTimeout, cfg.MaxBodyBytes, logger)
	if err != nil {
		return err
	}
	api, err := httpserver.New(cfg, store, fwd, logger)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	botURL, err := url.Parse(cfg.BotURL)
	if err != nil {
		if closeErr := ln.Close(); closeErr != nil {
			logger.Error("close listener", "error", closeErr.Error())
		}
		return fmt.Errorf("bot_url: %w", err)
	}
	logger.Info("listening",
		"listen_addr", ln.Addr().String(),
		"bot_host", botURL.Host,
		"bot_path", botURL.EscapedPath(),
		"max_body_bytes", cfg.MaxBodyBytes,
		"task_ttl", cfg.TaskTTL.String(),
		"forward_timeout", cfg.ForwardTimeout.String(),
		"cleanup_interval", cfg.CleanupInterval.String(),
		"git_commit", getenv("GIT_COMMIT"),
	)
	if err := taskstore.StartSweep(ctx, store, cfg.TaskTTL, cfg.CleanupInterval, logger); err != nil {
		if closeErr := ln.Close(); closeErr != nil {
			logger.Error("close listener", "error", closeErr.Error())
		}
		return err
	}
	return serve(ctx, ln, api.Handler(), logger)
}

func parseArgs(args []string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet("chat-bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *configPath == "" {
		return "", errors.New("flag -config is required")
	}
	if fs.NArg() != 0 {
		return "", errors.New("unexpected arguments")
	}
	return *configPath, nil
}

func serve(ctx context.Context, ln net.Listener, handler http.Handler, logger *slog.Logger) error {
	if ctx == nil || ln == nil || handler == nil || logger == nil {
		return errors.New("context, listener, handler, and logger are required")
	}
	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		if err := server.Close(); err != nil {
			logger.Error("close http server", "error", err.Error())
			return err
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
