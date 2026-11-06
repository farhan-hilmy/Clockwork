package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

const token = "test-admin-token-123456"

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

type fakeApps struct{}

func (fakeApps) List(context.Context) ([]apps.Application, error) {
	return []apps.Application{{ID: tasks.DefaultApplicationID, Name: "clockwork", Enabled: true}}, nil
}

func newTestServer(t *testing.T, db Pinger, webDir string) *httptest.Server {
	t.Helper()
	repo := tasks.NewMemoryRepository()
	svc := tasks.NewService(repo, tasks.Options{})
	execs := executions.NewService(executions.NewMemoryRepository(repo.Get), nil)
	h := NewRouter(Deps{
		Tasks: svc, Executions: execs, Apps: fakeApps{}, DB: db, AdminToken: token, WebDir: webDir,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

const validTask = `{"name":"health-check","runner":{"type":"http","config":{"url":"https://api.example.com/healthz","headers":{"Authorization":"Bearer s3cret"}}},"schedule":{"cron":"*/5 * * * *"}}`

func TestHealthAndReadiness(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	if code, _ := call(t, srv, "GET", "/healthz", "", false); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	if code, body := call(t, srv, "GET", "/readyz", "", false); code != 200 || body["database"] != "ok" {
		t.Fatalf("readyz = %d %v", code, body)
	}
	down := newTestServer(t, fakePinger{err: errors.New("connection refused")}, "")
	if code, _ := call(t, down, "GET", "/readyz", "", false); code != 503 {
		t.Fatalf("readyz with db down = %d", code)
	}
}

func TestAPIRequiresBearerToken(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	code, body := call(t, srv, "GET", "/api/v1/tasks", "", false)
	if code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
	if e, _ := body["error"].(map[string]any); e["code"] != "unauthorized" {
		t.Fatalf("bad error envelope: %v", body)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	res, _ := http.DefaultClient.Do(req)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("wrong token accepted: %d", res.StatusCode)
	}
}

func TestTaskLifecycleOverHTTP(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")

	code, created := call(t, srv, "POST", "/api/v1/tasks", validTask, true)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	id, _ := created["id"].(string)
	raw, _ := json.Marshal(created)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatalf("secret in create response: %s", raw)
	}
	if created["next_run_at"] == nil {
		t.Fatalf("next_run_at missing: %v", created)
	}

	if code, _ := call(t, srv, "POST", "/api/v1/tasks", validTask, true); code != 409 {
		t.Fatalf("duplicate create = %d", code)
	}

	if code, got := call(t, srv, "GET", "/api/v1/tasks/"+id, "", true); code != 200 || got["name"] != "health-check" {
		t.Fatalf("get = %d %v", code, got)
	}

	code, upd := call(t, srv, "PATCH", "/api/v1/tasks/"+id, `{"description":"pings the API","schedule":null}`, true)
	if code != 200 || upd["description"] != "pings the API" || upd["schedule"] != nil {
		t.Fatalf("patch = %d %v", code, upd)
	}

	if code, d := call(t, srv, "POST", "/api/v1/tasks/"+id+"/disable", "", true); code != 200 || d["enabled"] != false {
		t.Fatalf("disable = %d %v", code, d)
	}
	if code, e := call(t, srv, "POST", "/api/v1/tasks/"+id+"/enable", "", true); code != 200 || e["enabled"] != true {
		t.Fatalf("enable = %d %v", code, e)
	}

	code, list := call(t, srv, "GET", "/api/v1/tasks?limit=10", "", true)
	if items, _ := list["tasks"].([]any); code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}

	if code, _ := call(t, srv, "DELETE", "/api/v1/tasks/"+id, "", true); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := call(t, srv, "GET", "/api/v1/tasks/"+id, "", true); code != 404 {
		t.Fatalf("get after delete = %d", code)
	}
}

func TestRunNowAndExecutionsOverHTTP(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	_, created := call(t, srv, "POST", "/api/v1/tasks", validTask, true)
	id, _ := created["id"].(string)

	code, run := call(t, srv, "POST", "/api/v1/tasks/"+id+"/run", "", true)
	if code != 202 || run["status"] != "queued" || run["trigger"] != "manual" || run["task_name"] != "health-check" {
		t.Fatalf("run = %d %v", code, run)
	}
	exeID, _ := run["id"].(string)

	if code, got := call(t, srv, "GET", "/api/v1/executions/"+exeID, "", true); code != 200 || got["task_id"] != id {
		t.Fatalf("get execution = %d %v", code, got)
	}
	code, list := call(t, srv, "GET", "/api/v1/executions?task_id="+id+"&status=queued", "", true)
	if items, _ := list["executions"].([]any); code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, list)
	}

	for path, want := range map[string]int{
		"/api/v1/executions/exe_missing":  404,
		"/api/v1/executions?status=bogus": 400,
		"/api/v1/executions?limit=500":    400,
	} {
		if code, _ := call(t, srv, "GET", path, "", true); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
	if code, _ := call(t, srv, "POST", "/api/v1/tasks/tsk_missing/run", "", true); code != 404 {
		t.Errorf("run missing task = %d", code)
	}
	if code, _ := call(t, srv, "POST", "/api/v1/tasks/"+id+"/run", "", false); code != 401 {
		t.Errorf("unauthenticated run = %d", code)
	}
}

