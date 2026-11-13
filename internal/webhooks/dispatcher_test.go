package webhooks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/farhan-hilmy/clockwork/internal/store"
	"github.com/farhan-hilmy/clockwork/internal/webhooks"
)

func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("CLOCKWORK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CLOCKWORK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	pool, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `TRUNCATE executions, tasks, webhook_endpoints CASCADE;
		DELETE FROM applications WHERE id <> 'app_clockwork';
		INSERT INTO applications (id, name) VALUES ('app_ext', 'ext')`); err != nil {
		t.Fatal(err)
	}
	return pool
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	pool  *pgxpool.Pool
	clock *clock
	svc   *webhooks.Service
}

func setup(t *testing.T) env {
	pool := openTestDB(t)
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	return env{pool: pool, clock: c, svc: webhooks.NewService(pool, c.now)}
}

func (e env) dispatcher(o webhooks.DispatcherOptions) *webhooks.Dispatcher {
	o.Now = e.clock.now
	o.DefaultApplicationID = "app_clockwork"
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return webhooks.NewDispatcher(e.pool, o)
}

func (e env) endpoint(t *testing.T, app, url string, events ...string) webhooks.Endpoint {
	t.Helper()
	ep, err := e.svc.Create(t.Context(), webhooks.EndpointInput{ApplicationID: app, URL: url, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func (e env) emit(t *testing.T, app, eventType string) {
	t.Helper()
	data := func() (any, error) {
		return map[string]any{"execution": map[string]any{"id": "exe_test", "status": "failed"}}, nil
	}
	if err := webhooks.Emit(t.Context(), e.pool, app, eventType, data, e.clock.now()); err != nil {
		t.Fatal(err)
	}
}

func (e env) deliveries(t *testing.T, endpointID string) []webhooks.Delivery {
	t.Helper()
	p, err := e.svc.Deliveries(t.Context(), webhooks.DeliveryParams{EndpointID: endpointID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return p.Deliveries
}

func dispatch(t *testing.T, d *webhooks.Dispatcher) int {
	t.Helper()
	n, err := d.DispatchOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The M6 exit criterion, part 1: events are delivered with a signature the receiver can verify.
func TestEventsAreDeliveredSigned(t *testing.T) {
	e := setup(t)
	var (
		mu  sync.Mutex
		got []*http.Request
		raw [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, raw = append(got, r), append(raw, b)
		mu.Unlock()
		_, _ = w.Write([]byte("thanks"))
	}))
	defer srv.Close()
	ep := e.endpoint(t, "app_clockwork", srv.URL+"/hook")
	e.emit(t, "app_clockwork", webhooks.EventExecutionFailed)

	if n := dispatch(t, e.dispatcher(webhooks.DispatcherOptions{})); n != 1 {
		t.Fatalf("dispatched %d", n)
	}
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests", len(got))
	}
	r, body := got[0], raw[0]
	if err := webhooks.Verify(ep.Secret, r.Header.Get(webhooks.HeaderSignature), body, e.clock.now(), 5*time.Minute); err != nil {
		t.Fatalf("signature does not verify: %v (header %q)", err, r.Header.Get(webhooks.HeaderSignature))
	}
	var ev webhooks.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatal(err)
	}
	d := e.deliveries(t, ep.ID)[0]
	if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get(webhooks.HeaderEvent) != webhooks.EventExecutionFailed || r.Header.Get(webhooks.HeaderDelivery) != d.ID ||
		r.Header.Get(webhooks.HeaderEventID) != ev.ID || ev.Type != webhooks.EventExecutionFailed || ev.ApplicationID != "app_clockwork" {
		t.Fatalf("request: %s %v event=%+v", r.Method, r.Header, ev)
	}
	if d.Status != webhooks.StatusDelivered || d.Attempts != 1 || d.LastStatusCode == nil || *d.LastStatusCode != 200 ||
		d.DeliveredAt == nil || d.NextAttemptAt != nil || d.LastResponse == nil || *d.LastResponse != "thanks" {
		t.Fatalf("delivery log: %+v", d)
	}
	full, err := e.svc.Delivery(t.Context(), ep.ID, d.ID)
	if err != nil || full.Payload == nil {
		t.Fatalf("delivery with payload: %+v %v", full, err)
	}
	if n := dispatch(t, e.dispatcher(webhooks.DispatcherOptions{})); n != 0 {
		t.Fatalf("delivered twice")
	}
}

// The M6 exit criterion, part 2: failed deliveries are retried on schedule and every attempt is tracked.
func TestFailedDeliveriesAreRetriedAndTracked(t *testing.T) {
	e := setup(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte("down for maintenance"))
		}
	}))
	defer srv.Close()
	ep := e.endpoint(t, "app_clockwork", srv.URL)
	e.emit(t, "app_clockwork", webhooks.EventExecutionSucceeded)
	disp := e.dispatcher(webhooks.DispatcherOptions{})

	dispatch(t, disp) // attempt 1: 503
	d := e.deliveries(t, ep.ID)[0]
	if d.Status != webhooks.StatusPending || d.Attempts != 1 || *d.LastStatusCode != 503 || *d.LastError != "endpoint answered 503" ||
		*d.LastResponse != "down for maintenance" || !d.NextAttemptAt.Equal(e.clock.now().Add(10*time.Second)) {
		t.Fatalf("after attempt 1: %+v", d)
	}
	if dispatch(t, disp) != 0 {
		t.Fatal("retried before its backoff")
	}
	e.clock.advance(10 * time.Second)
	dispatch(t, disp) // attempt 2: 503, next in 30s
	e.clock.advance(29 * time.Second)
	if dispatch(t, disp) != 0 {
		t.Fatal("second backoff too short")
	}
	e.clock.advance(time.Second)
	dispatch(t, disp) // attempt 3: 200
	d = e.deliveries(t, ep.ID)[0]
	if d.Status != webhooks.StatusDelivered || d.Attempts != 3 || *d.LastStatusCode != 200 || d.LastError != nil {
		t.Fatalf("after recovery: %+v", d)
	}
}

func TestDeliveryGivesUpThenCanBeRedelivered(t *testing.T) {
	e := setup(t)
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	ep := e.endpoint(t, "app_clockwork", srv.URL)
	e.emit(t, "app_clockwork", webhooks.EventExecutionFailed)
	disp := e.dispatcher(webhooks.DispatcherOptions{})

	for n := 1; n <= webhooks.MaxAttempts; n++ {
		if dispatch(t, disp) != 1 {
			t.Fatalf("attempt %d not made", n)
		}
		e.clock.advance(webhooks.Backoff(n))
	}
	e.clock.advance(48 * time.Hour)
	if dispatch(t, disp) != 0 {
		t.Fatal("kept retrying past MaxAttempts")
	}
	d := e.deliveries(t, ep.ID)[0]
	if d.Status != webhooks.StatusFailed || d.Attempts != webhooks.MaxAttempts || d.NextAttemptAt != nil || *d.LastStatusCode != 500 {
		t.Fatalf("given up: %+v", d)
	}

	healthy.Store(true)
	if _, err := e.svc.Redeliver(t.Context(), ep.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	dispatch(t, disp)
	if d := e.deliveries(t, ep.ID)[0]; d.Status != webhooks.StatusDelivered || d.Attempts != 1 {
		t.Fatalf("redelivered: %+v", d)
	}
}

func TestEventFilterAndDisabledEndpoint(t *testing.T) {
	e := setup(t)
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
	defer srv.Close()
	onlyFailures := e.endpoint(t, "app_clockwork", srv.URL, webhooks.EventExecutionFailed)
	paused := e.endpoint(t, "app_clockwork", srv.URL)
	off := false
	if _, err := e.svc.Update(t.Context(), paused.ID, webhooks.EndpointUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	other := e.endpoint(t, "app_ext", srv.URL) // another application's endpoint hears nothing of app_clockwork

	e.emit(t, "app_clockwork", webhooks.EventExecutionStarted)
	e.emit(t, "app_clockwork", webhooks.EventExecutionFailed)
	disp := e.dispatcher(webhooks.DispatcherOptions{})
	dispatch(t, disp)
	if n := len(e.deliveries(t, onlyFailures.ID)); n != 1 {
		t.Fatalf("filtered endpoint got %d deliveries", n)
	}
	if n := len(e.deliveries(t, paused.ID)) + len(e.deliveries(t, other.ID)); n != 0 {
		t.Fatalf("disabled or foreign endpoint got %d deliveries", n)
	}
	if count.Load() != 1 {
		t.Fatalf("receiver called %d times", count.Load())
	}
	// A test event reaches even a disabled endpoint.
	if _, err := e.svc.SendTest(t.Context(), paused.ID); err != nil {
		t.Fatal(err)
	}
	dispatch(t, disp)
	if d := e.deliveries(t, paused.ID); len(d) != 1 || d[0].EventType != webhooks.EventTest || d[0].Status != webhooks.StatusDelivered {
		t.Fatalf("test delivery: %+v", d)
	}
}

func TestApplicationEndpointCannotReachPrivateAddress(t *testing.T) {
	e := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	ep := e.endpoint(t, "app_ext", srv.URL)
	e.emit(t, "app_ext", webhooks.EventExecutionFailed)

	dispatch(t, e.dispatcher(webhooks.DispatcherOptions{}))
	d := e.deliveries(t, ep.ID)[0]
	if d.Status != webhooks.StatusPending || d.LastError == nil || *d.LastError != "blocked: the endpoint resolves to a non-public address" {
		t.Fatalf("application endpoint to 127.0.0.1: %+v", d)
	}
	e.clock.advance(time.Minute)
	dispatch(t, e.dispatcher(webhooks.DispatcherOptions{AppsMayReachPrivate: true}))
	if d := e.deliveries(t, ep.ID)[0]; d.Status != webhooks.StatusDelivered {
		t.Fatalf("with AppsMayReachPrivate: %+v", d)
	}
}

func TestConcurrentDispatchersDeliverEachOnce(t *testing.T) {
	e := setup(t)
	var (
		mu   sync.Mutex
		seen = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get(webhooks.HeaderDelivery)]++
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}))
	defer srv.Close()
	for i := range 3 {
		e.endpoint(t, "app_clockwork", fmt.Sprintf("%s/%d", srv.URL, i))
	}
	for range 20 {
		e.emit(t, "app_clockwork", webhooks.EventExecutionSucceeded)
	}
	var wg sync.WaitGroup
	for range 4 {
		d := e.dispatcher(webhooks.DispatcherOptions{Workers: 5})
		wg.Go(func() {
			for {
				n, err := d.DispatchOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if len(seen) != 60 {
		t.Fatalf("%d distinct deliveries received, want 60", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("delivery %s received %d times", id, n)
		}
	}
}

func TestCrashedDispatcherClaimIsRetriedAfterLease(t *testing.T) {
	e := setup(t)
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { count.Add(1) }))
	defer srv.Close()
	ep := e.endpoint(t, "app_clockwork", srv.URL)
	e.emit(t, "app_clockwork", webhooks.EventExecutionFailed)
	// A dispatcher claimed the delivery (attempt 1, reserved for a minute) and then died.
	if _, err := e.pool.Exec(t.Context(), `UPDATE webhook_deliveries SET attempts = 1, last_attempt_at = $2, next_attempt_at = $3
		WHERE endpoint_id = $1`, ep.ID, e.clock.now(), e.clock.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	disp := e.dispatcher(webhooks.DispatcherOptions{})
	if dispatch(t, disp) != 0 {
		t.Fatal("claimed delivery was taken before its lease expired")
	}
	e.clock.advance(time.Minute)
	dispatch(t, disp)
	if d := e.deliveries(t, ep.ID)[0]; d.Status != webhooks.StatusDelivered || d.Attempts != 2 || count.Load() != 1 {
		t.Fatalf("after lease: %+v (receiver calls %d)", d, count.Load())
	}
}

func TestDispatcherRunDeliversAndStops(t *testing.T) {
	e := setup(t)
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got <- r.Header.Get(webhooks.HeaderEvent) }))
	defer srv.Close()
	e.endpoint(t, "app_clockwork", srv.URL)
	e.emit(t, "app_clockwork", webhooks.EventExecutionStarted)

	d := webhooks.NewDispatcher(e.pool, webhooks.DispatcherOptions{PollInterval: 20 * time.Millisecond, DefaultApplicationID: "app_clockwork",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	select {
	case ev := <-got:
		if ev != webhooks.EventExecutionStarted {
			t.Fatalf("event %q", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not deliver")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}
