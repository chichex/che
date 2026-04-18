// Package harness provides the e2e test infrastructure for che.
//
// Every test starts with harness.New(t), which creates:
//   - An isolated HOME and working directory under t.TempDir().
//   - A dir of symlinks (gh, claude, codex, gemini, git) pointing to the
//     pre-built chefake binary.
//   - A scriptDir where per-bin matcher scripts and the invocations log live.
//   - A PATH that prepends the symlinks dir, so che's subprocess calls hit the
//     fakes instead of whatever is on the developer machine.
//
// Tests script responses via ExpectGh / ExpectAgent / ExpectGit, then invoke
// che with Run / MustRun / MustFail and assert on stdout, exit, and the
// captured invocations via Invocations().
package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeIdentities are the external binaries che may shell out to. The harness
// creates a symlink per identity in the fake bin dir.
var fakeIdentities = []string{"gh", "claude", "codex", "gemini", "git", "brew"}

// Env is the per-test sandbox.
type Env struct {
	t         *testing.T
	HomeDir   string
	RepoDir   string
	FakeBin   string // dir containing symlinks for each fake identity
	ScriptDir string // per-bin script files + invocations log

	envOverrides map[string]string // extra env vars injected into che invocations
	cleanup      []func()
}

// New builds a fresh sandbox. It calls t.Fatal on any setup failure.
func New(t *testing.T) *Env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("che e2e tests do not support Windows (symlinks + fakes)")
	}
	checkTestMainDidBuild(t)

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	fakeBin := filepath.Join(tmp, "fakebin")
	scriptDir := filepath.Join(tmp, "scripts")

	for _, d := range []string{home, repo, fakeBin, scriptDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("harness: mkdir %s: %v", d, err)
		}
	}

	for _, id := range fakeIdentities {
		link := filepath.Join(fakeBin, id)
		if err := os.Symlink(fakePath(), link); err != nil {
			t.Fatalf("harness: symlink %s: %v", link, err)
		}
	}

	e := &Env{
		t:            t,
		HomeDir:      home,
		RepoDir:      repo,
		FakeBin:      fakeBin,
		ScriptDir:    scriptDir,
		envOverrides: map[string]string{},
	}
	t.Cleanup(func() {
		for _, fn := range e.cleanup {
			fn()
		}
	})
	return e
}

// SetEnv injects an env var into every subsequent che invocation.
func (e *Env) SetEnv(key, val string) {
	e.envOverrides[key] = val
}

// WithInstallPath is sugar over SetEnv for the upgrade flow.
func (e *Env) WithInstallPath(path string) {
	e.SetEnv("CHE_UPGRADE_TARGET_PATH", path)
}

// registerCleanup stores a teardown fn to run when the test ends.
func (e *Env) registerCleanup(fn func()) {
	e.cleanup = append(e.cleanup, fn)
}

// RemoveFake deletes the symlink for the given identity so the binary is no
// longer on PATH. Used by tests that simulate a missing external tool.
func (e *Env) RemoveFake(identity string) {
	e.t.Helper()
	link := filepath.Join(e.FakeBin, identity)
	if err := os.Remove(link); err != nil {
		e.t.Fatalf("harness: remove fake %s: %v", identity, err)
	}
}
