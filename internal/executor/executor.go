// Package executor runs queued executions.
//
// One dispatcher claims queued executions (FOR UPDATE SKIP LOCKED, so several
// Clockwork instances can share the queue) and hands each to a worker, up to
// the configured concurrency. A worker runs the task's runner under the task's
// timeout, records the result, and queues the next attempt with exponential
// backoff when the failure is retryable.
//
// Reliability (docs/ARCHITECTURE.md §3):
//   - Every running execution holds a lease that its worker renews. A worker
//     that crashes or hangs stops renewing; a recovery sweep then marks the
//     execution interrupted and applies the retry policy.
//   - concurrency_policy=forbid: a claim locks the task row and records the
//     execution as skipped if another run of the task is active.
//   - On shutdown, in-flight runs get a grace period, then are interrupted and
//     go through the retry policy.
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
	// is canceled before they are interrupted. Defaults to 30s.
	ShutdownGrace time.Duration
	// Lease is how long a running execution stays owned without a renewal.
	// Workers renew it every Lease/3. Defaults to 30s.
	Lease time.Duration
	// RecoveryInterval is how often expired leases are swept. Defaults to 15s.
	RecoveryInterval time.Duration
	// Runners maps runner types to implementations. A missing type fails the
	// execution without retry (for example shell tasks when shell is disabled).
	Runners map[tasks.RunnerType]runners.Runner
	// AppsMayReachPrivate lets HTTP tasks owned by external applications reach
	// loopback, link-local and private addresses. Off by default: an application
	// can read its executions' output, so reaching internal services would let an
	// API key holder read them.
	AppsMayReachPrivate bool
	// Now is an injectable clock for tests. Defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Pool claims and runs queued executions.
type Pool struct {
	db       *pgxpool.Pool
	workers  int
	poll     time.Duration
	grace    time.Duration
	lease    time.Duration
	recovery time.Duration
	runners  map[tasks.RunnerType]runners.Runner
	appsPriv bool
	now      func() time.Time
	log      *slog.Logger
}

