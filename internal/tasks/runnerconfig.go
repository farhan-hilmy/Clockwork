package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Mask replaces secret values in API responses. Sending it back on update keeps the stored value.
const Mask = "********"

// HTTPConfig configures the HTTP request runner.
type HTTPConfig struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers,omitempty"`
	Query          map[string]string `json:"query,omitempty"`
	Body           string            `json:"body,omitempty"`
	ExpectedStatus []int             `json:"expected_status,omitempty"` // empty means any 2xx
}

// ShellConfig configures the shell runner. Command is run with `sh -c`.
type ShellConfig struct {
	Command    string            `json:"command"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

const (
	maxURLLen     = 2048
	maxHeaders    = 50
	maxQuery      = 50
	maxBodyBytes  = 256 << 10
	maxCommandLen = 16 << 10
	maxEnv        = 100
	maxValueLen   = 8 << 10
)

var (
	httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	headerName  = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	sensitiveHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true}
	sensitiveWords   = []string{"token", "secret", "password", "passwd", "api-key", "apikey", "api_key", "auth", "credential", "private"}
)

// IsSensitiveName reports whether a header or environment variable name likely holds a secret.
func IsSensitiveName(name string) bool {
	n := strings.ToLower(name)
	if sensitiveHeaders[n] {
		return true
	}
	for _, w := range sensitiveWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func decodeStrict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("config is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid config: %v", err)
	}
	return nil
}

// normalizeRunner validates a runner and returns it with canonical JSON config.
func normalizeRunner(r Runner, allowShell bool, fe fieldErrors) Runner {
	switch r.Type {
	case RunnerHTTP:
		var c HTTPConfig
		if err := decodeStrict(r.Config, &c); err != nil {
			fe.add("runner.config", "%v", err)
			return r
		}
		validateHTTP(&c, fe)
		r.Config = mustJSON(c)
	case RunnerShell:
		if !allowShell {
			fe.add("runner.type", "shell tasks are disabled on this server (set CLOCKWORK_ENABLE_SHELL=true to allow them)")
			return r
		}
		var c ShellConfig
		if err := decodeStrict(r.Config, &c); err != nil {
			fe.add("runner.config", "%v", err)
			return r
		}
		validateShell(&c, fe)
		r.Config = mustJSON(c)
	case "":
		fe.add("runner.type", "is required (http or shell)")
	default:
		fe.add("runner.type", "unknown runner %q (expected http or shell)", r.Type)
	}
	return r
}

func validateHTTP(c *HTTPConfig, fe fieldErrors) {
	c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
	if c.Method == "" {
		c.Method = "GET"
	}
	if !httpMethods[c.Method] {
		fe.add("runner.config.method", "must be one of GET, POST, PUT, PATCH, DELETE")
	}
	c.URL = strings.TrimSpace(c.URL)
	u, err := url.Parse(c.URL)
	switch {
	case c.URL == "":
		fe.add("runner.config.url", "is required")
	case len(c.URL) > maxURLLen:
		fe.add("runner.config.url", "must be at most %d characters", maxURLLen)
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		fe.add("runner.config.url", "must be an absolute http or https URL")
	case u.User != nil:
		fe.add("runner.config.url", "must not contain credentials; use a header instead")
	}
	if len(c.Headers) > maxHeaders {
		fe.add("runner.config.headers", "at most %d headers", maxHeaders)
	}
	for k, v := range c.Headers {
		if !headerName.MatchString(k) {
			fe.add("runner.config.headers", "invalid header name %q", k)
		}
		if len(v) > maxValueLen || strings.ContainsAny(v, "\r\n") {
			fe.add("runner.config.headers", "invalid value for header %q", k)
		}
	}
	if len(c.Query) > maxQuery {
		fe.add("runner.config.query", "at most %d query parameters", maxQuery)
	}
	if len(c.Body) > maxBodyBytes {
		fe.add("runner.config.body", "must be at most %d bytes", maxBodyBytes)
	}
	for _, s := range c.ExpectedStatus {
		if s < 100 || s > 599 {
			fe.add("runner.config.expected_status", "status codes must be between 100 and 599")
		}
	}
}

func validateShell(c *ShellConfig, fe fieldErrors) {
	if strings.TrimSpace(c.Command) == "" {
		fe.add("runner.config.command", "is required")
	}
	if len(c.Command) > maxCommandLen {
		fe.add("runner.config.command", "must be at most %d bytes", maxCommandLen)
	}
	if strings.ContainsRune(c.Command, 0) || strings.ContainsRune(c.WorkingDir, 0) {
		fe.add("runner.config", "must not contain NUL bytes")
	}
	if len(c.Env) > maxEnv {
		fe.add("runner.config.env", "at most %d variables", maxEnv)
	}
	for k, v := range c.Env {
		if !envName.MatchString(k) {
			fe.add("runner.config.env", "invalid variable name %q", k)
		}
		if len(v) > maxValueLen {
			fe.add("runner.config.env", "value for %q is too long", k)
		}
	}
}

// Redact returns a copy of the runner with secret values replaced by Mask.
// It is applied to every task leaving the service.
func Redact(r Runner) Runner {
	switch r.Type {
	case RunnerHTTP:
		var c HTTPConfig
		if json.Unmarshal(r.Config, &c) != nil {
			return Runner{Type: r.Type, Config: json.RawMessage(`{}`)}
		}
		c.Headers = maskMap(c.Headers)
		c.Query = maskMap(c.Query)
		r.Config = mustJSON(c)
	case RunnerShell:
		var c ShellConfig
		if json.Unmarshal(r.Config, &c) != nil {
			return Runner{Type: r.Type, Config: json.RawMessage(`{}`)}
		}
		c.Env = maskMap(c.Env)
		r.Config = mustJSON(c)
	}
	return r
}

func maskMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if IsSensitiveName(k) && v != "" {
			v = Mask
		}
		out[k] = v
	}
	return out
}

// restoreMasked replaces Mask values in an incoming runner with the stored ones,
// so clients can round-trip a redacted task without wiping its secrets.
func restoreMasked(incoming, stored Runner, fe fieldErrors) Runner {
	if incoming.Type != stored.Type {
		// Nothing to restore from: any masked value will be rejected below.
		stored = Runner{Type: incoming.Type, Config: json.RawMessage(`{}`)}
	}
	restore := func(in, old map[string]string, field string) {
		for k, v := range in {
			if v != Mask {
				continue
			}
			if prev, ok := old[k]; ok {
				in[k] = prev
			} else {
				fe.add(field, "%q is masked but has no stored value", k)
			}
		}
	}
	// Decode strictly: if the incoming config is invalid, leave it untouched so
	// normalizeRunner reports the problem instead of re-encoding it away.
	switch incoming.Type {
	case RunnerHTTP:
		var in, old HTTPConfig
		if decodeStrict(incoming.Config, &in) != nil || json.Unmarshal(stored.Config, &old) != nil {
			return incoming
		}
		restore(in.Headers, old.Headers, "runner.config.headers")
		restore(in.Query, old.Query, "runner.config.query")
		incoming.Config = mustJSON(in)
	case RunnerShell:
		var in, old ShellConfig
		if decodeStrict(incoming.Config, &in) != nil || json.Unmarshal(stored.Config, &old) != nil {
			return incoming
		}
		restore(in.Env, old.Env, "runner.config.env")
		incoming.Config = mustJSON(in)
	}
	return incoming
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs of strings, ints and maps are marshalled here
	}
	return b
}
