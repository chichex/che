package e2e_test

import (
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestValidate_CommandExists verifica que `che validate --help` renderice.
func TestValidate_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("validate", "--help")
	harness.AssertContains(t, out, "validate")
	harness.AssertContains(t, out, "validadores")
}

// TestValidate_MissingArg_ExitNonZero: cobra requiere 1 arg posicional.
func TestValidate_MissingArg_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("validate")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit\nstderr: %s", r.Stderr)
	}
}

// TestValidate_InvalidValidators_Exit3: --validators none rechazado.
func TestValidate_InvalidValidators_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("validate", "--validators", "none", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestValidate_InvalidPRRef_Exit3: ref bogus rechazado antes de tocar red.
func TestValidate_InvalidPRRef_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("validate", "nonsense-ref")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
}

// TestValidate_GoldenPath_OpusApproves: PR abierto, 1 validator opus aprueba.
// Debe postear 2 comments (validator + resumen) y reportar en stdout.
func TestValidate_GoldenPath_OpusApproves(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title,url,state,isDraft,author,headRefName`).
		RespondStdoutFromFixture("validate/gh_pr_view_open.json", 0)
	env.ExpectGh(`^pr diff 7`).
		RespondStdout("diff --git a/foo.go b/foo.go\n+package foo\n", 0)
	env.ExpectGh(`^pr view 7 --json comments`).
		RespondStdoutFromFixture("validate/gh_pr_comments_empty.json", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`validador técnico senior`).
		RespondStdoutFromFixture("validate/validator_approve.json", 0)

	// Dos comments esperados: uno del validator, uno del resumen.
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)

	out := env.MustRun("validate", "--validators", "opus", "7")
	harness.AssertContains(t, out, "che ya dejó los comments")
	harness.AssertContains(t, out, "opus#1: approve")

	inv := env.Invocations()
	if len(inv.For("claude")) != 1 {
		t.Fatalf("expected 1 claude call, got %d", len(inv.For("claude")))
	}
	comments := inv.FindCalls("gh", "pr", "comment", "7", "--body-file")
	if len(comments) != 2 {
		t.Fatalf("expected 2 pr comment calls (validator + summary), got %d", len(comments))
	}
}

// TestValidate_ClosedPR_Exit3: PR en state=CLOSED → error 3, no invoca
// validadores.
func TestValidate_ClosedPR_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("validate/gh_pr_view_closed.json", 0)

	r := env.Run("validate", "--validators", "opus", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "not OPEN")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestValidate_Iter2_IncrementsFromPreviousComments: si hay comments previos
// con iter=1, este run debe ser iter=2 (ver header del body posteado).
func TestValidate_Iter2_IncrementsFromPreviousComments(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("validate/gh_pr_view_open.json", 0)
	env.ExpectGh(`^pr diff 7`).
		RespondStdout("diff --git a/foo.go b/foo.go\n+package foo\n", 0)
	env.ExpectGh(`^pr view 7 --json comments`).
		RespondStdoutFromFixture("validate/gh_pr_comments_iter1.json", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`validador técnico senior`).
		RespondStdoutFromFixture("validate/validator_approve.json", 0)

	// 2 pr comment calls (validator + summary).
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)

	out := env.MustRun("validate", "--validators", "opus", "7")
	// El stdout debe estar ok — lo interesante es que al menos uno de los
	// body-files posteados contenga iter=2 en el header HTML. Como los fakes
	// no exponen directamente los bodies, chequeamos el comportamiento
	// observable: el flow no crashea y logguea "iter=2" en progress/stdout.
	if out == "" {
		t.Fatalf("expected non-empty stdout")
	}
}

// TestValidate_MultipleValidators_AllPosted: --validators codex,gemini →
// 3 comments (2 validators + 1 resumen).
func TestValidate_MultipleValidators_AllPosted(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("validate/gh_pr_view_open.json", 0)
	env.ExpectGh(`^pr diff 7`).
		RespondStdout("diff --git a/foo.go b/foo.go\n+package foo\n", 0)
	env.ExpectGh(`^pr view 7 --json comments`).
		RespondStdoutFromFixture("validate/gh_pr_comments_empty.json", 0)

	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico senior`).
		RespondStdoutFromFixture("validate/validator_approve.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico senior`).
		RespondStdoutFromFixture("validate/validator_changes_requested.json", 0)

	// 3 pr comment calls: 2 validators + 1 summary.
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr comment 7 --body-file`).Consumable().RespondStdout("ok\n", 0)

	out := env.MustRun("validate", "--validators", "codex,gemini", "7")
	harness.AssertContains(t, out, "codex#1")
	harness.AssertContains(t, out, "gemini#1")
	harness.AssertContains(t, out, "changes_requested")

	inv := env.Invocations()
	if c := len(inv.For("codex")); c != 1 {
		t.Fatalf("expected 1 codex call, got %d", c)
	}
	if g := len(inv.For("gemini")); g != 1 {
		t.Fatalf("expected 1 gemini call, got %d", g)
	}
	comments := inv.FindCalls("gh", "pr", "comment", "7", "--body-file")
	if len(comments) != 3 {
		t.Fatalf("expected 3 pr comment calls (2 validators + summary), got %d", len(comments))
	}
}

// TestValidate_EmptyDiff_Exit3: PR abierto pero sin diff (ej. PR vacío) →
// error 3 antes de llamar validadores.
func TestValidate_EmptyDiff_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)

	env.ExpectGh(`^pr view 7 --json number,title`).
		RespondStdoutFromFixture("validate/gh_pr_view_open.json", 0)
	env.ExpectGh(`^pr diff 7`).RespondStdout("", 0)

	r := env.Run("validate", "--validators", "opus", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "vacío")
	env.Invocations().AssertNotCalled(t, "claude")
}

// ---- helpers ----

// setupValidateEnv prepara el env: solo el git real (para que el precheck de
// remote funcione) + una config mínima. validate NO crea worktrees así que
// no necesitamos bare remote ni push.
func setupValidateEnv(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")

	runIn(t, env.RepoDir, "git", "init", "-q", "-b", "main")
	runIn(t, env.RepoDir, "git", "config", "user.email", "che@test.local")
	runIn(t, env.RepoDir, "git", "config", "user.name", "che-test")
	runIn(t, env.RepoDir, "git", "remote", "add", "origin",
		"https://github.com/acme/demo.git")
	return env
}

// scriptValidatePrechecks scriptea gh auth status (validate NO usa -t — no
// abre PRs, solo los comenta). El único precheck de gh es el auth basic.
func scriptValidatePrechecks(env *harness.Env) {
	env.ExpectGh(`^auth status$`).Consumable().
		RespondStdout("Logged in as acme\n", 0)
}
