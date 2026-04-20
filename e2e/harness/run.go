package harness

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Result captures the outcome of a che invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Run executes che with the given args inside the isolated env. It never
// calls t.Fatal; use MustRun / MustFail for pass/fail semantics.
func (e *Env) Run(args ...string) Result {
	return e.RunWithStdin("", args...)
}

// RunWithStdin is like Run but pipes stdin into che's stdin.
func (e *Env) RunWithStdin(stdin string, args ...string) Result {
	e.t.Helper()
	cmd := exec.Command(chePathOrFail(e.t), args...)
	cmd.Dir = e.RepoDir
	cmd.Env = e.buildEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	return Result{
		Stdout:   out.String(),
		Stderr:   errOut.String(),
		ExitCode: exit,
		Err:      err,
	}
}

// MustRun fails the test if che exits non-zero, otherwise returns combined output.
func (e *Env) MustRun(args ...string) string {
	e.t.Helper()
	r := e.Run(args...)
	if r.ExitCode != 0 {
		e.t.Fatalf("che %s: expected exit 0, got %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), r.ExitCode, r.Stdout, r.Stderr)
	}
	return r.Stdout + r.Stderr
}

// MustFail fails the test if che exits zero, otherwise returns combined output.
func (e *Env) MustFail(args ...string) string {
	e.t.Helper()
	r := e.Run(args...)
	if r.ExitCode == 0 {
		e.t.Fatalf("che %s: expected non-zero exit, got 0\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), r.Stdout, r.Stderr)
	}
	return r.Stdout + r.Stderr
}

// buildEnv assembles the env for a che invocation: isolated HOME, PATH with
// the fake bin dir prepended, and the script dir pointer the fake reads.
//
// We deliberately drop inherited HOMEBREW_* and related vars so a developer
// machine with brew installed doesn't leak paths into the test process.
func (e *Env) buildEnv() []string {
	base := []string{
		"HOME=" + e.HomeDir,
		"PATH=" + e.FakeBin + ":" + minimalPath(),
		"CHE_FAKE_SCRIPT_DIR=" + e.ScriptDir,
	}
	if term := os.Getenv("TERM"); term != "" {
		base = append(base, "TERM="+term)
	}
	for k, v := range e.envOverrides {
		base = append(base, k+"="+v)
	}
	return base
}

// minimalPath returns a sanitized PATH that still lets exec.Command find
// things like /bin/sh if the production code ever needs them. We include
// /usr/bin and /bin; nothing else leaks in.
func minimalPath() string {
	return "/usr/bin:/bin"
}

// AsyncRun is a handle over a background che invocation. Used by signal
// handling tests that need to start che, wait for a sentinel in the output,
// send a signal, and assert on the final state.
type AsyncRun struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	exit   int
	err    error
}

// RunAsync starts che with the given args without blocking. The caller can
// send signals via SendSignal, wait for output via WaitForStdout, and block
// on exit via Wait. The child runs with Setpgid=true so SendSignal can
// reach the whole process group — needed because the test needs to signal
// che's grandchildren (the fake agent) as if they were a terminal group.
func (e *Env) RunAsync(args ...string) *AsyncRun {
	e.t.Helper()
	cmd := exec.Command(chePathOrFail(e.t), args...)
	cmd.Dir = e.RepoDir
	cmd.Env = e.buildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	a := &AsyncRun{cmd: cmd, done: make(chan struct{})}
	// Escritores thread-safe: el test puede leer stdout/stderr mientras el
	// proceso aún está vivo vía SnapshotStdout/Stderr.
	cmd.Stdout = &lockedWriter{mu: &a.mu, buf: &a.stdout}
	cmd.Stderr = &lockedWriter{mu: &a.mu, buf: &a.stderr}

	if err := cmd.Start(); err != nil {
		e.t.Fatalf("RunAsync: start che: %v", err)
	}
	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		a.err = err
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				a.exit = ee.ExitCode()
			} else {
				a.exit = -1
			}
		}
		a.mu.Unlock()
		close(a.done)
	}()
	return a
}

// Pid returns the PID of the child che process.
func (a *AsyncRun) Pid() int { return a.cmd.Process.Pid }

// SnapshotStdout returns a copy of the child's stdout captured so far.
// Safe to call while the process is still running.
func (a *AsyncRun) SnapshotStdout() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stdout.String()
}

// SnapshotStderr returns a copy of the child's stderr captured so far.
func (a *AsyncRun) SnapshotStderr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stderr.String()
}

// SendSignal delivers sig to the child process. Tests typically pass
// os.Interrupt or syscall.SIGTERM.
func (a *AsyncRun) SendSignal(sig os.Signal) error {
	return a.cmd.Process.Signal(sig)
}

// WaitForOutput blocks until the combined stdout+stderr contains substr or
// timeout expires. Returns true if found, false on timeout. Used to
// synchronize with the fake's READY sentinel before sending a signal.
func (a *AsyncRun) WaitForOutput(t *testing.T, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(a.SnapshotStdout()+a.SnapshotStderr(), substr) {
			return true
		}
		select {
		case <-a.done:
			// El proceso salió antes de ver el sentinel; chequeamos una
			// última vez por si output llegó justo antes del exit.
			return strings.Contains(a.SnapshotStdout()+a.SnapshotStderr(), substr)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// WaitForFile polls for the existence of path up to timeout. Used as a
// sentinel mechanism when che swallows the fake's stdout (the CLI path
// doesn't pipe agent stdout to the caller's terminal) — the fake writes a
// file via TouchFile and the test watches for it.
func (a *AsyncRun) WaitForFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-a.done:
			_, err := os.Stat(path)
			return err == nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// Wait blocks until the che process exits and returns the final Result.
// Subsequent calls return cached data. If the process doesn't exit within
// timeout, returns a result with ExitCode=-1 and Err set.
func (a *AsyncRun) Wait(t *testing.T, timeout time.Duration) Result {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(timeout):
		_ = a.cmd.Process.Kill()
		<-a.done
		a.mu.Lock()
		a.err = errTimeout
		a.mu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return Result{
		Stdout:   a.stdout.String(),
		Stderr:   a.stderr.String(),
		ExitCode: a.exit,
		Err:      a.err,
	}
}

// errTimeout is a sentinel error used by AsyncRun.Wait when the child does
// not exit within the caller's timeout.
var errTimeout = &waitTimeoutError{}

type waitTimeoutError struct{}

func (e *waitTimeoutError) Error() string { return "che did not exit within timeout" }

// lockedWriter serializes writes to an underlying bytes.Buffer so two
// goroutines (stdout+stderr pipes from os/exec) don't race on Write, and
// readers can snapshot mid-run without tearing a partial write.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
