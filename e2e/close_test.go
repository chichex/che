package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/x$`).RespondStdout("", 0)
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
	inv := env.Invocations()
	if merges := inv.FindCalls("gh", "pr merge 7 --merge"); len(merges) != 1 {
		t.Fatalf("expected 1 gh pr merge call, got %d", len(merges))
	}
	// Nunca pasamos --delete-branch: el delete remoto es separado (gh api).
	if deletes := inv.FindCalls("gh", "pr merge", "--delete-branch"); len(deletes) != 0 {
		t.Fatalf("expected 0 gh pr merge --delete-branch calls, got %d", len(deletes))
	}
	if apiDels := inv.FindCalls("gh", "api -X DELETE", "refs/heads/feat/x"); len(apiDels) != 1 {
		t.Fatalf("expected 1 gh api DELETE refs/heads/feat/x call, got %d", len(apiDels))
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

	// Merge (sin --delete-branch — lo hacemos nosotros con gh api post-merge).
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged PR #7\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/exec/42-nuevo-flow$`).RespondStdout("", 0)

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
	if merges := inv.FindCalls("gh", "pr merge 7 --merge"); len(merges) != 1 {
		t.Fatalf("expected 1 gh pr merge call, got %d", len(merges))
	}
	// El merge NO debe pasar --delete-branch: evitamos que gh intente borrar
	// la branch local, lo cual falla cuando está checkouteada en un worktree
	// (caso típico: che execute deja un worktree por PR).
	if deletes := inv.FindCalls("gh", "pr merge", "--delete-branch"); len(deletes) != 0 {
		t.Fatalf("expected 0 gh pr merge --delete-branch calls, got %d", len(deletes))
	}
	if apiDels := inv.FindCalls("gh", "api -X DELETE", "refs/heads/exec/42-nuevo-flow"); len(apiDels) != 1 {
		t.Fatalf("expected 1 gh api DELETE call for remote branch, got %d", len(apiDels))
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
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/ready$`).RespondStdout("", 0)
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

// TestClose_Default_CleansCheManagedWorktree: happy path con un worktree
// real bajo `.worktrees/issue-42/` checkouteado en la head branch del PR.
// Tras mergear, che close debe remover el worktree y borrar la branch local.
func TestClose_Default_CleansCheManagedWorktree(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	branch := "feat/managed"
	wtPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42")
	runIn(t, env.RepoDir, "git", "branch", branch)
	runIn(t, env.RepoDir, "git", "worktree", "add", wtPath, branch)

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: managed worktree",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/managed",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/managed$`).RespondStdout("", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	_ = env.MustRun("close", "7")

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("expected che-managed worktree %s to be removed, but it still exists (err=%v)", wtPath, err)
	}
	if branchExists(t, env.RepoDir, branch) {
		t.Fatalf("expected local branch %s to be deleted after merge cleanup", branch)
	}
}

// TestClose_KeepBranch_PreservesCheManagedWorktree: con --keep-branch,
// incluso sobre un worktree real bajo `.worktrees/`, ni el worktree ni la
// branch local se deben tocar. Contrato público del flag (antes teníamos
// un bug: el cleanup corría igual si el worktree era wtOwned, contradiciendo
// el help del flag).
func TestClose_KeepBranch_PreservesCheManagedWorktree(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	branch := "feat/preserved-real"
	wtPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42")
	runIn(t, env.RepoDir, "git", "branch", branch)
	runIn(t, env.RepoDir, "git", "worktree", "add", wtPath, branch)

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: keep real worktree",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/preserved-real",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	_ = env.MustRun("close", "7", "--keep-branch")

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("expected worktree %s to be preserved with --keep-branch, but stat failed: %v", wtPath, err)
	}
	if !branchExists(t, env.RepoDir, branch) {
		t.Fatalf("expected local branch %s to be preserved with --keep-branch", branch)
	}
}

// TestClose_Default_DoesNotTouchMainWorktree: si la head branch del PR
// está checkouteada en el worktree principal (el cwd del usuario), che
// close NO debe hacer `checkout --detach` ni `branch -D` — dejar el repo
// en detached HEAD sería un side effect no anunciado. Es el blocker
// identificado por el validador codex en iter 1.
func TestClose_Default_DoesNotTouchMainWorktree(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	branch := "feat/on-main"
	// Checkoutear la branch en el worktree principal.
	runIn(t, env.RepoDir, "git", "checkout", "-b", branch)

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: on main worktree",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/on-main",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/on-main$`).RespondStdout("", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	_ = env.MustRun("close", "7")

	// HEAD del main debe seguir attached a la branch (no detached).
	head := gitOutput(t, env.RepoDir, "symbolic-ref", "--quiet", "HEAD")
	if head != "refs/heads/"+branch {
		t.Fatalf("expected main HEAD to stay attached to %s, got %q (detached HEAD means che touched the main worktree)", branch, head)
	}
	if !branchExists(t, env.RepoDir, branch) {
		t.Fatalf("expected local branch %s to remain (che should not branch -D when the branch is on the main worktree)", branch)
	}
}

// TestClose_Default_DoesNotTouchExternalWorktree: si la head branch está
// checkouteada en un worktree que el usuario creó fuera de `.worktrees/`,
// che close NO debe removerlo ni borrar la branch. Esos worktrees son del
// usuario — che solo administra los que vos crea bajo `.worktrees/`.
func TestClose_Default_DoesNotTouchExternalWorktree(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	branch := "feat/external"
	externalPath := filepath.Join(t.TempDir(), "user-worktree")
	runIn(t, env.RepoDir, "git", "branch", branch)
	runIn(t, env.RepoDir, "git", "worktree", "add", externalPath, branch)

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: external worktree",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/external",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/external$`).RespondStdout("", 0)
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	_ = env.MustRun("close", "7")

	if _, err := os.Stat(externalPath); err != nil {
		t.Fatalf("expected external worktree %s to be preserved, stat failed: %v", externalPath, err)
	}
	if !branchExists(t, env.RepoDir, branch) {
		t.Fatalf("expected local branch %s to remain (not a che-managed worktree)", branch)
	}
}

