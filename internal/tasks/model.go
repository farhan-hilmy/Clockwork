// Package tasks holds the task registry: the task model, validation, and CRUD service.
// It has no knowledge of HTTP or of how tasks are executed.
package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultApplicationID is the context for tasks created directly in Clockwork.
const DefaultApplicationID = "app_clockwork"

// RunnerType names a registered runner.
type RunnerType string

const (
	RunnerHTTP  RunnerType = "http"
	RunnerShell RunnerType = "shell"
)

// Misfire and concurrency policies. See docs/ARCHITECTURE.md §3.
const (
	MisfireSkip    = "skip"
	MisfireRunOnce = "run_once"

	ConcurrencyForbid = "forbid"
	ConcurrencyAllow  = "allow"
)

// Task is a stored automation definition.
type Task struct {
	ID             string      `json:"id"`
	ApplicationID  string      `json:"application_id"`
	Name           string      `json:"name"`
	Description    string      `json:"description"`
	Runner         Runner      `json:"runner"`
	Schedule       *Schedule   `json:"schedule"`
	TimeoutSeconds int         `json:"timeout_seconds"`
	Retry          RetryPolicy `json:"retry"`
	Enabled        bool        `json:"enabled"`
	NextRunAt      *time.Time  `json:"next_run_at"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// Runner selects a runner and carries its type-specific configuration.
type Runner struct {
	Type   RunnerType      `json:"type"`
	Config json.RawMessage `json:"config"`
}

// Schedule is an optional recurring trigger. Tasks without one run only manually or via the API.
type Schedule struct {
	Cron              string `json:"cron"`
	Timezone          string `json:"timezone"`
	MisfirePolicy     string `json:"misfire_policy"`
	ConcurrencyPolicy string `json:"concurrency_policy"`
}

// RetryPolicy controls re-attempts of retryable failures with exponential backoff.
type RetryPolicy struct {
	MaxAttempts           int `json:"max_attempts"`
	BackoffInitialSeconds int `json:"backoff_initial_seconds"`
	BackoffMaxSeconds     int `json:"backoff_max_seconds"`
}

// Defaults applied when a create request omits a field.
var (
	DefaultTimeoutSeconds = 30
	DefaultRetry          = RetryPolicy{MaxAttempts: 1, BackoffInitialSeconds: 10, BackoffMaxSeconds: 300}
)

// Errors returned by the service. Handlers map them to HTTP status codes.
var (
	ErrNotFound = errors.New("task not found")
	ErrConflict = errors.New("a task with this name already exists in the application")
)

// ValidationError reports every invalid field at once.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, e.Fields[k]))
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

type fieldErrors map[string]string

func (f fieldErrors) add(field, format string, args ...any) {
	if _, exists := f[field]; !exists {
		f[field] = fmt.Sprintf(format, args...)
	}
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}

// Opt distinguishes an absent JSON field from one explicitly set (including to null).
type Opt[T any] struct {
	Set   bool
	Value T
}

func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	return json.Unmarshal(b, &o.Value)
}

// Some returns a set Opt, for use in code and tests.
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, Value: v} }

// CreateInput is the payload for creating a task. Nil pointers take defaults.
type CreateInput struct {
	ApplicationID  string       `json:"application_id"`
	Name           string       `json:"name"`
	Description    string       `json:"description"`
	Runner         Runner       `json:"runner"`
	Schedule       *Schedule    `json:"schedule"`
	TimeoutSeconds *int         `json:"timeout_seconds"`
	Retry          *RetryPolicy `json:"retry"`
	Enabled        *bool        `json:"enabled"`
}

// UpdateInput is a partial update. Only fields present in the request change.
// Setting schedule to null removes the schedule.
type UpdateInput struct {
	Name           Opt[string]      `json:"name"`
	Description    Opt[string]      `json:"description"`
	Runner         Opt[Runner]      `json:"runner"`
	Schedule       Opt[*Schedule]   `json:"schedule"`
	TimeoutSeconds Opt[int]         `json:"timeout_seconds"`
	Retry          Opt[RetryPolicy] `json:"retry"`
	Enabled        Opt[bool]        `json:"enabled"`
}

// ListParams filters and paginates task listings. Results are newest first.
type ListParams struct {
	ApplicationID string
	Enabled       *bool
	Query         string // case-insensitive substring of the name
	Limit         int
	Cursor        string // return tasks with id < Cursor
}

// Page is one page of results.
type Page struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"next_cursor,omitempty"`
}
