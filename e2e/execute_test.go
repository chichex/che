package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestExecute_CommandExists verifica que `che execute --help` renderice.
func TestExecute_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("execute", "--help")
	harness.AssertContains(t, out, "execute")
	harness.AssertContains(t, out, "Plan consolidado")
}

// TestExecute_MissingArg_ExitNonZero: cobra requiere 1 arg posicional.
func TestExecute_MissingArg_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("execute")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit\nstderr: %s", r.Stderr)
	}
}

// TestExecute_InvalidAgent_Exit3: --agent bogus rechazado antes de tocar red.
func TestExecute_InvalidAgent_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("execute", "--agent", "bogus", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_NoGitRepo_Exit2: preflight falla si no estamos en un repo git.
func TestExecute_NoGitRepo_Exit2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	// Sin init de git; el fake git devuelve error para rev-parse.
	env.ExpectGit(`^rev-parse --show-toplevel`).RespondExitWithError(128, "fatal: not a git repo\n")

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_GoldenPath: issue válido → worktree + claude + commit + push
// + PR create + label transitions + comment.
func TestExecute_GoldenPath(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)

	// No hay PR existente para la branch.
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	// Label transitions: plan → executing (1er edit), executing → executed (2do).
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	// El agente claude "escribe" un archivo en el worktree.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("IMPLEMENTATION.md", "did the thing\n").
		RespondStdout("ok\n", 0)

	// Crear PR.
	env.ExpectGh(`^pr create --draft`).
		RespondStdout("https://github.com/acme/demo/pull/7\n", 0)

	// Segunda edit (executing → executed) + comment final.
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue comment 42 --body-file`).
		RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-xyz\n", 0)

	out := env.MustRun("execute", "42")
	harness.AssertContains(t, out, "Executed")
	harness.AssertContains(t, out, "https://github.com/acme/demo/pull/7")

	inv := env.Invocations()
	if len(inv.For("claude")) != 1 {
		t.Fatalf("expected 1 claude call, got %d", len(inv.For("claude")))
	}
	prCreates := inv.FindCalls("gh", "pr", "create")
	if len(prCreates) != 1 {
		t.Fatalf("expected 1 gh pr create, got %d", len(prCreates))
	}
	prCreates[0].AssertArgsContain(t, "--draft", "--base", "main")

	// Transiciones REST: 1) plan→executing (DELETE che:plan + POST
	// che:executing). 2) executing→executed (DELETE che:executing + POST
	// che:executed). awaiting-human ya no se aplica (el gate vive en
	// plan-validated:* / validated:*).
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executing"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executing, got %d", len(got))
	}
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executed"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executed, got %d", len(got))
	}

	if comments := inv.FindCalls("gh", "issue", "comment", "42", "--body-file"); len(comments) != 1 {
		t.Fatalf("expected 1 issue comment, got %d", len(comments))
	}
}

// TestExecute_IssueNotStatusPlan_Exit3: issue sin che:idea, che:plan ni
// che:validated (avanzado más allá o nunca clasificado) → exit 3.
func TestExecute_IssueNotStatusPlan_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_not_plan.json", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "che:idea, che:plan ni che:validated")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_PlanValidatedChangesRequested_Exit3: issue con
// plan-validated:changes-requested (el validador del plan pidió cambios)
// → execute corta con ExitSemantic y sugiere `che iterate`.
func TestExecute_PlanValidatedChangesRequested_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_plan_validated_changes_requested.json", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "plan-validated:changes-requested")
	harness.AssertContains(t, r.Stderr, "che iterate")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_PlanValidatedNeedsHuman_Exit3: issue con
// plan-validated:needs-human → execute corta con ExitSemantic. El humano
// tiene que resolver a mano antes de ejecutar.
func TestExecute_PlanValidatedNeedsHuman_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_plan_validated_needs_human.json", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "plan-validated:needs-human")
	harness.AssertContains(t, r.Stderr, "resolvé a mano")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_IssueAlreadyExecuting_Exit3: otro run dejó status:executing → refuse.
func TestExecute_IssueAlreadyExecuting_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_executing.json", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "executing")
}

// TestExecute_Idempotency_UpdatesExistingPR: segundo run con PR abierto
// reutiliza ese PR en vez de crear uno nuevo.
func TestExecute_Idempotency_UpdatesExistingPR(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)

	// PR existente para la branch exec/42-*.
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout(
		`[{"url":"https://github.com/acme/demo/pull/7","number":7}]`, 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("IMPLEMENTATION_V2.md", "update\n").
		RespondStdout("ok\n", 0)

	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue comment 42 --body-file`).RespondStdout("ok\n", 0)

	out := env.MustRun("execute", "42")
	harness.AssertContains(t, out, "https://github.com/acme/demo/pull/7")

	inv := env.Invocations()
	if creates := inv.FindCalls("gh", "pr", "create"); len(creates) != 0 {
		t.Fatalf("expected 0 gh pr create on idempotent run, got %d", len(creates))
	}
}

