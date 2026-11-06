// Package scheduler turns due cron schedules into queued executions.
//
// It never runs tasks itself: each pass inserts one queued execution per due
// occurrence and advances the task's next_run_at, in a single transaction.
// Workers (M3) pick up queued executions independently, so a slow task cannot
// delay scheduling. See docs/ARCHITECTURE.md §3.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/executions"
	"github.com/farhan-hilmy/clockwork/internal/ids"
	"github.com/farhan-hilmy/clockwork/internal/schedule"
)

// Options configures a Scheduler.
type Options struct {
	// Interval between passes. Defaults to 1s.
	Interval time.Duration
	// MisfireGrace is how late an occurrence may be and still count as on time. Defaults to 1m.
	MisfireGrace time.Duration
	// BatchSize caps the tasks locked per transaction. Defaults to 100.
	BatchSize int
	// Now is an injectable clock for tests. Defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Scheduler materialises due cron occurrences as queued executions.
type Scheduler struct {
	db       *pgxpool.Pool
	interval time.Duration
	grace    time.Duration
	batch    int
	now      func() time.Time
	log      *slog.Logger
}

// New builds a Scheduler.
func New(db *pgxpool.Pool, o Options) *Scheduler {
	s := &Scheduler{db: db, interval: o.Interval, grace: o.MisfireGrace, batch: o.BatchSize, now: o.Now, log: o.Logger}
	if s.interval <= 0 {
		s.interval = time.Second
	}
	if s.grace <= 0 {
		s.grace = time.Minute
	}
	if s.batch <= 0 {
		s.batch = 100
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Run passes immediately, then every interval, until ctx is canceled.
// The first pass handles anything missed while the process was down.
func (s *Scheduler) Run(ctx context.Context) {
	s.log.Info("scheduler started", "interval", s.interval, "misfire_grace", s.grace)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("scheduler pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return
		case <-t.C:
		}
	}
}

// Result summarises one pass.
type Result struct {
	Tasks    int // due tasks processed
	Enqueued int // executions inserted
}

// Tick runs one scheduling pass, repeating batches until no due task is left.
func (s *Scheduler) Tick(ctx context.Context) (Result, error) {
	var total Result
	now := s.now().UTC().Truncate(time.Microsecond)
	for {
		r, err := s.tickBatch(ctx, now)
		total.Tasks += r.Tasks
		total.Enqueued += r.Enqueued
		if err != nil || r.Tasks < s.batch {
			return total, err
		}
	}
}

type dueTask struct {
	id, cron, timezone, misfire string
	nextRun                     time.Time
}

func (s *Scheduler) tickBatch(ctx context.Context, now time.Time) (Result, error) {
	var res Result
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// SKIP LOCKED lets several instances share the work without blocking each other,
		// and keeps a task being edited from stalling the whole pass.
		rows, err := tx.Query(ctx, `SELECT id, coalesce(schedule_cron, ''), schedule_timezone, misfire_policy, next_run_at
			FROM tasks
			WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= $1
			ORDER BY next_run_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED`, now, s.batch)
		if err != nil {
			return fmt.Errorf("select due tasks: %w", err)
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dueTask, error) {
			var d dueTask
			err := r.Scan(&d.id, &d.cron, &d.timezone, &d.misfire, &d.nextRun)
			return d, err
		})
		if err != nil {
			return fmt.Errorf("scan due tasks: %w", err)
		}
		res.Tasks = len(due)
		for _, d := range due {
			n, err := s.schedule(ctx, tx, d, now)
			if err != nil {
				return fmt.Errorf("task %s: %w", d.id, err)
			}
			res.Enqueued += n
		}
		return nil
	})
	return res, err
}

// schedule enqueues the due occurrences of one locked task and advances next_run_at.
func (s *Scheduler) schedule(ctx context.Context, tx pgx.Tx, d dueTask, now time.Time) (int, error) {
	spec, err := schedule.Parse(d.cron, d.timezone)
	if err != nil {
		// Validation makes this unreachable for tasks written through the API. Clear
		// next_run_at so a bad row is not retried on every pass.
		s.log.Error("task has an invalid schedule; it will not be scheduled", "task_id", d.id, "err", err)
		_, err = tx.Exec(ctx, `UPDATE tasks SET next_run_at = NULL WHERE id = $1`, d.id)
		return 0, err
	}

	p := planDue(spec, d.nextRun, now, s.grace, d.misfire)
	enqueued := 0
	for _, at := range p.occurrences {
		// ON CONFLICT: another scheduler (or a stale write of next_run_at) already
		// materialised this occurrence. It must run once, so ours is dropped.
		tag, err := tx.Exec(ctx, `INSERT INTO executions (id, task_id, application_id, trigger, status, attempt, scheduled_for, created_at)
			SELECT $1, id, application_id, $3, $4, 1, $5, $6 FROM tasks WHERE id = $2
			ON CONFLICT DO NOTHING`,
			ids.New("exe"), d.id, executions.TriggerScheduled, executions.StatusQueued, at, now)
		if err != nil {
			return enqueued, fmt.Errorf("insert execution: %w", err)
		}
		enqueued += int(tag.RowsAffected())
	}

	var next *time.Time
	if !p.next.IsZero() {
		next = &p.next
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET next_run_at = $2 WHERE id = $1`, d.id, next); err != nil {
		return enqueued, fmt.Errorf("advance next_run_at: %w", err)
	}

	switch {
	case p.catchUp:
		s.log.Info("misfire: enqueued one catch-up run", "task_id", d.id, "scheduled_for", p.occurrences[0], "next_run_at", next)
	case p.missed:
		s.log.Info("misfire: skipped missed occurrences", "task_id", d.id, "missed_since", d.nextRun, "next_run_at", next)
	}
	if enqueued > 0 {
		s.log.Debug("enqueued scheduled runs", "task_id", d.id, "count", enqueued, "next_run_at", next)
	}
	return enqueued, nil
}
