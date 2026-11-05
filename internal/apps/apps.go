// Package apps manages application identities that own tasks.
// M1 only lists them; registration and API keys arrive in M5.
package apps

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Application is an integration context. Every task belongs to exactly one.
type Application struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	TaskCount   int       `json:"task_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// Lister lists applications.
type Lister interface {
	List(ctx context.Context) ([]Application, error)
}

// PostgresRepository reads applications from PostgreSQL.
type PostgresRepository struct{ db *pgxpool.Pool }

// NewPostgresRepository returns a repository backed by db.
func NewPostgresRepository(db *pgxpool.Pool) *PostgresRepository { return &PostgresRepository{db: db} }

// List returns all applications with their task counts, default application first.
func (r *PostgresRepository) List(ctx context.Context) ([]Application, error) {
	rows, err := r.db.Query(ctx, `
		SELECT a.id, a.name, a.description, a.enabled, a.created_at, count(t.id)
		FROM applications a LEFT JOIN tasks t ON t.application_id = a.id
		GROUP BY a.id
		ORDER BY (a.id = 'app_clockwork') DESC, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Application{}
	for rows.Next() {
		var a Application
		if err := rows.Scan(&a.ID, &a.Name, &a.Description, &a.Enabled, &a.CreatedAt, &a.TaskCount); err != nil {
			return nil, err
		}
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
