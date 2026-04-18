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
var fakeIdentities = []string{"gh", "claude", "codex", "gemini", "git"}

// Env is the per-test sandbox.
type Env struct {
	t         *testing.T
	HomeDir   string
	RepoDir   string
	FakeBin   string // dir containing symlinks for each fake identity
	ScriptDir string // per-bin script files + invocations log
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

	return &Env{
		t:         t,
		HomeDir:   home,
		RepoDir:   repo,
		FakeBin:   fakeBin,
		ScriptDir: scriptDir,
	}
}
