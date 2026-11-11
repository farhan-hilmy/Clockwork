// Package webhooks tells applications about execution lifecycle events by
// POSTing signed JSON to endpoints they register.
//
// Events are written to webhook_deliveries (one row per event and endpoint) in
// the same transaction as the change they describe: a transactional outbox, so
// an event is never lost and never sent for a change that rolled back. The
// Dispatcher sends due deliveries and retries failures with growing delays.
// Delivery is at-least-once: receivers should deduplicate on Clockwork-Delivery.
package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Event types.
const (
	EventExecutionStarted     = "execution.started"
	EventExecutionSucceeded   = "execution.succeeded"
	EventExecutionFailed      = "execution.failed"      // final: no attempts left, or not retryable
	EventExecutionRetrying    = "execution.retrying"    // failed; the next attempt is queued
	EventExecutionInterrupted = "execution.interrupted" // shutdown or lost worker; may be retried
	EventExecutionSkipped     = "execution.skipped"     // concurrency_policy=forbid
	EventTest                 = "webhook.test"          // sent on request, to check an endpoint
)

// EventTypes lists every event an endpoint can subscribe to.
var EventTypes = []string{
	EventExecutionStarted, EventExecutionSucceeded, EventExecutionFailed,
	EventExecutionRetrying, EventExecutionInterrupted, EventExecutionSkipped, EventTest,
}

// ValidEvent reports whether t is a known event type.
func ValidEvent(t string) bool { return slices.Contains(EventTypes, t) }

// Endpoint is a URL that receives an application's events.
type Endpoint struct {
	ID            string    `json:"id"`
	ApplicationID string    `json:"application_id"`
	URL           string    `json:"url"`
	Description   string    `json:"description"`
	Events        []string  `json:"events"` // empty: every event type
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Secret is only returned when an endpoint is created or its secret rotated.
	Secret string `json:"secret,omitempty"`
}

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed" // gave up after MaxAttempts
)

// Delivery is one event sent (or to be sent) to one endpoint.
type Delivery struct {
	ID             string     `json:"id"`
	EndpointID     string     `json:"endpoint_id"`
	ApplicationID  string     `json:"application_id"`
	EventID        string     `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  *time.Time `json:"next_attempt_at"` // nil once delivered or failed
	LastAttemptAt  *time.Time `json:"last_attempt_at"`
	LastStatusCode *int       `json:"last_status_code"`
	LastError      *string    `json:"last_error"`
	LastResponse   *string    `json:"last_response"` // first bytes of the last response body
	DeliveredAt    *time.Time `json:"delivered_at"`
	CreatedAt      time.Time  `json:"created_at"`
	Payload        any        `json:"payload,omitempty"`
}

// Event is the JSON body POSTed to endpoints.
type Event struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	CreatedAt     time.Time `json:"created_at"`
	ApplicationID string    `json:"application_id"`
	Data          any       `json:"data"`
}

// Errors returned by the service.
var (
	ErrNotFound         = errors.New("webhook endpoint not found")
	ErrDeliveryNotFound = errors.New("webhook delivery not found")
)

// ValidationError reports invalid fields.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return fmt.Sprintf("validation failed: %v", e.Fields) }

// Retry schedule: the delay after the nth failed attempt. About 26 hours in total.
var backoff = []time.Duration{
	10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute,
	30 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 15 * time.Hour,
}

// MaxAttempts is how many times a delivery is tried before it is marked failed.
var MaxAttempts = len(backoff) + 1

// Backoff returns the delay after failed attempt n (1-based).
func Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return backoff[min(n, len(backoff))-1]
}

// Signature headers.
const (
	HeaderSignature = "Clockwork-Signature"
	HeaderEvent     = "Clockwork-Event"
	HeaderEventID   = "Clockwork-Event-Id"
	HeaderDelivery  = "Clockwork-Delivery"
)

// Sign returns the Clockwork-Signature header value for body sent at t:
// "t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>".
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

func mac(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a Clockwork-Signature header against the raw request body, as a
// receiver should. It rejects signatures older or newer than tolerance (use a
// few minutes) to stop replays. Several v1 values are accepted, for forward
// compatibility with overlapping secrets.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		switch {
		case !ok:
		case k == "t":
			ts = v
		case k == "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("malformed signature header")
	}
	if age := now.Sub(time.Unix(sec, 0)); age > tolerance || age < -tolerance {
		return errors.New("signature timestamp outside tolerance")
	}
	want := mac(secret, ts, body)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

var secretEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newSecret returns "whsec_" plus 256 random bits.
func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "whsec_" + secretEncoding.EncodeToString(b)
}
