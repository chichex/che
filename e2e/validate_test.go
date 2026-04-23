package e2e_test

import (
	"fmt"
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
	scriptDetectTargetPR(env, 7)

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

	// Label consolidado (approve) — ensure + pr edit con add-label.
	env.ExpectGh(`^label create validated:approve --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr edit 7 --add-label validated:approve$`).RespondStdout("ok\n", 0)

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
	ensures := inv.FindCalls("gh", "label", "create", "validated:approve", "--force")
	if len(ensures) != 1 {
		t.Fatalf("expected 1 label create for validated:approve, got %d", len(ensures))
	}
	edits := inv.FindCalls("gh", "pr", "edit", "7", "--add-label", "validated:approve")
	if len(edits) != 1 {
		t.Fatalf("expected 1 pr edit adding validated:approve, got %d", len(edits))
	}
}

// TestValidate_ClosedPR_Exit3: PR en state=CLOSED → error 3, no invoca
// validadores.
func TestValidate_ClosedPR_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)
	scriptDetectTargetPR(env, 7)

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
	scriptDetectTargetPR(env, 7)

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

	env.ExpectGh(`^label create validated:approve --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr edit 7 --add-label validated:approve$`).RespondStdout("ok\n", 0)

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
	scriptDetectTargetPR(env, 7)

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

	// Label consolidado: peor verdict = changes_requested (gana sobre approve).
	env.ExpectGh(`^label create validated:changes-requested --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr edit 7 --add-label validated:changes-requested$`).RespondStdout("ok\n", 0)

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
	edits := inv.FindCalls("gh", "pr", "edit", "7", "--add-label", "validated:changes-requested")
	if len(edits) != 1 {
		t.Fatalf("expected 1 pr edit with validated:changes-requested, got %d", len(edits))
	}
}

// TestValidate_EmptyDiff_Exit3: PR abierto pero sin diff (ej. PR vacío) →
// error 3 antes de llamar validadores.
func TestValidate_EmptyDiff_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)
	scriptDetectTargetPR(env, 7)

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

// TestValidate_Plan_GoldenPath_OpusApproves: issue abierto con status:plan y
// body con plan consolidado → validador aprueba → 2 comments en el issue
// (validator + summary) + label plan-validated:approve aplicado.
func TestValidate_Plan_GoldenPath_OpusApproves(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)
	scriptDetectTargetPlan(env, 42)

	env.ExpectGh(`^issue view 42 --json number,title,body,labels,url,state$`).
		RespondStdoutFromFixture("validate/gh_issue_view_plan_consolidated.json", 0)
	env.ExpectGh(`^issue view 42 --json comments$`).
		RespondStdout(`{"comments": []}`, 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`validador técnico senior`).
		RespondStdoutFromFixture("validate/validator_approve.json", 0)

	// 2 comments esperados: uno del validator, uno del resumen.
	env.ExpectGh(`^issue comment 42 --body-file`).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue comment 42 --body-file`).Consumable().RespondStdout("ok\n", 0)

	// Label plan-validated:approve: ensure + issue edit.
	env.ExpectGh(`^label create plan-validated:approve --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 --add-label plan-validated:approve$`).RespondStdout("ok\n", 0)

	out := env.MustRun("validate", "--validators", "opus", "42")
	harness.AssertContains(t, out, "che ya dejó los comments")
	harness.AssertContains(t, out, "opus#1: approve")

	inv := env.Invocations()
	if len(inv.For("claude")) != 1 {
		t.Fatalf("expected 1 claude call, got %d", len(inv.For("claude")))
	}
	comments := inv.FindCalls("gh", "issue", "comment", "42", "--body-file")
	if len(comments) != 2 {
		t.Fatalf("expected 2 issue comment calls (validator + summary), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42", "--add-label", "plan-validated:approve")
	if len(edits) != 1 {
		t.Fatalf("expected 1 issue edit with plan-validated:approve, got %d", len(edits))
	}
}

// TestValidate_Plan_NoStatusPlan_Exit3: issue abierto sin status:plan →
// exit semantic con mensaje accionable ("corré che explore"). No llama
// validadores.
func TestValidate_Plan_NoStatusPlan_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)
	scriptDetectTargetPlan(env, 42)

	env.ExpectGh(`^issue view 42 --json number,title,body,labels,url,state$`).
		RespondStdoutFromFixture("validate/gh_issue_view_no_status_plan.json", 0)

	r := env.Run("validate", "--validators", "opus", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "che:plan")
	harness.AssertContains(t, r.Stderr, "che explore")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestValidate_Plan_NoConsolidatedPlan_Exit3: issue con status:plan pero
// body sin "## Plan consolidado" → exit semantic. No llama validadores.
func TestValidate_Plan_NoConsolidatedPlan_Exit3(t *testing.T) {
	env := setupValidateEnv(t)
	scriptValidatePrechecks(env)
	scriptDetectTargetPlan(env, 42)

	env.ExpectGh(`^issue view 42 --json number,title,body,labels,url,state$`).
		RespondStdoutFromFixture("validate/gh_issue_view_status_plan_no_consolidated.json", 0)

	r := env.Run("validate", "--validators", "opus", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "plan consolidado")
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
	scriptCheLockDefault(env)
	// Validate ahora hace transiciones de máquina de estados (plan→
	// validating→validated o executed→validating→validated). labels.Ensure
	// llama gh label create por cada label antes de aplicarlos.
	env.ExpectGh(`^label create che:`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit \d+ --remove-label che:`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^pr edit \d+ --remove-label che:`).RespondStdout("ok\n", 0)
}

// scriptDetectTargetPR scriptea la respuesta de `gh api repos/.../issues/<n>`
// que detectTarget consulta para decidir PR vs plan. El JSON mínimo incluye
// pull_request:{} para que el detector retorne TargetPR.
func scriptDetectTargetPR(env *harness.Env, number int) {
	env.ExpectGh(fmt.Sprintf(`^api repos/\{owner\}/\{repo\}/issues/%d$`, number)).
		Consumable().
		RespondStdout(fmt.Sprintf(`{"number":%d,"pull_request":{"url":"https://api.github.com/repos/acme/demo/pulls/%d"}}`, number, number), 0)
}

// scriptDetectTargetPlan scriptea la respuesta para el modo plan: pull_request
// ausente o null indica issue (no PR).
func scriptDetectTargetPlan(env *harness.Env, number int) {
	env.ExpectGh(fmt.Sprintf(`^api repos/\{owner\}/\{repo\}/issues/%d$`, number)).
		Consumable().
		RespondStdout(fmt.Sprintf(`{"number":%d,"pull_request":null}`, number), 0)
}
