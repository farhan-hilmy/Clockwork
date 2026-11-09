// Package runners executes one attempt of a task. A Runner knows nothing about
// scheduling, retries or storage: it gets a config and a context carrying the
// task's timeout, and reports what happened.
package runners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request is one attempt to run.
type Request struct {
	Config      json.RawMessage
	ExecutionID string
	TaskID      string
	Attempt     int
}

// Runner runs one attempt. ctx carries the task timeout.
//
// output is a small JSON document describing the attempt (status code, exit
// code, output excerpts). It may be non-nil even when err is non-nil, for
// example the body of an HTTP 500 response.
type Runner interface {
	Run(ctx context.Context, req Request) (output json.RawMessage, err error)
}

// Error kinds stored in executions.error.kind.
const (
	KindTimeout     = "timeout"     // the task timeout elapsed
	KindConnection  = "connection"  // DNS, connect, TLS or transport failure
	KindHTTPStatus  = "http_status" // response status was not the expected one
	KindExitCode    = "exit_code"   // shell command exited non-zero
	KindConfig      = "config"      // stored config is invalid or the runner is unavailable
	KindBlocked     = "blocked"     // target refused by policy (CLOCKWORK_HTTP_DENY_PRIVATE)
	KindInterrupted = "interrupted" // Clockwork shut down while the attempt ran
	KindInternal    = "internal"    // anything else
)

// Failure is a classified runner error. Retryable failures are re-attempted
// according to the task's retry policy.
type Failure struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (f *Failure) Error() string { return f.Kind + ": " + f.Message }

func fail(kind string, retryable bool, format string, args ...any) *Failure {
	return &Failure{Kind: kind, Message: fmt.Sprintf(format, args...), Retryable: retryable}
}

// AsFailure classifies any error as a Failure. Unclassified errors are internal and not retried.
func AsFailure(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return &Failure{Kind: KindInternal, Message: err.Error()}
}

// excerpt keeps the first max bytes written to it and counts the rest.
type excerpt struct {
	buf   []byte
	max   int
	total int64
}

func newExcerpt(limit int) *excerpt { return &excerpt{max: limit} }

func (e *excerpt) Write(p []byte) (int, error) {
	e.total += int64(len(p))
	if room := e.max - len(e.buf); room > 0 {
		e.buf = append(e.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// String returns the kept bytes; a multi-byte character cut at the limit becomes U+FFFD.
func (e *excerpt) String() string  { return strings.ToValidUTF8(string(e.buf), "�") }
func (e *excerpt) truncated() bool { return e.total > int64(len(e.buf)) }
