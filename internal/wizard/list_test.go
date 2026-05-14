package wizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperFakeAllCLIs hace que IsValid trate como instalados a los 4 CLIs v1.
// Necesario para que loadListItems no rebote al cargar el builtin che-funnel
// que invoca claude + codex.
func helperFakeAllCLIs(t *testing.T) {
	t.Helper()
	prev := detectInstalledCLIs
	t.Cleanup(func() { detectInstalledCLIs = prev })
	detectInstalledCLIs = func() []string { return []string{"claude", "codex", "gemini", "opencode"} }
}

func TestLoadListItems_EmptyDirReturnsOnlyBuiltins(t *testing.T) {
	helperFakeAllCLIs(t)
	home := t.TempDir()

	items, err := loadListItems(home, "")
	if err != nil {
		t.Fatalf("loadListItems: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("esperabamos al menos un builtin, got 0 items")
	}
	for _, it := range items {
		if !it.isBuiltin {
			t.Errorf("item %q no esperaba (no builtin) en dir vacio", it.name)
		}
	}
	// Sanity: che-funnel tiene que estar.
	found := false
	for _, it := range items {
		if it.slug == "che-funnel" {
			found = true
			break
		}
	}
	if !found {
		t.Error("che-funnel no aparecio entre los builtins")
	}
}

func TestLoadListItems_FSItemsCoexistWithBuiltins(t *testing.T) {
	helperFakeAllCLIs(t)
	home := t.TempDir()
	dir, err := EnsureDir(home)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	// Pipeline ready del usuario, slug distinto al builtin.
	user := Pipeline{
		Name: "mi-flujo",
		Steps: []Step{
			{Name: "uno", CLI: "claude", Kind: "prompt", Content: "hola", Input: "text"},
		},
	}
	if err := Save(filepath.Join(dir, "mi-flujo.yaml"), user); err != nil {
		t.Fatalf("Save user: %v", err)
	}

	items, err := loadListItems(home, "")
	if err != nil {
		t.Fatalf("loadListItems: %v", err)
	}

	var sawUser, sawBuiltin bool
	for _, it := range items {
		switch it.slug {
		case "mi-flujo":
			sawUser = true
			if it.isBuiltin {
				t.Error("mi-flujo no deberia ser builtin")
			}
		case "che-funnel":
			sawBuiltin = true
			if !it.isBuiltin {
				t.Error("che-funnel deberia ser builtin")
			}
		}
	}
	if !sawUser {
		t.Error("mi-flujo no aparecio en la lista")
	}
	if !sawBuiltin {
		t.Error("che-funnel no aparecio en la lista")
	}
}

func TestLoadListItems_ShadowOverridesBuiltin(t *testing.T) {
	helperFakeAllCLIs(t)
	home := t.TempDir()
	dir, err := EnsureDir(home)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	// Shadow: archivo del usuario con el mismo slug que el builtin.
	shadow := Pipeline{
		Name: "che-funnel", // slug = "che-funnel"
		Steps: []Step{
			{Name: "custom", CLI: "claude", Kind: "prompt", Content: "x", Input: "text"},
		},
	}
	if err := Save(filepath.Join(dir, "che-funnel.yaml"), shadow); err != nil {
		t.Fatalf("Save shadow: %v", err)
	}

	items, err := loadListItems(home, "")
	if err != nil {
		t.Fatalf("loadListItems: %v", err)
	}

	var count int
	var found listItem
	for _, it := range items {
		if it.slug == "che-funnel" {
			count++
			found = it
		}
	}
	if count != 1 {
		t.Errorf("esperaba 1 che-funnel (shadow gana), got %d", count)
	}
	if found.isBuiltin {
		t.Error("el shadow deberia ganar — esperaba isBuiltin=false")
	}
	if found.path == "" {
		t.Error("el shadow deberia tener path del FS")
	}
	if found.nSteps != 1 {
		t.Errorf("nSteps del shadow: got %d want 1", found.nSteps)
	}
}

func TestLoadListItems_ProjectVisibleAndWins(t *testing.T) {
	helperFakeAllCLIs(t)
	home := t.TempDir()
	projectRoot := t.TempDir()

	// Pipeline ready en scope global con slug "shared" + uno solo global.
	gdir, err := EnsureDir(home)
	if err != nil {
		t.Fatalf("EnsureDir global: %v", err)
	}
	if err := Save(filepath.Join(gdir, "shared.yaml"), Pipeline{
		Name: "shared",
		Steps: []Step{
			{Name: "g", CLI: "claude", Kind: "prompt", Content: "global", Input: "text"},
		},
	}); err != nil {
		t.Fatalf("Save global shared: %v", err)
	}
	if err := Save(filepath.Join(gdir, "only-global.yaml"), Pipeline{
		Name: "only-global",
		Steps: []Step{
			{Name: "g", CLI: "claude", Kind: "prompt", Content: "x", Input: "text"},
		},
	}); err != nil {
		t.Fatalf("Save only-global: %v", err)
	}

	// Project: override de "shared" + uno exclusivo del project.
	pdir := filepath.Join(projectRoot, ".che", "pipelines")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := Save(filepath.Join(pdir, "shared.yaml"), Pipeline{
		Name: "shared",
		Steps: []Step{
			{Name: "p", CLI: "claude", Kind: "prompt", Content: "project", Input: "text"},
		},
	}); err != nil {
		t.Fatalf("Save project shared: %v", err)
	}
	if err := Save(filepath.Join(pdir, "only-project.yaml"), Pipeline{
		Name: "only-project",
		Steps: []Step{
			{Name: "p", CLI: "claude", Kind: "prompt", Content: "x", Input: "text"},
		},
	}); err != nil {
		t.Fatalf("Save only-project: %v", err)
	}

	items, err := loadListItems(home, projectRoot)
	if err != nil {
		t.Fatalf("loadListItems: %v", err)
	}

	var shared, onlyProject, onlyGlobal *listItem
	sharedCount := 0
	for i := range items {
		switch items[i].slug {
		case "shared":
			sharedCount++
			shared = &items[i]
		case "only-project":
			onlyProject = &items[i]
		case "only-global":
			onlyGlobal = &items[i]
		}
	}
	if sharedCount != 1 {
		t.Errorf("esperaba 1 shared (project gana), got %d", sharedCount)
	}
	if shared == nil {
		t.Fatal("shared no aparece")
	}
	if !shared.isProject {
		t.Errorf("shared deberia ser scope project, got isProject=false")
	}
	if !strings.Contains(shared.path, projectRoot) {
		t.Errorf("shared.path deberia apuntar al project dir: got %q", shared.path)
	}
	if onlyProject == nil {
		t.Fatal("only-project no aparece")
	}
	if !onlyProject.isProject {
		t.Errorf("only-project deberia ser scope project")
	}
	if onlyGlobal == nil {
		t.Fatal("only-global no aparece")
	}
	if onlyGlobal.isProject {
		t.Errorf("only-global no deberia ser scope project")
	}
}

func TestFindProjectRoot_WalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".che", "pipelines"), 0o700); err != nil {
		t.Fatalf("mkdir pipelines: %v", err)
	}

	if got := findProjectRoot(deep); got != root {
		t.Errorf("findProjectRoot(deep): got %q want %q", got, root)
	}
	if got := findProjectRoot(root); got != root {
		t.Errorf("findProjectRoot(root): got %q want %q", got, root)
	}

	// Sin ancestro con .che/pipelines/ → "".
	orphan := t.TempDir()
	if got := findProjectRoot(orphan); got != "" {
		t.Errorf("findProjectRoot(orphan): got %q want %q", got, "")
	}
	if got := findProjectRoot(""); got != "" {
		t.Errorf("findProjectRoot(\"\"): got %q want %q", got, "")
	}
}

