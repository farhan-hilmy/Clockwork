// Package retention deletes old finished history: executions and webhook
// deliveries older than the configured period (CLOCKWORK_RETENTION).
//
// Only finished rows are eligible. Queued and running executions and pending
// webhook deliveries are never deleted, however old. Deletes run in small
// batches, so a sweep never holds locks on many rows at once, and several
// instances sweeping at the same time only repeat harmless no-op deletes.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures a Sweeper.
type Options struct {
	// Keep is how long finished history is kept. Zero disables the sweeper.
	Keep time.Duration
	// Interval between sweeps. Defaults to 1h.
	Interval time.Duration
	// StartDelay before the first sweep, so startup is not slowed. Defaults to 1m;
	// negative means no delay.
	StartDelay time.Duration
	// BatchSize is how many rows one DELETE removes. Defaults to 1000.
	BatchSize int
	// MaxBatches bounds one sweep per table; the rest waits for the next sweep.
	// Defaults to 1000 (a million rows).
	MaxBatches int
	// Now is an injectable clock for tests. Defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Sweeper deletes history older than its retention period.
type Sweeper struct {
	db *pgxpool.Pool
	o  Options
}

// New builds a Sweeper.
func New(db *pgxpool.Pool, o Options) *Sweeper {
	if o.Interval <= 0 {
		o.Interval = time.Hour
	}
	if o.StartDelay < 0 {
		o.StartDelay = 0
	} else if o.StartDelay == 0 {
		o.StartDelay = time.Minute
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 1000
	}
	if o.MaxBatches <= 0 {
		o.MaxBatches = 1000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Sweeper{db: db, o: o}
}

// Result counts what one sweep deleted.
type Result struct {
	Executions int64
	Deliveries int64
}

// Run sweeps after StartDelay and then every Interval until ctx is canceled.
// With retention disabled it returns at once.
func (s *Sweeper) Run(ctx context.Context) {
	if s.o.Keep <= 0 {
		s.o.Logger.Info("retention disabled: run history and webhook deliveries are kept forever")
		return
	}
	s.o.Logger.Info("retention enabled", "keep", s.o.Keep, "interval", s.o.Interval)
	wait := s.o.StartDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = s.o.Interval
		start := time.Now()
		r, err := s.Sweep(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.o.Logger.Error("retention sweep failed", "err", err, "executions_deleted", r.Executions, "deliveries_deleted", r.Deliveries)
		case err == nil:
			s.o.Logger.Info("retention sweep", "executions_deleted", r.Executions, "deliveries_deleted", r.Deliveries,
				"cutoff", s.cutoff(), "duration_ms", time.Since(start).Milliseconds())
		}
	}
}

func (s *Sweeper) cutoff() time.Time { return s.o.Now().UTC().Add(-s.o.Keep) }

// Sweep deletes finished executions and deliveries older than the retention
// period, batch by batch. It does nothing when retention is disabled.
func (s *Sweeper) Sweep(ctx context.Context) (Result, error) {
	var r Result
	if s.o.Keep <= 0 {
		return r, nil
	}
	cutoff := s.cutoff()
	var err error
	// The conditions match the partial indexes from migration 00005.
	r.Executions, err = s.deleteBatches(ctx, `DELETE FROM executions WHERE id IN (
			SELECT id FROM executions
			WHERE finished_at IS NOT NULL AND status NOT IN ('queued', 'running') AND finished_at < $1
			LIMIT $2)`, cutoff)
	if err != nil {
		return r, fmt.Errorf("delete executions: %w", err)
	}
	r.Deliveries, err = s.deleteBatches(ctx, `DELETE FROM webhook_deliveries WHERE id IN (
			SELECT id FROM webhook_deliveries
			WHERE status <> 'pending' AND last_attempt_at < $1
			LIMIT $2)`, cutoff)
	if err != nil {
		return r, fmt.Errorf("delete webhook deliveries: %w", err)
	}
	return r, nil
}

func (s *Sweeper) deleteBatches(ctx context.Context, sql string, cutoff time.Time) (int64, error) {
	var total int64
	for range s.o.MaxBatches {
		tag, err := s.db.Exec(ctx, sql, cutoff, s.o.BatchSize)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(s.o.BatchSize) {
			break
		}
	}
	return total, nil
}