// New builds a Pool.
func New(db *pgxpool.Pool, o Options) *Pool {
	p := &Pool{db: db, workers: o.Workers, poll: o.PollInterval, grace: o.ShutdownGrace,
		lease: o.Lease, recovery: o.RecoveryInterval, runners: o.Runners, appsPriv: o.AppsMayReachPrivate, now: o.Now, log: o.Logger}
	if p.workers <= 0 {
		p.workers = 4
	}
	if p.poll <= 0 {
		p.poll = time.Second
	}
	if p.grace <= 0 {
		p.grace = 30 * time.Second
	}
	if p.lease <= 0 {
		p.lease = 30 * time.Second
	}
	if p.recovery <= 0 {
		p.recovery = 15 * time.Second
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	return p
}

func (p *Pool) clock() time.Time { return p.now().UTC().Truncate(time.Microsecond) }

// attempt is an execution being finished, with what is needed to record it and retry it.
type attempt struct {
	id, taskID, appID, trigger string
	number                     int
	scheduledFor               *time.Time
	retry                      tasks.RetryPolicy
	startedAt                  time.Time
}

// claimed is an attempt claimed for running.
type claimed struct {
	attempt
	runnerType tasks.RunnerType
	config     json.RawMessage
	timeout    time.Duration
}

// Run recovers expired leases, then dispatches executions until ctx is canceled.
// It then stops claiming and waits for in-flight runs; runs still going after
// ShutdownGrace are interrupted. Run returns once every run has been recorded.
func (p *Pool) Run(ctx context.Context) {
	p.log.Info("executor started", "workers", p.workers, "runners", p.runnerNames(), "lease", p.lease)
	// runCtx outlives ctx so in-flight runs can finish during the grace period.
	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()

	var wg sync.WaitGroup
	wg.Go(func() { p.recoveryLoop(ctx) })

	slots := make(chan struct{}, p.workers)
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

// claim returns the next runnable execution, marking it running. Executions
// that would overlap an active run of a forbid-policy task are recorded as
// skipped along the way.
func (p *Pool) claim(ctx context.Context) (claimed, bool, error) {
	for range 100 {
		e, outcome, err := p.claimOne(ctx)
		if err != nil || outcome != claimSkipped {
			return e, outcome == claimRunning, err
		}
	}
	return claimed{}, false, nil // many skips in a row: yield and poll again
}

type claimOutcome int

const (
	claimNone claimOutcome = iota
	claimRunning
	claimSkipped
)

func (p *Pool) claimOne(ctx context.Context) (claimed, claimOutcome, error) {
	var (
		e       claimed
		outcome claimOutcome
	)
	err := pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		now := p.clock()
		var (
			rtype, policy string
			secs          int
		)
		err := tx.QueryRow(ctx, `SELECT e.id, e.task_id, e.application_id, e.trigger, e.attempt, e.scheduled_for,
				t.runner_type, t.runner_config, t.timeout_seconds, t.concurrency_policy,
				t.max_attempts, t.backoff_initial_seconds, t.backoff_max_seconds
			FROM executions e JOIN tasks t ON t.id = e.task_id
			WHERE e.status = 'queued' AND e.run_after <= $1
			ORDER BY e.run_after, e.id
			LIMIT 1
			FOR UPDATE OF e SKIP LOCKED`, now,
		).Scan(&e.id, &e.taskID, &e.appID, &e.trigger, &e.number, &e.scheduledFor,
			&rtype, &e.config, &secs, &policy,
			&e.retry.MaxAttempts, &e.retry.BackoffInitialSeconds, &e.retry.BackoffMaxSeconds)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = claimNone
			return nil
		}
		if err != nil {
			return fmt.Errorf("select queued execution: %w", err)
		}
		e.runnerType = tasks.RunnerType(rtype)
		e.timeout = time.Duration(secs) * time.Second
		e.startedAt = now

		if policy == tasks.ConcurrencyForbid {
			// Serialise claims per task: a second claimer waits here until this
			// transaction commits, then sees our run as active. An advisory lock rather
			// than a row lock, so it cannot deadlock with a task delete cascading into
			// the execution row we already hold.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, e.taskID); err != nil {
				return fmt.Errorf("lock task: %w", err)
			}
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM executions WHERE task_id = $1 AND status = 'running')`,
				e.taskID).Scan(&active); err != nil {
				return fmt.Errorf("check active runs: %w", err)
			}
			if active {
				outcome = claimSkipped
				errJSON, _ := json.Marshal(runners.Failure{Kind: "concurrency",
					Message: "skipped: a previous run of this task is still active (concurrency_policy=forbid)"})
				_, err := tx.Exec(ctx, `UPDATE executions SET status = 'skipped', error = $2, finished_at = $3 WHERE id = $1`,
					e.id, errJSON, now)
				return err
			}
		}

		outcome = claimRunning
		_, err = tx.Exec(ctx, `UPDATE executions SET status = 'running', started_at = $2, lease_expires_at = $3 WHERE id = $1`,
			e.id, now, now.Add(p.lease))
		return err
	})
	if err != nil {
		return claimed{}, claimNone, fmt.Errorf("claim: %w", err)
	}
	if outcome == claimSkipped {
		p.log.Info("execution skipped: previous run still active", "execution_id", e.id, "task_id", e.taskID)
	}
	return e, outcome, nil
}

// execute runs one claimed execution, renewing its lease, and records the outcome.
func (p *Pool) execute(ctx context.Context, e claimed) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopHeartbeat := p.heartbeat(runCtx, e.id, cancel)

	start := time.Now() // monotonic, for the duration
	output, runErr := p.run(runCtx, e)
	duration := time.Since(start)
	stopHeartbeat()

	if errors.Is(context.Cause(runCtx), errLeaseLost) {
		// Recovery already interrupted this execution and owns what happens next.
		p.log.Warn("lease lost while running; result discarded", "execution_id", e.id, "task_id", e.taskID)
		return
	}

	var failure *runners.Failure
	status := executions.StatusSucceeded
	if runErr != nil {
		failure = runners.AsFailure(runErr)
		status = executions.StatusFailed
		if failure.Kind == runners.KindInterrupted {
			status = executions.StatusInterrupted
			failure.Retryable = true // shutdown is not the task's fault
		}
	}
	retryAt := p.retryAt(e.attempt, failure)
	if retryAt != nil && status == executions.StatusFailed {
		status = executions.StatusRetrying
	}

	// Record even if ctx was canceled during shutdown: the pool stays open until Run returns.
	recCtx, cancelRec := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelRec()
	finished := e.startedAt.Add(duration).Truncate(time.Microsecond)
	err := pgx.BeginFunc(recCtx, p.db, func(tx pgx.Tx) error {
		return p.finish(recCtx, tx, e.attempt, status, output, failure, retryAt, finished, duration.Milliseconds())
	})
	if err != nil {
		p.log.Error("record execution result failed", "execution_id", e.id, "err", err)
		return
	}

	attrs := []any{"execution_id", e.id, "task_id", e.taskID, "attempt", e.number, "status", status, "duration_ms", duration.Milliseconds()}
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

var errLeaseLost = errors.New("lease lost")

// heartbeat renews the lease every lease/3 until stopped. If the execution is no
// longer running (recovered by another instance after a stall), it cancels the
// run with errLeaseLost so the work is not done twice.
func (p *Pool) heartbeat(ctx context.Context, id string, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(p.lease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			tag, err := p.db.Exec(ctx, `UPDATE executions SET lease_expires_at = $2 WHERE id = $1 AND status = 'running'`,
				id, p.clock().Add(p.lease))
			if err != nil {
				if ctx.Err() == nil {
					p.log.Warn("lease renewal failed; will retry", "execution_id", id, "err", err)
				}
				continue
			}
			if tag.RowsAffected() == 0 {
				cancel(errLeaseLost)
				return
			}
		}
	})
	return func() { close(done); wg.Wait() }
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
	return r.Run(tctx, runners.Request{
		Config: e.config, ExecutionID: e.id, TaskID: e.taskID, Attempt: e.number,
		DenyPrivate: e.appID != tasks.DefaultApplicationID && !p.appsPriv,
	})
}

// retryAt returns when the next attempt may start, or nil when there is none.
func (p *Pool) retryAt(a attempt, f *runners.Failure) *time.Time {
	if f == nil || !f.Retryable || a.number >= a.retry.MaxAttempts {
		return nil
	}
	t := p.clock().Add(Backoff(a.retry, a.number))
	return &t
}

// errorDoc is stored as executions.error.
type errorDoc struct {
	*runners.Failure
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

// finish records the outcome of a running attempt and, when retryAt is set,
// queues the next attempt in the same transaction.
func (p *Pool) finish(ctx context.Context, tx pgx.Tx, a attempt, status string, output json.RawMessage,
	failure *runners.Failure, retryAt *time.Time, finished time.Time, durationMS int64) error {
	var errJSON []byte
	if failure != nil {
		errJSON, _ = json.Marshal(errorDoc{Failure: failure, RetryAt: retryAt})
	}
	tag, err := tx.Exec(ctx, `UPDATE executions
		SET status = $2, output = $3, error = $4, finished_at = $5, duration_ms = $6, lease_expires_at = NULL
		WHERE id = $1 AND status = 'running'`,
		a.id, status, nullJSON(output), nullJSON(errJSON), finished, durationMS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 || retryAt == nil {
		// RowsAffected 0: the row changed under us (deleted with its task, or
		// recovered after an expired lease). Its new owner decides what happens next.
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO executions
		(id, task_id, application_id, trigger, status, attempt, scheduled_for, created_at, run_after)
		VALUES ($1, $2, $3, $4, 'queued', $5, $6, $7, $8)
		ON CONFLICT DO NOTHING`,
		ids.New("exe"), a.taskID, a.appID, a.trigger, a.number+1, a.scheduledFor, finished, *retryAt)
	return err
}