func TestBuiltinTargetSentinel(t *testing.T) {
	cases := []struct {
		target  string
		wantHit bool
		wantSlug string
	}{
		{"builtin:che-funnel", true, "che-funnel"},
		{"builtin:foo", true, "foo"},
		{"/abs/path.yaml", false, ""},
		{"", false, ""},
		{"builtin:", true, ""}, // prefijo solo: hit pero slug vacio
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			if got := IsBuiltinTarget(tc.target); got != tc.wantHit {
				t.Errorf("IsBuiltinTarget(%q): got %v want %v", tc.target, got, tc.wantHit)
			}
			if got := BuiltinSlugFromTarget(tc.target); got != tc.wantSlug {
				t.Errorf("BuiltinSlugFromTarget(%q): got %q want %q", tc.target, got, tc.wantSlug)
			}
		})
	}
}

func TestBuiltinBySlug(t *testing.T) {
	b, err := BuiltinBySlug("che-funnel")
	if err != nil {
		t.Fatalf("BuiltinBySlug: %v", err)
	}
	if b == nil {
		t.Fatal("che-funnel no encontrado")
	}
	if b.Slug != "che-funnel" {
		t.Errorf("Slug: got %q want che-funnel", b.Slug)
	}

	b2, err := BuiltinBySlug("no-existe")
	if err != nil {
		t.Fatalf("BuiltinBySlug no-existe: %v", err)
	}
	if b2 != nil {
		t.Errorf("esperaba nil para slug inexistente, got %+v", b2)
	}
}

func TestCopyBuiltinToFS_WritesFile(t *testing.T) {
	home := t.TempDir()
	source := []byte("name: che-funnel\nsteps:\n  - name: x\n    cli: claude\n    kind: prompt\n    content: y\n    input: text\n")
	path, err := copyBuiltinToFS(home, "che-funnel", source)
	if err != nil {
		t.Fatalf("copyBuiltinToFS: %v", err)
	}
	want := filepath.Join(home, ".che/pipelines/che-funnel.yaml")
	if path != want {
		t.Errorf("path: got %q want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(got), "che-funnel") {
		t.Errorf("contenido no contiene slug: got %q", string(got))
	}
}

func TestCopyBuiltinToFS_DoesNotOverwriteExisting(t *testing.T) {
	home := t.TempDir()
	dir, err := EnsureDir(home)
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	existing := []byte("# editado por el usuario\nname: che-funnel\n")
	path := filepath.Join(dir, "che-funnel.yaml")
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatalf("WriteFile setup: %v", err)
	}

	// Intentar copy-on-edit no deberia pisar.
	source := []byte("# del binario (no me copies encima)\nname: che-funnel\n")
	got, err := copyBuiltinToFS(home, "che-funnel", source)
	if err != nil {
		t.Fatalf("copyBuiltinToFS: %v", err)
	}
	if got != path {
		t.Errorf("path: got %q want %q", got, path)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "editado por el usuario") {
		t.Errorf("contenido fue pisado: got %q", string(content))
	}
}
