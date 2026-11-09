package executor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/executor"
	"github.com/farhan-hilmy/clockwork/internal/runners"
	"github.com/farhan-hilmy/clockwork/internal/scheduler"
	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func TestBackoff(t *testing.T) {
	r := tasks.RetryPolicy{MaxAttempts: 10, BackoffInitialSeconds: 5, BackoffMaxSeconds: 60}
	for attempt, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 4: 40 * time.Second, 5: 60 * time.Second, 50: 60 * time.Second} {
		if got := executor.Backoff(r, attempt); got != want {
			t.Errorf("Backoff(attempt %d) = %v, want %v", attempt, got, want)
		}
	}
}

func openTestDB(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(t.Context(), `TRUNCATE executions, tasks`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// clock is a settable time source; it starts at the real current time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type env struct {
	pool  *pgxpool.Pool
	clock *clock
	tasks *tasks.Service
	execs *executions.Service
	exec  *executor.Pool
}

func setup(t *testing.T, shell bool) env {
	t.Helper()
	pool := openTestDB(t)
	c := &clock{t: time.Now().UTC()}
	run := map[tasks.RunnerType]runners.Runner{tasks.RunnerHTTP: runners.NewHTTP(runners.HTTPOptions{})}
	if shell {
		run[tasks.RunnerShell] = &runners.Shell{}
	}
	return env{
		pool:  pool,
		clock: c,
		tasks: tasks.NewService(tasks.NewPostgresRepository(pool), tasks.Options{Now: c.now, AllowShell: true}),
		execs: executions.NewService(executions.NewPostgresRepository(pool), c.now),
		exec:  executor.New(pool, executor.Options{Runners: run, Now: c.now, Logger: quiet(), ShutdownGrace: 200 * time.Millisecond, PollInterval: 20 * time.Millisecond}),
	}
}

func httpTask(t *testing.T, e env, name, url string, sched *tasks.Schedule, retry *tasks.RetryPolicy) tasks.Task {
	t.Helper()
	tk, err := e.tasks.Create(t.Context(), tasks.CreateInput{
		Name: name, Schedule: sched, Retry: retry,
		Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"` + url + `"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func history(t *testing.T, e env, taskID string) []executions.Execution {
	t.Helper()
	p, err := e.execs.List(t.Context(), executions.ListParams{TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first reads more naturally in assertions.
	out := p.Executions
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func runOnce(t *testing.T, e env) bool {
	t.Helper()
	ran, err := e.exec.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return ran
}

// The M3 exit criterion: a scheduled HTTP task records status code, duration and an excerpt.
func TestScheduledHTTPTaskRecordsResult(t *testing.T) {
	e := setup(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer srv.Close()
	tk := httpTask(t, e, "health", srv.URL, &tasks.Schedule{Cron: "* * * * *"}, nil)

	// Let the schedule fall due, and let the scheduler queue it.
	e.clock.advance(time.Until(*tk.NextRunAt) + time.Second)
	sched := scheduler.New(e.pool, scheduler.Options{Now: e.clock.now, Logger: quiet()})
	if r, err := sched.Tick(t.Context()); err != nil || r.Enqueued != 1 {
		t.Fatalf("tick: %+v %v", r, err)
	}
	if !runOnce(t, e) {
		t.Fatal("nothing to run")
	}
	if runOnce(t, e) {
		t.Fatal("ran a second time")
	}

	h := history(t, e, tk.ID)
	if len(h) != 1 {
		t.Fatalf("history: %+v", h)
	}
	x := h[0]
	var out runners.HTTPOutput
	_ = json.Unmarshal(x.Output, &out)
	if x.Status != executions.StatusSucceeded || x.Trigger != executions.TriggerScheduled ||
		out.StatusCode != 200 || out.Body != `{"status":"healthy"}` ||
		x.DurationMS == nil || *x.DurationMS < 20 || x.StartedAt == nil || x.FinishedAt == nil || x.Error != nil {
		t.Fatalf("execution: %+v output=%s", x, x.Output)
	}
}

func TestRetryableFailureIsRetriedWithBackoff(t *testing.T) {
	e := setup(t, false)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("busy"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	tk := httpTask(t, e, "flaky", srv.URL, nil, &tasks.RetryPolicy{MaxAttempts: 3, BackoffInitialSeconds: 10, BackoffMaxSeconds: 60})
	if _, err := e.execs.RunNow(t.Context(), tk.ID); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 fails with 503 and queues attempt 2 ten seconds later.
	runOnce(t, e)
	if runOnce(t, e) {
		t.Fatal("retry ran before its backoff elapsed")
	}
	e.clock.advance(10 * time.Second)
	runOnce(t, e) // attempt 2: 503 again, attempt 3 after 20s
	e.clock.advance(19 * time.Second)
	if runOnce(t, e) {
		t.Fatal("second backoff was not doubled")
	}
	e.clock.advance(time.Second)
	runOnce(t, e) // attempt 3 succeeds

	h := history(t, e, tk.ID)
	if len(h) != 3 {
		t.Fatalf("history has %d attempts", len(h))
	}
	for i, want := range []string{executions.StatusRetrying, executions.StatusRetrying, executions.StatusSucceeded} {
		if h[i].Status != want || h[i].Attempt != i+1 || h[i].Trigger != executions.TriggerManual {
			t.Fatalf("attempt %d: %+v", i+1, h[i])
		}
	}
	var f struct {
		Kind      string    `json:"kind"`
		Retryable bool      `json:"retryable"`
		RetryAt   time.Time `json:"retry_at"`
	}
	_ = json.Unmarshal(h[1].Error, &f)
	if f.Kind != runners.KindHTTPStatus || !f.Retryable || !f.RetryAt.Equal(h[2].RunAfter) {
		t.Fatalf("attempt 2 error %s, attempt 3 run_after %v", h[1].Error, h[2].RunAfter)
	}
}

func TestLastAttemptFailsWithoutRetry(t *testing.T) {
	e := setup(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	tk := httpTask(t, e, "down", srv.URL, nil, &tasks.RetryPolicy{MaxAttempts: 2, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1})
	_, _ = e.execs.RunNow(t.Context(), tk.ID)
	runOnce(t, e)
	e.clock.advance(time.Second)
	runOnce(t, e)
	e.clock.advance(time.Hour)
	if runOnce(t, e) {
		t.Fatal("retried beyond max_attempts")
	}
	h := history(t, e, tk.ID)
	if len(h) != 2 || h[0].Status != executions.StatusRetrying || h[1].Status != executions.StatusFailed {
		t.Fatalf("history: %+v", h)
	}
}

func TestNonRetryableFailureAndDisabledRunner(t *testing.T) {
	e := setup(t, false) // shell runner not registered
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	notFound := httpTask(t, e, "missing", srv.URL, nil, &tasks.RetryPolicy{MaxAttempts: 5, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1})
	shell, err := e.tasks.Create(t.Context(), tasks.CreateInput{Name: "sh",
		Runner: tasks.Runner{Type: tasks.RunnerShell, Config: json.RawMessage(`{"command":"true"}`)},
		Retry:  &tasks.RetryPolicy{MaxAttempts: 5, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{notFound.ID, shell.ID} {
		_, _ = e.execs.RunNow(t.Context(), id)
	}
	for runOnce(t, e) {
	}
	for id, kind := range map[string]string{notFound.ID: runners.KindHTTPStatus, shell.ID: runners.KindConfig} {
		h := history(t, e, id)
		var f runners.Failure
		_ = json.Unmarshal(h[0].Error, &f)
		if len(h) != 1 || h[0].Status != executions.StatusFailed || f.Kind != kind || f.Retryable {
			t.Fatalf("task %s: %+v error=%s", id, h, h[0].Error)
		}
	}
}

func TestRunProcessesQueueAndInterruptsOnShutdown(t *testing.T) {
	e := setup(t, false)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer srv.Close()
	defer close(release)
	fast := httpTask(t, e, "fast", srv.URL+"/fast", nil, nil)
	slow := httpTask(t, e, "slow", srv.URL+"/slow", nil, nil)
	for range 3 {
		_, _ = e.execs.RunNow(t.Context(), fast.ID)
	}
	_, _ = e.execs.RunNow(t.Context(), slow.ID)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.exec.Run(ctx); close(done) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		h := history(t, e, fast.ID)
		s := history(t, e, slow.ID)
		if len(h) == 3 && h[0].Status == executions.StatusSucceeded && h[1].Status == executions.StatusSucceeded &&
			h[2].Status == executions.StatusSucceeded && s[0].Status == executions.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue not processed: fast=%+v slow=%+v", h, s)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel() // the slow run outlives the 200ms grace and is interrupted
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	s := history(t, e, slow.ID)
	if s[0].Status != executions.StatusInterrupted || s[0].FinishedAt == nil {
		t.Fatalf("slow run after shutdown: %+v", s[0])
	}
}
