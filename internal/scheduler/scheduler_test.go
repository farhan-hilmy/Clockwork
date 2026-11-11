package scheduler_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/scheduler"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// openTestDB connects to CLOCKWORK_TEST_DATABASE_URL, migrates, and empties the task tables.
// The test is skipped when the variable is unset, so `go test ./...` works without Postgres.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	return pool
}

// clock is a settable time source shared by the task service and the scheduler.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(s string)     { c.mu.Lock(); defer c.mu.Unlock(); c.t = mustTime(s) }
func mustTime(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func quiet() *slog.Logger         { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func newClock(s string) *clock    { c := &clock{}; c.set(s); return c }

func newScheduler(pool *pgxpool.Pool, c *clock) *scheduler.Scheduler {
	return scheduler.New(pool, scheduler.Options{MisfireGrace: time.Minute, Now: c.now, Logger: quiet(), BatchSize: 2})
}

func createTask(t *testing.T, svc *tasks.Service, name, cron, misfire string, enabled bool) tasks.Task {
	t.Helper()
	tk, err := svc.Create(t.Context(), tasks.CreateInput{
		Name:     name,
		Runner:   tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)},
		Schedule: &tasks.Schedule{Cron: cron, MisfirePolicy: misfire},
		Enabled:  &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func scheduledFor(t *testing.T, pool *pgxpool.Pool, taskID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT scheduled_for FROM executions
		WHERE task_id = $1 AND trigger = 'scheduled' AND status = 'queued' ORDER BY scheduled_for`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatal(err)
		}
		out = append(out, ts.UTC().Format(time.RFC3339))
	}
	return out
}

func nextRunAt(t *testing.T, pool *pgxpool.Pool, taskID string) string {
	t.Helper()
	var ts *time.Time
	if err := pool.QueryRow(t.Context(), `SELECT next_run_at FROM tasks WHERE id = $1`, taskID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts == nil {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func equal(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The M2 exit criterion: an enabled cron task produces queued executions that survive a restart.
func TestScheduledRunsAreQueuedAndSurviveRestart(t *testing.T) {
	pool := openTestDB(t)
	c := newClock("2026-11-06T10:02:30Z")
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now})
	tk := createTask(t, svc, "every-5", "*/5 * * * *", tasks.MisfireSkip, true)
	paused := createTask(t, svc, "paused", "*/5 * * * *", tasks.MisfireSkip, false)

	s := newScheduler(pool, c)
	// Nothing is due yet.
	if r, err := s.Tick(t.Context()); err != nil || r.Enqueued != 0 {
		t.Fatalf("early tick: %+v %v", r, err)
	}

	c.set("2026-11-06T10:05:00.300Z")
	r, err := s.Tick(t.Context())
	if err != nil || r.Enqueued != 1 {
		t.Fatalf("tick at 10:05: %+v %v", r, err)
	}
	// A second pass in the same instant finds nothing: next_run_at already moved on.
	if r, _ := s.Tick(t.Context()); r.Enqueued != 0 {
		t.Fatalf("repeat tick enqueued %d", r.Enqueued)
	}
	if got := nextRunAt(t, pool, tk.ID); got != "2026-11-06T10:10:00Z" {
		t.Fatalf("next_run_at = %s", got)
	}

	// "Restart": a brand-new scheduler and connection pool see the same state and carry on.
	pool.Close()
	pool2 := openPoolNoTruncate(t)
	s2 := newScheduler(pool2, c)
	c.set("2026-11-06T10:10:01Z")
	if r, err := s2.Tick(t.Context()); err != nil || r.Enqueued != 1 {
		t.Fatalf("tick after restart: %+v %v", r, err)
	}
	if got := scheduledFor(t, pool2, tk.ID); !equal(got, "2026-11-06T10:05:00Z", "2026-11-06T10:10:00Z") {
		t.Fatalf("queued executions = %v", got)
	}
	if got := scheduledFor(t, pool2, paused.ID); len(got) != 0 {
		t.Fatalf("paused task was scheduled: %v", got)
	}

	// The executions read back through the service, with the task name joined in.
	page, err := executions.NewService(executions.NewPostgresRepository(pool2), nil).List(t.Context(), executions.ListParams{TaskID: tk.ID})
	if err != nil || len(page.Executions) != 2 {
		t.Fatalf("list: %+v %v", page, err)
	}
	e := page.Executions[0]
	if e.TaskName != "every-5" || e.Trigger != executions.TriggerScheduled || e.Status != executions.StatusQueued ||
		e.ApplicationID != tasks.DefaultApplicationID || e.ScheduledFor == nil || !e.ScheduledFor.Equal(mustTime("2026-11-06T10:10:00Z")) {
		t.Fatalf("newest execution: %+v", e)
	}
}

func openPoolNoTruncate(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(t.Context(), os.Getenv("CLOCKWORK_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMisfirePolicies(t *testing.T) {
	pool := openTestDB(t)
	c := newClock("2026-11-06T07:30:00Z")
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now})
	skip := createTask(t, svc, "hourly-skip", "0 * * * *", tasks.MisfireSkip, true)
	once := createTask(t, svc, "hourly-once", "0 * * * *", tasks.MisfireRunOnce, true)

	// The process was down from 07:30 to 10:30: three hourly runs were missed.
	c.set("2026-11-06T10:30:00Z")
	if _, err := newScheduler(pool, c).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := scheduledFor(t, pool, skip.ID); len(got) != 0 {
		t.Fatalf("skip policy enqueued %v", got)
	}
	if got := scheduledFor(t, pool, once.ID); !equal(got, "2026-11-06T08:00:00Z") {
		t.Fatalf("run_once policy enqueued %v", got)
	}
	for _, id := range []string{skip.ID, once.ID} {
		if got := nextRunAt(t, pool, id); got != "2026-11-06T11:00:00Z" {
			t.Fatalf("next_run_at = %s", got)
		}
	}
}

// Several schedulers racing over the same due tasks enqueue each occurrence exactly once.
func TestConcurrentSchedulersDoNotDuplicate(t *testing.T) {
	pool := openTestDB(t)
	c := newClock("2026-11-06T10:00:30Z")
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now})
	var ids []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, createTask(t, svc, n, "* * * * *", tasks.MisfireSkip, true).ID)
	}
	c.set("2026-11-06T10:01:10Z")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			if _, err := newScheduler(pool, c).Tick(t.Context()); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, id := range ids {
		if got := scheduledFor(t, pool, id); !equal(got, "2026-11-06T10:01:00Z") {
			t.Fatalf("task %s: %v", id, got)
		}
	}
}

// A stale write that resets next_run_at to an already-materialised occurrence must not duplicate it.
func TestStaleNextRunDoesNotDuplicateOccurrence(t *testing.T) {
	pool := openTestDB(t)
	c := newClock("2026-11-06T10:02:30Z")
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now})
	tk := createTask(t, svc, "x", "*/5 * * * *", tasks.MisfireSkip, true)
	s := newScheduler(pool, c)

	c.set("2026-11-06T10:05:00.500Z")
	if _, err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET next_run_at = '2026-11-06T10:05:00Z' WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Tick(t.Context()); err != nil || r.Enqueued != 0 {
		t.Fatalf("re-tick: %+v %v", r, err)
	}
	if got := scheduledFor(t, pool, tk.ID); !equal(got, "2026-11-06T10:05:00Z") {
		t.Fatalf("queued = %v", got)
	}
	if got := nextRunAt(t, pool, tk.ID); got != "2026-11-06T10:10:00Z" {
		t.Fatalf("next_run_at = %s", got)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	pool := openTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		scheduler.New(pool, scheduler.Options{Interval: 10 * time.Millisecond, Logger: quiet()}).Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Tasks of a disabled application stay due and are not queued until it is re-enabled.
func TestDisabledApplicationIsNotScheduled(t *testing.T) {
	pool := openTestDB(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO applications (id, name, enabled) VALUES ('app_off', 'off', false)`); err != nil {
		t.Fatal(err)
	}
	c := newClock("2026-11-06T10:02:30Z")
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now})
	tk, err := svc.Create(t.Context(), tasks.CreateInput{ApplicationID: "app_off", Name: "x",
		Runner:   tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)},
		Schedule: &tasks.Schedule{Cron: "*/5 * * * *"}})
	if err != nil {
		t.Fatal(err)
	}
	s := newScheduler(pool, c)
	c.set("2026-11-06T10:05:01Z")
	if r, err := s.Tick(t.Context()); err != nil || r.Enqueued != 0 {
		t.Fatalf("disabled app tick: %+v %v", r, err)
	}
	if got := nextRunAt(t, pool, tk.ID); got != "2026-11-06T10:05:00Z" {
		t.Fatalf("next_run_at moved while disabled: %s", got)
	}
	_, _ = pool.Exec(t.Context(), `UPDATE applications SET enabled = true WHERE id = 'app_off'`)
	if r, err := s.Tick(t.Context()); err != nil || r.Enqueued != 1 {
		t.Fatalf("re-enabled app tick: %+v %v", r, err)
	}
}
