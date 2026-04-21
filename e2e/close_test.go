package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestClose_CommandExists verifica que `che close --help` renderice.
func TestClose_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("close", "--help")
	harness.AssertContains(t, out, "close")
	harness.AssertContains(t, out, "merge")
}

// TestClose_MissingArg_ExitNonZero: cobra requiere 1 arg posicional.
func TestClose_MissingArg_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("close")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit\nstderr: %s", r.Stderr)
	}
}

// TestClose_InvalidPRRef_Exit3: ref bogus rechazado antes de tocar red.
func TestClose_InvalidPRRef_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("close", "nonsense-ref")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestClose_ClosedPR_Exit3: PR con state=MERGED → exit 3, no invoca opus.
func TestClose_ClosedPR_Exit3(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)

	env.ExpectGh(`^pr view 7`).RespondStdoutFromFixture("close/gh_pr_view_closed.json", 0)

	r := env.Run("close", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "not OPEN")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestClose_ChangesRequestedLabel_WarnsButProceeds: PR con verdict
// bloqueante NO es un hard gate — el usuario lo pidió explícito, close
// warnea y mergea igual. (Decisión de producto v0.0.31: che cierra lo
// que el humano le diga sin filtrar por verdict.)
func TestClose_ChangesRequestedLabel_WarnsButProceeds(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	env.ExpectGh(`^pr view 7`).RespondStdoutFromFixture("close/gh_pr_view_changes_requested.json", 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge --delete-branch$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("close", "7")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0 (proceeds with warning), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	// El warning aparece en stderr pero no bloquea.
	harness.AssertContains(t, r.Stderr, "changes-requested")
	harness.AssertContains(t, r.Stderr, "warning")
	// Sí debe haber mergeado con --delete-branch.
	if merges := env.Invocations().FindCalls("gh", "pr merge 7 --merge --delete-branch"); len(merges) != 1 {
		t.Fatalf("expected 1 gh pr merge --delete-branch call, got %d", len(merges))
	}
}

// TestClose_GoldenPath_DraftToReadyAndMerge: PR draft, CI verde, sin
// conflictos → pasa a ready, merge, cierra issue asociado, transiciona
// labels status:executed → status:closed. También verifica que el merge
// invoca --delete-branch (default de che close).
func TestClose_GoldenPath_DraftToReadyAndMerge(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	// Skip el ls-remote pre-merge: el fixture origin apunta a una URL
	// bogus y no queremos network flakiness en el happy path.
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	// FetchPR inicial (gate) + FetchPR dentro del loop (iter 1). Ambas
	// devuelven el mismo fixture — el paso a ready no es visible al fake.
	env.ExpectGh(`^pr view 7`).RespondStdoutFromFixture("close/gh_pr_view_draft_clean.json", 0)

	// prReady (draft → ready).
	env.ExpectGh(`^pr ready 7$`).RespondStdout("", 0)

	// FetchChecks iter 1 — todos SUCCESS.
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)

	// Merge con --delete-branch (default).
	env.ExpectGh(`^pr merge 7 --merge --delete-branch$`).RespondStdout("Merged PR #7\n", 0)

	// Cerrar issue + transition labels (executed → closed).
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("close", "7")
	harness.AssertContains(t, out, "Closed PR https://github.com/acme/demo/pull/7")
	harness.AssertContains(t, out, "#42")
	// Confirma que el delete se reporta en stdout.
	harness.AssertContains(t, out, "Deleted branch exec/42-nuevo-flow")

	inv := env.Invocations()
	// Needles compuestos: "pr merge" y "issue close" evitan que
	// "mergeable" y "status:closed" (substrings en otras calls) metan
	// falsos positivos.
	if reads := inv.FindCalls("gh", "pr ready 7"); len(reads) != 1 {
		t.Fatalf("expected 1 gh pr ready call, got %d", len(reads))
	}
	if merges := inv.FindCalls("gh", "pr merge 7 --merge --delete-branch"); len(merges) != 1 {
		t.Fatalf("expected 1 gh pr merge --delete-branch call, got %d", len(merges))
	}
	if closes := inv.FindCalls("gh", "issue close 42"); len(closes) != 1 {
		t.Fatalf("expected 1 gh issue close call, got %d", len(closes))
	}
	// labels.Apply: 1 ensure + 1 edit con --add-label status:closed.
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 issue edit (status transition), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:closed")
	edits[0].AssertArgsContain(t, "--remove-label", "status:executed")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestClose_NotDraft_SkipsReady: PR ya ready (isDraft=false) → no llama
// a `gh pr ready`.
func TestClose_NotDraft_SkipsReady(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	// Custom fixture inline: copy del clean pero isDraft=false.
	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: ready PR",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/ready",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)

	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge --delete-branch$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	_ = env.MustRun("close", "7")
	if readys := env.Invocations().FindCalls("gh", "pr ready"); len(readys) != 0 {
		t.Fatalf("expected 0 gh pr ready calls on non-draft PR, got %d", len(readys))
	}
}

