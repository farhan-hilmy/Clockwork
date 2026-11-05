package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 11, 5, 14, 2, 30, 0, time.UTC)

func newSvc(t *testing.T, shell bool) (*Service, *MemoryRepository) {
	t.Helper()
	repo := NewMemoryRepository("app_other")
	return NewService(repo, Options{AllowShell: shell, Now: func() time.Time { return fixedNow }}), repo
}

func httpRunner(cfg string) Runner {
	return Runner{Type: RunnerHTTP, Config: json.RawMessage(cfg)}
}

func validInput() CreateInput {
	return CreateInput{
		Name:     "health-check",
		Runner:   httpRunner(`{"url":"https://api.example.com/healthz","headers":{"Authorization":"Bearer s3cret","Accept":"application/json"}}`),
		Schedule: &Schedule{Cron: "*/5 * * * *", Timezone: "Asia/Jakarta"},
	}
}

func fieldErr(t *testing.T, err error, field string) {
	t.Helper()
	ve, ok := IsValidation(err)
	if !ok {
		t.Fatalf("expected validation error on %s, got %v", field, err)
	}
	if _, ok := ve.Fields[field]; !ok {
		t.Fatalf("expected error on %s, got %v", field, ve.Fields)
	}
}

func TestCreateAppliesDefaultsAndComputesNextRun(t *testing.T) {
	svc, _ := newSvc(t, false)
	task, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(task.ID, "tsk_") || task.ApplicationID != DefaultApplicationID {
		t.Fatalf("bad id/app: %s %s", task.ID, task.ApplicationID)
	}
	if !task.Enabled || task.TimeoutSeconds != DefaultTimeoutSeconds || task.Retry != DefaultRetry {
		t.Fatalf("defaults not applied: %+v", task)
	}
	s := task.Schedule
	if s.MisfirePolicy != MisfireSkip || s.ConcurrencyPolicy != ConcurrencyForbid {
		t.Fatalf("schedule defaults not applied: %+v", s)
	}
	want := time.Date(2026, 11, 5, 14, 5, 0, 0, time.UTC)
	if task.NextRunAt == nil || !task.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at = %v, want %v", task.NextRunAt, want)
	}
	var cfg HTTPConfig
	_ = json.Unmarshal(task.Runner.Config, &cfg)
	if cfg.Method != "GET" {
		t.Fatalf("method not defaulted: %q", cfg.Method)
	}
}

func TestCreateRedactsSecretsButStoresThem(t *testing.T) {
	svc, repo := newSvc(t, false)
	task, err := svc.Create(context.Background(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(task.Runner.Config), "s3cret") {
		t.Fatalf("secret leaked in response: %s", task.Runner.Config)
	}
	if !strings.Contains(string(task.Runner.Config), Mask) || !strings.Contains(string(task.Runner.Config), "application/json") {
		t.Fatalf("expected masked Authorization and plain Accept: %s", task.Runner.Config)
	}
	raw, _ := repo.Raw(task.ID)
	if !strings.Contains(string(raw.Runner.Config), "s3cret") {
		t.Fatalf("secret not stored: %s", raw.Runner.Config)
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*CreateInput)
	}{
		{"name", func(in *CreateInput) { in.Name = "  " }},
		{"name", func(in *CreateInput) { in.Name = strings.Repeat("x", 101) }},
		{"runner.type", func(in *CreateInput) { in.Runner.Type = "ftp" }},
		{"runner.config", func(in *CreateInput) { in.Runner.Config = json.RawMessage(`{"url":"https://x","bogus":1}`) }},
		{"runner.config.url", func(in *CreateInput) { in.Runner = httpRunner(`{"url":"file:///etc/passwd"}`) }},
		{"runner.config.url", func(in *CreateInput) { in.Runner = httpRunner(`{"url":"https://user:pw@example.com"}`) }},
		{"runner.config.method", func(in *CreateInput) { in.Runner = httpRunner(`{"url":"https://x.dev","method":"TRACE"}`) }},
		{"runner.config.headers", func(in *CreateInput) {
			in.Runner = httpRunner(`{"url":"https://x.dev","headers":{"X-Evil":"a\r\nInjected: 1"}}`)
		}},
		{"runner.config.headers", func(in *CreateInput) {
			in.Runner = httpRunner(`{"url":"https://x.dev","headers":{"Authorization":"********"}}`)
		}},
		{"runner.config.expected_status", func(in *CreateInput) { in.Runner = httpRunner(`{"url":"https://x.dev","expected_status":[700]}`) }},
		{"schedule.cron", func(in *CreateInput) { in.Schedule.Cron = "every day" }},
		{"schedule.timezone", func(in *CreateInput) { in.Schedule.Timezone = "Mars/Base" }},
		{"schedule.misfire_policy", func(in *CreateInput) { in.Schedule.MisfirePolicy = "run_all" }},
		{"timeout_seconds", func(in *CreateInput) { z := 0; in.TimeoutSeconds = &z }},
		{"retry.max_attempts", func(in *CreateInput) {
			in.Retry = &RetryPolicy{MaxAttempts: 0, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1}
		}},
		{"retry.backoff_max_seconds", func(in *CreateInput) {
			in.Retry = &RetryPolicy{MaxAttempts: 3, BackoffInitialSeconds: 60, BackoffMaxSeconds: 10}
		}},
		{"application_id", func(in *CreateInput) { in.ApplicationID = "app_missing" }},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			svc, _ := newSvc(t, false)
			in := validInput()
			c.mutate(&in)
			_, err := svc.Create(context.Background(), in)
			fieldErr(t, err, c.field)
		})
	}
}

