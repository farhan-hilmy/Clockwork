package apps_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/apps"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func TestPostgresApplicationsAndKeys(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE executions, tasks, app_credentials; DELETE FROM applications WHERE id <> 'app_clockwork'`); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	svc := apps.NewService(apps.NewPostgresRepository(pool), func() time.Time { return now })

	a, err := svc.Create(ctx, apps.CreateInput{Name: "billing", Description: "invoices"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, apps.CreateInput{Name: "billing"}); !errors.Is(err, apps.ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	list, _ := svc.List(ctx)
	if len(list) != 2 || list[0].ID != apps.DefaultID || list[1].Name != "billing" {
		t.Fatalf("list: %+v", list)
	}

	// Keys: stored hashed, authenticate, track last use, revoke.
	k, err := svc.CreateKey(ctx, a.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	_ = pool.QueryRow(ctx, `SELECT secret_hash FROM app_credentials WHERE id = $1`, k.ID).Scan(&stored)
	if len(stored) != 32 || string(stored) == k.Secret {
		t.Fatalf("key not stored as a sha256 hash")
	}
	got, err := svc.Authenticate(ctx, k.Secret)
	if err != nil || got.ID != a.ID {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	keys, _ := svc.ListKeys(ctx, a.ID)
	if len(keys) != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(now) {
		t.Fatalf("last_used_at: %+v", keys)
	}
	if _, err := svc.Authenticate(ctx, k.Secret+"x"); !errors.Is(err, apps.ErrUnauthorized) {
		t.Fatalf("wrong key: %v", err)
	}

	disabled := false
	if _, err := svc.Update(ctx, a.ID, apps.UpdateInput{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, k.Secret); !errors.Is(err, apps.ErrAppDisabled) {
		t.Fatalf("disabled app: %v", err)
	}
	enabled := true
	_, _ = svc.Update(ctx, a.ID, apps.UpdateInput{Enabled: &enabled})

	if err := svc.RevokeKey(ctx, a.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, k.Secret); !errors.Is(err, apps.ErrUnauthorized) {
		t.Fatalf("revoked key: %v", err)
	}
	if err := svc.RevokeKey(ctx, apps.DefaultID, k.ID); !errors.Is(err, apps.ErrKeyNotFound) {
		t.Fatalf("revoke via another app: %v", err)
	}

	// Delete is refused while the application owns tasks, then cascades to keys.
	taskSvc := tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{})
	tk, err := taskSvc.Create(ctx, tasks.CreateInput{ApplicationID: a.ID, Name: "x",
		Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"https://example.com"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, a.ID); got.TaskCount != 1 {
		t.Fatalf("task_count = %d", got.TaskCount)
	}
	if err := svc.Delete(ctx, a.ID); !errors.Is(err, apps.ErrHasTasks) {
		t.Fatalf("delete with tasks: %v", err)
	}
	_ = taskSvc.Delete(ctx, tk.ID)
	if err := svc.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM app_credentials WHERE application_id = $1`, a.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d keys survived their application", n)
	}
	if err := svc.Delete(ctx, apps.DefaultID); !errors.Is(err, apps.ErrDefaultApp) {
		t.Fatalf("delete default: %v", err)
	}
}
