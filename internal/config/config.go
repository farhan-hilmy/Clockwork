// Package config loads Clockwork's runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully validated runtime configuration.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	AdminToken      string
	WebDir          string
	LogLevel        slog.Level
	LogFormat       string // "json" or "text"
	EnableShell     bool
	ShutdownTimeout time.Duration
}

// Load reads configuration using getenv (os.Getenv in production) and validates it.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	var errs []error
	cfg := Config{
		HTTPAddr:    get("CLOCKWORK_HTTP_ADDR", ":8080"),
		DatabaseURL: get("CLOCKWORK_DATABASE_URL", ""),
		AdminToken:  get("CLOCKWORK_ADMIN_TOKEN", ""),
		WebDir:      get("CLOCKWORK_WEB_DIR", ""),
		LogFormat:   strings.ToLower(get("CLOCKWORK_LOG_FORMAT", "json")),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("CLOCKWORK_DATABASE_URL is required"))
	}
	if cfg.AdminToken == "" {
		errs = append(errs, errors.New("CLOCKWORK_ADMIN_TOKEN is required (every API call must be authenticated)"))
	} else if len(cfg.AdminToken) < 16 {
		errs = append(errs, errors.New("CLOCKWORK_ADMIN_TOKEN must be at least 16 characters"))
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("CLOCKWORK_LOG_FORMAT must be json or text, got %q", cfg.LogFormat))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("CLOCKWORK_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_LOG_LEVEL: %w", err))
	}

	shell, err := strconv.ParseBool(get("CLOCKWORK_ENABLE_SHELL", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_ENABLE_SHELL: %w", err))
	}
	cfg.EnableShell = shell

	cfg.ShutdownTimeout, err = time.ParseDuration(get("CLOCKWORK_SHUTDOWN_TIMEOUT", "30s"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_SHUTDOWN_TIMEOUT: %w", err))
	}

	return cfg, errors.Join(errs...)
}
