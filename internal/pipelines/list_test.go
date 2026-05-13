package pipelines

import (
	"path/filepath"
	"testing"
)

func TestList_DualScopeWithOverride(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	// proyecto y global definen "demo" → gana project; global tiene "only-global".
	writePipelineYAML(t, filepath.Join(cwd, ".che", "pipelines", "demo.yaml"), "demo-project")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "demo.yaml"), "demo-global")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "only-global.yaml"), "only-global")

	got, err := List(cwd, home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	bySlug := map[string]Resolved{}
	for _, r := range got {
		bySlug[r.Slug] = r
	}

	demo, ok := bySlug["demo"]
	if !ok {
		t.Fatalf("expected demo in list")
	}
	if demo.Scope != ScopeProject {
		t.Errorf("demo.Scope = %v, want ScopeProject", demo.Scope)
	}
	if demo.Pipeline.Name != "demo-project" {
		t.Errorf("demo.Pipeline.Name = %q, want demo-project", demo.Pipeline.Name)
	}

	onlyGlobal, ok := bySlug["only-global"]
	if !ok {
		t.Fatalf("expected only-global in list")
	}
	if onlyGlobal.Scope != ScopeGlobal {
		t.Errorf("only-global.Scope = %v, want ScopeGlobal", onlyGlobal.Scope)
	}

	// builtin che-funnel siempre disponible (no esta shadow-eado por FS).
	cheFunnel, ok := bySlug["che-funnel"]
	if !ok {
		t.Fatalf("expected che-funnel builtin in list")
	}
	if cheFunnel.Scope != ScopeBuiltin {
		t.Errorf("che-funnel.Scope = %v, want ScopeBuiltin", cheFunnel.Scope)
	}
}

func TestList_BuiltinShadowedByProject(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	// Override del builtin desde scope project.
	writePipelineYAML(t, filepath.Join(cwd, ".che", "pipelines", "che-funnel.yaml"), "mi-che-funnel")

	got, err := List(cwd, home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var cheFunnel *Resolved
	for i := range got {
		if got[i].Slug == "che-funnel" {
			cheFunnel = &got[i]
		}
	}
	if cheFunnel == nil {
		t.Fatalf("expected che-funnel in list")
	}
	if cheFunnel.Scope != ScopeProject {
		t.Errorf("scope = %v, want ScopeProject (override)", cheFunnel.Scope)
	}
	if cheFunnel.Pipeline.Name != "mi-che-funnel" {
		t.Errorf("Pipeline.Name = %q, want mi-che-funnel", cheFunnel.Pipeline.Name)
	}
}

func TestList_MissingProjectDir(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "no-existe")
	home := filepath.Join(tmp, "home")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "alpha.yaml"), "alpha")

	got, err := List(cwd, home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Espero al menos alpha (global) y che-funnel (builtin).
	var sawAlpha, sawCheFunnel bool
	for _, r := range got {
		if r.Slug == "alpha" {
			sawAlpha = true
		}
		if r.Slug == "che-funnel" {
			sawCheFunnel = true
		}
	}
	if !sawAlpha {
		t.Errorf("expected alpha in list")
	}
	if !sawCheFunnel {
		t.Errorf("expected che-funnel builtin in list")
	}
}

func TestList_OrderedBySlug(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")
	home := filepath.Join(tmp, "home")

	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "zeta.yaml"), "z")
	writePipelineYAML(t, filepath.Join(home, ".che", "pipelines", "alpha.yaml"), "a")

	got, err := List(cwd, home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// alpha < che-funnel < zeta
	for i := 1; i < len(got); i++ {
		if got[i-1].Slug > got[i].Slug {
			t.Errorf("not sorted: %q > %q at i=%d", got[i-1].Slug, got[i].Slug, i)
		}
	}
}
