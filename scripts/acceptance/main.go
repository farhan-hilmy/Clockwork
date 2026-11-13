// Command acceptance checks docs/DEFINITION_OF_DONE.md against a running
// Clockwork, usually a fresh Docker Compose install.
//
//	CLOCKWORK_URL=http://localhost:8080 CLOCKWORK_ADMIN_TOKEN=... go run ./scripts/acceptance
//
// It runs a small HTTP server that Clockwork calls back, both as a task target
// and as a webhook receiver, so no internet access is needed. Clockwork reaches
// it at ACCEPTANCE_CALLBACK_HOST (default host.docker.internal; use 127.0.0.1
// when Clockwork runs directly on this machine).
//
// Points that need crashes, races or CI are covered by the Go tests and the CI
// workflow; for those it prints where the evidence is.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	a := &acc{
		base:  strings.TrimRight(envOr("CLOCKWORK_URL", "http://localhost:8080"), "/"),
		admin: os.Getenv("CLOCKWORK_ADMIN_TOKEN"),
		run:   strconv.FormatInt(time.Now().Unix(), 36),
		http:  &http.Client{Timeout: 15 * time.Second},
	}
	if a.admin == "" {
		fmt.Fprintln(os.Stderr, "set CLOCKWORK_ADMIN_TOKEN")
		os.Exit(2)
	}
	if err := a.startCallback(envOr("ACCEPTANCE_CALLBACK_HOST", "host.docker.internal")); err != nil {
		fmt.Fprintln(os.Stderr, "callback server:", err)
		os.Exit(2)
	}
	defer a.cleanup()

	fmt.Printf("Clockwork acceptance check against %s (run %s)\n\n", a.base, a.run)
	a.point(1, "Clean install", a.cleanInstall)
	a.point(2, "Fails fast on bad config", a.failsFast)
	a.point(3, "Tasks in UI and API", a.tasksCRUD)
	a.point(4, "Schedules", a.schedules)
	a.point(12, "Application isolation", a.isolation)
	a.point(13, "Integration contract", a.contract)
	a.point(15, "Security baseline", a.security)
	// 5, 7 and 14 share one scheduled run, which takes up to a minute.
	a.point(5, "On-time runs", a.onTime)
	a.point(7, "Run history", a.history)
	a.point(14, "Webhooks", a.webhooks)
	for _, p := range []struct {
		n     int
		name  string
		where string
	}{
		{6, "Missed runs", "tests: TestMisfirePolicies, TestPlanDue"},
		{8, "Timeouts and retries", "tests: TestRetryableFailureIsRetriedWithBackoff, TestShellTimeoutKillsCommand, ..."},
		{9, "Concurrency policy", "tests: TestForbidSkipsOverlappingRunsAcrossInstances, TestAllowRunsOverlap"},
		{10, "Crash safety and several instances", "tests: TestCrashedWorkerIsRecoveredAndRetried, TestConcurrentWorkersRunEachExecutionOnce, ..."},
		{11, "Graceful shutdown", "tests: TestShutdownWaitsForRunsWithinGrace, TestShutdownInterruptsAndRetries"},
		{16, "Quality gates in CI", ".github/workflows/ci.yml"},
	} {
		fmt.Printf("  --   %2d  %-36s covered by %s\n", p.n, p.name, p.where)
	}

	fmt.Printf("\n%d checked live: %d passed, %d failed, %d skipped.\n", a.passed+a.failed+a.skipped, a.passed, a.failed, a.skipped)
	if a.failed > 0 {
		defer os.Exit(1)
	}
}

type acc struct {
	base, admin, run string
	http             *http.Client
	cb               *callback

	passed, failed, skipped int

	// Shared between points 5, 7 and 14.
	scheduledTask, hookID, hookSecret, flakyHookID string
	scheduledExe                                   map[string]any

	cleanups []func()
}

var errSkip = errors.New("skipped")

