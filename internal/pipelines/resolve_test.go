package pipelines

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chichex/che/internal/wizard"
)

// writePipelineYAML escribe un pipeline minimo viable parseable por
// wizard.Load. Helper compartido entre tests de resolve/list/persist.
func writePipelineYAML(t *testing.T, path, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "name: " + name + "\nsteps:\n  - name: paso1\n    cli: claude\n    kind: prompt\n    content: hola\n    input: none\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestResolve_ProjectOverridesGlobal(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	writePipelineYAML(t, filepath.Join(cwd, ".che", "pipelines", "demo.yaml"), "demo-project")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "demo.yaml"), "demo-global")

	got, found, err := Resolve(cwd, home, "demo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if got.Scope != ScopeProject {
		t.Errorf("scope = %v, want ScopeProject", got.Scope)
	}
	if got.Pipeline.Name != "demo-project" {
		t.Errorf("pipeline.Name = %q, want demo-project", got.Pipeline.Name)
	}
}

func TestResolve_FallsBackToGlobal(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	// solo global existe
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "alpha.yaml"), "alpha")

	got, found, err := Resolve(cwd, home, "alpha")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if got.Scope != ScopeGlobal {
		t.Errorf("scope = %v, want ScopeGlobal", got.Scope)
	}
}

func TestResolve_FallsBackToBuiltin(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	got, found, err := Resolve(cwd, home, "che-funnel")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !found {
		t.Fatalf("expected che-funnel builtin to resolve")
	}
	if got.Scope != ScopeBuiltin {
		t.Errorf("scope = %v, want ScopeBuiltin", got.Scope)
	}
	if !wizard.IsBuiltinTarget(got.Target) {
		t.Errorf("target = %q, want builtin: prefix", got.Target)
	}
}

func TestResolve_NotFound(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	_, found, err := Resolve(cwd, home, "nope-no-existo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for missing slug")
	}
}

func TestResolve_EmptyCwdSkipsProject(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "x.yaml"), "x")

	got, found, err := Resolve("", home, "x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if got.Scope != ScopeGlobal {
		t.Errorf("scope = %v, want ScopeGlobal", got.Scope)
	}
}

func TestResolve_EmptySlugReturnsNotFound(t *testing.T) {
	tmp := t.TempDir()
	_, found, err := Resolve(tmp, tmp, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for empty slug")
	}
}
