package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/farhan-hilmy/clockwork/internal/ids"
	"github.com/farhan-hilmy/clockwork/internal/schedule"
)

// Repository persists tasks. The Postgres implementation lives in postgres.go.
type Repository interface {
	Create(ctx context.Context, t Task) error
	Get(ctx context.Context, id string) (Task, error)
	List(ctx context.Context, p ListParams) ([]Task, error)
	Update(ctx context.Context, t Task) error
	Delete(ctx context.Context, id string) error
	ApplicationExists(ctx context.Context, id string) (bool, error)
}

// Service implements task CRUD with validation. All returned tasks are redacted.
type Service struct {
	repo       Repository
	now        func() time.Time
	allowShell bool
}

// Options configures a Service.
type Options struct {
	AllowShell bool
	Now        func() time.Time // injectable clock for tests; defaults to time.Now
}

// NewService builds a Service.
func NewService(repo Repository, opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }, allowShell: opts.AllowShell}
}

// ShellEnabled reports whether shell tasks may be created.
func (s *Service) ShellEnabled() bool { return s.allowShell }

// Create validates input, applies defaults and stores a new task.
func (s *Service) Create(ctx context.Context, in CreateInput) (Task, error) {
	now := s.now()
	t := Task{
		ID:             ids.New("tsk"),
		ApplicationID:  strings.TrimSpace(in.ApplicationID),
		Name:           strings.TrimSpace(in.Name),
		Description:    strings.TrimSpace(in.Description),
		Runner:         in.Runner,
		Schedule:       in.Schedule,
		TimeoutSeconds: DefaultTimeoutSeconds,
		Retry:          DefaultRetry,
		Enabled:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if t.ApplicationID == "" {
		t.ApplicationID = DefaultApplicationID
	}
	if in.TimeoutSeconds != nil {
		t.TimeoutSeconds = *in.TimeoutSeconds
	}
	if in.Retry != nil {
		t.Retry = *in.Retry
	}
	if in.Enabled != nil {
		t.Enabled = *in.Enabled
	}

	fe := fieldErrors{}
	// A masked value on create has nothing to restore from, so it is rejected.
	t.Runner = restoreMasked(t.Runner, Runner{Type: t.Runner.Type, Config: json.RawMessage(`{}`)}, fe)
	if err := s.finalize(&t, fe); err != nil {
		return Task{}, err
	}
	ok, err := s.repo.ApplicationExists(ctx, t.ApplicationID)
	if err != nil {
		return Task{}, err
	}
	if !ok {
		return Task{}, &ValidationError{Fields: map[string]string{"application_id": "unknown application"}}
	}
	if err := s.repo.Create(ctx, t); err != nil {
		return Task{}, err
	}
	return redacted(t), nil
}

// Get returns one task.
func (s *Service) Get(ctx context.Context, id string) (Task, error) {
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return redacted(t), nil
}

// List returns a page of tasks, newest first.
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	switch {
	case p.Limit <= 0:
		p.Limit = 50
	case p.Limit > 200:
		p.Limit = 200
	}
	want := p.Limit
	p.Limit++ // fetch one extra to know whether another page exists
	ts, err := s.repo.List(ctx, p)
	if err != nil {
		return Page{}, err
	}
	page := Page{Tasks: make([]Task, 0, min(len(ts), want))}
	for i, t := range ts {
		if i == want {
			page.NextCursor = ts[want-1].ID
			break
		}
		page.Tasks = append(page.Tasks, redacted(t))
	}
	return page, nil
}

