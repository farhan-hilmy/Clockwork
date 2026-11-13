package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository stores tasks in PostgreSQL.
type PostgresRepository struct {
	db *pgxpool.Pool
}

// NewPostgresRepository returns a repository backed by db.
func NewPostgresRepository(db *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const taskColumns = `id, application_id, name, description, runner_type, runner_config,
	schedule_cron, schedule_timezone, misfire_policy, concurrency_policy,
	timeout_seconds, max_attempts, backoff_initial_seconds, backoff_max_seconds,
	enabled, next_run_at, created_at, updated_at`

func scanTask(row pgx.Row) (Task, error) {
	var (
		t       Task
		rtype   string
		cron    *string
		tz      string
		misfire string
		concur  string
		nextRun *time.Time
	)
	err := row.Scan(&t.ID, &t.ApplicationID, &t.Name, &t.Description, &rtype, &t.Runner.Config,
		&cron, &tz, &misfire, &concur,
		&t.TimeoutSeconds, &t.Retry.MaxAttempts, &t.Retry.BackoffInitialSeconds, &t.Retry.BackoffMaxSeconds,
		&t.Enabled, &nextRun, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Task{}, err
	}
	t.Runner.Type = RunnerType(rtype)
	if cron != nil {
		t.Schedule = &Schedule{Cron: *cron, Timezone: tz, MisfirePolicy: misfire, ConcurrencyPolicy: concur}
	}
	if nextRun != nil {
		u := nextRun.UTC()
		t.NextRunAt = &u
	}
	t.CreatedAt = t.CreatedAt.UTC()
	t.UpdatedAt = t.UpdatedAt.UTC()
	return t, nil
}

// scheduleArgs flattens the optional schedule into column values.
func scheduleArgs(t Task) (cron *string, tz, misfire, concur string) {
	tz, misfire, concur = "UTC", MisfireSkip, ConcurrencyForbid
	if s := t.Schedule; s != nil {
		c := s.Cron
		return &c, s.Timezone, s.MisfirePolicy, s.ConcurrencyPolicy
	}
	return nil, tz, misfire, concur
}

func mapWriteErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation
			return ErrConflict
		case "23503": // foreign_key_violation
			return &ValidationError{Fields: map[string]string{"application_id": "unknown application"}}
		}
	}
	return err
}

func (r *PostgresRepository) Create(ctx context.Context, t Task) error {
	cron, tz, misfire, concur := scheduleArgs(t)
	_, err := r.db.Exec(ctx, `INSERT INTO tasks (`+taskColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		t.ID, t.ApplicationID, t.Name, t.Description, string(t.Runner.Type), t.Runner.Config,
		cron, tz, misfire, concur,
		t.TimeoutSeconds, t.Retry.MaxAttempts, t.Retry.BackoffInitialSeconds, t.Retry.BackoffMaxSeconds,
		t.Enabled, t.NextRunAt, t.CreatedAt, t.UpdatedAt)
	return mapWriteErr(err)
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(r.db.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

func (r *PostgresRepository) List(ctx context.Context, p ListParams) ([]Task, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if p.ApplicationID != "" {
		where = append(where, "application_id = "+arg(p.ApplicationID))
	}
	if p.Enabled != nil {
		where = append(where, "enabled = "+arg(*p.Enabled))
	}
	if p.RunnerType != "" {
		where = append(where, "runner_type = "+arg(string(p.RunnerType)))
	}
	if q := strings.TrimSpace(p.Query); q != "" {
		// Escape LIKE wildcards so the query is a literal substring match.
		q = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
		where = append(where, "name ILIKE "+arg("%"+q+"%"))
	}
	if p.Cursor != "" {
		where = append(where, "id < "+arg(p.Cursor))
	}
	sql := `SELECT ` + taskColumns + ` FROM tasks`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY id DESC LIMIT " + arg(p.Limit)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) Update(ctx context.Context, t Task) error {
	cron, tz, misfire, concur := scheduleArgs(t)
	tag, err := r.db.Exec(ctx, `UPDATE tasks SET
		name = $2, description = $3, runner_type = $4, runner_config = $5,
		schedule_cron = $6, schedule_timezone = $7, misfire_policy = $8, concurrency_policy = $9,
		timeout_seconds = $10, max_attempts = $11, backoff_initial_seconds = $12, backoff_max_seconds = $13,
		enabled = $14, next_run_at = $15, updated_at = $16
		WHERE id = $1`,
		t.ID, t.Name, t.Description, string(t.Runner.Type), t.Runner.Config,
		cron, tz, misfire, concur,
		t.TimeoutSeconds, t.Retry.MaxAttempts, t.Retry.BackoffInitialSeconds, t.Retry.BackoffMaxSeconds,
		t.Enabled, t.NextRunAt, t.UpdatedAt)
	if err != nil {
		return mapWriteErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) ApplicationExists(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM applications WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}
