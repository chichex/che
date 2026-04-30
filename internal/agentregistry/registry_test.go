package agentregistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeAgent es un helper de tests que escribe un .md con frontmatter
// mínimo (name + model) en el path indicado, creando los dirs intermedios.
func writeAgent(t *testing.T, path, name, model string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "---\nname: " + name + "\n"
	if model != "" {
		body += "model: " + model + "\n"
	}
	body += "---\nbody\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestDiscover_BuiltinsAlone valida el caso default: ningún path
// existe, solo built-ins. Tiene que devolver opus/sonnet/haiku con
// Source=built-in y nada más.
func TestDiscover_BuiltinsAlone(t *testing.T) {
	tmp := t.TempDir()
	reg, errs := Discover(Options{
		CWD:             tmp,
		HomeDir:         tmp,
		IncludeBuiltins: true,
	})
	if len(errs) != 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
	all := reg.All()
	if len(all) != 3 {
		t.Fatalf("got %d agents, want 3 built-ins", len(all))
	}
	want := map[string]string{
		"claude-opus":   "opus",
		"claude-sonnet": "sonnet",
		"claude-haiku":  "haiku",
	}
	for _, a := range all {
		if a.Source != SourceBuiltin {
			t.Errorf("%s: Source = %v, want built-in", a.Name, a.Source)
		}
		if want[a.Name] != a.Model {
			t.Errorf("%s: Model = %q, want %q", a.Name, a.Model, want[a.Name])
		}
	}
}

// TestDiscover_FourSources arma un layout con un agente en cada
// ubicación oficial (managed/project/user/plugin) y un built-in
// implícito. Verifica que aparezcan los 4 + los 3 built-in.
func TestDiscover_FourSources(t *testing.T) {
	tmp := t.TempDir()
	managed := filepath.Join(tmp, "managed")
	cwd := filepath.Join(tmp, "repo", "subdir")
	home := filepath.Join(tmp, "home")

	writeAgent(t, filepath.Join(managed, "managed-agent.md"), "managed-agent", "opus")
	writeAgent(t, filepath.Join(tmp, "repo", ".claude", "agents", "project-agent.md"), "project-agent", "sonnet")
	writeAgent(t, filepath.Join(home, ".claude", "agents", "user-agent.md"), "user-agent", "haiku")
	writeAgent(t, filepath.Join(home, ".claude", "plugins", "myplugin", "agents", "plug-agent.md"), "plug-agent", "opus")
	writeAgent(t, filepath.Join(home, ".claude", "plugins", "myplugin", "agents", "sub", "nested.md"), "nested", "opus")

	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	reg, errs := Discover(Options{
		CWD:             cwd,
		HomeDir:         home,
		ManagedDir:      managed,
		IncludeBuiltins: true,
	})
	if len(errs) != 0 {
		t.Errorf("errs: %v", errs)
	}

	wantNames := map[string]Source{
		"managed-agent":             SourceManaged,
		"project-agent":             SourceProject,
		"user-agent":                SourceUser,
		"myplugin:plug-agent":       SourcePlugin,
		"myplugin:sub:nested":       SourcePlugin,
		"claude-opus":               SourceBuiltin,
		"claude-sonnet":             SourceBuiltin,
		"claude-haiku":              SourceBuiltin,
	}
	got := map[string]Source{}
	for _, a := range reg.All() {
		got[a.Name] = a.Source
	}
	for name, src := range wantNames {
		gotSrc, ok := got[name]
		if !ok {
			t.Errorf("missing agent %q", name)
			continue
		}
		if gotSrc != src {
			t.Errorf("agent %q: source = %v, want %v", name, gotSrc, src)
		}
	}
	if len(got) != len(wantNames) {
		t.Errorf("got %d agents, want %d (got: %v)", len(got), len(wantNames), got)
	}
}

// TestDiscover_PrecedenceManagedWinsOverProject: si el mismo nombre
// aparece en managed Y project, gana managed (§2.a precedencia).
func TestDiscover_PrecedenceManagedWinsOverProject(t *testing.T) {
	tmp := t.TempDir()
	managed := filepath.Join(tmp, "managed")
	repo := filepath.Join(tmp, "repo")

	writeAgent(t, filepath.Join(managed, "shared.md"), "shared", "opus")
	writeAgent(t, filepath.Join(repo, ".claude", "agents", "shared.md"), "shared", "sonnet")

	reg, _ := Discover(Options{
		CWD:        repo,
		HomeDir:    tmp,
		ManagedDir: managed,
	})
	got, ok := reg.Get("shared")
	if !ok {
		t.Fatal("shared not found")
	}
	if got.Source != SourceManaged {
		t.Errorf("winner Source = %v, want managed", got.Source)
	}
	// El de project tiene que aparecer en shadows.
	if len(reg.Shadows()) != 1 || reg.Shadows()[0].Source != SourceProject {
		t.Errorf("shadows = %+v, want 1 project entry", reg.Shadows())
	}
}

// TestDiscover_CustomShadowsBuiltinEmitsWarning: un agente custom
// llamado claude-opus debe tapar al built-in pero emitir un
// CollisionWarning (§2.a "con warning al cargar para evitar confusión").
func TestDiscover_CustomShadowsBuiltinEmitsWarning(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	writeAgent(t, filepath.Join(repo, ".claude", "agents", "claude-opus.md"), "claude-opus", "haiku")

	reg, errs := Discover(Options{
		CWD:             repo,
		HomeDir:         tmp,
		IncludeBuiltins: true,
	})
	winner, _ := reg.Get("claude-opus")
	if winner.Source != SourceProject {
		t.Errorf("winner Source = %v, want project", winner.Source)
	}
	var warn CollisionWarning
	found := false
	for _, e := range errs {
		if errors.As(e, &warn) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a CollisionWarning, got: %v", errs)
	}
	if warn.Name != "claude-opus" || warn.Winner != SourceProject || warn.ShadowedSource != SourceBuiltin {
		t.Errorf("warn = %+v", warn)
	}
}

// TestDiscover_WalkUpProject: el .claude/agents puede vivir varios
// niveles arriba del CWD. Discover debe encontrarlo.
func TestDiscover_WalkUpProject(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "monorepo")
	deep := filepath.Join(root, "packages", "foo", "src")
	writeAgent(t, filepath.Join(root, ".claude", "agents", "monorepo-agent.md"), "monorepo-agent", "opus")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}
	reg, _ := Discover(Options{CWD: deep, HomeDir: tmp})
	if _, ok := reg.Get("monorepo-agent"); !ok {
		t.Error("walk-up failed to find monorepo-agent")
	}
}

// TestDiscover_BadFileNonFatal: un .md mal formado no debe abortar
// el discover. El error se acumula en el slice y los demás agentes
// siguen apareciendo.
func TestDiscover_BadFileNonFatal(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	dir := filepath.Join(repo, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Un archivo bueno, uno malo.
	writeAgent(t, filepath.Join(dir, "good.md"), "good-agent", "opus")
	if err := os.WriteFile(filepath.Join(dir, "bad.md"), []byte("no frontmatter here\n"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	reg, errs := Discover(Options{CWD: repo, HomeDir: tmp})
	if _, ok := reg.Get("good-agent"); !ok {
		t.Error("good-agent missing — discover bailed out on bad neighbor")
	}
	if len(errs) == 0 {
		t.Error("expected at least one error from bad.md")
	}
}

// TestRegistry_GetUnknown: nombre no existente devuelve ok=false.
func TestRegistry_GetUnknown(t *testing.T) {
	tmp := t.TempDir()
	reg, _ := Discover(Options{CWD: tmp, HomeDir: tmp, IncludeBuiltins: true})
	if _, ok := reg.Get("does-not-exist"); ok {
		t.Error("Get on unknown returned ok=true")
	}
}
