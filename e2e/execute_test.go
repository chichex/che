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

	out := env.MustRun("execute", "--validators", "none", "42")
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

	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 2 {
		t.Fatalf("expected 2 issue edits (lock + unlock), got %d", len(edits))
	}
	// Primer edit: plan → executing.
	edits[0].AssertArgsContain(t, "--add-label", "status:executing")
	// Segundo edit: executing → executed + awaiting-human.
	edits[1].AssertArgsContain(t, "--add-label", "status:executed")
	edits[1].AssertArgsContain(t, "--add-label", "status:awaiting-human")

	if comments := inv.FindCalls("gh", "issue", "comment", "42", "--body-file"); len(comments) != 1 {
		t.Fatalf("expected 1 issue comment, got %d", len(comments))
	}
}

// TestExecute_IssueNotStatusPlan_Exit3: issue sin status:plan (está en idea) → exit 3.
func TestExecute_IssueNotStatusPlan_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_not_plan.json", 0)

	r := env.Run("execute", "--validators", "none", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "status:plan")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_IssueAwaitingHuman_Exit3: issue con awaiting-human → exit 3.
func TestExecute_IssueAwaitingHuman_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_awaiting.json", 0)

	r := env.Run("execute", "--validators", "none", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "awaiting-human")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExecute_IssueAlreadyExecuting_Exit3: otro run dejó status:executing → refuse.
func TestExecute_IssueAlreadyExecuting_Exit3(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("execute/gh_issue_view_executing.json", 0)

	r := env.Run("execute", "--validators", "none", "42")
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

	out := env.MustRun("execute", "--validators", "none", "42")
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

	r := env.Run("execute", "--validators", "none", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	if creates := inv.FindCalls("gh", "pr", "create"); len(creates) > 0 {
		t.Fatalf("expected 0 gh pr create after agent failure, got %d", len(creates))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 2 {
		t.Fatalf("expected 2 issue edits (lock + rollback), got %d", len(edits))
	}
	edits[1].AssertArgsContain(t, "--add-label", "status:plan")
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

	r := env.Run("execute", "--validators", "none", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 issue edit (only lock, rollback skipped), got %d", len(edits))
	}
	// El único edit debe ser el lock; inspeccionamos la secuencia
	// consecutiva `--add-label status:plan` para descartar rollback.
	rollbackCount := 0
	for _, e := range edits {
		for i := 0; i+1 < len(e.Args); i++ {
			if e.Args[i] == "--add-label" && e.Args[i+1] == "status:plan" {
				rollbackCount++
			}
		}
	}
	if rollbackCount != 0 {
		t.Fatalf("expected 0 rollback edits (consecutive --add-label status:plan), got %d", rollbackCount)
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

	r := env.Run("execute", "--validators", "none", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "sin cambios")
}

// TestExecute_ListCandidates_FiltersAwaiting: integration-style — esta prueba
// solo verifica que `che execute` no liste issues con awaiting-human. La TUI
// es difícil de testear directamente; chequeamos por side-effect del filter
// cuando se corre el list interno.
// Nota: este test no tiene sentido sin una ruta TUI; lo dejamos como test
// unitario en execute_test.go y no acá.

// ---- helpers ----

// setupExecuteEnv prepara un repo git real en env.RepoDir + quita el fake de
// git (dejamos el git real del sistema para que worktree y commits funcionen).
func setupExecuteEnv(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")

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

// scriptExecutePrechecks scriptea los 3 prechecks: git remote, gh auth, gh pr
// list (scope check). Cuando se usa con un repo git real, NO hay que
// scriptear git — es el real del sistema.
func scriptExecutePrechecks(env *harness.Env) {
	// git remote get-url origin → lo tira git real del sistema (tenemos
	// origin agregado en setupExecuteEnv). Pero precheckGitHubRemote exige
	// que la URL contenga "github.com" — el remote es un path bare. Usamos
	// el fake git solo para esa llamada. Pero si ya quitamos el fake…
	// Solución: agregamos un remote falso "github.com" reescribiendo el
	// origin antes de cada test. Mejor: modificamos el helper para que el
	// remote apunte a un URL github.com + el push usa otro remote.
	//
	// Por simplicidad, redirigimos solo `git remote get-url origin`
	// reinyectando el fake git SOLO para ese call pattern, y todo lo demás
	// queda en el git real. Pero no podemos mezclar así — el fake es un
	// único binario. Alternativa: cambiar el origin URL después del init.
	env.ExpectGh(`^auth status`).RespondStdout("Logged in as acme\n", 0)
	env.ExpectGh(`^pr list --limit 1`).RespondStdout("[]\n", 0)
}
