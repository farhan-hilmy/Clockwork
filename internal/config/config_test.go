package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"CLOCKWORK_DATABASE_URL": "postgres://x",
		"CLOCKWORK_ADMIN_TOKEN":  "0123456789abcdef",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.EnableShell || cfg.ShutdownTimeout != 30*time.Second || cfg.LogFormat != "json" ||
		cfg.SchedulerInterval != time.Second || cfg.MisfireGrace != time.Minute || cfg.Workers != 4 || cfg.HTTPDenyPrivate {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadRequiresDatabaseAndStrongToken(t *testing.T) {
	_, err := Load(env(map[string]string{"CLOCKWORK_ADMIN_TOKEN": "short"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"CLOCKWORK_DATABASE_URL", "at least 16"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	_, err := Load(env(map[string]string{
		"CLOCKWORK_DATABASE_URL":       "postgres://x",
		"CLOCKWORK_ADMIN_TOKEN":        "0123456789abcdef",
		"CLOCKWORK_ENABLE_SHELL":       "maybe",
		"CLOCKWORK_SHUTDOWN_TIMEOUT":   "soon",
		"CLOCKWORK_LOG_FORMAT":         "xml",
		"CLOCKWORK_SCHEDULER_INTERVAL": "1h",
		"CLOCKWORK_MISFIRE_GRACE":      "later",
		"CLOCKWORK_WORKERS":            "0",
		"CLOCKWORK_HTTP_DENY_PRIVATE":  "sometimes",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"ENABLE_SHELL", "SHUTDOWN_TIMEOUT", "LOG_FORMAT", "SCHEDULER_INTERVAL must be between", "MISFIRE_GRACE", "WORKERS", "HTTP_DENY_PRIVATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestAppPrivateAccessCannotContradictGlobalDeny(t *testing.T) {
	base := map[string]string{"CLOCKWORK_DATABASE_URL": "postgres://x", "CLOCKWORK_ADMIN_TOKEN": "0123456789abcdef"}
	base["CLOCKWORK_APP_HTTP_ALLOW_PRIVATE"] = "true"
	if cfg, err := Load(env(base)); err != nil || !cfg.AppHTTPAllowPrivate {
		t.Fatalf("allow alone: %+v %v", cfg, err)
	}
	base["CLOCKWORK_HTTP_DENY_PRIVATE"] = "true"
	if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "cannot be true while") {
		t.Fatalf("contradiction accepted: %v", err)
	}
}