// TestExecute_AgentFails_Rollback: claude falla → worktree limpio + label
// vuelve a status:plan. El defer re-fetchea el issue (ownership-aware) y ve
// que sigue con status:executing, entonces aplica el rollback.
func TestExecute_AgentFails_Rollback(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	// 1er issue view: previo al lock (status:plan). 2do: durante rollback
	// (status:executing — el lock sigue siendo nuestro, rollback procede).
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// 1er edit: lock; 2do edit: rollback.
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		RespondExitWithError(1, "claude exploded\n")

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	if creates := inv.FindCalls("gh", "pr", "create"); len(creates) > 0 {
		t.Fatalf("expected 0 gh pr create after agent failure, got %d", len(creates))
	}
	// Lock: DELETE che:plan + POST che:executing. Rollback: DELETE
	// che:executing + POST che:plan.
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executing"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executing (lock), got %d", len(got))
	}
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:plan"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:plan (rollback), got %d", len(got))
	}
}

// TestExecute_AgentFails_RollbackSkippedIfLockLost: claude falla y cuando
// el defer re-fetchea el issue, ya no tiene status:executing (otra
// instancia transitó). El rollback NO debe aplicar el label transition.
func TestExecute_AgentFails_RollbackSkippedIfLockLost(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	// 1er issue view: status:plan (para gate). 2do issue view (en rollback):
	// ya no tiene status:executing (otra instancia se lo robó).
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_no_longer_executing.json", 0)
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// Solo el 1er edit (lock) debería ocurrir; el 2do (rollback) NO.
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		RespondExitWithError(1, "claude exploded\n")

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	// Lock: 1 POST adding che:executing. Rollback NO debe ocurrir → 0
	// POSTs adding che:plan.
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executing"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executing (lock), got %d", len(got))
	}
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:plan"); len(got) != 0 {
		t.Fatalf("expected 0 POST adding che:plan (rollback skipped), got %d", len(got))
	}
	harness.AssertContains(t, r.Stderr, "rollback abortado")
}

// TestExecute_NoChanges_Rollback: claude termina sin tocar archivos → retry +
// rollback. El 2do issue view del rollback ve status:executing intacto.
func TestExecute_NoChanges_Rollback(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	// Claude no toca archivos.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		RespondStdout("hmm\n", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "no se generaron cambios")
	harness.AssertContains(t, r.Stderr, "no hay PR previo")
}

// TestExecute_NoChanges_ExistingPR_Rollback: claude no toca archivos pero
// hay un PR abierto; NO debe transicionar a executed ni refrescar el PR —
// rollback a status:plan y mensaje diferenciado.
func TestExecute_NoChanges_ExistingPR_Rollback(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)
	// PR abierto existente.
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout(
		`[{"url":"https://github.com/acme/demo/pull/7","number":7}]`, 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// Esperamos 2 edits: lock + rollback. NO debería haber un edit que
	// agregue status:executed.
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	// Claude no toca archivos.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		RespondStdout("noop\n", 0)

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "no se generaron cambios")
	harness.AssertContains(t, r.Stderr, "PR no actualizado")

	inv := env.Invocations()
	// No debe haber gh pr create (había uno existente igual).
	if creates := inv.FindCalls("gh", "pr", "create"); len(creates) != 0 {
		t.Fatalf("expected 0 gh pr create, got %d", len(creates))
	}
	// Ninguna transición debe agregar che:executed (POST con ese label).
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executed"); len(got) != 0 {
		t.Fatalf("rogue transition to che:executed: %v", got)
	}
}

// (placeholder eliminado: el filter por awaiting-human ya no existe)

// ---- helpers ----

