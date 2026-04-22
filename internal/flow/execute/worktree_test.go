package execute

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Comando che execute con flujo tipo explore", "comando-che-execute-con-flujo-tipo-explo"}, // trim a 40 chars
		{"  Hello  WORLD  ", "hello-world"},
		{"a/b/c", "a-b-c"},
		{"", "issue"},
		{"---", "issue"},
		{"con acentos: implementación", "con-acentos-implementaci-n"},
		{strings.Repeat("a", 50), strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// setupTempRepo inicializa un repo git vacío con un commit inicial en main y
// un remote local que apunta al mismo repo, devolviendo la ruta raíz. Setea
// CHE_EXEC_SKIP_FETCH=1 porque estos tests no tienen origin configurado.
func setupTempRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("CHE_EXEC_SKIP_FETCH", "1")
	root := t.TempDir()
	runOrFail(t, root, "git", "init", "-q", "-b", "main")
	runOrFail(t, root, "git", "config", "user.email", "test@example.com")
	runOrFail(t, root, "git", "config", "user.name", "Test")
	readme := filepath.Join(root, "README.md")
	if err := os.WriteFile(readme, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runOrFail(t, root, "git", "add", "README.md")
	runOrFail(t, root, "git", "commit", "-q", "-m", "initial")
	return root
}

func runOrFail(t *testing.T, dir, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

func TestCreateWorktree_GoldenPath(t *testing.T) {
	root := setupTempRepo(t)
	wt, err := CreateWorktree(WorktreeOpts{
		RepoRoot: root, IssueNum: 42, Slug: "my-feature",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wt.Branch != "exec/42-my-feature" {
		t.Errorf("branch: got %q", wt.Branch)
	}
	expectedPath := filepath.Join(root, ".worktrees", "issue-42")
	if wt.Path != expectedPath {
		t.Errorf("path: got %q want %q", wt.Path, expectedPath)
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("worktree dir not created: %v", err)
	}
	// Cleanup lo deja sin worktree y sin branch.
	if err := wt.Cleanup(context.Background(), root, false); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after cleanup: %v", err)
	}
}

func TestCreateWorktree_ReusesExistingSameBranch(t *testing.T) {
	root := setupTempRepo(t)
	first, err := CreateWorktree(WorktreeOpts{RepoRoot: root, IssueNum: 7, Slug: "x"})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := CreateWorktree(WorktreeOpts{RepoRoot: root, IssueNum: 7, Slug: "x"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Path != second.Path || first.Branch != second.Branch {
		t.Errorf("expected same worktree on reuse: first=%+v second=%+v", first, second)
	}
	// cleanup sólo una vez, no doble.
	_ = first.Cleanup(context.Background(), root, false)
}

func TestCreateWorktree_RejectsDivergentBranch(t *testing.T) {
	root := setupTempRepo(t)
	// Armamos un worktree "a mano" apuntando a otra branch.
	otherBranch := "exec/7-zzz"
	otherPath := filepath.Join(root, ".worktrees", "issue-7")
	runOrFail(t, root, "git", "worktree", "add", "-b", otherBranch, otherPath, "main")
	_, err := CreateWorktree(WorktreeOpts{RepoRoot: root, IssueNum: 7, Slug: "aaa"})
	if err == nil {
		t.Fatalf("expected error when worktree exists with different branch")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("unexpected error msg: %v", err)
	}
}

func TestCreateWorktree_ReusesExistingBranchWithoutWorktree(t *testing.T) {
	root := setupTempRepo(t)
	// Creamos la branch pero no el worktree.
	runOrFail(t, root, "git", "branch", "exec/9-foo", "main")
	wt, err := CreateWorktree(WorktreeOpts{RepoRoot: root, IssueNum: 9, Slug: "foo"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if wt.Branch != "exec/9-foo" {
		t.Errorf("branch: got %q", wt.Branch)
	}
	_ = wt.Cleanup(context.Background(), root, false)
}

func TestCreateWorktree_InvalidInputs(t *testing.T) {
	cases := []WorktreeOpts{
		{RepoRoot: "/tmp", IssueNum: 0, Slug: "x"},
		{RepoRoot: "/tmp", IssueNum: 1, Slug: ""},
		{RepoRoot: "", IssueNum: 1, Slug: "x"},
	}
	for _, c := range cases {
		if _, err := CreateWorktree(c); err == nil {
			t.Errorf("expected error for %+v", c)
		}
	}
}

// TestCreateWorktree_FetchesOriginMainAndUsesRemoteRef: sin
// CHE_EXEC_SKIP_FETCH, CreateWorktree debe hacer `git fetch origin main`
// y crear la branch desde `origin/main` (no desde `main` local). Armamos
// un setup con un bare remote local y un main remoto que tiene un commit
// que no está en el main local — la branch nueva debería apuntar al head
// remoto, no al local.
func TestCreateWorktree_FetchesOriginMainAndUsesRemoteRef(t *testing.T) {
	// No seteamos CHE_EXEC_SKIP_FETCH — queremos que el fetch ocurra.
	// Unset defensivo por si el parent suite lo seteó.
	t.Setenv("CHE_EXEC_SKIP_FETCH", "")

	base := t.TempDir()
	// Bare remote.
	bare := filepath.Join(base, "origin.git")
	runOrFail(t, "", "git", "init", "--bare", "-b", "main", "-q", bare)

	// Clone A: inicializamos sin clone porque bare está vacío — usamos
	// un repo standalone + remote add + push.
	cloneA := filepath.Join(base, "a")
	runOrFail(t, "", "git", "init", "-b", "main", "-q", cloneA)
	runOrFail(t, cloneA, "git", "config", "user.email", "a@example.com")
	runOrFail(t, cloneA, "git", "config", "user.name", "A")
	runOrFail(t, cloneA, "git", "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(cloneA, "a.txt"), []byte("a1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOrFail(t, cloneA, "git", "add", "a.txt")
	runOrFail(t, cloneA, "git", "commit", "-q", "-m", "a1")
	runOrFail(t, cloneA, "git", "push", "-q", "-u", "origin", "main")

	// Repo bajo test: clone del bare, luego añadimos un commit remoto desde A
	// que el repo local aún no ha fetcheado.
	root := filepath.Join(base, "repo")
	runOrFail(t, "", "git", "clone", "-q", bare, root)
	runOrFail(t, root, "git", "config", "user.email", "test@example.com")
	runOrFail(t, root, "git", "config", "user.name", "Test")

	// Nuevo commit en A → push al bare. El repo local (root) aún no lo tiene.
	if err := os.WriteFile(filepath.Join(cloneA, "a.txt"), []byte("a2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOrFail(t, cloneA, "git", "commit", "-aq", "-m", "a2-remote")
	runOrFail(t, cloneA, "git", "push", "-q", "origin", "main")

	// Pre-check: el main local y el origin/main local (pre-fetch) apuntan
	// al commit viejo (a1) — porque root fue clonado antes del segundo
	// push desde A. El bare ya tiene a2.
	localHead := mustRevParse(t, root, "HEAD")
	remoteCommitInBare := mustRevParse(t, bare, "refs/heads/main")
	if localHead == remoteCommitInBare {
		t.Fatal("setup inválido: local y bare apuntan al mismo commit")
	}

	// CreateWorktree debe hacer fetch + usar origin/main (que ahora tiene a2).
	wt, err := CreateWorktree(WorktreeOpts{
		RepoRoot: root, IssueNum: 42, Slug: "fetch-test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer wt.Cleanup(context.Background(), root, false)

	// La branch debe apuntar al mismo commit que origin/main (el nuevo a2).
	branchHead := mustRevParse(t, root, wt.Branch)
	remoteHead := mustRevParse(t, root, "origin/main")
	if branchHead != remoteHead {
		t.Errorf("branch head %s != origin/main head %s — indicates the worktree was created from stale local main", branchHead, remoteHead)
	}
	if branchHead == localHead {
		t.Errorf("branch head matches stale local main (a1) instead of origin/main (a2)")
	}
}

// TestDetectBaseBranch_EnvOverride: si CHE_BASE_BRANCH está seteado, se usa
// sin llamar a gh ni git. Es el escape hatch que usan los e2e y el usuario
// cuando tiene un repo con default branch no-convencional.
func TestDetectBaseBranch_EnvOverride(t *testing.T) {
	t.Setenv("CHE_BASE_BRANCH", "develop")
	got, err := DetectBaseBranch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "develop" {
		t.Errorf("got %q, want %q", got, "develop")
	}
}

// TestDetectBaseBranch_GhRepoView: con un fake gh que devuelve el JSON
// esperado, DetectBaseBranch parsea defaultBranchRef.name.
func TestDetectBaseBranch_GhRepoView(t *testing.T) {
	t.Setenv("CHE_BASE_BRANCH", "")
	tmp := t.TempDir()
	fakeGH := filepath.Join(tmp, "gh")
	script := "#!/bin/sh\ncat <<EOF\n{\"defaultBranchRef\":{\"name\":\"master\"}}\nEOF\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	got, err := DetectBaseBranch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "master" {
		t.Errorf("got %q, want %q", got, "master")
	}
}

// TestDetectBaseBranch_FallbackToSymbolicRef: si gh falla, caemos a
// `git symbolic-ref refs/remotes/origin/HEAD`. Armamos un repo con origin/HEAD
// apuntando a master y un fake gh que sale con error.
func TestDetectBaseBranch_FallbackToSymbolicRef(t *testing.T) {
	t.Setenv("CHE_BASE_BRANCH", "")
	base := t.TempDir()

	bare := filepath.Join(base, "origin.git")
	runOrFail(t, "", "git", "init", "--bare", "-b", "master", "-q", bare)

	seed := filepath.Join(base, "seed")
	runOrFail(t, "", "git", "init", "-b", "master", "-q", seed)
	runOrFail(t, seed, "git", "config", "user.email", "a@example.com")
	runOrFail(t, seed, "git", "config", "user.name", "A")
	runOrFail(t, seed, "git", "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOrFail(t, seed, "git", "add", "a.txt")
	runOrFail(t, seed, "git", "commit", "-q", "-m", "init")
	runOrFail(t, seed, "git", "push", "-q", "-u", "origin", "master")

	root := filepath.Join(base, "repo")
	runOrFail(t, "", "git", "clone", "-q", bare, root)

	tmpBin := t.TempDir()
	fakeGH := filepath.Join(tmpBin, "gh")
	script := "#!/bin/sh\necho 'not authenticated' >&2\nexit 4\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", tmpBin+":"+os.Getenv("PATH"))

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got, err := DetectBaseBranch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "master" {
		t.Errorf("got %q, want %q", got, "master")
	}
}

func mustRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