func (a *acc) point(n int, name string, check func() error) {
	start := time.Now()
	err := check()
	switch {
	case err == nil:
		a.passed++
		fmt.Printf("  PASS %2d  %-36s %s\n", n, name, time.Since(start).Round(100*time.Millisecond))
	case errors.Is(err, errSkip):
		a.skipped++
		fmt.Printf("  SKIP %2d  %-36s %v\n", n, name, err)
	default:
		a.failed++
		fmt.Printf("  FAIL %2d  %-36s %v\n", n, name, err)
	}
}

func (a *acc) cleanup() {
	for i := len(a.cleanups) - 1; i >= 0; i-- {
		a.cleanups[i]()
	}
}

// ---- HTTP helpers ----

type resp struct {
	code int
	body map[string]any
	raw  []byte
	hdr  http.Header
}

func (a *acc) call(method, path, token string, body any, hdr ...string) (resp, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return resp{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := a.http.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{code: res.StatusCode, raw: raw, hdr: res.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r, nil
}

// must calls and checks the status code, returning the decoded body.
func (a *acc) must(want int, method, path, token string, body any, hdr ...string) (map[string]any, error) {
	r, err := a.call(method, path, token, body, hdr...)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if r.code != want {
		return nil, fmt.Errorf("%s %s: status %d, want %d: %s", method, path, r.code, want, truncate(string(r.raw), 200))
	}
	return r.body, nil
}

func (a *acc) createTask(token string, spec map[string]any) (string, error) {
	t, err := a.must(201, "POST", "/api/v1/tasks", token, spec)
	if err != nil {
		return "", err
	}
	id := t["id"].(string)
	a.cleanups = append(a.cleanups, func() { _, _ = a.call("DELETE", "/api/v1/tasks/"+id, a.admin, nil) })
	return id, nil
}

func (a *acc) createApp(name string) (id, key string, err error) {
	app, err := a.must(201, "POST", "/api/v1/applications", a.admin, map[string]any{"name": "acceptance-" + a.run + "-" + name})
	if err != nil {
		return "", "", err
	}
	id = app["id"].(string)
	// Registered first so it runs last, after the application's tasks are gone.
	a.cleanups = append(a.cleanups, func() { _, _ = a.call("DELETE", "/api/v1/applications/"+id, a.admin, nil) })
	k, err := a.must(201, "POST", "/api/v1/applications/"+id+"/keys", a.admin, map[string]any{"name": "acceptance"})
	if err != nil {
		return "", "", err
	}
	return id, k["key"].(string), nil
}

// ---- points ----

func (a *acc) cleanInstall() error {
	r, err := a.must(200, "GET", "/readyz", "", nil)
	if err != nil {
		return err
	}
	if r["database"] != "ok" {
		return fmt.Errorf("readyz: %v", r)
	}
	ui, err := a.call("GET", "/", "", nil)
	if err != nil || ui.code != 200 || !strings.Contains(string(ui.raw), "<title>Clockwork</title>") {
		return fmt.Errorf("web UI not served at /: %d", ui.code)
	}
	// The server's user: only checkable through Docker Compose.
	out, err := exec.Command("docker", "compose", "exec", "-T", "clockwork", "id", "-u").Output()
	if err != nil {
		fmt.Println("         (could not check the container user: not run from the Compose directory)")
		return nil
	}
	if uid := strings.TrimSpace(string(out)); uid == "0" {
		return errors.New("the server runs as root")
	}
	return nil
}

func (a *acc) failsFast() error {
	cmd := exec.Command("docker", "compose", "run", "--rm", "--no-deps", "-T", "-e", "CLOCKWORK_ADMIN_TOKEN=short", "clockwork")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return errors.New("the server started with a 5-character admin token")
	}
	if _, isExit := err.(*exec.ExitError); !isExit {
		return fmt.Errorf("%w (needs Docker Compose in the current directory)", errSkip)
	}
	if !strings.Contains(string(out), "CLOCKWORK_ADMIN_TOKEN must be at least 16 characters") {
		return fmt.Errorf("unexpected startup error: %s", truncate(string(out), 300))
	}
	return nil
}

func (a *acc) tasksCRUD() error {
	const secret = "acceptance-secret-value"
	id, err := a.createTask(a.admin, map[string]any{
		"name":   "acceptance-" + a.run + "-crud",
		"runner": map[string]any{"type": "http", "config": map[string]any{"url": a.cb.url("/task"), "headers": map[string]string{"Authorization": "Bearer " + secret}}},
	})
	if err != nil {
		return err
	}
	got, err := a.must(200, "GET", "/api/v1/tasks/"+id, a.admin, nil)
	if err != nil {
		return err
	}
	if b, _ := json.Marshal(got); bytes.Contains(b, []byte(secret)) {
		return errors.New("secret header returned unmasked")
	}
	if _, err := a.must(200, "PATCH", "/api/v1/tasks/"+id, a.admin, map[string]any{"description": "edited"}); err != nil {
		return err
	}
	for _, s := range []string{"disable", "enable"} {
		if _, err := a.must(200, "POST", "/api/v1/tasks/"+id+"/"+s, a.admin, nil); err != nil {
			return err
		}
	}
	bad, err := a.must(422, "POST", "/api/v1/tasks", a.admin, map[string]any{"name": "", "runner": map[string]any{"type": "http", "config": map[string]any{"url": "nope"}}, "schedule": map[string]any{"cron": "bad"}})
	if err != nil {
		return err
	}
	details, _ := bad["error"].(map[string]any)["details"].(map[string]any)
	for _, f := range []string{"name", "runner.config.url", "schedule.cron"} {
		if _, ok := details[f]; !ok {
			return fmt.Errorf("validation error missing field %s: %v", f, details)
		}
	}
	if _, err := a.must(204, "DELETE", "/api/v1/tasks/"+id, a.admin, nil); err != nil {
		return err
	}
	_, err = a.must(404, "GET", "/api/v1/tasks/"+id, a.admin, nil)
	return err
}

func (a *acc) schedules() error {
	id, err := a.createTask(a.admin, map[string]any{
		"name":     "acceptance-" + a.run + "-jakarta",
		"runner":   map[string]any{"type": "http", "config": map[string]any{"url": a.cb.url("/task")}},
		"schedule": map[string]any{"cron": "0 9 * * *", "timezone": "Asia/Jakarta"},
		"enabled":  false,
	})
	if err != nil {
		return err
	}
	t, err := a.must(200, "POST", "/api/v1/tasks/"+id+"/enable", a.admin, nil)
	if err != nil {
		return err
	}
	next, err := time.Parse(time.RFC3339, fmt.Sprint(t["next_run_at"]))
	if err != nil {
		return fmt.Errorf("next_run_at: %v", t["next_run_at"])
	}
	// 09:00 in Jakarta (UTC+7, no DST) is 02:00 UTC, within the next day.
	if next.UTC().Hour() != 2 || next.Minute() != 0 || time.Until(next) > 24*time.Hour || time.Until(next) <= 0 {
		return fmt.Errorf("next run %s is not the next 09:00 Asia/Jakarta", next)
	}
	return nil
}

func (a *acc) isolation() error {
	_, keyA, err := a.createApp("a")
	if err != nil {
		return err
	}
	appB, keyB, err := a.createApp("b")
	if err != nil {
		return err
	}
	taskA, err := a.createTask(keyA, map[string]any{"name": "isolated", "runner": map[string]any{"type": "http", "config": map[string]any{"url": "https://example.com"}}})
	if err != nil {
		return err
	}
	list, err := a.must(200, "GET", "/api/v1/tasks", keyB, nil)
	if err != nil {
		return err
	}
	if n := len(list["tasks"].([]any)); n != 0 {
		return fmt.Errorf("application B lists %d of A's tasks", n)
	}
	for _, c := range [][2]string{{"GET", ""}, {"PATCH", ""}, {"DELETE", ""}, {"POST", "/run"}} {
		var body any
		if c[0] == "PATCH" {
			body = map[string]any{"description": "x"}
		}
		if _, err := a.must(404, c[0], "/api/v1/tasks/"+taskA+c[1], keyB, body); err != nil {
			return fmt.Errorf("B touching A's task: %w", err)
		}
	}
	// Disabled applications are refused; revoked keys are rejected.
	if _, err := a.must(200, "PATCH", "/api/v1/applications/"+appB, a.admin, map[string]any{"enabled": false}); err != nil {
		return err
	}
	if _, err := a.must(403, "GET", "/api/v1/meta", keyB, nil); err != nil {
		return err
	}
	keys, err := a.must(200, "GET", "/api/v1/applications/"+appB+"/keys", a.admin, nil)
	if err != nil {
		return err
	}
	kid := keys["keys"].([]any)[0].(map[string]any)["id"].(string)
	if _, err := a.must(200, "PATCH", "/api/v1/applications/"+appB, a.admin, map[string]any{"enabled": true}); err != nil {
		return err
	}
	if _, err := a.must(204, "DELETE", "/api/v1/applications/"+appB+"/keys/"+kid, a.admin, nil); err != nil {
		return err
	}
	_, err = a.must(401, "GET", "/api/v1/meta", keyB, nil)
	return err
}

func (a *acc) contract() error {
	_, key, err := a.createApp("contract")
	if err != nil {
		return err
	}
	id, err := a.createTask(key, map[string]any{"name": "contract", "enabled": false, "runner": map[string]any{"type": "http", "config": map[string]any{"url": "https://example.com"}}})
	if err != nil {
		return err
	}
	// A retried create of the same name is a conflict, not a duplicate.
	if _, err := a.must(409, "POST", "/api/v1/tasks", key, map[string]any{"name": "contract", "runner": map[string]any{"type": "http", "config": map[string]any{"url": "https://example.com"}}}); err != nil {
		return err
	}
	idem := "acceptance-" + a.run
	first, err := a.must(202, "POST", "/api/v1/tasks/"+id+"/run", key, nil, "Idempotency-Key", idem)
	if err != nil {
		return err
	}
	again, err := a.call("POST", "/api/v1/tasks/"+id+"/run", key, nil, "Idempotency-Key", idem)
	if err != nil || again.code != 200 || again.hdr.Get("Idempotent-Replayed") != "true" || again.body["id"] != first["id"] {
		return fmt.Errorf("idempotent replay: %d %v", again.code, again.body)
	}
	spec, err := a.must(200, "GET", "/api/v1/openapi.json", "", nil)
	if err != nil {
		return err
	}
	paths, _ := spec["paths"].(map[string]any)
	for _, p := range []string{"/tasks", "/tasks/{id}/run", "/executions", "/applications", "/webhooks", "/stats"} {
		if _, ok := paths[p]; !ok {
			return fmt.Errorf("openapi.json lacks %s", p)
		}
	}
	return nil
}

func (a *acc) security() error {
	for _, p := range []string{"/api/v1/tasks", "/api/v1/executions", "/api/v1/applications", "/api/v1/webhooks", "/api/v1/stats"} {
		if _, err := a.must(401, "GET", p, "", nil); err != nil {
			return fmt.Errorf("unauthenticated: %w", err)
		}
	}
	r, err := a.call("GET", "/api/v1/meta", a.admin, nil)
	if err != nil {
		return err
	}
	if r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("X-Frame-Options") != "DENY" {
		return fmt.Errorf("security headers missing: %v", r.hdr)
	}
	if _, err := a.must(413, "POST", "/api/v1/tasks", a.admin, `{"name":"`+strings.Repeat("x", 2<<20)+`"}`); err != nil {
		return fmt.Errorf("body size cap: %w", err)
	}
	_, key, err := a.createApp("security")
	if err != nil {
		return err
	}
	if _, err := a.must(422, "POST", "/api/v1/tasks", key, map[string]any{"name": "sh", "runner": map[string]any{"type": "shell", "config": map[string]any{"command": "id"}}}); err != nil {
		return fmt.Errorf("application shell task: %w", err)
	}
	// An application's task aimed at this machine (a private address) is blocked.
	id, err := a.createTask(key, map[string]any{"name": "probe", "runner": map[string]any{"type": "http", "config": map[string]any{"url": a.cb.url("/task")}}})
	if err != nil {
		return err
	}
	exe, err := a.must(202, "POST", "/api/v1/tasks/"+id+"/run", key, nil)
	if err != nil {
		return err
	}
	done, err := a.waitExecution(key, exe["id"].(string), 20*time.Second)
	if err != nil {
		return err
	}
	if kind, _ := done["error"].(map[string]any)["kind"].(string); done["status"] != "failed" || kind != "blocked" {
		if !isPrivateHost(a.cb.host) {
			return fmt.Errorf("%w (callback host %s is not private)", errSkip, a.cb.host)
		}
		return fmt.Errorf("application task to %s was not blocked: %v", a.cb.host, done)
	}
	return nil
}

func (a *acc) onTime() error {
	// Webhook endpoints first, so they hear this run: one healthy, one that fails once.
	hook, err := a.must(201, "POST", "/api/v1/webhooks", a.admin, map[string]any{"url": a.cb.url("/hook"), "events": []string{"execution.started", "execution.succeeded"}})
	if err != nil {
		return err
	}
	a.hookID, a.hookSecret = hook["id"].(string), hook["secret"].(string)
	a.cleanups = append(a.cleanups, func() { _, _ = a.call("DELETE", "/api/v1/webhooks/"+a.hookID, a.admin, nil) })
	flaky, err := a.must(201, "POST", "/api/v1/webhooks", a.admin, map[string]any{"url": a.cb.url("/flaky-hook"), "events": []string{"execution.succeeded"}})
	if err != nil {
		return err
	}
	a.flakyHookID = flaky["id"].(string)
	a.cleanups = append(a.cleanups, func() { _, _ = a.call("DELETE", "/api/v1/webhooks/"+a.flakyHookID, a.admin, nil) })

	id, err := a.createTask(a.admin, map[string]any{
		"name":     "acceptance-" + a.run + "-every-minute",
		"runner":   map[string]any{"type": "http", "config": map[string]any{"url": a.cb.url("/task")}},
		"schedule": map[string]any{"cron": "* * * * *"},
	})
	if err != nil {
		return err
	}
	a.scheduledTask = id
	fmt.Println("         (waiting for the next minute…)")
	deadline := time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		page, err := a.must(200, "GET", "/api/v1/executions?task_id="+id+"&status=succeeded", a.admin, nil)
		if err != nil {
			return err
		}
		if list := page["executions"].([]any); len(list) > 0 {
			e := list[len(list)-1].(map[string]any)
			a.scheduledExe = e
			sched, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(e["scheduled_for"]))
			started, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(e["started_at"]))
			if lag := started.Sub(sched); lag > 5*time.Second || lag < 0 {
				return fmt.Errorf("started %s after its scheduled time", lag)
			}
			_, _ = a.must(200, "POST", "/api/v1/tasks/"+id+"/disable", a.admin, nil)
			return nil
		}
		time.Sleep(time.Second)
	}
	return errors.New("no scheduled run succeeded within 75 seconds")
}

