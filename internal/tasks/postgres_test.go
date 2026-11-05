package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	// Running migrations twice must be a no-op.
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks; DELETE FROM applications WHERE id <> 'app_clockwork'`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPostgresRepository(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 14, 2, 30, 0, time.UTC)
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: func() time.Time { return now }})

	in := tasks.CreateInput{
		Name:     "health-check",
		Runner:   tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://api.example.com/healthz","headers":{"X-Api-Key":"k-123"}}`)},
		Schedule: &tasks.Schedule{Cron: "0 9 * * *", Timezone: "Asia/Jakarta", MisfirePolicy: tasks.MisfireRunOnce},
	}
	created, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	// Round trip through the database.
	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schedule == nil || got.Schedule.Timezone != "Asia/Jakarta" || got.Schedule.MisfirePolicy != tasks.MisfireRunOnce {
		t.Fatalf("schedule not persisted: %+v", got.Schedule)
	}
	if want := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC); got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at = %v, want %v", got.NextRunAt, want)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v", got.CreatedAt)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT runner_config->'headers'->>'X-Api-Key' FROM tasks WHERE id=$1`, created.ID).Scan(&stored); err != nil || stored != "k-123" {
		t.Fatalf("secret not stored in db: %q %v", stored, err)
	}
	if strings.Contains(string(got.Runner.Config), "k-123") {
		t.Fatal("secret returned by Get")
	}

	// Unique name per application.
	if _, err := svc.Create(ctx, in); !errors.Is(err, tasks.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}

	// Remove schedule and pause.
	upd, err := svc.Update(ctx, created.ID, tasks.UpdateInput{Schedule: tasks.Some[*tasks.Schedule](nil), Enabled: tasks.Some(false)})
	if err != nil {
		t.Fatal(err)
	}
	reread, _ := svc.Get(ctx, upd.ID)
	if reread.Schedule != nil || reread.NextRunAt != nil || reread.Enabled {
		t.Fatalf("update not persisted: %+v", reread)
	}

	// Filters, search (with LIKE wildcards treated literally), and pagination.
	for _, n := range []string{"alpha", "beta", "gam%ma"} {
		c := in
		c.Name = n
		c.Schedule = nil
		if _, err := svc.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := svc.List(ctx, tasks.ListParams{Limit: 2})
	if err != nil || len(p1.Tasks) != 2 || p1.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", p1, err)
	}
	p2, err := svc.List(ctx, tasks.ListParams{Limit: 2, Cursor: p1.NextCursor})
	if err != nil || len(p2.Tasks) != 2 || p2.NextCursor != "" {
		t.Fatalf("page 2: %+v %v", p2, err)
	}
	if p1.Tasks[0].Name != "gam%ma" || p2.Tasks[1].Name != "health-check" {
		t.Fatalf("not newest-first: %v / %v", p1.Tasks, p2.Tasks)
	}
	enabled := false
	if p, _ := svc.List(ctx, tasks.ListParams{Enabled: &enabled}); len(p.Tasks) != 1 {
		t.Fatalf("enabled filter: %d", len(p.Tasks))
	}
	if p, _ := svc.List(ctx, tasks.ListParams{Query: "%"}); len(p.Tasks) != 1 || p.Tasks[0].Name != "gam%ma" {
		t.Fatalf("literal %% search: %+v", p.Tasks)
	}

	// Executions cascade with their task.
	if _, err := pool.Exec(ctx, `INSERT INTO executions (id, task_id, application_id, trigger, status) VALUES ('exe_1', $1, 'app_clockwork', 'manual', 'succeeded')`, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE task_id=$1`, created.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("executions not deleted with task: %d", n)
	}
	if err := svc.Delete(ctx, created.ID); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPostgresScheduledOccurrenceIsUnique(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	svc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{})
	tk, err := svc.Create(ctx, tasks.CreateInput{Name: "x", Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://x.dev"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO executions (id, task_id, application_id, trigger, status, scheduled_for, attempt)
		VALUES ($1, $2, 'app_clockwork', 'scheduled', 'queued', '2026-11-05T14:05:00Z', 1)`
	if _, err := pool.Exec(ctx, ins, "exe_a", tk.ID); err != nil {
		t.Fatal(err)
	}
	// A second scheduler materialising the same occurrence must be rejected.
	if _, err := pool.Exec(ctx, ins, "exe_b", tk.ID); err == nil {
		t.Fatal("duplicate scheduled occurrence was accepted")
	}
}
