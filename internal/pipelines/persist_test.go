package pipelines

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chichex/che/internal/wizard"
)

func TestSaveScoped_Global(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")

	p := wizard.Pipeline{
		Name:        "demo",
		Description: "test",
		Steps: []wizard.Step{
			{Name: "s1", CLI: "claude", Kind: "prompt", Content: "hola", Input: "none"},
		},
	}

	path, err := SaveScoped(ScopeGlobal, "", home, "demo", p)
	if err != nil {
		t.Fatalf("SaveScoped: %v", err)
	}
	want := filepath.Join(home, ".che", "pipelines", "demo.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist: %v", err)
	}
}

func TestSaveScoped_Project(t *testing.T) {
	tmp := t.TempDir()
	cwd := filepath.Join(tmp, "proj")

	p := wizard.Pipeline{
		Name: "demo",
		Steps: []wizard.Step{
			{Name: "s1", CLI: "claude", Kind: "prompt", Content: "hola", Input: "none"},
		},
	}

	path, err := SaveScoped(ScopeProject, cwd, "", "demo", p)
	if err != nil {
		t.Fatalf("SaveScoped: %v", err)
	}
	want := filepath.Join(cwd, ".che", "pipelines", "demo.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestSaveScoped_BuiltinFails(t *testing.T) {
	_, err := SaveScoped(ScopeBuiltin, "/tmp", "/tmp", "x", wizard.Pipeline{})
	if err == nil {
		t.Errorf("expected error saving builtin scope")
	}
}

func TestSaveScoped_ProjectWithEmptyCwdFails(t *testing.T) {
	_, err := SaveScoped(ScopeProject, "", "/tmp", "x", wizard.Pipeline{})
	if err == nil {
		t.Errorf("expected error saving project with empty cwd")
	}
}

func TestSaveScoped_EmptySlugFails(t *testing.T) {
	_, err := SaveScoped(ScopeGlobal, "", "/tmp", "", wizard.Pipeline{})
	if err == nil {
		t.Errorf("expected error saving with empty slug")
	}
}

func TestPathHelpers(t *testing.T) {
	if got := ProjectPipelinesDir(""); got != "" {
		t.Errorf("ProjectPipelinesDir(\"\") = %q, want \"\"", got)
	}
	if got := ProjectPipelinesDir("/p"); got != filepath.Join("/p", ".che", "pipelines") {
		t.Errorf("ProjectPipelinesDir: %q", got)
	}
	if got := ProjectPathFor("/p", "x"); got != filepath.Join("/p", ".che", "pipelines", "x.yaml") {
		t.Errorf("ProjectPathFor: %q", got)
	}
	if got := ProjectPathFor("", "x"); got != "" {
		t.Errorf("ProjectPathFor empty cwd: %q", got)
	}
}

func TestScopeString(t *testing.T) {
	cases := map[Scope]string{
		ScopeProject: "project",
		ScopeGlobal:  "global",
		ScopeBuiltin: "builtin",
		ScopeUnknown: "",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("Scope(%d).String() = %q, want %q", s, got, want)
		}
	}
}