func TestValidationErrorsAre422WithFieldDetails(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	code, body := call(t, srv, "POST", "/api/v1/tasks", `{"name":"","runner":{"type":"http","config":{"url":"nope"}},"schedule":{"cron":"bad"}}`, true)
	if code != 422 {
		t.Fatalf("expected 422, got %d", code)
	}
	e := body["error"].(map[string]any)
	details := e["details"].(map[string]any)
	for _, f := range []string{"name", "runner.config.url", "schedule.cron"} {
		if _, ok := details[f]; !ok {
			t.Errorf("missing detail for %s: %v", f, details)
		}
	}
}

func TestMalformedRequests(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	cases := map[string]struct {
		path, body string
		want       int
	}{
		"bad json":      {"/api/v1/tasks", `{"name":`, 400},
		"unknown field": {"/api/v1/tasks", `{"name":"x","oops":true}`, 400},
		"trailing data": {"/api/v1/tasks", `{"name":"x"} {}`, 400},
		"bad limit":     {"/api/v1/tasks?limit=0", "", 400},
		"unknown route": {"/api/v1/nope", "", 404},
	}
	for name, c := range cases {
		method := "POST"
		if c.body == "" {
			method = "GET"
		}
		if code, _ := call(t, srv, method, c.path, c.body, true); code != c.want {
			t.Errorf("%s: got %d want %d", name, code, c.want)
		}
	}
	big := `{"name":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if code, _ := call(t, srv, "POST", "/api/v1/tasks", big, true); code != 413 {
		t.Errorf("oversized body: got %d want 413", code)
	}
}

func TestMetaAndApplications(t *testing.T) {
	srv := newTestServer(t, fakePinger{}, "")
	if code, meta := call(t, srv, "GET", "/api/v1/meta", "", true); code != 200 || meta["shell_enabled"] != false {
		t.Fatalf("meta = %d %v", code, meta)
	}
	if code, a := call(t, srv, "GET", "/api/v1/applications", "", true); code != 200 || len(a["applications"].([]any)) != 1 {
		t.Fatalf("applications = %d %v", code, a)
	}
}

func TestServesSPAWithFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>clockwork</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, fakePinger{}, dir)
	// Path traversal is rejected outright by net/http.
	res, err := http.Get(srv.URL + "/../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode == 200 && !strings.Contains(string(b), "clockwork") {
		t.Fatalf("traversal served unexpected content: %q", b)
	}
	for _, p := range []string{"/", "/tasks/tsk_123"} {
		res, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(b), "clockwork") {
			t.Errorf("%s: %d %q", p, res.StatusCode, b)
		}
	}
}
