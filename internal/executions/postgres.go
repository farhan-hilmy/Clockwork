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
	e.scheduled_for, e.output, e.error, e.created_at, e.run_after, e.started_at, e.finished_at, e.duration_ms
	FROM executions e JOIN tasks t ON t.id = e.task_id`

func scanExecution(row pgx.Row) (Execution, error) {
	var e Execution
	err := row.Scan(&e.ID, &e.TaskID, &e.TaskName, &e.ApplicationID, &e.Trigger, &e.Status, &e.Attempt,
		&e.ScheduledFor, &e.Output, &e.Error, &e.CreatedAt, &e.RunAfter, &e.StartedAt, &e.FinishedAt, &e.DurationMS)
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
	if p.TaskID != "" {
		where = append(where, "e.task_id = "+arg(p.TaskID))
	}
	if p.Status != "" {
		where = append(where, "e.status = "+arg(p.Status))
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

func (r *PostgresRepository) Enqueue(ctx context.Context, e Execution) (Execution, error) {
	// Copy the application from the task in the same statement, so a missing task inserts nothing.
	err := r.db.QueryRow(ctx, `WITH t AS (SELECT id, application_id, name FROM tasks WHERE id = $2),
		ins AS (
			INSERT INTO executions (id, task_id, application_id, trigger, status, attempt, scheduled_for, created_at, run_after)
			SELECT $1, t.id, t.application_id, $3, $4, $5, $6, $7, $7 FROM t
			RETURNING application_id)
		SELECT ins.application_id, t.name FROM ins, t`,
		e.ID, e.TaskID, e.Trigger, e.Status, e.Attempt, e.ScheduledFor, e.CreatedAt,
	).Scan(&e.ApplicationID, &e.TaskName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Execution{}, tasks.ErrNotFound
	}
	if err != nil {
		return Execution{}, err
	}
	return e, nil
}
