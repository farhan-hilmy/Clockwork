package runners

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/farhan-hilmy/clockwork/internal/tasks"
)

// MaxStreamExcerpt is how much of stdout and of stderr is stored.
const MaxStreamExcerpt = 16 << 10

// Shell runs a task's command with `sh -c`.
//
// The process gets only the task's environment variables, plus PATH (inherited
// from Clockwork unless the task sets it) and CLOCKWORK_EXECUTION_ID,
// CLOCKWORK_TASK_ID and CLOCKWORK_ATTEMPT. On timeout the whole process group
// is killed, so background children do not outlive the run.
type Shell struct {
	// Shell is the interpreter. Defaults to "sh".
	Shell string
}

// ShellOutput is stored as executions.output for shell tasks.
type ShellOutput struct {
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	Stderr          string `json:"stderr"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

func (s *Shell) Run(ctx context.Context, req Request) (json.RawMessage, error) {
	var c tasks.ShellConfig
	if err := json.Unmarshal(req.Config, &c); err != nil {
		return nil, fail(KindConfig, false, "invalid shell config: %v", err)
	}
	sh := s.Shell
	if sh == "" {
		sh = "sh"
	}
	path, err := exec.LookPath(sh)
	if err != nil {
		return nil, fail(KindConfig, false, "shell %q not found on this server", sh)
	}

	cmd := exec.CommandContext(ctx, path, "-c", c.Command)
	cmd.Dir = c.WorkingDir
	cmd.Env = shellEnv(c.Env, req)
	stdout, stderr := newExcerpt(MaxStreamExcerpt), newExcerpt(MaxStreamExcerpt)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd) // kill children too when ctx ends; see shell_unix.go
	// If a killed process's descendants keep the output pipes open, stop waiting after this.
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	if cmd.ProcessState == nil {
		// The process never started: bad working directory, permissions, …
		return nil, fail(KindConfig, false, "could not start command: %v", runErr)
	}
	out := ShellOutput{
		ExitCode:        cmd.ProcessState.ExitCode(),
		Stdout:          stdout.String(),
		StdoutTruncated: stdout.truncated(),
		Stderr:          stderr.String(),
		StderrTruncated: stderr.truncated(),
	}
	output := mustJSON(out)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return output, fail(KindTimeout, true, "task timeout exceeded; process group killed")
	case errors.Is(ctx.Err(), context.Canceled):
		return output, fail(KindInterrupted, false, "canceled while the command was running")
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return output, fail(KindExitCode, true, "command exited with status %d", out.ExitCode)
		}
		return output, fail(KindInternal, false, "%v", runErr)
	}
	return output, nil
}

func shellEnv(taskEnv map[string]string, req Request) []string {
	env := make([]string, 0, len(taskEnv)+5)
	if _, ok := taskEnv["PATH"]; !ok {
		env = append(env, "PATH="+os.Getenv("PATH"))
	}
	for _, k := range windowsEssentials {
		if _, ok := taskEnv[k]; !ok && os.Getenv(k) != "" {
			env = append(env, k+"="+os.Getenv(k))
		}
	}
	for k, v := range taskEnv {
		env = append(env, k+"="+v)
	}
	return append(env,
		"CLOCKWORK_EXECUTION_ID="+req.ExecutionID,
		"CLOCKWORK_TASK_ID="+req.TaskID,
		"CLOCKWORK_ATTEMPT="+strconv.Itoa(req.Attempt),
	)
}
