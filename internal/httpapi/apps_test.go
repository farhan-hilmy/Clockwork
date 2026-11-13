package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// req sends a request with an explicit bearer token and extra headers.
func req(t *testing.T, srv *httptest.Server, method, path, body, bearer string, hdr map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, _ := http.NewRequest(method, srv.URL+path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, res.Header
}

// newApp registers an application through the API and returns its ID and a fresh API key.
func newApp(t *testing.T, env testEnv, name string) (id, key string) {
	t.Helper()
	code, a, _ := req(t, env.srv, "POST", "/api/v1/applications", `{"name":"`+name+`"}`, token, nil)
	if code != 201 {
		t.Fatalf("create app %s: %d %v", name, code, a)
	}
	id = a["id"].(string)
	env.tasks.AddApplication(id) // the in-memory task store learns about it, as the shared database would
	code, k, hdr := req(t, env.srv, "POST", "/api/v1/applications/"+id+"/keys", `{"name":"test"}`, token, nil)
	if code != 201 || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("create key: %d %v", code, k)
	}
	return id, k["key"].(string)
}

const appTask = `{"name":"nightly-sync","runner":{"type":"http","config":{"url":"https://api.example.com/sync"}},"schedule":{"cron":"0 2 * * *"}}`

func ids(t *testing.T, list any) []string {
	t.Helper()
	var out []string
	for _, x := range list.([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

// The M5 exit criterion: app A cannot see app B's tasks or executions.
func TestApplicationsCannotSeeEachOther(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	appA, keyA := newApp(t, env, "app-a")
	appB, keyB := newApp(t, env, "app-b")

	// Both create a task with the same name: names are unique per application only.
	code, ta, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, keyA, nil)
	if code != 201 || ta["application_id"] != appA {
		t.Fatalf("A create: %d %v", code, ta)
	}
	code, tb, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, keyB, nil)
	if code != 201 || tb["application_id"] != appB {
		t.Fatalf("B create: %d %v", code, tb)
	}
	taskA, taskB := ta["id"].(string), tb["id"].(string)
	code, ea, _ := req(t, env.srv, "POST", "/api/v1/tasks/"+taskA+"/run", "", keyA, nil)
	if code != 202 {
		t.Fatalf("A run: %d %v", code, ea)
	}
	execA := ea["id"].(string)

	// Lists only show the caller's own resources, even when asking for the other app's.
	for _, path := range []string{"/api/v1/tasks", "/api/v1/tasks?application_id=" + appB} {
		_, list, _ := req(t, env.srv, "GET", path, "", keyA, nil)
		if got := ids(t, list["tasks"]); len(got) != 1 || got[0] != taskA {
			t.Fatalf("A lists %s: %v", path, got)
		}
	}
	_, list, _ := req(t, env.srv, "GET", "/api/v1/executions", "", keyB, nil)
	if got := list["executions"].([]any); len(got) != 0 {
		t.Fatalf("B sees executions: %v", got)
	}
	_, list, _ = req(t, env.srv, "GET", "/api/v1/executions?task_id="+taskA, "", keyB, nil)
	if got := list["executions"].([]any); len(got) != 0 {
		t.Fatalf("B sees A's executions by task_id: %v", got)
	}

	// Every way of touching the other app's resources answers 404, as if they did not exist.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tasks/" + taskA, ""},
		{"PATCH", "/api/v1/tasks/" + taskA, `{"description":"hijacked"}`},
		{"DELETE", "/api/v1/tasks/" + taskA, ""},
		{"POST", "/api/v1/tasks/" + taskA + "/disable", ""},
		{"POST", "/api/v1/tasks/" + taskA + "/enable", ""},
		{"POST", "/api/v1/tasks/" + taskA + "/run", ""},
		{"GET", "/api/v1/executions/" + execA, ""},
	} {
		if code, body, _ := req(t, env.srv, c.method, c.path, c.body, keyB, nil); code != 404 {
			t.Errorf("B %s %s = %d %v, want 404", c.method, c.path, code, body)
		}
	}
	// Nothing B tried changed A's task.
	if _, got, _ := req(t, env.srv, "GET", "/api/v1/tasks/"+taskA, "", keyA, nil); got["description"] != "" || got["enabled"] != true {
		t.Fatalf("A's task was modified: %v", got)
	}
	if _, got, _ := req(t, env.srv, "GET", "/api/v1/executions?task_id="+taskA, "", keyA, nil); len(got["executions"].([]any)) != 1 {
		t.Fatalf("B queued a run of A's task: %v", got)
	}

	// An application cannot create tasks for another one.
	body := strings.Replace(appTask, `{"name":"nightly-sync"`, `{"application_id":"`+appA+`","name":"sneaky"`, 1)
	if code, b, _ := req(t, env.srv, "POST", "/api/v1/tasks", body, keyB, nil); code != 422 {
		t.Fatalf("B creates task for A: %d %v", code, b)
	}

	// The admin sees everything.
	_, list, _ = req(t, env.srv, "GET", "/api/v1/tasks", "", token, nil)
	if got := ids(t, list["tasks"]); len(got) != 2 {
		t.Fatalf("admin task list: %v", got)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/tasks/"+taskB, "", token, nil); code != 200 {
		t.Fatalf("admin get B's task: %d", code)
	}
}

