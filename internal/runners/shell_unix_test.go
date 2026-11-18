//go:build unix

package runners

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Security: a background child does not outlive a run that exits normally.
func TestShellBackgroundChildIsKilledAfterExit(t *testing.T) {
	requireSh(t)
	out, err := (&Shell{}).Run(context.Background(), shellReq(t, map[string]any{"command": "sleep 60 >/dev/null 2>&1 & echo $!"}))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(decodeShell(t, out).Stdout))
	if err != nil {
		t.Fatalf("no pid in output: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d still running after the run ended", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
