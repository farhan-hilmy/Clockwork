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
}

// ListParams filters and paginates executions. Results are newest first.
type ListParams struct {
	TaskID string
	Status string
	Limit  int
	Cursor string // return executions with id < Cursor
}

// Page is one page of executions.
type Page struct {
	Executions []Execution `json:"executions"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// ErrNotFound is returned when an execution does not exist.
var ErrNotFound = errors.New("execution not found")
