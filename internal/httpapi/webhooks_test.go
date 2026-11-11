package httpapi

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
	"github.com/farhan-hilmy/clockwork/internal/webhooks"
)

// newPostgresServer is the router over the real Postgres services.
func newPostgresServer(t *testing.T) *httptest.Server {
	t.Helper()
	url := os.Getenv("CLOCKWORK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CLOCKWORK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	pool, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `TRUNCATE executions, tasks, app_credentials, webhook_endpoints CASCADE;
		DELETE FROM applications WHERE id <> 'app_clockwork'`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(Deps{
		Tasks:      tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{}),
		Executions: executions.NewService(executions.NewPostgresRepository(pool), nil),
		Apps:       apps.NewService(apps.NewPostgresRepository(pool), nil),
		Webhooks:   webhooks.NewService(pool, nil),
		DB:         pool, AdminToken: token,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newPgApp(t *testing.T, srv *httptest.Server, name string) (id, key string) {
	t.Helper()
	code, a, _ := req(t, srv, "POST", "/api/v1/applications", `{"name":"`+name+`"}`, token, nil)
	if code != 201 {
		t.Fatalf("create app: %d %v", code, a)
	}
	id = a["id"].(string)
	_, k, _ := req(t, srv, "POST", "/api/v1/applications/"+id+"/keys", `{"name":"k"}`, token, nil)
	return id, k["key"].(string)
}

func TestWebhookEndpointsOverHTTP(t *testing.T) {
	srv := newPostgresServer(t)
	appA, keyA := newPgApp(t, srv, "app-a")
	_, keyB := newPgApp(t, srv, "app-b")

	// Create: the secret is returned once, the endpoint belongs to the caller.
	code, ep, hdr := req(t, srv, "POST", "/api/v1/webhooks", `{"url":"https://hooks.example.com/a","events":["execution.failed"]}`, keyA, nil)
	if code != 201 || ep["application_id"] != appA || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d %v", code, ep)
	}
	id, secret := ep["id"].(string), ep["secret"].(string)
	if len(secret) < 50 {
		t.Fatalf("secret: %q", secret)
	}
	_, got, _ := req(t, srv, "GET", "/api/v1/webhooks/"+id, "", keyA, nil)
	if _, leaked := got["secret"]; leaked {
		t.Fatalf("secret returned by GET: %v", got)
	}

	// Rotation returns a new secret.
	if _, rot, _ := req(t, srv, "POST", "/api/v1/webhooks/"+id+"/rotate-secret", "", keyA, nil); rot["secret"] == secret || rot["secret"] == nil {
		t.Fatalf("rotate: %v", rot)
	}

	// Test event, delivery log, single delivery with payload, redeliver.
	code, d, _ := req(t, srv, "POST", "/api/v1/webhooks/"+id+"/test", "", keyA, nil)
	if code != 202 || d["event_type"] != "webhook.test" || d["status"] != "pending" {
		t.Fatalf("test: %d %v", code, d)
	}
	did := d["id"].(string)
	_, log, _ := req(t, srv, "GET", "/api/v1/webhooks/"+id+"/deliveries?status=pending", "", keyA, nil)
	if n := len(log["deliveries"].([]any)); n != 1 {
		t.Fatalf("delivery log: %v", log)
	}
	if _, one, _ := req(t, srv, "GET", "/api/v1/webhooks/"+id+"/deliveries/"+did, "", keyA, nil); one["payload"] == nil {
		t.Fatalf("delivery without payload: %v", one)
	}
	if code, _, _ := req(t, srv, "POST", "/api/v1/webhooks/"+id+"/deliveries/"+did+"/redeliver", "", keyA, nil); code != 202 {
		t.Fatalf("redeliver: %d", code)
	}

	// Another application sees none of it.
	if _, list, _ := req(t, srv, "GET", "/api/v1/webhooks", "", keyB, nil); len(list["webhooks"].([]any)) != 0 {
		t.Fatalf("B lists A's webhooks: %v", list)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/webhooks/" + id, ""},
		{"PATCH", "/api/v1/webhooks/" + id, `{"url":"https://evil.example.com"}`},
		{"DELETE", "/api/v1/webhooks/" + id, ""},
		{"POST", "/api/v1/webhooks/" + id + "/rotate-secret", ""},
		{"POST", "/api/v1/webhooks/" + id + "/test", ""},
		{"GET", "/api/v1/webhooks/" + id + "/deliveries", ""},
		{"GET", "/api/v1/webhooks/" + id + "/deliveries/" + did, ""},
		{"POST", "/api/v1/webhooks/" + id + "/deliveries/" + did + "/redeliver", ""},
	} {
		if code, b, _ := req(t, srv, c.method, c.path, c.body, keyB, nil); code != 404 {
			t.Errorf("B %s %s = %d %v, want 404", c.method, c.path, code, b)
		}
	}
	if code, _, _ := req(t, srv, "POST", "/api/v1/webhooks", `{"application_id":"`+appA+`","url":"https://x.dev"}`, keyB, nil); code != 422 {
		t.Fatalf("B creates webhook for A: %d", code)
	}
	if _, still, _ := req(t, srv, "GET", "/api/v1/webhooks/"+id, "", keyA, nil); still["url"] != "https://hooks.example.com/a" {
		t.Fatalf("A's endpoint changed: %v", still)
	}

	// Validation and the admin's view.
	if code, b, _ := req(t, srv, "POST", "/api/v1/webhooks", `{"url":"ftp://x","events":["nope"]}`, keyA, nil); code != 422 ||
		len(b["error"].(map[string]any)["details"].(map[string]any)) != 2 {
		t.Fatalf("validation: %d %v", code, b)
	}
	if code, b, _ := req(t, srv, "POST", "/api/v1/webhooks", `{"url":"https://ops.example.com"}`, token, nil); code != 201 || b["application_id"] != "app_clockwork" {
		t.Fatalf("admin create: %d %v", code, b)
	}
	if _, list, _ := req(t, srv, "GET", "/api/v1/webhooks", "", token, nil); len(list["webhooks"].([]any)) != 2 {
		t.Fatalf("admin list: %v", list)
	}
	if code, _, _ := req(t, srv, "GET", "/api/v1/webhooks/"+id+"/deliveries?status=bogus", "", keyA, nil); code != 400 {
		t.Fatalf("bad status filter: %d", code)
	}
	if code, b, _ := req(t, srv, "PATCH", "/api/v1/webhooks/"+id, `{"enabled":false}`, keyA, nil); code != 200 || b["enabled"] != false {
		t.Fatalf("disable: %d %v", code, b)
	}
	if code, _, _ := req(t, srv, "DELETE", "/api/v1/webhooks/"+id, "", keyA, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}
