package runners

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func shellReq(t *testing.T, cfg map[string]any) Request {
	t.Helper()
	b, _ := json.Marshal(cfg)
	return Request{Config: b, ExecutionID: "exe_9", TaskID: "tsk_9", Attempt: 1}
}

func decodeShell(t *testing.T, raw json.RawMessage) ShellOutput {
	t.Helper()
	var o ShellOutput
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("output %s: %v", raw, err)
	}
	return o
}

func TestShellCapturesOutputAndOnlyTaskEnv(t *testing.T) {
	requireSh(t)
	t.Setenv("CLOCKWORK_ADMIN_TOKEN", "must-not-leak")
	out, err := (&Shell{}).Run(context.Background(), shellReq(t, map[string]any{
		"command": `echo "greeting=$GREETING id=$CLOCKWORK_EXECUTION_ID task=$CLOCKWORK_TASK_ID attempt=$CLOCKWORK_ATTEMPT admin=[$CLOCKWORK_ADMIN_TOKEN]"; echo oops >&2`,
		"env":     map[string]string{"GREETING": "hi"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	o := decodeShell(t, out)
	if o.ExitCode != 0 || strings.TrimSpace(o.Stdout) != "greeting=hi id=exe_9 task=tsk_9 attempt=1 admin=[]" || strings.TrimSpace(o.Stderr) != "oops" {
		t.Fatalf("output: %+v", o)
	}
}

func TestShellNonZeroExitIsRetryable(t *testing.T) {
	requireSh(t)
	out, err := (&Shell{}).Run(context.Background(), shellReq(t, map[string]any{"command": "echo partial; exit 3"}))
	wantFailure(t, err, KindExitCode, true)
	if o := decodeShell(t, out); o.ExitCode != 3 || strings.TrimSpace(o.Stdout) != "partial" {
		t.Fatalf("output: %+v", o)
	}
}

func TestShellTimeoutKillsCommand(t *testing.T) {
	requireSh(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&Shell{}).Run(ctx, shellReq(t, map[string]any{"command": "sleep 30"}))
	wantFailure(t, err, KindTimeout, true)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v: command was not killed", d)
	}
}

func TestShellOutputIsBounded(t *testing.T) {
	requireSh(t)
	out, err := (&Shell{}).Run(context.Background(), shellReq(t, map[string]any{
		"command": "i=0; while [ $i -lt 2000 ]; do echo 0123456789abcdef; i=$((i+1)); done",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if o := decodeShell(t, out); len(o.Stdout) != MaxStreamExcerpt || !o.StdoutTruncated {
		t.Fatalf("stdout len=%d truncated=%v", len(o.Stdout), o.StdoutTruncated)
	}
}

func TestShellBadWorkingDirIsConfigFailure(t *testing.T) {
	requireSh(t)
	_, err := (&Shell{}).Run(context.Background(), shellReq(t, map[string]any{"command": "true", "working_dir": "/definitely/not/here"}))
	wantFailure(t, err, KindConfig, false)
}
