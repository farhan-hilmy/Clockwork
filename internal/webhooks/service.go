package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/ids"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx, so Emit can join the caller's transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Service manages endpoints and the delivery log, and emits events.
type Service struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// NewService builds a Service. now may be nil, meaning time.Now.
func NewService(db *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: func() time.Time { return now().UTC().Truncate(time.Microsecond) }}
}

// EndpointInput creates an endpoint.
type EndpointInput struct {
	ApplicationID string   `json:"application_id"` // set by the caller's principal for applications
	URL           string   `json:"url"`
	Description   string   `json:"description"`
	Events        []string `json:"events"`
}

// EndpointUpdate changes an endpoint. Nil fields are left alone.
type EndpointUpdate struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	Events      *[]string `json:"events"`
	Enabled     *bool     `json:"enabled"`
}

func validate(e *Endpoint) error {
	fe := map[string]string{}
	e.URL = strings.TrimSpace(e.URL)
	u, err := url.Parse(e.URL)
	switch {
	case e.URL == "":
		fe["url"] = "is required"
	case len(e.URL) > 2048:
		fe["url"] = "must be at most 2048 characters"
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		fe["url"] = "must be an absolute http or https URL"
	case u.User != nil:
		fe["url"] = "must not contain credentials; verify the Clockwork-Signature header instead"
	}
	e.Description = strings.TrimSpace(e.Description)
	if utf8.RuneCountInString(e.Description) > 2000 {
		fe["description"] = "must be at most 2000 characters"
	}
	if e.Events == nil {
		e.Events = []string{}
	}
	for _, t := range e.Events {
		if !ValidEvent(t) {
			fe["events"] = fmt.Sprintf("unknown event type %q (expected one of %s)", t, strings.Join(EventTypes, ", "))
		}
	}
	slices.Sort(e.Events)
	e.Events = slices.Compact(e.Events)
	if len(fe) > 0 {
		return &ValidationError{Fields: fe}
	}
	return nil
}

const endpointCols = `id, application_id, url, description, events, enabled, created_at, updated_at`

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.ApplicationID, &e.URL, &e.Description, &e.Events, &e.Enabled, &e.CreatedAt, &e.UpdatedAt)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return e, err
}

// Create registers an endpoint. The returned Secret is shown only now.
func (s *Service) Create(ctx context.Context, in EndpointInput) (Endpoint, error) {
	now := s.now()
	e := Endpoint{ID: ids.New("whk"), ApplicationID: in.ApplicationID, URL: in.URL, Description: in.Description,
		Events: in.Events, Enabled: true, CreatedAt: now, UpdatedAt: now, Secret: newSecret()}
	if err := validate(&e); err != nil {
		return Endpoint{}, err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO webhook_endpoints (`+endpointCols+`, secret) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID, e.ApplicationID, e.URL, e.Description, e.Events, e.Enabled, e.CreatedAt, e.UpdatedAt, e.Secret)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return Endpoint{}, &ValidationError{Fields: map[string]string{"application_id": "unknown application"}}
	}
	return e, err
}

