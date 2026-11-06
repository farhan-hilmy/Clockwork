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

	// SchedulerInterval is how often the scheduler looks for due tasks.
	SchedulerInterval time.Duration
	// MisfireGrace is how late an occurrence may be materialised and still count as on time.
	// Later occurrences are misfires and follow the task's misfire policy.
	MisfireGrace time.Duration
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

	cfg.SchedulerInterval = duration(get, "CLOCKWORK_SCHEDULER_INTERVAL", "1s", 100*time.Millisecond, time.Minute, &errs)
	cfg.MisfireGrace = duration(get, "CLOCKWORK_MISFIRE_GRACE", "1m", time.Second, 24*time.Hour, &errs)

	return cfg, errors.Join(errs...)
}

// duration parses a bounded duration variable, appending an error when it is invalid.
func duration(get func(string, string) string, key, def string, lo, hi time.Duration, errs *[]error) time.Duration {
	d, err := time.ParseDuration(get(key, def))
	switch {
	case err != nil:
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
	case d < lo || d > hi:
		*errs = append(*errs, fmt.Errorf("%s must be between %s and %s, got %s", key, lo, hi, d))
	}
	return d
}