// TestClose_RemoteDeleteFails_WarnsButExitsOK: el merge en GitHub OK pero el
// delete de la branch remota (gh api DELETE) falla — el flow debe warnear,
// reportar "kept on remote", y salir 0 (no ExitRetry).
//
// Este es el caso que motivó el refactor: pasar --delete-branch a gh pr
// merge arrastraba un exit != 0 cuando la branch estaba checkouteada en
// un worktree, incluso si el merge remoto se había hecho. Al separar merge
// y delete, un delete fallido no invalida el merge.
func TestClose_RemoteDeleteFails_WarnsButExitsOK(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_REMOTE_CHECK", "1")

	env.ExpectGh(`^pr view 7`).RespondStdout(`{
  "number": 7,
  "title": "feat: delete fails",
  "url": "https://github.com/acme/demo/pull/7",
  "state": "OPEN",
  "isDraft": false,
  "headRefName": "feat/delete-fails",
  "mergeable": "MERGEABLE",
  "mergeStateStatus": "CLEAN",
  "author": {"login": "acme-bot"},
  "closingIssuesReferences": [{"number": 42, "state": "OPEN"}],
  "labels": []
}`, 0)
	env.ExpectGh(`^pr checks 7`).RespondStdoutFromFixture("close/gh_pr_checks_pass.json", 0)
	env.ExpectGh(`^pr merge 7 --merge$`).RespondStdout("Merged\n", 0)
	// El api DELETE devuelve un error cualquiera distinto a "Reference does
	// not exist" (ese caso es idempotente y no warnearía).
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/git/refs/heads/feat/delete-fails$`).
		RespondExitWithError(1, "HTTP 403: You need admin access to delete branches\n")
	env.ExpectGh(`^issue close 42$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create status:closed --force$`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("close", "7")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0 (merge OK, delete remoto es best-effort), got %d\nstderr: %s",
			r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stdout, "Branch feat/delete-fails kept on remote (delete failed)")
	harness.AssertContains(t, r.Stderr, "warning")
	harness.AssertContains(t, r.Stderr, "git push origin --delete feat/delete-fails")
	// El merge debe haber sucedido y el issue cerrado — el delete fallido
	// no invalida el resto del flow.
	harness.AssertContains(t, r.Stdout, "Closed PR")
	harness.AssertContains(t, r.Stdout, "#42")
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

// TestClose_NoChecksReported_TreatedAsCINone: PR con conflicts y sin CI
// configurado. `gh pr checks` sale con exit=1 y stderr "no checks reported
// on the '<branch>' branch". Tratamos ese caso como CINone (no bloquea)
// — el único problema real es el conflict, que el flow debe reportar
// normalmente en vez de abortar con "error: gh pr checks: no checks
// reported…".
func TestClose_NoChecksReported_TreatedAsCINone(t *testing.T) {
	env := setupCloseEnv(t)
	scriptClosePrechecks(env)
	env.SetEnv("CHE_CLOSE_SKIP_FETCH", "1")
	runIn(t, env.RepoDir, "git", "branch", "feat/conflicting")

	env.ExpectGh(`^pr view 7`).RespondStdoutFromFixture("close/gh_pr_view_conflicting.json", 0)
	// gh pr checks: exit 1 con el mensaje típico del caso "PR con
	// conflicts donde CI nunca corrió" — reproduce el bug reportado.
	env.ExpectGh(`^pr checks 7`).RespondExitWithError(1,
		"no checks reported on the 'feat/conflicting' branch\n")

	env.ExpectAgent("claude").
		WhenArgsMatch(`Conflictos con main`).
		RespondStdout("no pude resolver los conflictos\n", 0)

	r := env.Run("close", "7")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2 (retry por conflicts sin resolver), got %d\nstderr: %s",
			r.ExitCode, r.Stderr)
	}
	// El flow debe reportar el problema real (conflicts) y agotar intentos
	// — no abortar con el mensaje de gh pr checks.
	harness.AssertContains(t, r.Stderr, "conflicts")
	if strings.Contains(r.Stderr, "gh pr checks: no checks reported") {
		t.Fatalf("no checks reported debió tratarse como CINone, pero close abortó con el error de gh\nstderr: %s",
			r.Stderr)
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

// branchExists devuelve true si refs/heads/<branch> existe en el repo.
func branchExists(t *testing.T, repoDir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// gitOutput corre `git -C repoDir args...` y devuelve stdout trim. Falla
// el test si el comando falla. Útil para leer HEAD, branch actual, etc.
func gitOutput(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", repoDir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