func TestApplicationRestrictions(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", true) // shell enabled for the operator
	_, key := newApp(t, env, "app-a")

	shell := `{"name":"sh","runner":{"type":"shell","config":{"command":"id"}}}`
	if code, b, _ := req(t, env.srv, "POST", "/api/v1/tasks", shell, key, nil); code != 422 || !strings.Contains(b["error"].(map[string]any)["details"].(map[string]any)["runner.type"].(string), "shell") {
		t.Fatalf("app creates shell task: %d %v", code, b)
	}
	if code, _, _ := req(t, env.srv, "POST", "/api/v1/tasks", shell, token, nil); code != 201 {
		t.Fatalf("admin creates shell task: %d", code)
	}
	_, task, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, key, nil)
	if code, _, _ := req(t, env.srv, "PATCH", "/api/v1/tasks/"+task["id"].(string), `{"runner":{"type":"shell","config":{"command":"id"}}}`, key, nil); code != 422 {
		t.Fatalf("app switches task to shell: %d", code)
	}

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/applications"},
		{"POST", "/api/v1/applications"},
		{"GET", "/api/v1/applications/app_clockwork"},
		{"GET", "/api/v1/applications/app_clockwork/keys"},
	} {
		if code, _, _ := req(t, env.srv, c.method, c.path, `{"name":"x"}`, key, nil); code != 403 {
			t.Errorf("app %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	_, meta, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key, nil)
	who := meta["principal"].(map[string]any)
	if who["type"] != "application" || who["application_name"] != "app-a" || meta["shell_enabled"] != false {
		t.Fatalf("app meta: %v", meta)
	}
	_, meta, _ = req(t, env.srv, "GET", "/api/v1/meta", "", token, nil)
	if meta["principal"].(map[string]any)["type"] != "admin" || meta["shell_enabled"] != true {
		t.Fatalf("admin meta: %v", meta)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	appID, key := newApp(t, env, "billing")
	if !strings.HasPrefix(key, "cwk_") || len(key) < 50 {
		t.Fatalf("key format: %q", key)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key, nil); code != 200 {
		t.Fatalf("new key rejected: %d", code)
	}

	// The key is never shown again; lists carry only a prefix.
	_, keys, _ := req(t, env.srv, "GET", "/api/v1/applications/"+appID+"/keys", "", token, nil)
	k := keys["keys"].([]any)[0].(map[string]any)
	if _, leaked := k["key"]; leaked || !strings.HasPrefix(key, k["prefix"].(string)) || k["last_used_at"] == nil {
		t.Fatalf("key listing: %v", k)
	}

	// Rotation: a second key works alongside the first until the first is revoked.
	_, k2, _ := req(t, env.srv, "POST", "/api/v1/applications/"+appID+"/keys", `{"name":"rotated"}`, token, nil)
	key2 := k2["key"].(string)
	if code, _, _ := req(t, env.srv, "DELETE", "/api/v1/applications/"+appID+"/keys/"+k["id"].(string), "", token, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key, nil); code != 401 {
		t.Fatalf("revoked key accepted: %d", code)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key2, nil); code != 200 {
		t.Fatalf("rotated key rejected: %d", code)
	}

	// Disabling the application blocks its keys; enabling restores them.
	req(t, env.srv, "PATCH", "/api/v1/applications/"+appID, `{"enabled":false}`, token, nil)
	if code, b, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key2, nil); code != 403 || b["error"].(map[string]any)["code"] != "application_disabled" {
		t.Fatalf("disabled app key: %d %v", code, b)
	}
	req(t, env.srv, "PATCH", "/api/v1/applications/"+appID, `{"enabled":true}`, token, nil)
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key2, nil); code != 200 {
		t.Fatalf("re-enabled app key: %d", code)
	}

	// An application with tasks cannot be deleted; once empty it can, and its keys die with it.
	_, task, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, key2, nil)
	if code, _, _ := req(t, env.srv, "DELETE", "/api/v1/applications/"+appID, "", token, nil); code != 409 {
		t.Fatalf("delete app with tasks: %d", code)
	}
	req(t, env.srv, "DELETE", "/api/v1/tasks/"+task["id"].(string), "", key2, nil)
	if code, _, _ := req(t, env.srv, "DELETE", "/api/v1/applications/"+appID, "", token, nil); code != 204 {
		t.Fatalf("delete empty app: %d", code)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", key2, nil); code != 401 {
		t.Fatalf("key of deleted app accepted: %d", code)
	}

	// Malformed and unknown keys.
	for _, bad := range []string{"cwk_", "cwk_notarealkey", strings.Repeat("x", 300), "Basic abc"} {
		if code, _, _ := req(t, env.srv, "GET", "/api/v1/meta", "", bad, nil); code != 401 {
			t.Errorf("bad key %q = %d", bad[:min(len(bad), 12)], code)
		}
	}
}

