package executor_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/executor"
	"github.com/farhan-hilmy/clockwork/internal/runners"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

func addApp(t *testing.T, e env, id string) {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO applications (id, name) VALUES ($1, $1)`, id); err != nil {
		t.Fatal(err)
	}
}

// Security: a shell task owned by an application never runs, even if one got
// into the database without validation (for example, before the rule existed).
func TestShellTaskOfAnApplicationDoesNotRun(t *testing.T) {
	e := setup(t, true) // shell runner registered
	addApp(t, e, "app_ext")
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO tasks (id, application_id, name, runner_type, runner_config, timeout_seconds, max_attempts, backoff_initial_seconds, backoff_max_seconds)
		VALUES ('tsk_sh', 'app_ext', 'sh', 'shell', '{"command":"echo pwned"}', 30, 1, 10, 300)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.execs.RunNow(t.Context(), "tsk_sh", ""); err != nil {
		t.Fatal(err)
	}
	runOnce(t, e)
	h := history(t, e, "tsk_sh")
	var f runners.Failure
	_ = json.Unmarshal(h[0].Error, &f)
	if h[0].Status != executions.StatusFailed || f.Kind != runners.KindConfig || h[0].Output != nil {
		t.Fatalf("shell task of an application ran: %v %s %s", statuses(h), h[0].Error, h[0].Output)
	}
}

func appTask(t *testing.T, e env, name, url string, timeout int) tasks.Task {
	t.Helper()
	tk, err := e.tasks.Create(t.Context(), tasks.CreateInput{ApplicationID: "app_ext", Name: name, TimeoutSeconds: &timeout,
		Schedule: allowOverlap, Runner: tasks.Runner{Type: tasks.RunnerHTTP, Config: json.RawMessage(`{"url":"` + url + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// Security: one application cannot occupy every worker.
func TestApplicationRunningCap(t *testing.T) {
	e := setup(t, false)
	addApp(t, e, "app_ext")
	srv := newSlowServer(t)
	mine := appTask(t, e, "slow", srv.URL+"/slow", 60)
	ops := httpTask(t, e, "operator", srv.URL+"/slow", allowOverlap, nil)
	for range 3 {
		_, _, _ = e.execs.RunNow(t.Context(), mine.ID, "")
	}
	_, _, _ = e.execs.RunNow(t.Context(), ops.ID, "")

	stop := start(t, realPool(e, executor.Options{Workers: 4, AppMaxRunning: 1, AppsMayReachPrivate: true}))
	defer stop()
	// The operator's run and exactly one of the application's runs start; the
	// application's other two wait although workers are free.
	for range 2 {
		select {
		case <-srv.started:
		case <-time.After(10 * time.Second):
			t.Fatal("runs did not start")
		}
	}
	select {
	case id := <-srv.started:
		t.Fatalf("a second run of the capped application started: %s", id)
	case <-time.After(700 * time.Millisecond):
	}
	running := 0
	for _, x := range history(t, e, mine.ID) {
		if x.Status == executions.StatusRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("%d runs of the application running, want 1", running)
	}
	close(srv.release) // they then run one after another
	waitFor(t, "all runs to finish", func() bool {
		for _, x := range history(t, e, mine.ID) {
			if x.Status != executions.StatusSucceeded {
				return false
			}
		}
		return true
	})
}

// Security: an application task's timeout is capped at run time, including
// tasks saved before the cap existed.
func TestApplicationTimeoutCapAtRunTime(t *testing.T) {
	e := setup(t, false)
	addApp(t, e, "app_ext")
	srv := newSlowServer(t)
	tk := appTask(t, e, "tarpit", srv.URL+"/slow", 3600) // the test service has no timeout limit
	_, _, _ = e.execs.RunNow(t.Context(), tk.ID, "")

	pool := executor.New(e.pool, executor.Options{AppMaxTimeout: 300 * time.Millisecond, AppsMayReachPrivate: true,
		Now: e.clock.now, Logger: quiet(),
		Runners: map[tasks.RunnerType]runners.Runner{tasks.RunnerHTTP: runners.NewHTTP(runners.HTTPOptions{})}})
	start := time.Now()
	if ran, err := pool.RunOnce(t.Context()); !ran || err != nil {
		t.Fatalf("run: %v %v", ran, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("run took %s: timeout not capped", d)
	}
	var f runners.Failure
	h := history(t, e, tk.ID)
	_ = json.Unmarshal(h[0].Error, &f)
	if f.Kind != runners.KindTimeout {
		t.Fatalf("want a timeout, got %v %s", statuses(h), h[0].Error)
	}
}
