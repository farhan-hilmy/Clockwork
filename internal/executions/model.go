// Package executions holds execution history: one row per attempt of a task.
// The scheduler and (from M3) the executor write executions; this package reads
// them and enqueues manual runs.
package executions

import (
	"encoding/json"
	"errors"
	"time"
)

// Triggers record why an execution exists.
const (
	TriggerScheduled = "scheduled" // created by the scheduler for a cron occurrence
	TriggerManual    = "manual"    // "Run now" from the UI or the admin API
	TriggerAPI       = "api"       // an external application (M5)
)

// Statuses. Only queued is produced before the executor ships in M3.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusRetrying    = "retrying"
	StatusInterrupted = "interrupted"
	StatusCanceled    = "canceled"
	StatusSkipped     = "skipped"
)

// ValidStatus reports whether s is a known status.
func ValidStatus(s string) bool {
	switch s {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed,
		StatusRetrying, StatusInterrupted, StatusCanceled, StatusSkipped:
		return true
	}
	return false
}

// Execution is one attempt of a task.
type Execution struct {
	ID            string          `json:"id"`
	TaskID        string          `json:"task_id"`
	TaskName      string          `json:"task_name"`
	ApplicationID string          `json:"application_id"`
	Trigger       string          `json:"trigger"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	ScheduledFor  *time.Time      `json:"scheduled_for"`
	Output        json.RawMessage `json:"output"`
	Error         json.RawMessage `json:"error"`
	CreatedAt     time.Time       `json:"created_at"`
	RunAfter      time.Time       `json:"run_after"` // earliest start; later than created_at for retries
	StartedAt     *time.Time      `json:"started_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
	DurationMS    *int64          `json:"duration_ms"`
	// IdempotencyKey is the client's Idempotency-Key for a manual run, if it sent one.
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
}

// ListParams filters and paginates executions. Results are newest first.
type ListParams struct {
	ApplicationID string // restrict to one application's executions
	TaskID        string
	Status        string
	Since, Until  *time.Time // created_at range: Since inclusive, Until exclusive
	Limit         int
	Cursor        string // return executions with id < Cursor
}

// Stats summarises executions created in a time window.
type Stats struct {
	Since    time.Time      `json:"since"`
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
	// SuccessRate is succeeded / (succeeded + failed), or null when neither happened.
	// Retried and interrupted attempts are not counted: their final attempt is.
	SuccessRate   *float64 `json:"success_rate"`
	AvgDurationMS *int64   `json:"avg_duration_ms"`
	P95DurationMS *int64   `json:"p95_duration_ms"`
}

// Page is one page of executions.
type Page struct {
	Executions []Execution `json:"executions"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// Errors returned by the service.
var (
	ErrNotFound = errors.New("execution not found")
	// ErrIdempotencyConflict means the Idempotency-Key was already used for a different task.
	ErrIdempotencyConflict = errors.New("this Idempotency-Key was already used to run a different task")
)

// MaxIdempotencyKeyLen bounds the Idempotency-Key header.
const MaxIdempotencyKeyLen = 255
