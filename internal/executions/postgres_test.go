package executions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func TestPostgresRunNowListGet(t *testing.T) {
	url := os.Getenv("CLOCKWORK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CLOCKWORK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks, webhook_endpoints CASCADE; DELETE FROM applications WHERE id <> 'app_clockwork'`); err != nil {
		t.Fatal(err)
	}

	taskSvc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{})
	off := false
	// Manual runs work for paused, unscheduled tasks too.
	tk, err := taskSvc.Create(ctx, tasks.CreateInput{Name: "manual-only", Enabled: &off,
		Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)}})
	if err != nil {
		t.Fatal(err)
	}

	svc := executions.NewService(executions.NewPostgresRepository(pool), nil)
	var runIDs []string
	for range 3 {
		e, _, err := svc.RunNow(ctx, tk.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if e.Trigger != executions.TriggerManual || e.Status != executions.StatusQueued || e.TaskName != "manual-only" ||
			e.ApplicationID != tasks.DefaultApplicationID || e.ScheduledFor != nil {
			t.Fatalf("run now: %+v", e)
		}
		runIDs = append(runIDs, e.ID)
	}

	got, err := svc.Get(ctx, runIDs[1])
	if err != nil || got.ID != runIDs[1] || got.TaskName != "manual-only" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := svc.Get(ctx, "exe_missing"); !errors.Is(err, executions.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if _, _, err := svc.RunNow(ctx, "tsk_missing", ""); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("run missing task: %v", err)
	}

	p1, err := svc.List(ctx, executions.ListParams{TaskID: tk.ID, Limit: 2})
	if err != nil || len(p1.Executions) != 2 || p1.NextCursor == "" || p1.Executions[0].ID != runIDs[2] {
		t.Fatalf("page 1: %+v %v", p1, err)
	}
	p2, err := svc.List(ctx, executions.ListParams{TaskID: tk.ID, Limit: 2, Cursor: p1.NextCursor})
	if err != nil || len(p2.Executions) != 1 || p2.NextCursor != "" || p2.Executions[0].ID != runIDs[0] {
		t.Fatalf("page 2: %+v %v", p2, err)
	}
	if p, _ := svc.List(ctx, executions.ListParams{Status: executions.StatusSucceeded}); len(p.Executions) != 0 {
		t.Fatalf("status filter: %+v", p)
	}
}