func (p *Pool) recoveryLoop(ctx context.Context) {
	for {
		if n, err := p.RecoverExpired(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("lease recovery failed", "err", err)
		} else if n > 0 {
			p.log.Warn("recovered executions with expired leases", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.recovery):
		}
	}
}

// RecoverExpired marks running executions whose lease has expired as interrupted
// and applies their task's retry policy. It returns how many it recovered.
// SKIP LOCKED lets several instances sweep concurrently without double work.
func (p *Pool) RecoverExpired(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		now := p.clock()
		rows, err := tx.Query(ctx, `SELECT e.id, e.task_id, e.application_id, e.trigger, e.attempt, e.scheduled_for,
				coalesce(e.started_at, e.created_at), t.max_attempts, t.backoff_initial_seconds, t.backoff_max_seconds
			FROM executions e JOIN tasks t ON t.id = e.task_id
			WHERE e.status = 'running' AND e.lease_expires_at < $1
			ORDER BY e.lease_expires_at
			LIMIT 100
			FOR UPDATE OF e SKIP LOCKED`, now)
		if err != nil {
			return err
		}
		expired, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (attempt, error) {
			var a attempt
			err := r.Scan(&a.id, &a.taskID, &a.appID, &a.trigger, &a.number, &a.scheduledFor,
				&a.startedAt, &a.retry.MaxAttempts, &a.retry.BackoffInitialSeconds, &a.retry.BackoffMaxSeconds)
			return a, err
		})
		if err != nil {
			return err
		}
		for _, a := range expired {
			f := &runners.Failure{Kind: runners.KindInterrupted, Retryable: true,
				Message: "lease expired: the worker running this attempt stopped responding (crash or stall)"}
			retryAt := p.retryAt(a, f)
			if err := p.finish(ctx, tx, a, executions.StatusInterrupted, nil, f, retryAt, now, now.Sub(a.startedAt).Milliseconds()); err != nil {
				return fmt.Errorf("recover %s: %w", a.id, err)
			}
			p.log.Warn("execution interrupted: lease expired", "execution_id", a.id, "task_id", a.taskID, "attempt", a.number, "retry_at", retryAt)
		}
		n = len(expired)
		return nil
	})
	return n, err
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
