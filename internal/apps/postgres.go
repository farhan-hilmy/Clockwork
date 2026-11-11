package apps

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository stores applications and keys in PostgreSQL.
type PostgresRepository struct{ db *pgxpool.Pool }

// NewPostgresRepository returns a repository backed by db.
func NewPostgresRepository(db *pgxpool.Pool) *PostgresRepository { return &PostgresRepository{db: db} }

const appSelect = `SELECT a.id, a.name, a.description, a.enabled, a.created_at, a.updated_at,
	(SELECT count(*) FROM tasks t WHERE t.application_id = a.id)
	FROM applications a`

func scanApp(row pgx.Row) (Application, error) {
	var a Application
	err := row.Scan(&a.ID, &a.Name, &a.Description, &a.Enabled, &a.CreatedAt, &a.UpdatedAt, &a.TaskCount)
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	return a, err
}

func mapErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation (applications.name)
			return ErrNameTaken
		case "23503": // foreign_key_violation (tasks.application_id ON DELETE RESTRICT)
			return ErrHasTasks
		}
	}
	return err
}

func (r *PostgresRepository) List(ctx context.Context) ([]Application, error) {
	rows, err := r.db.Query(ctx, appSelect+` ORDER BY (a.id = '`+DefaultID+`') DESC, a.name`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Application, error) { return scanApp(row) })
	if out == nil {
		out = []Application{}
	}
	return out, err
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Application, error) {
	a, err := scanApp(r.db.QueryRow(ctx, appSelect+` WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	return a, err
}

func (r *PostgresRepository) Create(ctx context.Context, a Application) error {
	_, err := r.db.Exec(ctx, `INSERT INTO applications (id, name, description, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, a.ID, a.Name, a.Description, a.Enabled, a.CreatedAt, a.UpdatedAt)
	return mapErr(err)
}

func (r *PostgresRepository) Update(ctx context.Context, a Application) error {
	tag, err := r.db.Exec(ctx, `UPDATE applications SET name = $2, description = $3, enabled = $4, updated_at = $5 WHERE id = $1`,
		a.ID, a.Name, a.Description, a.Enabled, a.UpdatedAt)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM applications WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const keyCols = `k.id, k.application_id, k.name, k.prefix, k.created_at, k.last_used_at, k.revoked_at`

func scanKey(row pgx.Row, extra ...any) (Key, error) {
	var k Key
	err := row.Scan(append([]any{&k.ID, &k.ApplicationID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt}, extra...)...)
	k.CreatedAt = k.CreatedAt.UTC()
	for _, p := range []**time.Time{&k.LastUsedAt, &k.RevokedAt} {
		if *p != nil {
			u := (*p).UTC()
			*p = &u
		}
	}
	return k, err
}

func (r *PostgresRepository) CreateKey(ctx context.Context, k Key, hash []byte) error {
	_, err := r.db.Exec(ctx, `INSERT INTO app_credentials (id, application_id, name, prefix, secret_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, k.ID, k.ApplicationID, k.Name, k.Prefix, hash, k.CreatedAt)
	return err
}

func (r *PostgresRepository) ListKeys(ctx context.Context, appID string) ([]Key, error) {
	rows, err := r.db.Query(ctx, `SELECT `+keyCols+` FROM app_credentials k WHERE k.application_id = $1 ORDER BY k.created_at DESC, k.id DESC`, appID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) { return scanKey(row) })
	if out == nil {
		out = []Key{}
	}
	return out, err
}

func (r *PostgresRepository) RevokeKey(ctx context.Context, appID, keyID string, at time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE app_credentials SET revoked_at = coalesce(revoked_at, $3)
		WHERE id = $1 AND application_id = $2`, keyID, appID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (r *PostgresRepository) KeyByHash(ctx context.Context, hash []byte) (Key, Application, error) {
	var a Application
	k, err := scanKey(r.db.QueryRow(ctx, `SELECT `+keyCols+`, a.id, a.name, a.description, a.enabled, a.created_at, a.updated_at
		FROM app_credentials k JOIN applications a ON a.id = k.application_id
		WHERE k.secret_hash = $1`, hash),
		&a.ID, &a.Name, &a.Description, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, Application{}, ErrKeyNotFound
	}
	return k, a, err
}

func (r *PostgresRepository) TouchKey(ctx context.Context, keyID string, at time.Time) error {
	// At most one write per key per minute, however busy the key is.
	_, err := r.db.Exec(ctx, `UPDATE app_credentials SET last_used_at = $2::timestamptz
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, keyID, at)
	return err
}
