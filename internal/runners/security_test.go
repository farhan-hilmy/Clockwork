package runners

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Security: redirects never carry headers or a body to another origin.
func TestCrossOriginRedirectsAreNotFollowed(t *testing.T) {
	var other atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	defer elsewhere.Close()
	var sameOriginGotKey atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/collect", http.StatusTemporaryRedirect)
		case "/moved":
			http.Redirect(w, r, "/new-home", http.StatusTemporaryRedirect)
		case "/new-home":
			sameOriginGotKey.Store(r.Header.Get("X-Api-Key") == "k-123")
		}
	}))
	defer srv.Close()

	out, err := NewHTTP(HTTPOptions{}).Run(context.Background(),
		httpReq(t, `{"method":"POST","url":"`+srv.URL+`/away","headers":{"X-Api-Key":"k-123"},"body":"payload"}`))
	wantFailure(t, err, KindHTTPStatus, false)
	if o := decodeHTTP(t, out); o.StatusCode != http.StatusTemporaryRedirect || other.Load() != 0 {
		t.Fatalf("cross-origin redirect followed: status %d, other host hit %d times", o.StatusCode, other.Load())
	}
	if _, err := NewHTTP(HTTPOptions{}).Run(context.Background(),
		httpReq(t, `{"url":"`+srv.URL+`/moved","headers":{"X-Api-Key":"k-123"}}`)); err != nil || !sameOriginGotKey.Load() {
		t.Fatalf("same-origin redirect not followed with its headers: %v", err)
	}
}

func TestHostileResponseHeadersAreBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/huge" {
			w.Header().Set("X-Filler", strings.Repeat("x", 100<<10))
		}
		w.Header().Set("Content-Type", "text/plain; "+strings.Repeat("p=1; ", 200))
	}))
	defer srv.Close()
	out, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if o := decodeHTTP(t, out); len(o.ContentType) > maxContentTypeLen {
		t.Fatalf("content type stored at %d bytes", len(o.ContentType))
	}
	if _, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"url":"`+srv.URL+`/huge"}`)); err == nil {
		t.Fatal("a 100 KiB response header was accepted")
	}
}

func TestBlockedErrorHidesTheAddressAndMessagesAreCapped(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := NewHTTP(HTTPOptions{DenyPrivate: true}).Run(context.Background(), httpReq(t, `{"url":"`+srv.URL+`"}`))
	if f := AsFailure(err); f.Kind != KindBlocked || strings.Contains(f.Message, "127.0.0.1") {
		t.Fatalf("blocked error: %+v", f)
	}
	if f := fail(KindConnection, true, "%s", strings.Repeat("y", 5000)); len(f.Message) > maxMessageLen+len("…") {
		t.Fatalf("message not capped: %d bytes", len(f.Message))
	}
}
