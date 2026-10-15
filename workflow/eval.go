package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultEvalTimeoutSeconds bounds a hook run when none is declared.
	DefaultEvalTimeoutSeconds = 60
	// MaxEvalTimeoutSeconds caps a declared timeout.
	MaxEvalTimeoutSeconds = 600
	// MaxEvalOutputBytes truncates captured output; eval logs belong in
	// artifacts (component 7), not in the transition response.
	MaxEvalOutputBytes = 32 * 1024
)

// EvalResult is the outcome of one hook run. The store persists it (see
// store.EvalRun); component 7's artifact storage will adopt these rows.
type EvalResult struct {
	HookName  string
	Command   string
	Passed    bool
	ExitCode  int
	TimedOut  bool
	Output    string // truncated to MaxEvalOutputBytes
	Duration  time.Duration
	Truncated bool
}

// EvalContext identifies what is being gated.
type EvalContext struct {
	WorkItemKey string
	From, To    string
}

// RunEvalHook executes the hook's command with a timeout and returns the
// result. It never returns a non-nil error for hook failure itself: a
// failing check is a valid EvalResult{Passed:false}. Errors are reserved
// for "could not run the check at all" (e.g. empty command).
//
// SECURITY: the command runs as the API server's OS user with no sandbox.
// Workflow definitions are admin-controlled; treat the workflow admin API
// accordingly and sandbox the server process in production.
func RunEvalHook(ctx context.Context, hook EvalHook, ectx EvalContext) (EvalResult, error) {
	if err := hook.Validate(); err != nil {
		return EvalResult{}, err
	}
	timeout := time.Duration(hook.Timeout()) * time.Second
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(rctx, "sh", "-c", hook.Command)
	// Own process group so the timeout kill below reaches the whole tree:
	// "sh -c" spawns children that would otherwise outlive the shell and
	// hold our output pipes open, hanging cmd.Wait().
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{
		"FORGE_WORK_ITEM_KEY=" + ectx.WorkItemKey,
		"FORGE_TRANSITION=" + ectx.From + "->" + ectx.To,
		"FORGE_HOOK_NAME=" + hook.Name,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, n: MaxEvalOutputBytes}
	cmd.Stdout = lw
	cmd.Stderr = lw
	runErr := cmd.Run()
	duration := time.Since(start)

	res := EvalResult{
		HookName:  hook.Name,
		Command:   hook.Command,
		Duration:  duration,
		Output:    buf.String(),
		Truncated: lw.truncated,
	}
	if rctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return EvalResult{}, fmt.Errorf("workflow: eval %q: %w", hook.Name, runErr)
	}
	res.Passed = true
	return res, nil
}

// limitedWriter caps total bytes written, flagging truncation.
// os/exec copies stdout and stderr from separate goroutines, so the
// check-and-write must hold a mutex: a torn remaining computation makes
// Write report n < len(p), which io.Copy turns into a "short write" that
// kills the reader side and SIGPIPEs the child.
type limitedWriter struct {
	mu        sync.Mutex
	w         *bytes.Buffer
	n         int
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Always report the full length consumed: io.Copy treats n < len(p)
	// as ErrShortWrite, which would abort the copy and SIGPIPE the child.
	orig := len(p)
	remaining := l.n - l.w.Len()
	if remaining <= 0 {
		l.truncated = true
		return orig, nil // discard, report success
	}
	if len(p) > remaining {
		l.truncated = true
		p = p[:remaining]
	}
	l.w.Write(p)
	return orig, nil
}
