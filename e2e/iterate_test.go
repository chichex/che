package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestIterate_CommandExists verifica que `che iterate --help` renderice.
func TestIterate_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("iterate", "--help")
	harness.AssertContains(t, out, "iterate")
	harness.AssertContains(t, out, "findings")
}

// TestIterate_MissingArg_ExitNonZero: cobra requiere 1 arg.
func TestIterate_MissingArg_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("iterate")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit\nstderr: %s", r.Stderr)
	}
}

// TestIterate_InvalidPRRef_Exit3.
func TestIterate_InvalidPRRef_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("iterate", "nonsense-ref")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestIterate_NoFindings_Exit3: PR sin comments de validate → exit 3 con
// mensaje claro (no hay nada que iterar).
func TestIterate_NoFindings_Exit3(t *testing.T) {
	env := setupIterateEnv(t)
	scriptIteratePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("iterate/gh_pr_view_changes_requested.json", 0)
	env.ExpectGh(`^pr view 7 --json comments`).
		RespondStdoutFromFixture("iterate/gh_pr_comments_empty.json", 0)

	r := env.Run("iterate", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "no tiene findings")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestIterate_AgentNoChanges_Exit2: opus no produce commits → exit 2 y
// el label validated:changes-requested se queda (no mentir al próximo
// validador).
func TestIterate_AgentNoChanges_Exit2(t *testing.T) {
	env := setupIterateEnv(t)
	scriptIteratePrechecks(env)
	env.SetEnv("CHE_ITERATE_SKIP_FETCH", "1")
	runIn(t, env.RepoDir, "git", "branch", "feat/x")

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("iterate/gh_pr_view_changes_requested.json", 0)
	env.ExpectGh(`^pr view 7 --json comments`).
		RespondStdoutFromFixture("iterate/gh_pr_comments_with_findings.json", 0)

	// Opus corre pero no toca nada.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdout("no hay mucho que arreglar\n", 0)

	r := env.Run("iterate", "7")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "no produjo cambios")

	inv := env.Invocations()
	// NO debe haber intentado remover el label (solo se remueve si hubo push).
	if rm := inv.FindCalls("gh", "pr edit", "--remove-label"); len(rm) != 0 {
		t.Fatalf("expected 0 remove-label calls when no changes, got %d", len(rm))
	}
	// NO debe haber posteado comment de iterate.
	if cm := inv.FindCalls("gh", "pr comment 7"); len(cm) != 0 {
		t.Fatalf("expected 0 pr comment calls when no changes, got %d", len(cm))
	}
}

// ---- helpers ----

// setupIterateEnv prepara un repo git real + remote bare para que el
// push del iterate flow funcione sin red.
func setupIterateEnv(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")

	runIn(t, env.RepoDir, "git", "init", "-q", "-b", "main")
	runIn(t, env.RepoDir, "git", "config", "user.email", "che@test.local")
	runIn(t, env.RepoDir, "git", "config", "user.name", "che-test")
	runIn(t, env.RepoDir, "git", "remote", "add", "origin",
		"https://github.com/acme/demo.git")
	if err := os.WriteFile(filepath.Join(env.RepoDir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runIn(t, env.RepoDir, "git", "add", "README.md")
	runIn(t, env.RepoDir, "git", "commit", "-q", "-m", "initial")
	return env
}

func scriptIteratePrechecks(env *harness.Env) {
	env.ExpectGh(`^auth status$`).Consumable().
		RespondStdout("Logged in as acme\n", 0)
}