func TestDefaultApplicationIsProtected(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/api/v1/applications/app_clockwork", `{"enabled":false}`},
		{"PATCH", "/api/v1/applications/app_clockwork", `{"name":"renamed"}`},
		{"DELETE", "/api/v1/applications/app_clockwork", ""},
		{"POST", "/api/v1/applications/app_clockwork/keys", `{"name":"k"}`},
	} {
		if code, _, _ := req(t, env.srv, c.method, c.path, c.body, token, nil); code != 409 {
			t.Errorf("%s %s = %d, want 409", c.method, c.path, code)
		}
	}
	if code, a, _ := req(t, env.srv, "PATCH", "/api/v1/applications/app_clockwork", `{"description":"ours"}`, token, nil); code != 200 || a["description"] != "ours" {
		t.Fatalf("describe default app: %d %v", code, a)
	}
	if code, b, _ := req(t, env.srv, "POST", "/api/v1/applications", `{"name":"Not Valid!"}`, token, nil); code != 422 {
		t.Fatalf("invalid app name: %d %v", code, b)
	}
	newApp(t, env, "dup")
	if code, _, _ := req(t, env.srv, "POST", "/api/v1/applications", `{"name":"dup"}`, token, nil); code != 409 {
		t.Fatalf("duplicate app name: %d", code)
	}
}

func TestIdempotentRunNow(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	_, keyA := newApp(t, env, "app-a")
	_, keyB := newApp(t, env, "app-b")
	_, t1, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, keyA, nil)
	_, t2, _ := req(t, env.srv, "POST", "/api/v1/tasks", strings.Replace(appTask, "nightly-sync", "other", 1), keyA, nil)
	_, tb, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, keyB, nil)
	idem := map[string]string{"Idempotency-Key": "order-42"}

	code, first, _ := req(t, env.srv, "POST", "/api/v1/tasks/"+t1["id"].(string)+"/run", "", keyA, idem)
	if code != 202 || first["idempotency_key"] != "order-42" {
		t.Fatalf("first run: %d %v", code, first)
	}
	code, again, hdr := req(t, env.srv, "POST", "/api/v1/tasks/"+t1["id"].(string)+"/run", "", keyA, idem)
	if code != 200 || hdr.Get("Idempotent-Replayed") != "true" || again["id"] != first["id"] {
		t.Fatalf("replay: %d %v %v", code, again, hdr)
	}
	if code, b, _ := req(t, env.srv, "POST", "/api/v1/tasks/"+t2["id"].(string)+"/run", "", keyA, idem); code != 409 || b["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("same key, other task: %d %v", code, b)
	}
	// Keys are scoped per application: B may use the same key independently.
	if code, _, _ := req(t, env.srv, "POST", "/api/v1/tasks/"+tb["id"].(string)+"/run", "", keyB, idem); code != 202 {
		t.Fatalf("B with A's key value: %d", code)
	}
	_, list, _ := req(t, env.srv, "GET", "/api/v1/executions?task_id="+t1["id"].(string), "", keyA, nil)
	if n := len(list["executions"].([]any)); n != 1 {
		t.Fatalf("task 1 has %d executions, want 1", n)
	}
	for _, bad := range []string{strings.Repeat("k", 256), "naïve"} {
		if code, _, _ := req(t, env.srv, "POST", "/api/v1/tasks/"+t1["id"].(string)+"/run", "", keyA, map[string]string{"Idempotency-Key": bad}); code != 400 {
			t.Errorf("bad Idempotency-Key %q = %d, want 400", bad[:5], code)
		}
	}
}

