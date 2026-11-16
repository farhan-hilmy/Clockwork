package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// client calls Clockwork's public /api/v1 API with one bearer token. The MCP
// server uses nothing else: it is an integration like any other.
type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(baseURL, token string, hc *http.Client) *client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &client{base: strings.TrimRight(baseURL, "/") + "/api/v1", token: token, http: hc}
}

// apiError is Clockwork's error envelope, turned into a message an agent can act on.
type apiError struct {
	Status  int
	Code    string
	Message string
	Details map[string]string
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Clockwork answered %d %s: %s", e.Status, e.Code, e.Message)
	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(" (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "%s: %s", k, e.Details[k])
		}
		b.WriteString(")")
	}
	return b.String()
}

// do sends a request and decodes a JSON object answer into out (if non-nil).
// It returns the response headers for callers that need them.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body any, hdr map[string]string, out any) (http.Header, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "clockwork-mcp")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL may carry nothing secret, but keep messages short
		}
		return nil, 0, fmt.Errorf("cannot reach Clockwork at %s: %w", c.base, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	if res.StatusCode >= 400 {
		var env struct {
			Error struct {
				Code    string            `json:"code"`
				Message string            `json:"message"`
				Details map[string]string `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.Error.Code == "" {
			env.Error.Code, env.Error.Message = "error", strings.TrimSpace(string(raw))
		}
		return res.Header, res.StatusCode, &apiError{Status: res.StatusCode, Code: env.Error.Code, Message: env.Error.Message, Details: env.Error.Details}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.Header, res.StatusCode, fmt.Errorf("unexpected answer from Clockwork: %w", err)
		}
	}
	return res.Header, res.StatusCode, nil
}

// principal describes who the token belongs to, from GET /meta.
type principal struct {
	Type            string `json:"type"` // "admin" or "application"
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
}

func (c *client) whoami(ctx context.Context) (principal, string, error) {
	var meta struct {
		Version   string    `json:"version"`
		Principal principal `json:"principal"`
	}
	if _, _, err := c.do(ctx, http.MethodGet, "/meta", nil, nil, nil, &meta); err != nil {
		return principal{}, "", err
	}
	return meta.Principal, meta.Version, nil
}
