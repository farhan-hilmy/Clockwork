package webhooks_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/webhooks"
)

func TestEndpointLimitPerApplication(t *testing.T) {
	e := setup(t)
	e.svc.MaxPerApplication = 2
	for range 2 {
		e.endpoint(t, "app_ext", "https://hooks.example.com")
	}
	if _, err := e.svc.Create(t.Context(), webhooks.EndpointInput{ApplicationID: "app_ext", URL: "https://hooks.example.com"}); !errors.Is(err, webhooks.ErrLimit) {
		t.Fatalf("third endpoint: %v", err)
	}
	for range 3 { // the default (operator) application is exempt
		e.endpoint(t, "app_clockwork", "https://hooks.example.com")
	}
}

// Security: a hanging endpoint holds one delivery slot, not the dispatcher.
func TestHangingEndpointDoesNotStallOthers(t *testing.T) {
	e := setup(t)
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Deferred in this order so the handler is released before Close, which
	// waits for it. (The handler does not read the body, so the server cannot
	// tell the client gave up.)
	defer hang.Close()
	defer close(release)
	var fast atomic.Int32
	quick := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fast.Add(1) }))
	defer quick.Close()
	e.endpoint(t, "app_clockwork", hang.URL, webhooks.EventExecutionFailed)
	e.endpoint(t, "app_clockwork", quick.URL, webhooks.EventExecutionSucceeded)
	e.emit(t, "app_clockwork", webhooks.EventExecutionFailed) // due first, so it is in the first claim; then it hangs
	e.clock.advance(time.Second)
	for range 6 {
		e.emit(t, "app_clockwork", webhooks.EventExecutionSucceeded)
	}

	d := webhooks.NewDispatcher(e.pool, webhooks.DispatcherOptions{Workers: 3, Timeout: 20 * time.Second,
		PollInterval: 10 * time.Millisecond, DefaultApplicationID: "app_clockwork", Now: e.clock.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for fast.Load() < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 6 deliveries to a healthy endpoint went out while another endpoint hung", fast.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
