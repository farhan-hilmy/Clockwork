package executions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// PostgresRepository stores executions in PostgreSQL.
type PostgresRepository struct {
	db *pgxpool.Pool
}

// NewPostgresRepository returns a repository backed by db.
func NewPostgresRepository(db *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const selectExecution = `SELECT e.id, e.task_id, t.name, e.application_id, e.trigger, e.status, e.attempt,
	e.scheduled_for, e.output, e.error, e.created_at, e.run_after, e.started_at, e.finished_at, e.duration_ms, e.idempotency_key
	FROM executions e JOIN tasks t ON t.id = e.task_id`

func scanExecution(row pgx.Row) (Execution, error) {
	var e Execution
	err := row.Scan(&e.ID, &e.TaskID, &e.TaskName, &e.ApplicationID, &e.Trigger, &e.Status, &e.Attempt,
		&e.ScheduledFor, &e.Output, &e.Error, &e.CreatedAt, &e.RunAfter, &e.StartedAt, &e.FinishedAt, &e.DurationMS, &e.IdempotencyKey)
	if err != nil {
		return Execution{}, err
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.RunAfter = e.RunAfter.UTC()
	for _, p := range []**time.Time{&e.ScheduledFor, &e.StartedAt, &e.FinishedAt} {
		if *p != nil {
			u := (*p).UTC()
			*p = &u
		}
	}
	return e, nil
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Execution, error) {
	e, err := scanExecution(r.db.QueryRow(ctx, selectExecution+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, ErrNotFound
	}
	return e, err
}

func (r *PostgresRepository) List(ctx context.Context, p ListParams) ([]Execution, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if p.ApplicationID != "" {
		where = append(where, "e.application_id = "+arg(p.ApplicationID))
	}
	if p.TaskID != "" {
		where = append(where, "e.task_id = "+arg(p.TaskID))
	}
	if p.Status != "" {
		where = append(where, "e.status = "+arg(p.Status))
	}
	if p.Since != nil {
		where = append(where, "e.created_at >= "+arg(*p.Since))
	}
	if p.Until != nil {
		where = append(where, "e.created_at < "+arg(*p.Until))
	}
	if p.Cursor != "" {
		where = append(where, "e.id < "+arg(p.Cursor))
	}
	sql := selectExecution
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY e.id DESC LIMIT " + arg(p.Limit)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		e, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) Enqueue(ctx context.Context, e Execution) (Execution, bool, error) {
	replayed := false
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT application_id, name FROM tasks WHERE id = $1`, e.TaskID).
			Scan(&e.ApplicationID, &e.TaskName); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return tasks.ErrNotFound
			}
			return err
		}
		// A concurrent request with the same key waits here for ours to commit,
		// then inserts nothing and reads ours below.
		tag, err := tx.Exec(ctx, `INSERT INTO executions
			(id, task_id, application_id, trigger, status, attempt, scheduled_for, created_at, run_after, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9)
			ON CONFLICT (application_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
			e.ID, e.TaskID, e.ApplicationID, e.Trigger, e.Status, e.Attempt, e.ScheduledFor, e.CreatedAt, e.IdempotencyKey)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		prev, err := scanExecution(tx.QueryRow(ctx, selectExecution+` WHERE e.application_id = $1 AND e.idempotency_key = $2`,
			e.ApplicationID, e.IdempotencyKey))
		if err != nil {
			return err
		}
		if prev.TaskID != e.TaskID {
			return ErrIdempotencyConflict
		}
		e, replayed = prev, true
		return nil
	})
	if err != nil {
		return Execution{}, false, err
	}
	return e, replayed, nil
}

// RowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Load reads one execution through q, so callers can see their own uncommitted changes.
func Load(ctx context.Context, q RowQuerier, id string) (Execution, error) {
	e, err := scanExecution(q.QueryRow(ctx, selectExecution+` WHERE e.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, ErrNotFound
	}
	return e, err
}

// Stats summarises executions created since the given time. appID "" covers every application.
func (r *PostgresRepository) Stats(ctx context.Context, appID string, since time.Time) (Stats, error) {
	st := Stats{Since: since, ByStatus: map[string]int{}}
	rows, err := r.db.Query(ctx, `SELECT status, count(*) FROM executions
		WHERE created_at >= $1 AND ($2::text = '' OR application_id = $2::text) GROUP BY status`, since, appID)
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return Stats{}, err
		}
		st.ByStatus[status] = n
		st.Total += n
	}
	if err := rows.Err(); err != nil {
		return Stats{}, err
	}
	var avg, p95 *float64
	if err := r.db.QueryRow(ctx, `SELECT avg(duration_ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
		FROM executions WHERE created_at >= $1 AND ($2::text = '' OR application_id = $2::text) AND duration_ms IS NOT NULL
		AND status IN ('succeeded', 'failed')`, since, appID).Scan(&avg, &p95); err != nil {
		return Stats{}, err
	}
	st.AvgDurationMS, st.P95DurationMS = roundMS(avg), roundMS(p95)
	st.SuccessRate = successRate(st.ByStatus)
	return st, nil
}

func roundMS(v *float64) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v + 0.5)
	return &n
}

func successRate(by map[string]int) *float64 {
	ok, bad := by[StatusSucceeded], by[StatusFailed]
	if ok+bad == 0 {
		return nil
	}
	r := float64(ok) / float64(ok+bad)
	return &r
}
