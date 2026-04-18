package harness

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
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
