package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/runners"
)

// DispatcherOptions configures a Dispatcher.
type DispatcherOptions struct {
	// Workers is how many deliveries are sent at once. Defaults to 8.
	Workers int
	// PollInterval is how often due deliveries are looked for. Defaults to 1s.
	PollInterval time.Duration
	// Timeout bounds one delivery attempt. Defaults to 10s.
	Timeout time.Duration
	// Lease is how long a claimed delivery is reserved. If the dispatcher dies
	// mid-send, the delivery becomes due again after this. Defaults to 1m.
	Lease time.Duration
	// DenyPrivate refuses non-public endpoint addresses for every application.
	DenyPrivate bool
	// AppsMayReachPrivate lets endpoints of applications other than the default
	// one reach non-public addresses (mirrors CLOCKWORK_APP_HTTP_ALLOW_PRIVATE).
	AppsMayReachPrivate bool
	// DefaultApplicationID is exempt from the application rule above.
	DefaultApplicationID string
	UserAgent            string
	Now                  func() time.Time
	Logger               *slog.Logger
}

// Dispatcher sends due deliveries and records the outcome of each attempt.
type Dispatcher struct {
	db      *pgxpool.Pool
	o       DispatcherOptions
	open    *http.Client
	guarded *http.Client
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(db *pgxpool.Pool, o DispatcherOptions) *Dispatcher {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Lease <= 0 {
		o.Lease = time.Minute
	}
	if o.UserAgent == "" {
		o.UserAgent = "Clockwork-Webhooks"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	// Redirects are not followed: a receiver must answer itself, and following
	// a redirect could take a request to a host that was never validated.
	return &Dispatcher{db: db, o: o, open: runners.NewClient(false, false), guarded: runners.NewClient(true, false)}
}

func (d *Dispatcher) clock() time.Time { return d.o.Now().UTC().Truncate(time.Microsecond) }

// Run sends deliveries until ctx is canceled, then waits for sends in flight.
func (d *Dispatcher) Run(ctx context.Context) {
	d.o.Logger.Info("webhook dispatcher started", "workers", d.o.Workers)
	for {
		n, err := d.DispatchOnce(ctx)
		if err != nil && ctx.Err() == nil {
			d.o.Logger.Error("webhook dispatch failed", "err", err)
		}
		if n > 0 && ctx.Err() == nil {
			continue // more may be due right away
		}
		select {
		case <-ctx.Done():
			d.o.Logger.Info("webhook dispatcher stopped")
			return
		case <-time.After(d.o.PollInterval):
		}
	}
}

type claim struct {
	id, endpointID, appID, eventID, eventType, url, secret string
	payload                                                []byte
	attempt                                                int
}

// DispatchOnce claims a batch of due deliveries, sends them concurrently and
// records each result. It returns how many it sent.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	now := d.clock()
	rows, err := d.db.Query(ctx, `WITH due AS (
			SELECT w.id FROM webhook_deliveries w JOIN webhook_endpoints e ON e.id = w.endpoint_id
			WHERE w.status = 'pending' AND w.next_attempt_at <= $1 AND (e.enabled OR w.event_type = 'webhook.test')
			ORDER BY w.next_attempt_at
			LIMIT $2
			FOR UPDATE OF w SKIP LOCKED)
		UPDATE webhook_deliveries w
		SET attempts = w.attempts + 1, last_attempt_at = $1, next_attempt_at = $3
		FROM due, webhook_endpoints e
		WHERE w.id = due.id AND e.id = w.endpoint_id
		RETURNING w.id, w.endpoint_id, w.application_id, w.event_id, w.event_type, w.payload, w.attempts, e.url, e.secret`,
		now, d.o.Workers, now.Add(d.o.Lease))
	if err != nil {
		return 0, fmt.Errorf("claim deliveries: %w", err)
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (claim, error) {
		var c claim
		err := r.Scan(&c.id, &c.endpointID, &c.appID, &c.eventID, &c.eventType, &c.payload, &c.attempt, &c.url, &c.secret)
		return c, err
	})
	if err != nil {
		return 0, fmt.Errorf("claim deliveries: %w", err)
	}
	var wg sync.WaitGroup
	for _, c := range batch {
		wg.Go(func() { d.deliver(ctx, c) })
	}
	wg.Wait()
	return len(batch), nil
}

const maxResponseExcerpt = 1024

func (d *Dispatcher) deliver(ctx context.Context, c claim) {
	code, excerpt, sendErr := d.send(ctx, c)

	now := d.clock()
	status, next := StatusDelivered, (*time.Time)(nil)
	var errText *string
	if sendErr != nil {
		msg := sendErr.Error()
		errText = &msg
		status = StatusFailed
		if c.attempt < MaxAttempts {
			status = StatusPending
			t := now.Add(Backoff(c.attempt))
			next = &t
		}
	}
	var codePtr *int
	if code != 0 {
		codePtr = &code
	}
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// attempts = $2 guards against a redeliver that reset the row while we were sending.
	_, err := d.db.Exec(recCtx, `UPDATE webhook_deliveries
		SET status = $3::text, next_attempt_at = coalesce($4::timestamptz, next_attempt_at), last_status_code = $5::int,
		    last_error = $6::text, last_response = $7::text,
		    delivered_at = CASE WHEN $3::text = 'delivered' THEN $8::timestamptz END
		WHERE id = $1 AND attempts = $2`,
		c.id, c.attempt, status, next, codePtr, errText, excerpt, now)
	if err != nil {
		d.o.Logger.Error("record webhook delivery failed", "delivery_id", c.id, "err", err)
		return
	}
	attrs := []any{"delivery_id", c.id, "endpoint_id", c.endpointID, "event", c.eventType, "attempt", c.attempt, "status", status}
	if sendErr != nil {
		d.o.Logger.Warn("webhook delivery failed", append(attrs, "err", sendErr, "next_attempt_at", next)...)
	} else {
		d.o.Logger.Debug("webhook delivered", attrs...)
	}
}

// send POSTs one delivery. A non-2xx answer is an error carrying its status code.
func (d *Dispatcher) send(ctx context.Context, c claim) (code int, excerpt *string, err error) {
	ctx, cancel := context.WithTimeout(ctx, d.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(c.payload))
	if err != nil {
		return 0, nil, fmt.Errorf("invalid endpoint url: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.o.UserAgent)
	req.Header.Set(HeaderEvent, c.eventType)
	req.Header.Set(HeaderEventID, c.eventID)
	req.Header.Set(HeaderDelivery, c.id)
	req.Header.Set(HeaderSignature, Sign(c.secret, d.o.Now(), c.payload))

	client := d.open
	if d.o.DenyPrivate || (c.appID != d.o.DefaultApplicationID && !d.o.AppsMayReachPrivate) {
		client = d.guarded
	}
	res, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		switch {
		case runners.IsBlocked(err):
			return 0, nil, errors.New("blocked: the endpoint resolves to a non-public address")
		case errors.Is(err, context.DeadlineExceeded):
			return 0, nil, fmt.Errorf("no response within %s", d.o.Timeout)
		case errors.As(err, &ue):
			return 0, nil, ue.Err // drop the URL, which may carry secrets in its query
		}
		return 0, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, maxResponseExcerpt))
	body := strings.ToValidUTF8(string(b), "�")
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, &body, fmt.Errorf("endpoint answered %d", res.StatusCode)
	}
	return res.StatusCode, &body, nil
}