// Update applies a partial update.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (Task, error) {
	t, err := s.repo.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	prev := t
	stored := t.Runner
	fe := fieldErrors{}
	if in.Name.Set {
		t.Name = strings.TrimSpace(in.Name.Value)
	}
	if in.Description.Set {
		t.Description = strings.TrimSpace(in.Description.Value)
	}
	if in.Runner.Set {
		t.Runner = restoreMasked(in.Runner.Value, stored, fe)
	}
	if in.Schedule.Set {
		t.Schedule = in.Schedule.Value
	}
	if in.TimeoutSeconds.Set {
		t.TimeoutSeconds = in.TimeoutSeconds.Value
	}
	if in.Retry.Set {
		t.Retry = in.Retry.Value
	}
	if in.Enabled.Set {
		t.Enabled = in.Enabled.Value
	}
	t.UpdatedAt = s.now()
	if err := s.finalize(&t, fe); err != nil {
		return Task{}, err
	}
	// Edits that leave the timing alone keep the pending occurrence. Recomputing it
	// from now would drop a run that is due but not yet picked up by the scheduler.
	// If this read was stale and the scheduler already materialised that occurrence,
	// the unique index on executions keeps it from running twice.
	if t.Enabled && prev.Enabled && prev.NextRunAt != nil && sameTiming(prev.Schedule, t.Schedule) {
		t.NextRunAt = prev.NextRunAt
	}
	if err := s.repo.Update(ctx, t); err != nil {
		return Task{}, err
	}
	return redacted(t), nil
}

// SetEnabled pauses or resumes a task.
func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) (Task, error) {
	return s.Update(ctx, id, UpdateInput{Enabled: Some(enabled)})
}

// Delete removes a task and its execution history.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.repo.Delete(ctx, id)
}

// finalize validates the whole task and computes next_run_at.
func (s *Service) finalize(t *Task, fe fieldErrors) error {
	switch n := utf8.RuneCountInString(t.Name); {
	case n == 0:
		fe.add("name", "is required")
	case n > 100:
		fe.add("name", "must be at most 100 characters")
	}
	if utf8.RuneCountInString(t.Description) > 2000 {
		fe.add("description", "must be at most 2000 characters")
	}
	t.Runner = normalizeRunner(t.Runner, s.allowShell, fe)
	if t.TimeoutSeconds < 1 || t.TimeoutSeconds > 86400 {
		fe.add("timeout_seconds", "must be between 1 and 86400")
	}
	r := t.Retry
	if r.MaxAttempts < 1 || r.MaxAttempts > 20 {
		fe.add("retry.max_attempts", "must be between 1 and 20")
	}
	if r.BackoffInitialSeconds < 1 || r.BackoffInitialSeconds > 3600 {
		fe.add("retry.backoff_initial_seconds", "must be between 1 and 3600")
	}
	if r.BackoffMaxSeconds < r.BackoffInitialSeconds || r.BackoffMaxSeconds > 86400 {
		fe.add("retry.backoff_max_seconds", "must be between backoff_initial_seconds and 86400")
	}

	t.NextRunAt = nil
	if t.Schedule != nil {
		sc := *t.Schedule
		sc.Cron = strings.TrimSpace(sc.Cron)
		if sc.Timezone == "" {
			sc.Timezone = "UTC"
		}
		if sc.MisfirePolicy == "" {
			sc.MisfirePolicy = MisfireSkip
		}
		if sc.ConcurrencyPolicy == "" {
			sc.ConcurrencyPolicy = ConcurrencyForbid
		}
		if sc.MisfirePolicy != MisfireSkip && sc.MisfirePolicy != MisfireRunOnce {
			fe.add("schedule.misfire_policy", "must be skip or run_once")
		}
		if sc.ConcurrencyPolicy != ConcurrencyForbid && sc.ConcurrencyPolicy != ConcurrencyAllow {
			fe.add("schedule.concurrency_policy", "must be forbid or allow")
		}
		spec, err := schedule.Parse(sc.Cron, sc.Timezone)
		if err != nil {
			field := "schedule.cron"
			if _, tzErr := schedule.LoadLocation(sc.Timezone); tzErr != nil {
				field = "schedule.timezone"
			}
			fe.add(field, "%v", err)
		} else if next := spec.Next(s.now()); next.IsZero() {
			fe.add("schedule.cron", "never matches a date, for example February 30")
		} else if t.Enabled {
			t.NextRunAt = &next
		}
		t.Schedule = &sc
	}
	return fe.err()
}

// sameTiming reports whether two normalised schedules fire at the same times.
func sameTiming(a, b *Schedule) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cron == b.Cron && a.Timezone == b.Timezone
}

func redacted(t Task) Task {
	t.Runner = Redact(t.Runner)
	return t
}

// IsValidation reports whether err is a *ValidationError and returns it.
func IsValidation(err error) (*ValidationError, bool) {
	var ve *ValidationError
	ok := errors.As(err, &ve)
	return ve, ok
}
