package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func TestContentSecurityPolicy(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	for _, path := range []string{"/healthz", "/api/v1/meta"} {
		_, _, hdr := req(t, srv, "GET", path, "", token, nil)
		csp := hdr.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows inline or eval: %q", path, csp)
		}
	}
}

// Security: an admin can no longer put a shell task under an application, and
// application limits answer 409 limit_reached.
func TestApplicationLimitsAndShellOverHTTP(t *testing.T) {
	repo := tasks.NewMemoryRepository()
	appRepo := apps.NewMemoryRepository()
	srv := httptest.NewServer(NewRouter(Deps{
		Tasks:      tasks.NewService(repo, tasks.Options{AllowShell: true, AppMaxTasks: 1, AppMaxTimeout: time.Minute}),
		Executions: executions.NewService(executions.NewMemoryRepository(repo.Get), nil),
		Apps:       apps.NewService(appRepo, nil), AdminToken: token,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	defer srv.Close()
	env := testEnv{srv: srv, tasks: repo}
	appID, key := newApp(t, env, "limited")

	shell := `{"application_id":"` + appID + `","name":"sh","runner":{"type":"shell","config":{"command":"id"}}}`
	if code, b, _ := req(t, srv, "POST", "/api/v1/tasks", shell, token, nil); code != 422 {
		t.Fatalf("admin shell task under an application: %d %v", code, b)
	}
	long := strings.Replace(appTask, `"schedule"`, `"timeout_seconds":3600,"schedule"`, 1)
	if code, _, _ := req(t, srv, "POST", "/api/v1/tasks", long, key, nil); code != 422 {
		t.Fatalf("timeout above the application maximum: %d", code)
	}
	if code, _, _ := req(t, srv, "POST", "/api/v1/tasks", appTask, key, nil); code != 201 {
		t.Fatalf("first task: %d", code)
	}
	code, b, _ := req(t, srv, "POST", "/api/v1/tasks", strings.Replace(appTask, "nightly-sync", "second", 1), key, nil)
	if code != 409 || b["error"].(map[string]any)["code"] != "limit_reached" {
		t.Fatalf("second task over the limit: %d %v", code, b)
	}
}