func (a *acc) history() error {
	e := a.scheduledExe
	if e == nil {
		return fmt.Errorf("%w (point 5 produced no run)", errSkip)
	}
	out, _ := e["output"].(map[string]any)
	if e["trigger"] != "scheduled" || e["duration_ms"] == nil || out["status_code"] != float64(200) || out["body"] != "task ok" {
		return fmt.Errorf("recorded run lacks status code, duration or output: %v", e)
	}
	id := e["id"].(string)
	since := url(time.Now().Add(-10 * time.Minute))
	for _, q := range []string{"task_id=" + a.scheduledTask, "status=succeeded", "application_id=app_clockwork", "since=" + since} {
		page, err := a.must(200, "GET", "/api/v1/executions?limit=200&"+q, a.admin, nil)
		if err != nil {
			return err
		}
		if !containsID(page["executions"], id) {
			return fmt.Errorf("filter %s does not find the run", q)
		}
	}
	page, err := a.must(200, "GET", "/api/v1/executions?until="+url(time.Now().Add(-time.Hour)), a.admin, nil)
	if err != nil {
		return err
	}
	if containsID(page["executions"], id) {
		return errors.New("until filter includes a newer run")
	}
	st, err := a.must(200, "GET", "/api/v1/stats?window=1h", a.admin, nil)
	if err != nil {
		return err
	}
	if total, _ := st["executions"].(map[string]any)["total"].(float64); total < 1 {
		return fmt.Errorf("stats: %v", st)
	}
	return nil
}

