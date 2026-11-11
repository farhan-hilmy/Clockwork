package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/executor"
	"github.com/farhan-hilmy/clockwork/internal/runners"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// allowOverlap is a schedule that never fires during a test but sets concurrency_policy=allow.
var allowOverlap = &tasks.Schedule{Cron: "0 0 1 1 *", ConcurrencyPolicy: tasks.ConcurrencyAllow}

// realPool is a pool on the real clock, for tests that rely on leases and timers.
func realPool(e env, o executor.Options) *executor.Pool {
	if o.Runners == nil {
		o.Runners = map[tasks.RunnerType]runners.Runner{tasks.RunnerHTTP: runners.NewHTTP(runners.HTTPOptions{})}
	}
	if o.PollInterval == 0 {
		o.PollInterval = 10 * time.Millisecond
	}
	o.Logger = quiet()
	return executor.New(e.pool, o)
}

// start runs p until the returned stop function is called; stop waits for Run to return.
func start(t *testing.T, p *executor.Pool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func statuses(h []executions.Execution) []string {
	out := make([]string, len(h))
	for i, x := range h {
		out[i] = x.Status
	}
	return out
}

// slowServer blocks /slow requests until released or canceled, and reports
// when each request starts and whether it was canceled by the client.
type slowServer struct {
	*httptest.Server
	release  chan struct{}
	started  chan string
	canceled chan string
}

func newSlowServer(t *testing.T) *slowServer {
	s := &slowServer{release: make(chan struct{}), started: make(chan string, 100), canceled: make(chan string, 100)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Clockwork-Execution-Id")
		s.started <- id
		select {
		case <-s.release:
		case <-r.Context().Done():
			s.canceled <- id
		}
	}))
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
		s.Close()
	})
	return s
}

// The M4 exit criterion, part 1: several instances competing for one queue run
// every execution exactly once.
func TestConcurrentWorkersRunEachExecutionOnce(t *testing.T) {
	e := setup(t, false)
	var (
		mu   sync.Mutex
		seen = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("X-Clockwork-Execution-Id")]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}))
	defer srv.Close()

	const perTask, nTasks = 15, 4
	var ids []string
	for i := range nTasks {
		tk := httpTask(t, e, fmt.Sprintf("t%d", i), srv.URL, allowOverlap, nil)
		for range perTask {
			x, _, err := e.execs.RunNow(t.Context(), tk.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, x.ID)
		}
	}

	// Four independent pools stand in for four Clockwork instances.
	var stops []func()
	for range 4 {
		stops = append(stops, start(t, realPool(e, executor.Options{Workers: 4})))
	}
	waitFor(t, "all executions to finish", func() bool {
		p, _ := e.execs.List(t.Context(), executions.ListParams{Status: executions.StatusSucceeded, Limit: 200})
		return len(p.Executions) == len(ids)
	})
	for _, stop := range stops {
		stop()
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("execution %s ran %d times", id, seen[id])
		}
	}
	if len(seen) != len(ids) {
		t.Errorf("server saw %d distinct executions, want %d", len(seen), len(ids))
	}
}

func TestForbidSkipsOverlappingRunsAcrossInstances(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	forbid := httpTask(t, e, "forbid", srv.URL+"/slow", nil, nil) // unscheduled tasks use forbid
	for range 3 {
		_, _, _ = e.execs.RunNow(t.Context(), forbid.ID, "")
	}

	// Two instances race for three queued runs of the same task: one runs, two are skipped.
	stopA := start(t, realPool(e, executor.Options{Workers: 4}))
	stopB := start(t, realPool(e, executor.Options{Workers: 4}))
	<-srv.started
	waitFor(t, "overlapping runs to be skipped", func() bool {
		h := history(t, e, forbid.ID)
		skipped := 0
		for _, x := range h {
			if x.Status == executions.StatusSkipped {
				skipped++
			}
		}
		return skipped == 2
	})
	close(srv.release)
	waitFor(t, "the run to finish", func() bool {
		h := history(t, e, forbid.ID)
		for _, x := range h {
			if x.Status == executions.StatusSucceeded {
				return true
			}
		}
		return false
	})
	stopA()
	stopB()

	h := history(t, e, forbid.ID)
	succeeded, skipped := 0, 0
	for _, x := range h {
		switch x.Status {
		case executions.StatusSucceeded:
			succeeded++
		case executions.StatusSkipped:
			skipped++
			var f runners.Failure
			_ = json.Unmarshal(x.Error, &f)
			if f.Kind != "concurrency" || x.FinishedAt == nil {
				t.Errorf("skipped execution: %+v error=%s", x, x.Error)
			}
		}
	}
	if succeeded != 1 || skipped != 2 {
		t.Fatalf("statuses %v, want 1 succeeded and 2 skipped", statuses(h))
	}
}