// TestClose_KeepBranch_PreservesBranchAndWorktree: con --keep-branch el
// merge NO debe incluir --delete-branch y el stdout anuncia la preservación.
func TestClose_KeepBranch_PreservesBranchAndWorktree(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: keep branch",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/preserved",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)

	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	// Crucial: merge SIN --delete-branch.
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("close", "7", "--keep-branch")
	harness.AssertContains(t, out, "Keeping branch feat/preserved (--keep-branch)")

	inv := env.Invocations()
	// Sin --delete-branch en los args.
	if merges := inv.FindCalls("gh", "pr merge 7 --merge"); len(merges) != 1 {
		t.Fatalf("expected 1 gh pr merge call, got %d", len(merges))
	}
	if deletes := inv.FindCalls("gh", "pr merge", "--delete-branch"); len(deletes) != 0 {
		t.Fatalf("expected 0 gh pr merge --delete-branch calls (--keep-branch passed), got %d", len(deletes))
	}
}

// TestClose_ConflictsExhaustAttempts_Exit2: PR con CONFLICTING en las 4
// vistas del loop (1 gate + 3 fix attempts + 1 post-fix recheck). El
// agente fake no arregla nada (no commitea). Después del 3er intento sin
// mejora → exit 2 sin mergear.
func TestClose_ConflictsExhaustAttempts_Exit2(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	// Crear branch local que el worktree va a checkoutear, y saltar fetch.
	env.SetEnv("CHE_CLOSE_SKIP_FETCH", "1")
	runIn(t, env.RepoDir, "git", "branch", "feat/conflicting")

	// FetchPR siempre devuelve CONFLICTING. Matcher persistente (no
	// Consumable) → responde a todas las calls.
	env.ExpectGh(`^pr view 7`).RespondStdoutFromFixture("close/gh_pr_view_conflicting.json", 0)
	// FetchChecks: empty array (CINone → no bloquea por CI, el conflict
	// es el único problema).
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_empty.json", 0)

	// Agente invocado en cada intento (3 veces). Responde OK pero sin
	// tocar archivos ni pushear — el conflict persiste.
	env.ExpectAgent("claude").
		WhenArgsMatch(`Conflictos con main`).
		RespondStdout("no pude resolver los conflictos\n", 0)

	r := env.Run("close", "7")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "agotados")
	harness.AssertContains(t, r.Stderr, "conflicts")

	inv := env.Invocations()
	// 3 invocaciones al agente (máximo de intentos).
	if claudes := len(inv.For("claude")); claudes != 3 {
		t.Fatalf("expected 3 claude calls (max attempts), got %d", claudes)
	}
	// NO debe haber intentado mergear. Needle compuesto "pr merge "
	// evita falso positivo con "mergeable" en `gh pr view --json`.
	if merges := inv.FindCalls("gh", "pr merge "); len(merges) != 0 {
		t.Fatalf("expected 0 gh pr merge calls, got %d", len(merges))
	}
	// NO debe haber cerrado el issue.
	if closes := inv.FindCalls("gh", "issue close"); len(closes) != 0 {
		t.Fatalf("expected 0 gh issue close calls, got %d", len(closes))
	}
}

// ---- helpers ----

// setupCloseEnv prepara un repo git real (como execute) + quita el fake
// de git para que los shellouts a git (rev-parse, worktree, branch)
// funcionen con el git del sistema.
func setupCloseEnv(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")

	runIn(t, env.RepoDir, "git", "init", "-q", "-b", "main")
	runIn(t, env.RepoDir, "git", "config", "user.email", "che@test.local")
	runIn(t, env.RepoDir, "git", "config", "user.name", "che-test")
	runIn(t, env.RepoDir, "git", "remote", "add", "origin",
		"https://github.com/acme/demo.git")
	// Commit inicial para que HEAD exista + poder crear branches.
	if err := os.WriteFile(filepath.Join(env.RepoDir, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runIn(t, env.RepoDir, "git", "add", "README.md")
	runIn(t, env.RepoDir, "git", "commit", "-q", "-m", "initial")
	return env
}

// scriptClosePrechecks solo scriptea gh auth status (close no usa -t).
func scriptClosePrechecks(env *harness.Env) {
	env.ExpectGh(`^auth status$`).Consumable().
		RespondStdout("Logged in as acme\n", 0)
}
