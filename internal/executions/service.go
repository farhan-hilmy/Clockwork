package executions

import (
	"context"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/ids"
)

// Repository persists executions. The Postgres implementation lives in postgres.go.
type Repository interface {
	Get(ctx context.Context, id string) (Execution, error)
	List(ctx context.Context, p ListParams) ([]Execution, error)
	// Enqueue inserts a queued, unscheduled execution of e.TaskID, filling in its
	// application and task name. It returns tasks.ErrNotFound when the task does
	// not exist. When e.IdempotencyKey was already used in the task's application,
	// it inserts nothing and returns the earlier execution with replayed=true, or
	// ErrIdempotencyConflict if that execution belongs to another task.
	Enqueue(ctx context.Context, e Execution) (exe Execution, replayed bool, err error)
}

// Service reads execution history and enqueues manual runs.
type Service struct {
	repo Repository
	now  func() time.Time
}

// NewService builds a Service. now may be nil, meaning time.Now.
func NewService(repo Repository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

// Get returns one execution.
func (s *Service) Get(ctx context.Context, id string) (Execution, error) {
	return s.repo.Get(ctx, id)
}

// List returns a page of executions, newest first.
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	switch {
	case p.Limit <= 0:
		p.Limit = 50
	case p.Limit > 200:
		p.Limit = 200
	}
	want := p.Limit
	p.Limit++ // fetch one extra to know whether another page exists
	es, err := s.repo.List(ctx, p)
	if err != nil {
		return Page{}, err
	}
	page := Page{Executions: make([]Execution, 0, min(len(es), want))}
	for i, e := range es {
		if i == want {
			page.NextCursor = es[want-1].ID
			break
		}
		page.Executions = append(page.Executions, e)
	}
	return page, nil
}

// RunNow queues an immediate run of a task, whether or not it is enabled or scheduled.
// A worker picks it up within a second or so.
//
// idempotencyKey, if not empty, makes the call safe to retry: a repeat with the
// same key returns the original execution (replayed=true) instead of queuing another.
func (s *Service) RunNow(ctx context.Context, taskID, idempotencyKey string) (exe Execution, replayed bool, err error) {
	now := s.now()
	e := Execution{
		ID:        ids.New("exe"),
		TaskID:    taskID,
		Trigger:   TriggerManual,
		Status:    StatusQueued,
		Attempt:   1,
		CreatedAt: now,
		RunAfter:  now,
	}
	if idempotencyKey != "" {
		e.IdempotencyKey = &idempotencyKey
	}
	return s.repo.Enqueue(ctx, e)
}