// setupExecuteEnv prepara un repo git real en env.RepoDir + quita el fake de
// git (dejamos el git real del sistema para que worktree y commits funcionen).
// El env var CHE_EXEC_SKIP_FETCH=1 salta el `git fetch origin main` que
// ahora es obligatorio — los tests usan bare remotes locales sin red.
// CHE_BASE_BRANCH=main evita que DetectBaseBranch llame al fake gh para
// resolver `gh repo view --json defaultBranchRef`; los tests asumen main.
func setupExecuteEnv(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")
	env.SetEnv("CHE_EXEC_SKIP_FETCH", "1")
	env.SetEnv("CHE_BASE_BRANCH", "main")

	// init repo + commit inicial.
	runIn(t, env.RepoDir, "git", "init", "-q", "-b", "main")
	runIn(t, env.RepoDir, "git", "config", "user.email", "che@test.local")
	runIn(t, env.RepoDir, "git", "config", "user.name", "che-test")
	// Agregamos un origin con URL github.com (para pasar precheckGitHubRemote).
	// Como push a github.com no va a funcionar, seteamos un pushurl separado
	// que apunta a un bare local. `git remote get-url origin` devuelve el
	// URL de fetch (github.com) — lo que el precheck chequea — pero los
	// push/fetch reales van al bare sin necesidad de red. El fetch en
	// execute se hace best-effort (ignoramos errores de `git fetch origin
	// main`), así que no hace falta redirigir fetch.
	bare := filepath.Join(env.RepoDir, "..", "remote.git")
	runIn(t, "", "git", "init", "--bare", "-q", bare)
	fakeGHURL := "https://github.com/acme/demo.git"
	runIn(t, env.RepoDir, "git", "remote", "add", "origin", fakeGHURL)
	runIn(t, env.RepoDir, "git", "config", "remote.origin.pushurl", bare)
	// Push inicial del main inicial hacia el bare (usa pushurl).
	if err := os.WriteFile(filepath.Join(env.RepoDir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runIn(t, env.RepoDir, "git", "add", "README.md")
	runIn(t, env.RepoDir, "git", "commit", "-q", "-m", "initial")
	runIn(t, env.RepoDir, "git", "push", "-q", "origin", "main")

	return env
}

func runIn(t *testing.T, dir, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
}

// scriptExecutePrechecks scriptea los prechecks: gh auth status (auth) y gh
// auth status -t (scope check). Cuando se usa con un repo git real, NO hay
// que scriptear git — es el real del sistema.
func scriptExecutePrechecks(env *harness.Env) {
	// Primer call: auth status (sin -t). Chequeo de login.
	env.ExpectGh(`^auth status$`).Consumable().RespondStdout("Logged in as acme\n", 0)
	// Segundo call: auth status -t. Incluye scopes válidos.
	env.ExpectGh(`^auth status -t`).Consumable().RespondStdout(
		"github.com\n  - Token: gho_xxx\n  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'\n", 0)
	scriptCheLockDefault(env)
}

// TestExecute_PreviousPRMerged_CreatesNewPR: si hubo un PR previo para esta
// branch que ya fue mergeado a main, `findOpenPRForBranch` lo filtra (usa
// `--state open`) y el flow crea un PR nuevo contra main con los commits del
// re-run. Contrato: merged no bloquea; se abre uno nuevo. Si alguien sacara
// `--state open` del query, este test rompe porque encontraría el PR mergeado
// y dispararía el path de "reuse" en vez de crear.
func TestExecute_PreviousPRMerged_CreatesNewPR(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)

	// PR previo mergeado → `gh pr list --state open` devuelve lista vacía.
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("FOLLOWUP.md", "follow-up iteration\n").
		RespondStdout("ok\n", 0)

	// Se crea un PR nuevo — no reuse.
	env.ExpectGh(`^pr create --draft`).
		RespondStdout("https://github.com/acme/demo/pull/99\n", 0)

	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue comment 42 --body-file`).RespondStdout("ok\n", 0)

	out := env.MustRun("execute", "42")
	harness.AssertContains(t, out, "https://github.com/acme/demo/pull/99")

	inv := env.Invocations()
	// Contrato #1: la query siempre filtra por `--state open`.
	prLists := inv.FindCalls("gh", "pr", "list", "--head")
	if len(prLists) != 1 {
		t.Fatalf("expected 1 gh pr list call, got %d", len(prLists))
	}
	prLists[0].AssertArgsContain(t, "--state", "open")

	// Contrato #2: PR nuevo creado contra main (no se intentó reusar).
	creates := inv.FindCalls("gh", "pr", "create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 gh pr create, got %d", len(creates))
	}
	creates[0].AssertArgsContain(t, "--base", "main", "--draft")
}

// TestExecute_PreviousPRClosed_NoAutoReopen: si hubo un PR previo cerrado
// SIN merge, `gh pr list --state open` también devuelve []. El flow intenta
// `gh pr create`, que falla porque GitHub exige reopen explícito. El código
// actual NO reabre automáticamente — retorna exit 2 con rollback de label.
// El operador debe ejecutar `gh pr reopen <n>` a mano para reanudar.
//
// Si en el futuro se agrega auto-reopen, este test debe cambiarse para
// esperar exit 0 y que `gh pr reopen` aparezca en las invocaciones.
func TestExecute_PreviousPRClosed_NoAutoReopen(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	// 1er view (gate): status:plan. 2do view (rollback): status:executing.
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)

	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0) // lock
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0) // rollback

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("IMPL.md", "work\n").
		RespondStdout("ok\n", 0)

	// gh pr create falla con el error específico que emite gh cuando hay
	// un PR cerrado existente para la misma head-branch.
	env.ExpectGh(`^pr create --draft`).RespondExitWithError(1,
		"a pull request for branch \"exec/42-implementar-comando-che-execute\" into branch \"main\" already exists:\n"+
			"https://github.com/acme/demo/pull/7\n"+
			"To reopen the PR, use `gh pr reopen`\n")

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	// El stderr del gh se propaga al flow para que el operador vea la
	// sugerencia de reopen sin tener que revisar el PR a mano.
	harness.AssertContains(t, r.Stderr, "gh pr reopen")

	inv := env.Invocations()
	// Se intentó crear una sola vez (no hay retry automático).
	creates := inv.FindCalls("gh", "pr", "create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 gh pr create attempt, got %d", len(creates))
	}
	// El flow NO reabre PRs cerrados automáticamente.
	if reopens := inv.FindCalls("gh", "pr", "reopen"); len(reopens) != 0 {
		t.Fatalf("expected 0 gh pr reopen (auto-reopen no implementado), got %d", len(reopens))
	}
	// Rollback aplicado: lock POST adds che:executing, rollback POST adds
	// che:plan.
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executing"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executing (lock), got %d", len(got))
	}
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:plan"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:plan (rollback), got %d", len(got))
	}
}

// TestExecute_PRCreateFails_PostPush_CleanupCorrect: si `gh pr create` falla
// después de que el push ya tuvo éxito (rate limit, network, scope transitorio),
// el flow aplica rollback:
//   - label: status:executing → status:plan (ownership-aware).
//   - worktree local: .worktrees/issue-N removido.
//   - branch local: exec/N-slug borrada.
//   - branch remota: PERMANECE en el origin (el push ya consumó). Esto es
//     intencional — un segundo `che execute` puede reusar el push sin re-subir
//     el diff, y borrarla exigiría un `git push --delete` que puede fallar
//     silenciosamente y dejar basura indetectable.
//   - exit 2 (remediable).
func TestExecute_PRCreateFails_PostPush_CleanupCorrect(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)

	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)

	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0) // lock
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0) // rollback

	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("FEATURE.md", "done\n").
		RespondStdout("ok\n", 0)

	// Falla genérica post-push: rate limit.
	env.ExpectGh(`^pr create --draft`).RespondExitWithError(1,
		"API rate limit exceeded; try again in 30 minutes\n")

	r := env.Run("execute", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "rate limit")

	// Cleanup local #1: worktree removido.
	wtPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42")
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree %s removed, got stat err=%v", wtPath, err)
	}

	// Cleanup local #2: branch local (exec/42-*) borrada.
	localBranches := gitListBranches(t, env.RepoDir, "exec/42-*")
	if localBranches != "" {
		t.Fatalf("expected exec/42-* local branch deleted, got: %q", localBranches)
	}

	// Cleanup remoto: branch remota SIGUE en el bare — el push ya consumó
	// y el flow deliberadamente no hace `git push --delete` (best-effort
	// para retry manual).
	bare := filepath.Join(env.RepoDir, "..", "remote.git")
	remoteBranches := gitListBranches(t, bare, "exec/42-*")
	if remoteBranches == "" {
		t.Fatalf("expected exec/42-* to persist on bare remote (best-effort post-push), got empty")
	}

	inv := env.Invocations()
	// Rollback del label aplicado: lock POST agrega che:executing, rollback
	// POST agrega che:plan.
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:executing"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:executing (lock), got %d", len(got))
	}
	if got := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels", "labels[]=che:plan"); len(got) != 1 {
		t.Fatalf("expected 1 POST adding che:plan (rollback), got %d", len(got))
	}

	// Se intentó crear una sola vez — el fallo no desencadena retry.
	if creates := inv.FindCalls("gh", "pr", "create"); len(creates) != 1 {
		t.Fatalf("expected 1 gh pr create attempt, got %d", len(creates))
	}
	// No hubo comentario al issue — eso pasa solo después del PR exitoso.
	if comments := inv.FindCalls("gh", "issue", "comment", "42"); len(comments) != 0 {
		t.Fatalf("expected 0 issue comments on failure, got %d", len(comments))
	}
}

// gitListBranches devuelve la salida de `git -C dir branch --list <glob>`
// trimmed. Lo usamos para verificar el cleanup: "" significa que no hay
// branches que coincidan con el glob.
func gitListBranches(t *testing.T, dir, glob string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "branch", "--list", glob)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list %s (dir=%s): %v\n%s", glob, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