func (a *acc) webhooks() error {
	if a.scheduledExe == nil {
		return fmt.Errorf("%w (point 5 produced no run)", errSkip)
	}
	exeID := a.scheduledExe["id"].(string)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got := a.cb.events(exeID)
		if got["execution.started"] && got["execution.succeeded"] {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if bad := a.cb.badSignatures(); bad > 0 {
		return fmt.Errorf("%d deliveries had an invalid signature", bad)
	}
	if got := a.cb.events(exeID); !got["execution.started"] || !got["execution.succeeded"] {
		return fmt.Errorf("signed events received for the run: %v", got)
	}
	// The flaky endpoint answered 500 once: the delivery must be retried (after 10s) and logged.
	deadline = time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		log, err := a.must(200, "GET", "/api/v1/webhooks/"+a.flakyHookID+"/deliveries", a.admin, nil)
		if err != nil {
			return err
		}
		for _, x := range log["deliveries"].([]any) {
			d := x.(map[string]any)
			if d["status"] == "delivered" && d["attempts"].(float64) >= 2 {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("the failed delivery was not retried and delivered within 40 seconds")
}

func (a *acc) waitExecution(token, id string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e, err := a.must(200, "GET", "/api/v1/executions/"+id, token, nil)
		if err != nil {
			return nil, err
		}
		if s := e["status"]; s != "queued" && s != "running" {
			return e, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("execution %s did not finish within %s", id, timeout)
}

// ---- callback server: task target and webhook receiver ----

type callback struct {
	host   string
	port   int
	secret func() string

	mu       sync.Mutex
	received map[string]map[string]bool // execution id -> event types with valid signatures
	bad      int
	flaky    map[string]int // delivery id -> attempts seen
}

func (c *callback) url(path string) string {
	return fmt.Sprintf("http://%s:%d%s", c.host, c.port, path)
}

func (a *acc) startCallback(host string) error {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	c := &callback{host: host, port: ln.Addr().(*net.TCPAddr).Port, received: map[string]map[string]bool{}, flaky: map[string]int{},
		secret: func() string { return a.hookSecret }}
	mux := http.NewServeMux()
	mux.HandleFunc("/task", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "task ok") })
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := verify(c.secret(), r.Header.Get("Clockwork-Signature"), body, 5*time.Minute); err != nil {
			c.bad++
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Execution struct {
					ID string `json:"id"`
				} `json:"execution"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &ev)
		if c.received[ev.Data.Execution.ID] == nil {
			c.received[ev.Data.Execution.ID] = map[string]bool{}
		}
		c.received[ev.Data.Execution.ID][ev.Type] = true
	})
	mux.HandleFunc("/flaky-hook", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		id := r.Header.Get("Clockwork-Delivery")
		c.flaky[id]++
		if c.flaky[id] == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	go func() { _ = http.Serve(ln, mux) }()
	a.cb = c
	return nil
}

func (c *callback) events(exeID string) map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for k, v := range c.received[exeID] {
		out[k] = v
	}
	return out
}

func (c *callback) badSignatures() int { c.mu.Lock(); defer c.mu.Unlock(); return c.bad }

// verify is the receiver-side check from examples/webhook-receiver.
func verify(secret, header string, body []byte, tolerance time.Duration) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("malformed signature")
	}
	if age := time.Since(time.Unix(sec, 0)); age > tolerance || age < -tolerance {
		return errors.New("stale signature")
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	want := hex.EncodeToString(m.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

// ---- small helpers ----

func containsID(list any, id string) bool {
	items, _ := list.([]any)
	for _, x := range items {
		if m, _ := x.(map[string]any); m["id"] == id {
			return true
		}
	}
	return false
}

func isPrivateHost(host string) bool {
	ips, err := net.LookupIP(host)
	if err != nil {
		return host == "host.docker.internal" // resolvable inside the container even if not here
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func url(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
