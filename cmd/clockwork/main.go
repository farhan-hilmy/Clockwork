// Command clockwork runs the Clockwork server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA timezone database so schedules work in minimal containers

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/config"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/executor"
	"github.com/farhan-hilmy/clockwork/internal/httpapi"
	"github.com/farhan-hilmy/clockwork/internal/runners"
	"github.com/farhan-hilmy/clockwork/internal/scheduler"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "clockwork:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	logger.Info("database ready")

	taskSvc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{AllowShell: cfg.EnableShell})
	if cfg.EnableShell {
		logger.Warn("shell runner is ENABLED: anyone with the admin token can run commands as this process's user")
	}

	run := map[tasks.RunnerType]runners.Runner{
		tasks.RunnerHTTP: runners.NewHTTP(runners.HTTPOptions{DenyPrivate: cfg.HTTPDenyPrivate, UserAgent: "Clockwork/" + version}),
	}
	if cfg.EnableShell {
		run[tasks.RunnerShell] = &runners.Shell{}
	}
	if cfg.HTTPDenyPrivate {
		logger.Info("HTTP tasks may only reach public addresses (CLOCKWORK_HTTP_DENY_PRIVATE=true)")
	}

	// The scheduler and executor stop with ctx (SIGTERM). Wait for both before the
	// database pool closes: the scheduler's pass finishes or rolls back, and the
	// executor records every in-flight run (interrupting them after the grace period).
	sched := scheduler.New(pool, scheduler.Options{
		Interval:     cfg.SchedulerInterval,
		MisfireGrace: cfg.MisfireGrace,
		Logger:       logger,
	})
	exec := executor.New(pool, executor.Options{
		Workers:       cfg.Workers,
		ShutdownGrace: cfg.ShutdownTimeout,
		Runners:       run,
		Logger:        logger,
		// Applications' HTTP tasks may only reach public addresses unless the operator allows more.
		AppsMayReachPrivate: cfg.AppHTTPAllowPrivate,
	})
	var background sync.WaitGroup
	background.Go(func() { sched.Run(ctx) })
	background.Go(func() { exec.Run(ctx) })
	defer func() { stop(); background.Wait() }() // stop() also covers returning on an HTTP server error

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Tasks:      taskSvc,
			Executions: executions.NewService(executions.NewPostgresRepository(pool), nil),
			Apps:       apps.NewService(apps.NewPostgresRepository(pool), nil),
			DB:         pool,
			AdminToken: cfg.AdminToken,
			WebDir:     cfg.WebDir,
			Logger:     logger,
			Version:    version,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr, "version", version, "web_ui", cfg.WebDir != "")
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
