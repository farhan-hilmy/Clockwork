// Package executor runs queued executions.
//
// One dispatcher claims queued executions (FOR UPDATE SKIP LOCKED, so several
// Clockwork instances can share the queue) and hands each to a worker, up to
// the configured concurrency. A worker runs the task's runner under the task's
// timeout, records the result, and queues the next attempt with exponential
// backoff when the failure is retryable. See docs/ARCHITECTURE.md §3.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/ids"
	"github.com/farhan-hilmy/clockwork/internal/runners"
	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// Options configures a Pool.
type Options struct {
	// Workers is the maximum number of executions running at once. Defaults to 4.
	Workers int
	// PollInterval is how often the queue is checked when it is empty. Defaults to 1s.
	PollInterval time.Duration
	// ShutdownGrace is how long in-flight runs may continue after Run's context
	// is canceled before their contexts are canceled too. Defaults to 30s.
	ShutdownGrace time.Duration
	// Runners maps runner types to implementations. A missing type fails the
	// execution without retry (for example shell tasks when shell is disabled).
	Runners map[tasks.RunnerType]runners.Runner
	// Now is an injectable clock for tests. Defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Pool claims and runs queued executions.
type Pool struct {
	db      *pgxpool.Pool
	workers int
	poll    time.Duration
	grace   time.Duration
	runners map[tasks.RunnerType]runners.Runner
	now     func() time.Time
	log     *slog.Logger
}

// New builds a Pool.
func New(db *pgxpool.Pool, o Options) *Pool {
	p := &Pool{db: db, workers: o.Workers, poll: o.PollInterval, grace: o.ShutdownGrace, runners: o.Runners, now: o.Now, log: o.Logger}
	if p.workers <= 0 {
		p.workers = 4
	}
	if p.poll <= 0 {
		p.poll = time.Second
	}
	if p.grace <= 0 {
		p.grace = 30 * time.Second
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

// claimed is an execution claimed for running, with what is needed to run it.
type claimed struct {
	id, taskID, appID, trigger string
	attempt                    int
	scheduledFor               *time.Time
	runnerType                 tasks.RunnerType
	config                     json.RawMessage
	timeout                    time.Duration
	retry                      tasks.RetryPolicy
	startedAt                  time.Time
}

// Run dispatches executions until ctx is canceled, then stops claiming and
// waits for in-flight runs. Runs still going after ShutdownGrace are canceled
// and recorded as interrupted. Run returns once every run has been recorded.
func (p *Pool) Run(ctx context.Context) {
	p.log.Info("executor started", "workers", p.workers, "runners", p.runnerNames())
	// runCtx outlives ctx so in-flight runs can finish during the grace period.
	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()

	slots := make(chan struct{}, p.workers)
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		e, ok, err := p.claim(ctx)
		if err != nil || !ok {
			<-slots
			if err != nil && ctx.Err() == nil {
				p.log.Error("claim execution failed", "err", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(p.poll):
			}
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			p.execute(runCtx, e)
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.grace):
		p.log.Warn("shutdown grace elapsed; interrupting running executions", "grace", p.grace)
		cancelRuns()
		<-done
	}
	p.log.Info("executor stopped")
}

// RunOnce claims and runs a single execution synchronously. It reports whether
// one was available. Tests use it to drive the pool step by step.
func (p *Pool) RunOnce(ctx context.Context) (bool, error) {
	e, ok, err := p.claim(ctx)
	if err != nil || !ok {
		return false, err
	}
	p.execute(ctx, e)
	return true, nil
}

func (p *Pool) claim(ctx context.Context) (claimed, bool, error) {
	now := p.now().UTC().Truncate(time.Microsecond)
	var (
		e     claimed
		rtype string
		secs  int
	)
	// The lease is a deadline after which an execution is presumed abandoned.
	// M4 adds extension and recovery; here it is set to the timeout plus a margin.
	err := p.db.QueryRow(ctx, `WITH next AS (
			SELECT id FROM executions
			WHERE status = 'queued' AND run_after <= $1
			ORDER BY run_after, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		UPDATE executions e
		SET status = 'running', started_at = $1,
		    lease_expires_at = $1 + make_interval(secs => t.timeout_seconds + 60)
		FROM next, tasks t
		WHERE e.id = next.id AND t.id = e.task_id
		RETURNING e.id, e.task_id, e.application_id, e.trigger, e.attempt, e.scheduled_for,
		          t.runner_type, t.runner_config, t.timeout_seconds,
		          t.max_attempts, t.backoff_initial_seconds, t.backoff_max_seconds`, now,
	).Scan(&e.id, &e.taskID, &e.appID, &e.trigger, &e.attempt, &e.scheduledFor,
		&rtype, &e.config, &secs,
		&e.retry.MaxAttempts, &e.retry.BackoffInitialSeconds, &e.retry.BackoffMaxSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimed{}, false, nil
	}
	if err != nil {
		return claimed{}, false, fmt.Errorf("claim: %w", err)
	}
	e.runnerType = tasks.RunnerType(rtype)
	e.timeout = time.Duration(secs) * time.Second
	e.startedAt = now
	return e, true, nil
}

// execute runs one claimed execution and records its outcome.
func (p *Pool) execute(ctx context.Context, e claimed) {
	start := time.Now() // monotonic, for the duration
	output, runErr := p.run(ctx, e)
	duration := time.Since(start)

	var failure *runners.Failure
	status := executions.StatusSucceeded
	if runErr != nil {
		failure = runners.AsFailure(runErr)
		status = executions.StatusFailed
		if failure.Kind == runners.KindInterrupted {
			status = executions.StatusInterrupted
		}
	}
	var retryAt *time.Time
	if failure != nil && failure.Retryable && e.attempt < e.retry.MaxAttempts {
		t := p.now().UTC().Truncate(time.Microsecond).Add(Backoff(e.retry, e.attempt))
		retryAt = &t
		status = executions.StatusRetrying
	}

	// Record even if ctx was canceled during shutdown: the pool stays open until Run returns.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := p.record(recCtx, e, status, output, failure, retryAt, duration); err != nil {
		p.log.Error("record execution result failed", "execution_id", e.id, "err", err)
		return
	}

	attrs := []any{"execution_id", e.id, "task_id", e.taskID, "attempt", e.attempt, "status", status, "duration_ms", duration.Milliseconds()}
	if failure != nil {
		attrs = append(attrs, "error_kind", failure.Kind, "error", failure.Message)
	}
	if retryAt != nil {
		attrs = append(attrs, "retry_at", *retryAt)
	}
	if failure != nil {
		p.log.Warn("execution failed", attrs...)
	} else {
		p.log.Info("execution succeeded", attrs...)
	}
}

func (p *Pool) run(ctx context.Context, e claimed) (out json.RawMessage, err error) {
	r, ok := p.runners[e.runnerType]
	if !ok {
		return nil, &runners.Failure{Kind: runners.KindConfig, Message: fmt.Sprintf("the %s runner is not enabled on this server", e.runnerType)}
	}
	defer func() {
		// A runner bug must not take down the worker pool.
		if rec := recover(); rec != nil {
			out, err = nil, &runners.Failure{Kind: runners.KindInternal, Message: fmt.Sprintf("runner panicked: %v", rec)}
		}
	}()
	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	return r.Run(tctx, runners.Request{Config: e.config, ExecutionID: e.id, TaskID: e.taskID, Attempt: e.attempt})
}

// errorDoc is stored as executions.error.
type errorDoc struct {
	*runners.Failure
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

func (p *Pool) record(ctx context.Context, e claimed, status string, output json.RawMessage, failure *runners.Failure, retryAt *time.Time, d time.Duration) error {
	var errJSON []byte
	if failure != nil {
		errJSON, _ = json.Marshal(errorDoc{Failure: failure, RetryAt: retryAt})
	}
	finished := e.startedAt.Add(d).Truncate(time.Microsecond)
	return pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE executions
			SET status = $2, output = $3, error = $4, finished_at = $5, duration_ms = $6, lease_expires_at = NULL
			WHERE id = $1 AND status = 'running'`,
			e.id, status, nullJSON(output), nullJSON(errJSON), finished, d.Milliseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// The row changed under us (deleted with its task, or recovered after an
			// expired lease in M4). Its new owner decides what happens next.
			return nil
		}
		if retryAt == nil {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO executions
			(id, task_id, application_id, trigger, status, attempt, scheduled_for, created_at, run_after)
			VALUES ($1, $2, $3, $4, 'queued', $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`,
			ids.New("exe"), e.taskID, e.appID, e.trigger, e.attempt+1, e.scheduledFor, finished, *retryAt)
		return err
	})
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Backoff is the delay before the attempt after failedAttempt (1-based):
// backoff_initial_seconds doubled per attempt, capped at backoff_max_seconds.
func Backoff(r tasks.RetryPolicy, failedAttempt int) time.Duration {
	d := time.Duration(r.BackoffInitialSeconds) * time.Second
	limit := time.Duration(r.BackoffMaxSeconds) * time.Second
	for i := 1; i < failedAttempt && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (p *Pool) runnerNames() []string {
	names := make([]string, 0, len(p.runners))
	for t := range p.runners {
		names = append(names, string(t))
	}
	return names
}
