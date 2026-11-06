package executions_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

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
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks`); err != nil {
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
		e, err := svc.RunNow(ctx, tk.ID)
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
	if _, err := svc.RunNow(ctx, "tsk_missing"); !errors.Is(err, tasks.ErrNotFound) {
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