func TestAllowRunsOverlap(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	tk := httpTask(t, e, "allow", srv.URL+"/slow", allowOverlap, nil)
	for range 3 {
		_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")
	}
	stop := start(t, realPool(e, executor.Options{Workers: 4}))
	for range 3 {
		select {
		case <-srv.started:
		case <-time.After(10 * time.Second):
			t.Fatal("runs did not overlap")
		}
	}
	close(srv.release)
	waitFor(t, "runs to finish", func() bool {
		h := history(t, e, tk.ID)
		return len(h) == 3 && h[0].Status == executions.StatusSucceeded && h[1].Status == executions.StatusSucceeded && h[2].Status == executions.StatusSucceeded
	})
	stop()
}

// The M4 exit criterion, part 2: work held by a crashed worker is recovered and retried.
func TestCrashedWorkerIsRecoveredAndRetried(t *testing.T) {
	e := setup(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	retried := httpTask(t, e, "retried", srv.URL, nil, &tasks.RetryPolicy{MaxAttempts: 2, BackoffInitialSeconds: 5, BackoffMaxSeconds: 5})
	final := httpTask(t, e, "final", srv.URL, nil, nil) // max_attempts 1: no retry

	// A worker claims both executions, then the process dies: nothing renews the leases.
	for _, id := range []string{retried.ID, final.ID} {
		x, _, _ := e.execs.RunNow(t.Context(), id, "")
		if _, err := e.pool.Exec(t.Context(), `UPDATE executions SET status = 'running', started_at = $2, lease_expires_at = $3 WHERE id = $1`,
			x.ID, e.clock.now(), e.clock.now().Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	// Before the lease expires, recovery leaves the runs alone.
	if n, err := e.exec.RecoverExpired(t.Context()); err != nil || n != 0 {
		t.Fatalf("early recovery: %d %v", n, err)
	}
	// A new instance starts after the lease has expired.
	e.clock.advance(31 * time.Second)
	if n, err := e.exec.RecoverExpired(t.Context()); err != nil || n != 2 {
		t.Fatalf("recovery: %d %v", n, err)
	}

	h := history(t, e, retried.ID)
	if len(h) != 2 || h[0].Status != executions.StatusInterrupted || h[1].Status != executions.StatusQueued || h[1].Attempt != 2 {
		t.Fatalf("retried task: %v", statuses(h))
	}
	var f struct {
		Kind      string     `json:"kind"`
		Retryable bool       `json:"retryable"`
		RetryAt   *time.Time `json:"retry_at"`
	}
	_ = json.Unmarshal(h[0].Error, &f)
	if f.Kind != runners.KindInterrupted || !f.Retryable || f.RetryAt == nil || !f.RetryAt.Equal(h[1].RunAfter) || h[0].FinishedAt == nil {
		t.Fatalf("interrupted attempt error: %s", h[0].Error)
	}
	if h := history(t, e, final.ID); len(h) != 1 || h[0].Status != executions.StatusInterrupted {
		t.Fatalf("final task: %v", statuses(h))
	}

	// The retry runs once its backoff has passed.
	e.clock.advance(5 * time.Second)
	if !runOnce(t, e) {
		t.Fatal("retry was not runnable")
	}
	if h := history(t, e, retried.ID); h[1].Status != executions.StatusSucceeded {
		t.Fatalf("retry: %v", statuses(h))
	}
}

func TestLeaseIsRenewedWhileRunning(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	tk := httpTask(t, e, "long", srv.URL+"/slow", nil, nil)
	_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")

	// The run lasts several lease periods; frequent recovery sweeps must not touch it.
	stop := start(t, realPool(e, executor.Options{Lease: 300 * time.Millisecond, RecoveryInterval: 50 * time.Millisecond}))
	<-srv.started
	time.Sleep(1200 * time.Millisecond)
	if h := history(t, e, tk.ID); h[0].Status != executions.StatusRunning {
		t.Fatalf("long run was recovered while its worker was alive: %v", statuses(h))
	}
	close(srv.release)
	waitFor(t, "the run to succeed", func() bool { return history(t, e, tk.ID)[0].Status == executions.StatusSucceeded })
	stop()
}

func TestStalledWorkerLosesLeaseAndStops(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	tk := httpTask(t, e, "stalled", srv.URL+"/slow", nil, &tasks.RetryPolicy{MaxAttempts: 2, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1})
	_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")

	stop := start(t, realPool(e, executor.Options{Lease: 300 * time.Millisecond, RecoveryInterval: time.Hour}))
	id := <-srv.started
	// Another instance decides this worker is dead, as if the worker had stalled
	// past its lease. Its clock runs an hour ahead, so the lease looks long expired.
	other := realPool(e, executor.Options{Now: func() time.Time { return time.Now().Add(time.Hour) }})
	if n, err := other.RecoverExpired(t.Context()); err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	// The original worker notices at its next renewal and abandons the request.
	select {
	case got := <-srv.canceled:
		if got != id {
			t.Fatalf("canceled %s, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled worker kept running after losing its lease")
	}
	stop()

	h := history(t, e, tk.ID)
	if h[0].Status != executions.StatusInterrupted {
		t.Fatalf("first attempt %v: the stalled worker must not overwrite the recovery", statuses(h))
	}
}

func TestShutdownInterruptsAndRetries(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	tk := httpTask(t, e, "slow", srv.URL+"/slow", nil, &tasks.RetryPolicy{MaxAttempts: 2, BackoffInitialSeconds: 1, BackoffMaxSeconds: 1})
	_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")

	stop := start(t, realPool(e, executor.Options{ShutdownGrace: 200 * time.Millisecond}))
	<-srv.started
	stop() // the run outlives the grace period

	h := history(t, e, tk.ID)
	if len(h) != 2 || h[0].Status != executions.StatusInterrupted || h[0].FinishedAt == nil ||
		h[1].Status != executions.StatusQueued || h[1].Attempt != 2 {
		t.Fatalf("after shutdown: %v", statuses(h))
	}
}

func TestShutdownWaitsForRunsWithinGrace(t *testing.T) {
	e := setup(t, false)
	srv := newSlowServer(t)
	tk := httpTask(t, e, "quick-enough", srv.URL+"/slow", nil, nil)
	_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")

	stop := start(t, realPool(e, executor.Options{ShutdownGrace: 10 * time.Second}))
	<-srv.started
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(srv.release) // finishes during the grace period
	}()
	stop()
	if h := history(t, e, tk.ID); h[0].Status != executions.StatusSucceeded {
		t.Fatalf("run in flight at shutdown: %v", statuses(h))
	}
}

// Applications' HTTP tasks may not reach private addresses unless the operator allows it.
func TestApplicationTasksCannotReachPrivateAddresses(t *testing.T) {
	e := setup(t, false)
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO applications (id, name) VALUES ('app_ext', 'ext')`); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	own := httpTask(t, e, "operator", srv.URL, nil, nil)
	ext, err := e.tasks.Create(t.Context(), tasks.CreateInput{ApplicationID: "app_ext", Name: "external",
		Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"` + srv.URL + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{own.ID, ext.ID} {
		_, _, _ = e.execs.RunNow(t.Context(), id, "")
	}
	for runOnce(t, e) {
	}
	if h := history(t, e, own.ID); h[0].Status != executions.StatusSucceeded {
		t.Fatalf("operator task to 127.0.0.1: %v", statuses(h))
	}
	h := history(t, e, ext.ID)
	var f runners.Failure
	_ = json.Unmarshal(h[0].Error, &f)
	if h[0].Status != executions.StatusFailed || f.Kind != runners.KindBlocked {
		t.Fatalf("application task to 127.0.0.1: %v %s", statuses(h), h[0].Error)
	}

	// With CLOCKWORK_APP_HTTP_ALLOW_PRIVATE=true the same task may run.
	allowing := executor.New(e.pool, executor.Options{AppsMayReachPrivate: true, Now: e.clock.now, Logger: quiet(),
		Runners: map[tasks.RunnerType]runners.Runner{tasks.RunnerHTTP: runners.NewHTTP(runners.HTTPOptions{})}})
	_, _, _ = e.execs.RunNow(t.Context(), ext.ID, "")
	if ran, err := allowing.RunOnce(t.Context()); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	if h := history(t, e, ext.ID); h[1].Status != executions.StatusSucceeded {
		t.Fatalf("allowed application task: %v", statuses(h))
	}
}
