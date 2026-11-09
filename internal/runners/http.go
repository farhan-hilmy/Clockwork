package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// MaxResponseExcerpt is how much of a response body is stored.
const MaxResponseExcerpt = 8 << 10

// HTTPOptions configures the HTTP runner.
type HTTPOptions struct {
	// DenyPrivate refuses loopback, link-local, private and other non-public
	// targets. The check runs on the resolved IP at connect time, so DNS names
	// that point inward (or rebind) are caught too.
	DenyPrivate bool
	// UserAgent is sent unless the task sets its own.
	UserAgent string
}

// HTTP sends the request configured on a task.
type HTTP struct {
	client    *http.Client
	userAgent string
}

// NewHTTP builds the HTTP runner.
func NewHTTP(o HTTPOptions) *HTTP {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if o.DenyPrivate {
		dialer.Control = denyPrivate
	}
	transport := &http.Transport{
		Proxy:                 nil, // a proxy would bypass the private-address check
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "Clockwork"
	}
	return &HTTP{
		userAgent: ua,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				return nil
			},
		},
	}
}

// HTTPOutput is stored as executions.output for HTTP tasks.
type HTTPOutput struct {
	StatusCode    int    `json:"status_code"`
	ContentType   string `json:"content_type,omitempty"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
	BodyBytes     int64  `json:"body_bytes"`
}

func (h *HTTP) Run(ctx context.Context, req Request) (json.RawMessage, error) {
	var c tasks.HTTPConfig
	if err := json.Unmarshal(req.Config, &c); err != nil {
		return nil, fail(KindConfig, false, "invalid http config: %v", err)
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fail(KindConfig, false, "invalid url: %v", err)
	}
	if len(c.Query) > 0 {
		q := u.Query()
		for k, v := range c.Query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	var body io.Reader
	if c.Body != "" {
		body = strings.NewReader(c.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, c.Method, u.String(), body)
	if err != nil {
		return nil, fail(KindConfig, false, "build request: %v", err)
	}
	hreq.Header.Set("User-Agent", h.userAgent)
	// Lets the receiver recognise a re-delivery of the same attempt (delivery is at-least-once).
	hreq.Header.Set("X-Clockwork-Execution-Id", req.ExecutionID)
	hreq.Header.Set("X-Clockwork-Attempt", fmt.Sprint(req.Attempt))
	for k, v := range c.Headers {
		hreq.Header.Set(k, v)
	}

	res, err := h.client.Do(hreq)
	if err != nil {
		return nil, classifyTransport(ctx, err)
	}
	defer res.Body.Close()

	ex := newExcerpt(MaxResponseExcerpt)
	// Read a bounded amount past the excerpt so body_bytes is meaningful without
	// downloading arbitrarily large responses.
	_, readErr := io.Copy(ex, io.LimitReader(res.Body, 1<<20))
	out := HTTPOutput{
		StatusCode:    res.StatusCode,
		ContentType:   res.Header.Get("Content-Type"),
		Body:          ex.String(),
		BodyTruncated: ex.truncated(),
		BodyBytes:     ex.total,
	}
	output := mustJSON(out)
	if readErr != nil && ctx.Err() != nil {
		return output, classifyTransport(ctx, readErr)
	}
	if !statusOK(res.StatusCode, c.ExpectedStatus) {
		return output, fail(KindHTTPStatus, retryableStatus(res.StatusCode), "unexpected status %d", res.StatusCode)
	}
	return output, nil
}

func statusOK(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 300
	}
	return slices.Contains(expected, code)
}

func retryableStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout
}

func classifyTransport(ctx context.Context, err error) *Failure {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fail(KindTimeout, true, "task timeout exceeded")
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return fail(KindInterrupted, false, "canceled before the request finished")
	}
	var blocked *blockedAddrError
	if errors.As(err, &blocked) {
		return fail(KindBlocked, false, "%v", blocked)
	}
	return fail(KindConnection, true, "%v", unwrapURLError(err))
}

// unwrapURLError drops the `Get "https://…": ` prefix, which may repeat query secrets.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type blockedAddrError struct{ addr netip.Addr }

func (e *blockedAddrError) Error() string {
	return fmt.Sprintf("destination %s is not a public address (CLOCKWORK_HTTP_DENY_PRIVATE is on)", e.addr)
}

// denyPrivate runs after DNS resolution, on the exact address being dialled.
func denyPrivate(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if a := ap.Addr().Unmap(); !isPublic(a) {
		return &blockedAddrError{addr: a}
	}
	return nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 internals
}

func isPublic(a netip.Addr) bool {
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs are marshalled here
	}
	return bytes.TrimSpace(b)
}