// List returns endpoints, newest first. appID "" lists every application's.
func (s *Service) List(ctx context.Context, appID string) ([]Endpoint, error) {
	rows, err := s.db.Query(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints
		WHERE $1 = '' OR application_id = $1 ORDER BY id DESC`, appID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(r) })
	if out == nil {
		out = []Endpoint{}
	}
	return out, err
}

// Get returns one endpoint, without its secret.
func (s *Service) Get(ctx context.Context, id string) (Endpoint, error) {
	return scanEndpoint(s.db.QueryRow(ctx, `SELECT `+endpointCols+` FROM webhook_endpoints WHERE id = $1`, id))
}

// Update changes an endpoint's URL, description, event filter or enabled state.
func (s *Service) Update(ctx context.Context, id string, in EndpointUpdate) (Endpoint, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return Endpoint{}, err
	}
	if in.URL != nil {
		e.URL = *in.URL
	}
	if in.Description != nil {
		e.Description = *in.Description
	}
	if in.Events != nil {
		e.Events = *in.Events
	}
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	if err := validate(&e); err != nil {
		return Endpoint{}, err
	}
	e.UpdatedAt = s.now()
	_, err = s.db.Exec(ctx, `UPDATE webhook_endpoints SET url = $2, description = $3, events = $4, enabled = $5, updated_at = $6 WHERE id = $1`,
		e.ID, e.URL, e.Description, e.Events, e.Enabled, e.UpdatedAt)
	return e, err
}

// Delete removes an endpoint and its delivery log.
func (s *Service) Delete(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RotateSecret replaces the signing secret immediately and returns the new one.
// Deliveries sent from now on, including retries of earlier events, use it.
func (s *Service) RotateSecret(ctx context.Context, id string) (Endpoint, error) {
	secret := newSecret()
	tag, err := s.db.Exec(ctx, `UPDATE webhook_endpoints SET secret = $2, updated_at = $3 WHERE id = $1`, id, secret, s.now())
	if err != nil {
		return Endpoint{}, err
	}
	if tag.RowsAffected() == 0 {
		return Endpoint{}, ErrNotFound
	}
	e, err := s.Get(ctx, id)
	e.Secret = secret
	return e, err
}

// Emit records an event for every enabled endpoint of appID subscribed to its
// type. Pass the transaction that makes the change the event describes. data is
// only called when at least one endpoint is subscribed, so an application
// without webhooks costs one indexed lookup per event.
func Emit(ctx context.Context, q Querier, appID, eventType string, data func() (any, error), at time.Time) error {
	rows, err := q.Query(ctx, `SELECT id FROM webhook_endpoints
		WHERE application_id = $1 AND enabled AND (events = '{}' OR $2 = ANY(events))`, appID, eventType)
	if err != nil {
		return fmt.Errorf("find webhook endpoints: %w", err)
	}
	endpoints, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(endpoints) == 0 {
		return err
	}
	d, err := data()
	if err != nil {
		return err
	}
	ev := Event{ID: ids.New("evt"), Type: eventType, CreatedAt: at, ApplicationID: appID, Data: d}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	for _, ep := range endpoints {
		if _, err := insertDelivery(ctx, q, ep, ev, payload); err != nil {
			return err
		}
	}
	return nil
}

func insertDelivery(ctx context.Context, q Querier, endpointID string, ev Event, payload []byte) (string, error) {
	id := ids.New("dlv")
	_, err := q.Exec(ctx, `INSERT INTO webhook_deliveries
		(id, endpoint_id, application_id, event_id, event_type, payload, status, next_attempt_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $7)`,
		id, endpointID, ev.ApplicationID, ev.ID, ev.Type, payload, ev.CreatedAt)
	if err != nil {
		return "", fmt.Errorf("record webhook delivery: %w", err)
	}
	return id, nil
}

// SendTest queues a webhook.test event to one endpoint, whatever its filter or
// enabled state, so its owner can check the receiver.
func (s *Service) SendTest(ctx context.Context, id string) (Delivery, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return Delivery{}, err
	}
	ev := Event{ID: ids.New("evt"), Type: EventTest, CreatedAt: s.now(), ApplicationID: e.ApplicationID,
		Data: map[string]any{"endpoint_id": e.ID, "message": "This is a test event from Clockwork."}}
	payload, _ := json.Marshal(ev)
	did, err := insertDelivery(ctx, s.db, e.ID, ev, payload)
	if err != nil {
		return Delivery{}, err
	}
	return s.Delivery(ctx, e.ID, did)
}

// DeliveryParams filters an endpoint's delivery log. Results are newest first.
type DeliveryParams struct {
	EndpointID string
	Status     string
	Limit      int
	Cursor     string
}

// DeliveryPage is one page of the delivery log.
type DeliveryPage struct {
	Deliveries []Delivery `json:"deliveries"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

const deliveryCols = `id, endpoint_id, application_id, event_id, event_type, status, attempts,
	CASE WHEN status = 'pending' THEN next_attempt_at END, last_attempt_at, last_status_code, last_error, last_response,
	delivered_at, created_at`

func scanDelivery(row pgx.Row, extra ...any) (Delivery, error) {
	var d Delivery
	err := row.Scan(append([]any{&d.ID, &d.EndpointID, &d.ApplicationID, &d.EventID, &d.EventType, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastAttemptAt, &d.LastStatusCode, &d.LastError, &d.LastResponse, &d.DeliveredAt, &d.CreatedAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrDeliveryNotFound
	}
	d.CreatedAt = d.CreatedAt.UTC()
	for _, p := range []**time.Time{&d.NextAttemptAt, &d.LastAttemptAt, &d.DeliveredAt} {
		if *p != nil {
			u := (*p).UTC()
			*p = &u
		}
	}
	return d, err
}

// Deliveries lists an endpoint's delivery log, without payloads.
func (s *Service) Deliveries(ctx context.Context, p DeliveryParams) (DeliveryPage, error) {
	switch {
	case p.Limit <= 0:
		p.Limit = 50
	case p.Limit > 200:
		p.Limit = 200
	}
	rows, err := s.db.Query(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE endpoint_id = $1 AND ($2 = '' OR status = $2) AND ($3 = '' OR id < $3)
		ORDER BY id DESC LIMIT $4`, p.EndpointID, p.Status, p.Cursor, p.Limit+1)
	if err != nil {
		return DeliveryPage{}, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) { return scanDelivery(r) })
	if err != nil {
		return DeliveryPage{}, err
	}
	page := DeliveryPage{Deliveries: list}
	if len(list) > p.Limit {
		page.Deliveries = list[:p.Limit]
		page.NextCursor = list[p.Limit-1].ID
	}
	if page.Deliveries == nil {
		page.Deliveries = []Delivery{}
	}
	return page, nil
}

// Delivery returns one delivery of an endpoint, including its payload.
func (s *Service) Delivery(ctx context.Context, endpointID, id string) (Delivery, error) {
	var payload json.RawMessage
	d, err := scanDelivery(s.db.QueryRow(ctx, `SELECT `+deliveryCols+`, payload FROM webhook_deliveries
		WHERE id = $1 AND endpoint_id = $2`, id, endpointID), &payload)
	d.Payload = payload
	return d, err
}

// Redeliver sends a delivery again as soon as possible, with a fresh set of
// attempts. It works for delivered and failed deliveries alike.
func (s *Service) Redeliver(ctx context.Context, endpointID, id string) (Delivery, error) {
	tag, err := s.db.Exec(ctx, `UPDATE webhook_deliveries SET status = 'pending', attempts = 0, next_attempt_at = $3, delivered_at = NULL
		WHERE id = $1 AND endpoint_id = $2`, id, endpointID, s.now())
	if err != nil {
		return Delivery{}, err
	}
	if tag.RowsAffected() == 0 {
		return Delivery{}, ErrDeliveryNotFound
	}
	return s.Delivery(ctx, endpointID, id)
}
