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

	// Workers is how many executions may run at once.
	Workers int
	// HTTPDenyPrivate makes HTTP tasks refuse loopback, link-local and private targets.
	HTTPDenyPrivate bool
	// AppHTTPAllowPrivate lets tasks owned by external applications reach private addresses.
	AppHTTPAllowPrivate bool
	// Retention is how long finished run history and webhook deliveries are kept.
	// Zero keeps them forever.
	Retention time.Duration

	// Limits for applications (API-key holders). The default application is exempt.
	AppMaxTasks    int           // tasks per application
	AppMaxWebhooks int           // webhook endpoints per application
	AppMaxTimeout  time.Duration // longest timeout an application task may have
	AppMaxRunning  int           // executions of one application running at once
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

	cfg.Workers, err = strconv.Atoi(get("CLOCKWORK_WORKERS", "4"))
	if err != nil || cfg.Workers < 1 || cfg.Workers > 256 {
		errs = append(errs, fmt.Errorf("CLOCKWORK_WORKERS must be an integer between 1 and 256"))
	}

	cfg.HTTPDenyPrivate, err = strconv.ParseBool(get("CLOCKWORK_HTTP_DENY_PRIVATE", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_HTTP_DENY_PRIVATE: %w", err))
	}

	cfg.AppHTTPAllowPrivate, err = strconv.ParseBool(get("CLOCKWORK_APP_HTTP_ALLOW_PRIVATE", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_APP_HTTP_ALLOW_PRIVATE: %w", err))
	}
	if cfg.HTTPDenyPrivate && cfg.AppHTTPAllowPrivate {
		errs = append(errs, errors.New("CLOCKWORK_APP_HTTP_ALLOW_PRIVATE cannot be true while CLOCKWORK_HTTP_DENY_PRIVATE is true"))
	}

	cfg.AppMaxTasks = positiveInt(get, "CLOCKWORK_APP_MAX_TASKS", "500", &errs)
	cfg.AppMaxWebhooks = positiveInt(get, "CLOCKWORK_APP_MAX_WEBHOOKS", "20", &errs)
	cfg.AppMaxRunning = positiveInt(get, "CLOCKWORK_APP_MAX_RUNNING", "2", &errs)
	cfg.AppMaxTimeout = duration(get, "CLOCKWORK_APP_MAX_TIMEOUT", "5m", time.Second, 24*time.Hour, &errs)

	if cfg.Retention, err = ParseRetention(get("CLOCKWORK_RETENTION", "")); err != nil {
		errs = append(errs, fmt.Errorf("CLOCKWORK_RETENTION: %w", err))
	}

	return cfg, errors.Join(errs...)
}

func positiveInt(get func(string, string) string, key, def string, errs *[]error) int {
	n, err := strconv.Atoi(get(key, def))
	if err != nil || n < 1 {
		*errs = append(*errs, fmt.Errorf("%s must be a whole number of at least 1", key))
	}
	return n
}

// MinRetention guards against a typo such as "90m" wiping history.
const MinRetention = 24 * time.Hour

// ParseRetention reads a retention period: empty, "0" or "off" for none,
// whole days such as "90d", or a Go duration such as "36h".
func ParseRetention(v string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "off":
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(strings.TrimSpace(v), "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a number of days such as 90d", v)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(strings.TrimSpace(v)); err != nil {
			return 0, fmt.Errorf("%q is not a period such as 90d or 36h, or off", v)
		}
	}
	if d < MinRetention {
		return 0, fmt.Errorf("must be at least 24h (got %s), or off", v)
	}
	return d, nil
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