func TestShellRequiresServerOptIn(t *testing.T) {
	in := CreateInput{Name: "cleanup", Runner: Runner{Type: RunnerShell, Config: json.RawMessage(`{"command":"echo hi","env":{"DB_PASSWORD":"pw"}}`)}}

	off, _ := newSvc(t, false)
	_, err := off.Create(context.Background(), in)
	fieldErr(t, err, "runner.type")

	on, _ := newSvc(t, true)
	task, err := on.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(task.Runner.Config), `"pw"`) {
		t.Fatalf("shell env secret leaked: %s", task.Runner.Config)
	}
}

func TestDuplicateNameConflictsWithinApplicationOnly(t *testing.T) {
	svc, _ := newSvc(t, false)
	ctx := context.Background()
	if _, err := svc.Create(ctx, validInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, validInput()); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	other := validInput()
	other.ApplicationID = "app_other"
	if _, err := svc.Create(ctx, other); err != nil {
		t.Fatalf("same name in another app should be allowed: %v", err)
	}
}

func TestUpdateKeepsMaskedSecretsAndRemovesSchedule(t *testing.T) {
	svc, repo := newSvc(t, false)
	ctx := context.Background()
	created, err := svc.Create(ctx, validInput())
	if err != nil {
		t.Fatal(err)
	}
	// Client round-trips the redacted runner with a changed URL.
	var cfg HTTPConfig
	_ = json.Unmarshal(created.Runner.Config, &cfg)
	cfg.URL = "https://api.example.com/v2/healthz"
	b, _ := json.Marshal(cfg)

	updated, err := svc.Update(ctx, created.ID, UpdateInput{
		Runner:   Some(Runner{Type: RunnerHTTP, Config: b}),
		Schedule: Some[*Schedule](nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Schedule != nil || updated.NextRunAt != nil {
		t.Fatalf("schedule not removed: %+v", updated)
	}
	raw, _ := repo.Raw(created.ID)
	if !strings.Contains(string(raw.Runner.Config), "Bearer s3cret") || !strings.Contains(string(raw.Runner.Config), "/v2/healthz") {
		t.Fatalf("expected new URL and preserved secret, got %s", raw.Runner.Config)
	}
	if !updated.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("updated_at not set")
	}
}

func TestUpdateRejectsMaskWhenRunnerTypeChanges(t *testing.T) {
	svc, _ := newSvc(t, true)
	ctx := context.Background()
	created, _ := svc.Create(ctx, validInput())
	_, err := svc.Update(ctx, created.ID, UpdateInput{
		Runner: Some(Runner{Type: RunnerShell, Config: json.RawMessage(`{"command":"true","env":{"API_TOKEN":"********"}}`)}),
	})
	fieldErr(t, err, "runner.config.env")
}

func TestPauseAndResume(t *testing.T) {
	svc, _ := newSvc(t, false)
	ctx := context.Background()
	created, _ := svc.Create(ctx, validInput())

	paused, err := svc.SetEnabled(ctx, created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Enabled || paused.NextRunAt != nil {
		t.Fatalf("paused task should have no next run: %+v", paused)
	}
	resumed, err := svc.SetEnabled(ctx, created.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Enabled || resumed.NextRunAt == nil {
		t.Fatalf("resumed task should have a next run: %+v", resumed)
	}
}

func TestListPaginationNewestFirst(t *testing.T) {
	svc, _ := newSvc(t, false)
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		in := validInput()
		in.Name = n
		if _, err := svc.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	cursor := ""
	pages := 0
	for {
		p, err := svc.List(ctx, ListParams{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, tk := range p.Tasks {
			names = append(names, tk.Name)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if got := strings.Join(names, ""); got != "edcba" || pages != 3 {
		t.Fatalf("got %q in %d pages", got, pages)
	}
}

func TestGetAndDeleteMissing(t *testing.T) {
	svc, _ := newSvc(t, false)
	ctx := context.Background()
	if _, err := svc.Get(ctx, "tsk_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := svc.Delete(ctx, "tsk_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}
