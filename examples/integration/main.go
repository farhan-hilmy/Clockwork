// Command integration is an example application that uses Clockwork through its
// API with its own API key. It shows the patterns an integration needs:
//
//  1. Declare its task idempotently: create it, or update it if it exists.
//  2. Trigger a run with an Idempotency-Key, retrying network errors safely.
//  3. Wait for the execution and read its result.
//
// Usage (see README.md):
//
//	CLOCKWORK_URL=http://localhost:8080 CLOCKWORK_API_KEY=cwk_... go run ./examples/integration
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// The task this application owns. Declaring it in code keeps Clockwork in step
// with the application's own deployments.
const taskName = "example-heartbeat"

func desiredTask(target string) map[string]any {
	return map[string]any{
		"name":            taskName,
		"description":     "Pings the example target every 5 minutes. Managed by examples/integration.",
		"runner":          map[string]any{"type": "http", "config": map[string]any{"method": "GET", "url": target}},
		"schedule":        map[string]any{"cron": "*/5 * * * *", "timezone": "UTC"},
		"timeout_seconds": 10,
		"retry":           map[string]any{"max_attempts": 3, "backoff_initial_seconds": 5, "backoff_max_seconds": 60},
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	c := client{
		base: envOr("CLOCKWORK_URL", "http://localhost:8080") + "/api/v1",
		key:  os.Getenv("CLOCKWORK_API_KEY"),
		http: &http.Client{Timeout: 15 * time.Second},
	}
	if c.key == "" {
		return errors.New("set CLOCKWORK_API_KEY to an application API key (cwk_...)")
	}
	target := envOr("TARGET_URL", "https://api.github.com/zen")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Who are we? The key decides: an application only ever sees its own tasks.
	var meta struct {
		Principal struct {
			Type string `json:"type"`
			Name string `json:"application_name"`
		} `json:"principal"`
	}
	if _, err := c.do(ctx, "GET", "/meta", nil, nil, &meta); err != nil {
		return err
	}
	fmt.Printf("authenticated as %s %q\n", meta.Principal.Type, meta.Principal.Name)

	taskID, err := c.ensureTask(ctx, desiredTask(target))
	if err != nil {
		return err
	}
	fmt.Println("task:", taskID)

	exe, err := c.runNow(ctx, taskID)
	if err != nil {
		return err
	}
	fmt.Println("queued execution:", exe)

	return c.wait(ctx, exe)
}

type client struct {
	base, key string
	http      *http.Client
}

// apiError is Clockwork's error envelope.
type apiError struct {
	Status int
	Body   struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%d %s: %s %v", e.Status, e.Body.Error.Code, e.Body.Error.Message, e.Body.Error.Details)
}

func (c client) do(ctx context.Context, method, path string, body any, hdr map[string]string, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		e := &apiError{Status: res.StatusCode}
		_ = json.NewDecoder(res.Body).Decode(&e.Body)
		return res.StatusCode, e
	}
	if out != nil && res.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return res.StatusCode, err
		}
	}
	return res.StatusCode, nil
}

// ensureTask creates the task, or updates it to the desired definition if it
// already exists. Task names are unique per application, so a create that races
// with another instance of this app gets 409 and falls back to the update.
func (c client) ensureTask(ctx context.Context, want map[string]any) (string, error) {
	var created struct{ ID string }
	_, err := c.do(ctx, "POST", "/tasks", want, nil, &created)
	var ae *apiError
	if err == nil {
		return created.ID, nil
	}
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		return "", err
	}
	var page struct{ Tasks []struct{ ID, Name string } }
	if _, err := c.do(ctx, "GET", "/tasks?q="+url.QueryEscape(taskName), nil, nil, &page); err != nil {
		return "", err
	}
	for _, t := range page.Tasks {
		if t.Name == taskName {
			if _, err := c.do(ctx, "PATCH", "/tasks/"+t.ID, want, nil, nil); err != nil {
				return "", err
			}
			return t.ID, nil
		}
	}
	return "", errors.New("task reported as existing but not found")
}

// runNow queues a run. The Idempotency-Key is chosen once, before the first
// attempt, so retrying after a timeout cannot queue a second run.
func (c client) runNow(ctx context.Context, taskID string) (string, error) {
	key := newIdempotencyKey()
	var exe struct{ ID string }
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var code int
		code, err = c.do(ctx, "POST", "/tasks/"+taskID+"/run", nil, map[string]string{"Idempotency-Key": key}, &exe)
		if err == nil {
			if code == http.StatusOK {
				fmt.Println("(replayed: an earlier attempt had already queued this run)")
			}
			return exe.ID, nil
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status < 500 {
			return "", err // a client error will not fix itself
		}
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	return "", err
}

func (c client) wait(ctx context.Context, id string) error {
	for {
		var e struct {
			Status     string          `json:"status"`
			Attempt    int             `json:"attempt"`
			DurationMS *int64          `json:"duration_ms"`
			Output     json.RawMessage `json:"output"`
			Error      json.RawMessage `json:"error"`
		}
		if _, err := c.do(ctx, "GET", "/executions/"+id, nil, nil, &e); err != nil {
			return err
		}
		switch e.Status {
		case "queued", "running":
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		fmt.Printf("status: %s (attempt %d)\n", e.Status, e.Attempt)
		if e.DurationMS != nil {
			fmt.Printf("duration: %d ms\n", *e.DurationMS)
		}
		if len(e.Output) > 0 && string(e.Output) != "null" {
			fmt.Printf("output: %s\n", e.Output)
		}
		if len(e.Error) > 0 && string(e.Error) != "null" {
			fmt.Printf("error: %s\n", e.Error)
		}
		if e.Status == "retrying" {
			fmt.Println("a retry is queued; follow it with GET /api/v1/executions?task_id=...")
		}
		return nil
	}
}

func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "example-" + hex.EncodeToString(b)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