func TestPostgresIdempotencyAndApplicationFilter(t *testing.T) {
	url := os.Getenv("CLOCKWORK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CLOCKWORK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks, webhook_endpoints CASCADE; DELETE FROM applications WHERE id <> 'app_clockwork';
		INSERT INTO applications (id, name) VALUES ('app_a', 'a'), ('app_b', 'b')`); err != nil {
		t.Fatal(err)
	}
	taskSvc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{})
	mk := func(app, name string) tasks.Task {
		tk, err := taskSvc.Create(ctx, tasks.CreateInput{ApplicationID: app, Name: name,
			Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	a1, a2, b1 := mk("app_a", "one"), mk("app_a", "two"), mk("app_b", "one")
	svc := executions.NewService(executions.NewPostgresRepository(pool), nil)

	first, replayed, err := svc.RunNow(ctx, a1.ID, "order-42")
	if err != nil || replayed || first.IdempotencyKey == nil || *first.IdempotencyKey != "order-42" {
		t.Fatalf("first: %+v %v %v", first, replayed, err)
	}
	again, replayed, err := svc.RunNow(ctx, a1.ID, "order-42")
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay: %+v %v %v", again, replayed, err)
	}
	if _, _, err := svc.RunNow(ctx, a2.ID, "order-42"); !errors.Is(err, executions.ErrIdempotencyConflict) {
		t.Fatalf("same key, other task: %v", err)
	}
	if _, replayed, err := svc.RunNow(ctx, b1.ID, "order-42"); err != nil || replayed {
		t.Fatalf("same key, other application: %v %v", replayed, err)
	}

	// Concurrent requests with one key queue exactly one run.
	results := make(chan string, 10)
	for range 10 {
		go func() {
			e, _, err := svc.RunNow(ctx, a2.ID, "burst")
			if err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- e.ID
		}()
	}
	seen := map[string]bool{}
	for range 10 {
		seen[<-results] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent requests produced %v", seen)
	}

	page, _ := svc.List(ctx, executions.ListParams{ApplicationID: "app_b"})
	if len(page.Executions) != 1 || page.Executions[0].TaskID != b1.ID {
		t.Fatalf("application filter: %+v", page.Executions)
	}
}

func TestExecutionFilters(t *testing.T) {
	url := os.Getenv("CLOCKWORK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CLOCKWORK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks, webhook_endpoints CASCADE; DELETE FROM applications WHERE id <> 'app_clockwork';
		INSERT INTO applications (id, name) VALUES ('app_a', 'a')`); err != nil {
		t.Fatal(err)
	}
	taskSvc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{})
	mk := func(app string) string {
		tk, err := taskSvc.Create(ctx, tasks.CreateInput{ApplicationID: app, Name: "t-" + app,
			Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	own, other := mk("app_clockwork"), mk("app_a")
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	insert := func(id, task, app, status string, ago time.Duration, ms int) {
		if _, err := pool.Exec(ctx, `INSERT INTO executions (id, task_id, application_id, trigger, status, created_at, run_after, duration_ms)
			VALUES ($1, $2, $3, 'manual', $4, $5, $5, $6)`, id, task, app, status, now.Add(-ago), ms); err != nil {
			t.Fatal(err)
		}
	}
	insert("exe_01", own, "app_clockwork", "succeeded", 48*time.Hour, 100) // outside a 24h window
	insert("exe_02", own, "app_clockwork", "succeeded", 2*time.Hour, 100)
	insert("exe_03", own, "app_clockwork", "succeeded", time.Hour, 300)
	insert("exe_04", own, "app_clockwork", "failed", 30*time.Minute, 500)
	insert("exe_05", own, "app_clockwork", "retrying", 20*time.Minute, 900) // not a final outcome
	insert("exe_06", other, "app_a", "failed", 10*time.Minute, 50)

	svc := executions.NewService(executions.NewPostgresRepository(pool), func() time.Time { return now })
	ids := func(p executions.ListParams) []string {
		page, err := svc.List(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range page.Executions {
			out = append(out, e.ID)
		}
		return out
	}
	since, until := now.Add(-90*time.Minute), now.Add(-25*time.Minute)
	if got := fmt.Sprint(ids(executions.ListParams{Since: &since, Until: &until})); got != "[exe_04 exe_03]" {
		t.Fatalf("time range: %s", got)
	}
	if got := fmt.Sprint(ids(executions.ListParams{ApplicationID: "app_a"})); got != "[exe_06]" {
		t.Fatalf("application: %s", got)
	}
	if got := fmt.Sprint(ids(executions.ListParams{Status: "succeeded", Since: &since})); got != "[exe_03]" {
		t.Fatalf("status and since: %s", got)
	}

	st, err := svc.Stats(ctx, "app_clockwork", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Last 24h for app_clockwork: 2 succeeded, 1 failed, 1 retrying.
	if st.Total != 4 || st.ByStatus["succeeded"] != 2 || st.ByStatus["failed"] != 1 || st.ByStatus["retrying"] != 1 ||
		st.SuccessRate == nil || fmt.Sprintf("%.3f", *st.SuccessRate) != "0.667" ||
		*st.AvgDurationMS != 300 || *st.P95DurationMS != 480 {
		t.Fatalf("stats: %+v rate=%v avg=%v p95=%v", st, *st.SuccessRate, *st.AvgDurationMS, *st.P95DurationMS)
	}
	all, _ := svc.Stats(ctx, "", 24*time.Hour)
	if all.Total != 5 {
		t.Fatalf("all applications: %+v", all)
	}
	empty, _ := svc.Stats(ctx, "app_clockwork", time.Minute)
	if empty.Total != 0 || empty.SuccessRate != nil || empty.AvgDurationMS != nil {
		t.Fatalf("empty window: %+v", empty)
	}
}
