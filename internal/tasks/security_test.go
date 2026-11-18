package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Security: applications can never own shell tasks, whoever creates them.
func TestShellTasksOnlyInTheDefaultApplication(t *testing.T) {
	svc, _ := newSvc(t, true)
	ctx := context.Background()
	shell := Runner{Type: RunnerShell, Config: json.RawMessage(`{"command":"id"}`)}
	_, err := svc.Create(ctx, CreateInput{ApplicationID: "app_other", Name: "sh", Runner: shell})
	fieldErr(t, err, "runner.type")
	if _, err := svc.Create(ctx, CreateInput{Name: "sh", Runner: shell}); err != nil {
		t.Fatalf("default application shell task: %v", err)
	}
}

// Security: a masked secret is restored only while the request goes to the same origin.
func TestMaskedSecretsStayWithTheirOrigin(t *testing.T) {
	svc, repo := newSvc(t, false)
	ctx := context.Background()
	created, err := svc.Create(ctx, CreateInput{Name: "api",
		Runner: httpRunner(`{"url":"https://api.example.com/v1/x","headers":{"Authorization":"Bearer s3cret"},"query":{"token":"q-secret"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	masked := func(url string) Opt[Runner] {
		return Some(httpRunner(`{"url":"` + url + `","headers":{"Authorization":"********"},"query":{"token":"********"}}`))
	}
	for _, url := range []string{
		"https://evil.example/collect",
		"http://api.example.com/v1/x",       // downgrade
		"https://api.example.com:8443/v1/x", // other port
		"https://api.example.com.evil.example/",
	} {
		_, err := svc.Update(ctx, created.ID, UpdateInput{Runner: masked(url)})
		ve, ok := IsValidation(err)
		if !ok || !strings.Contains(ve.Fields["runner.config.headers"], "enter the secret value again") {
			t.Errorf("URL %s kept the masked secret: %v", url, err)
		}
	}
	raw, _ := repo.Raw(created.ID)
	if !strings.Contains(string(raw.Runner.Config), "api.example.com/v1/x") {
		t.Fatalf("task changed despite errors: %s", raw.Runner.Config)
	}

	// Same origin (case and default port normalised), different path: secrets are kept.
	if _, err := svc.Update(ctx, created.ID, UpdateInput{Runner: masked("https://API.example.com:443/v2/y")}); err != nil {
		t.Fatalf("same-origin update: %v", err)
	}
	raw, _ = repo.Raw(created.ID)
	if !strings.Contains(string(raw.Runner.Config), "s3cret") || !strings.Contains(string(raw.Runner.Config), "q-secret") {
		t.Fatalf("secrets lost on same-origin update: %s", raw.Runner.Config)
	}

	// A new origin with the secrets entered again is fine.
	if _, err := svc.Update(ctx, created.ID, UpdateInput{Runner: Some(httpRunner(`{"url":"https://new.example.com","headers":{"Authorization":"Bearer new"}}`))}); err != nil {
		t.Fatalf("re-entered secret: %v", err)
	}
}

func TestApplicationTaskLimits(t *testing.T) {
	repo := NewMemoryRepository("app_other")
	svc := NewService(repo, Options{AppMaxTasks: 2, AppMaxTimeout: 5 * time.Minute, Now: func() time.Time { return fixedNow }})
	ctx := context.Background()
	in := func(app, name string, timeout int) CreateInput {
		return CreateInput{ApplicationID: app, Name: name, TimeoutSeconds: &timeout, Runner: httpRunner(`{"url":"https://x.dev"}`)}
	}
	_, err := svc.Create(ctx, in("app_other", "long", 301))
	fieldErr(t, err, "timeout_seconds")
	for _, n := range []string{"a", "b"} {
		if _, err := svc.Create(ctx, in("app_other", n, 300)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(ctx, in("app_other", "c", 30)); !errors.Is(err, ErrLimit) {
		t.Fatalf("third task: %v", err)
	}
	// The default application is exempt from both limits.
	for _, n := range []string{"a", "b", "c"} {
		if _, err := svc.Create(ctx, in("", n, 86400)); err != nil {
			t.Fatalf("default application: %v", err)
		}
	}
}