// The OpenAPI document and the router must describe the same endpoints.
func TestOpenAPIMatchesRouter(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	code, _, hdr := req(t, env.srv, "GET", "/api/v1/openapi.json", "", "", nil)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("openapi.json: %d %v", code, hdr)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			if method != "parameters" {
				documented[strings.ToUpper(method)+" /api/v1"+path] = true
			}
		}
	}
	routed := map[string]bool{}
	router := NewRouter(Deps{AdminToken: token}).(chi.Routes)
	_ = chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/api/v1/") {
			routed[method+" "+route] = true
		}
		return nil
	})
	for r := range routed {
		if !documented[r] {
			t.Errorf("route %s is not in openapi.json", r)
		}
	}
	for d := range documented {
		if !routed[d] {
			t.Errorf("openapi.json documents %s, which is not routed", d)
		}
	}
	if len(routed) < 20 {
		t.Fatalf("only %d routes found; walk is broken", len(routed))
	}
}

func TestStatsAndFiltersOverHTTP(t *testing.T) {
	env := newTestEnv(t, fakePinger{}, "", false)
	_, keyA := newApp(t, env, "app-a")
	_, keyB := newApp(t, env, "app-b")
	_, ta, _ := req(t, env.srv, "POST", "/api/v1/tasks", appTask, keyA, nil)
	req(t, env.srv, "POST", "/api/v1/tasks/"+ta["id"].(string)+"/run", "", keyA, nil)

	// Stats are scoped like everything else.
	for key, want := range map[string]float64{keyA: 1, keyB: 0, token: 1} {
		code, st, _ := req(t, env.srv, "GET", "/api/v1/stats", "", key, nil)
		if code != 200 || st["window"] != "24h" || st["executions"].(map[string]any)["total"] != want {
			t.Errorf("stats = %d %v, want total %v", code, st, want)
		}
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/stats?window=2y", "", token, nil); code != 400 {
		t.Errorf("bad window = %d", code)
	}

	// Time-range filters on executions.
	future := "2999-01-01T00:00:00Z"
	if _, l, _ := req(t, env.srv, "GET", "/api/v1/executions?since="+future, "", token, nil); len(l["executions"].([]any)) != 0 {
		t.Errorf("since in the future: %v", l)
	}
	if _, l, _ := req(t, env.srv, "GET", "/api/v1/executions?until="+future, "", token, nil); len(l["executions"].([]any)) != 1 {
		t.Errorf("until in the future: %v", l)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/executions?since=yesterday", "", token, nil); code != 400 {
		t.Errorf("bad since = %d", code)
	}

	// Runner filter on tasks.
	if _, l, _ := req(t, env.srv, "GET", "/api/v1/tasks?runner=shell", "", token, nil); len(l["tasks"].([]any)) != 0 {
		t.Errorf("runner=shell: %v", l)
	}
	if _, l, _ := req(t, env.srv, "GET", "/api/v1/tasks?runner=http", "", token, nil); len(l["tasks"].([]any)) != 1 {
		t.Errorf("runner=http: %v", l)
	}
	if code, _, _ := req(t, env.srv, "GET", "/api/v1/tasks?runner=ftp", "", token, nil); code != 400 {
		t.Errorf("bad runner = %d", code)
	}
}
