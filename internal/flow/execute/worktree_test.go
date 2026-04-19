package execute

import (
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
// un remote local que apunta al mismo repo, devolviendo la ruta raíz.
func setupTempRepo(t *testing.T) string {
	t.Helper()
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
	if err := wt.Cleanup(root, false); err != nil {
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
	_ = first.Cleanup(root, false)
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
	_ = wt.Cleanup(root, false)
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
