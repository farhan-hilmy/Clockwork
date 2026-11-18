package retention_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/retention"
	"github.com/farhan-hilmy/clockwork/internal/store"
)

var now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// seed creates one task and a mix of old and recent, finished and unfinished
// executions and webhook deliveries.
func seed(t *testing.T) *pgxpool.Pool {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`TRUNCATE executions, tasks, webhook_endpoints CASCADE; DELETE FROM applications WHERE id <> 'app_clockwork'`)
	exec(`INSERT INTO tasks (id, application_id, name, runner_type, runner_config, timeout_seconds, max_attempts, backoff_initial_seconds, backoff_max_seconds)
		VALUES ('tsk_1', 'app_clockwork', 't', 'http', '{"url":"https://example.com"}', 30, 1, 10, 300)`)
	exec(`INSERT INTO webhook_endpoints (id, application_id, url, secret) VALUES ('whk_1', 'app_clockwork', 'https://hooks.example.com', 's')`)

	type exe struct {
		id, status string
		finished   *time.Duration // age when finished; nil = not finished
	}
	ago := func(d time.Duration) *time.Duration { return &d }
	for _, e := range []exe{
		{"exe_old_succeeded", "succeeded", ago(40 * day)},
		{"exe_old_failed", "failed", ago(31 * day)},
		{"exe_old_skipped", "skipped", ago(35 * day)},
		{"exe_old_interrupted", "interrupted", ago(60 * day)},
		{"exe_old_retrying", "retrying", ago(45 * day)},
		{"exe_recent_succeeded", "succeeded", ago(29 * day)},
		{"exe_old_queued", "queued", nil},   // created long ago, never run: kept
		{"exe_old_running", "running", nil}, // still running: kept
	} {
		var finished any
		if e.finished != nil {
			finished = now.Add(-*e.finished)
		}
		exec(`INSERT INTO executions (id, task_id, application_id, trigger, status, created_at, run_after, finished_at)
			VALUES ($1, 'tsk_1', 'app_clockwork', 'manual', $2, $3, $3, $4)`, e.id, e.status, now.Add(-90*day), finished)
	}
	for _, d := range []struct {
		id, status  string
		lastAttempt time.Duration
	}{
		{"dlv_old_delivered", "delivered", 40 * day},
		{"dlv_old_failed", "failed", 33 * day},
		{"dlv_recent_delivered", "delivered", 2 * day},
		{"dlv_old_pending", "pending", 50 * day}, // still being retried: kept
	} {
		exec(`INSERT INTO webhook_deliveries (id, endpoint_id, application_id, event_id, event_type, payload, status, next_attempt_at, last_attempt_at, created_at)
			VALUES ($1, 'whk_1', 'app_clockwork', $1, 'execution.succeeded', '{}', $2, $3, $3, $4)`,
			d.id, d.status, now.Add(-d.lastAttempt), now.Add(-90*day))
	}
	return pool
}

func ids(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), fmt.Sprintf("SELECT id FROM %s ORDER BY id", table))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The M9 exit criterion: old finished history is deleted; recent or unfinished history is kept.
func TestSweepDeletesOnlyOldFinishedHistory(t *testing.T) {
	pool := seed(t)
	s := retention.New(pool, retention.Options{Keep: 30 * day, BatchSize: 2, Now: func() time.Time { return now }, Logger: quiet()})

	r, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Executions != 5 || r.Deliveries != 2 {
		t.Fatalf("deleted %+v, want 5 executions and 2 deliveries", r)
	}
	if got, want := ids(t, pool, "executions"), []string{"exe_old_queued", "exe_old_running", "exe_recent_succeeded"}; !slices.Equal(got, want) {
		t.Fatalf("executions left %v, want %v", got, want)
	}
	if got, want := ids(t, pool, "webhook_deliveries"), []string{"dlv_old_pending", "dlv_recent_delivered"}; !slices.Equal(got, want) {
		t.Fatalf("deliveries left %v, want %v", got, want)
	}

	// A second sweep finds nothing more.
	if r, err := s.Sweep(context.Background()); err != nil || r != (retention.Result{}) {
		t.Fatalf("second sweep: %+v %v", r, err)
	}
}

func TestDisabledRetentionDeletesNothing(t *testing.T) {
	pool := seed(t)
	s := retention.New(pool, retention.Options{Keep: 0, Now: func() time.Time { return now }, Logger: quiet()})
	if r, err := s.Sweep(context.Background()); err != nil || r != (retention.Result{}) {
		t.Fatalf("sweep: %+v %v", r, err)
	}
	if n := len(ids(t, pool, "executions")); n != 8 {
		t.Fatalf("%d executions left, want all 8", n)
	}
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done: // Run returns at once when disabled
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept running with retention disabled")
	}
}

func TestRunSweepsUntilCanceled(t *testing.T) {
	pool := seed(t)
	s := retention.New(pool, retention.Options{Keep: 30 * day, StartDelay: -1, Interval: 20 * time.Millisecond,
		Now: func() time.Time { return now }, Logger: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(ids(t, pool, "executions")) != 3 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not sweep")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
