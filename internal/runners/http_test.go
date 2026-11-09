package runners

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func httpReq(t *testing.T, cfg string) Request {
	t.Helper()
	return Request{Config: json.RawMessage(cfg), ExecutionID: "exe_1", TaskID: "tsk_1", Attempt: 2}
}

func decodeHTTP(t *testing.T, raw json.RawMessage) HTTPOutput {
	t.Helper()
	var o HTTPOutput
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("output %s: %v", raw, err)
	}
	return o
}

func wantFailure(t *testing.T, err error, kind string, retryable bool) {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("want %s failure, got %v", kind, err)
	}
	if f.Kind != kind || f.Retryable != retryable {
		t.Fatalf("got kind=%s retryable=%v (%s), want %s %v", f.Kind, f.Retryable, f.Message, kind, retryable)
	}
}

func TestHTTPSuccessSendsConfiguredRequest(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := make([]byte, 100)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	out, err := NewHTTP(HTTPOptions{UserAgent: "Clockwork/test"}).Run(context.Background(), httpReq(t,
		`{"method":"POST","url":"`+srv.URL+`/hook?a=1","headers":{"Authorization":"Bearer s3cret"},"query":{"b":"2"},"body":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	o := decodeHTTP(t, out)
	if o.StatusCode != 201 || o.Body != `{"ok":true}` || o.ContentType != "application/json" || o.BodyTruncated {
		t.Fatalf("output: %+v", o)
	}
	if got.Method != "POST" || got.URL.Query().Get("a") != "1" || got.URL.Query().Get("b") != "2" || body != "hello" {
		t.Fatalf("request: %s %s body=%q", got.Method, got.URL, body)
	}
	if got.Header.Get("Authorization") != "Bearer s3cret" || got.Header.Get("User-Agent") != "Clockwork/test" ||
		got.Header.Get("X-Clockwork-Execution-Id") != "exe_1" || got.Header.Get("X-Clockwork-Attempt") != "2" {
		t.Fatalf("headers: %v", got.Header)
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		expected  string
		ok        bool
		retryable bool
	}{
		{200, "", true, false},
		{204, "", true, false},
		{500, "", false, true},
		{503, "", false, true},
		{429, "", false, true},
		{408, "", false, true},
		{404, "", false, false},
		{301, "", false, false},
		{404, `,"expected_status":[404]`, true, false},
		{200, `,"expected_status":[201]`, false, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if c.status == 301 {
				w.Header().Set("Location", "/elsewhere-that-also-301s")
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte("body"))
		}))
		out, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"method":"GET","url":"`+srv.URL+`"`+c.expected+`}`))
		srv.Close()
		if c.ok {
			if err != nil {
				t.Errorf("status %d%s: unexpected error %v", c.status, c.expected, err)
			}
			continue
		}
		if c.status == 301 {
			// Redirect loops stop after 5 hops as a connection-level error.
			wantFailure(t, err, KindConnection, true)
			continue
		}
		wantFailure(t, err, KindHTTPStatus, c.retryable)
		if o := decodeHTTP(t, out); o.StatusCode != c.status || o.Body != "body" {
			t.Errorf("status %d: output %+v", c.status, o)
		}
	}
}

func TestHTTPBodyExcerptIsBounded(t *testing.T) {
	big := strings.Repeat("x", MaxResponseExcerpt+500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(big)) }))
	defer srv.Close()
	out, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"method":"GET","url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	o := decodeHTTP(t, out)
	if len(o.Body) != MaxResponseExcerpt || !o.BodyTruncated || o.BodyBytes != int64(len(big)) {
		t.Fatalf("len=%d truncated=%v bytes=%d", len(o.Body), o.BodyTruncated, o.BodyBytes)
	}
}

func TestHTTPTimeoutIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := NewHTTP(HTTPOptions{}).Run(ctx, httpReq(t, `{"method":"GET","url":"`+srv.URL+`"}`))
	wantFailure(t, err, KindTimeout, true)
}

func TestHTTPConnectionRefusedIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there now
	_, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"method":"GET","url":"`+url+`/x?token=s3cret"}`))
	wantFailure(t, err, KindConnection, true)
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks the query string: %v", err)
	}
}

func TestHTTPDenyPrivateBlocksInternalTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Error("request reached a private address") }))
	defer srv.Close()
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		_, err := NewHTTP(HTTPOptions{DenyPrivate: true}).Run(context.Background(), httpReq(t, `{"method":"GET","url":"`+u+`"}`))
		wantFailure(t, err, KindBlocked, false)
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fc00::1": false, "::ffff:127.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := isPublic(netip.MustParseAddr(addr).Unmap()); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestBadConfigIsNotRetryable(t *testing.T) {
	_, err := NewHTTP(HTTPOptions{}).Run(context.Background(), httpReq(t, `{"url":`))
	wantFailure(t, err, KindConfig, false)
}
