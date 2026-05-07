package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// stubLookPath instala un lookPath fake para el test y restaura el original
// al terminar. Sin esto, los tests dependerian de que el ejecutable este o
// no en el PATH del runner.
func stubLookPath(t *testing.T, installed map[string]string) {
	t.Helper()
	prev := lookPath
	lookPath = func(name string) (string, bool) {
		p, ok := installed[name]
		return p, ok
	}
	t.Cleanup(func() { lookPath = prev })
}

// fakeHome apunta UserHomeDir a un directorio temporal. Devuelve el path
// para que el test arme el layout esperado adentro.
func fakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findCLI(clis []CLI, name string) *CLI {
	for i := range clis {
		if clis[i].Name == name {
			return &clis[i]
		}
	}
	return nil
}

func findSkill(skills []Skill, name string) *Skill {
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i]
		}
	}
	return nil
}

func TestDetect_NoCLIsInstalled(t *testing.T) {
	stubLookPath(t, nil)
	fakeHome(t)
	cwd := t.TempDir()

	clis := Detect(cwd)
	if len(clis) != 4 {
		t.Fatalf("expected 4 CLI entries, got %d", len(clis))
	}
	for _, c := range clis {
		if c.Installed {
			t.Errorf("%s should not be installed", c.Name)
		}
		if len(c.Skills) != 0 {
			t.Errorf("%s should have no skills when not installed, got %d", c.Name, len(c.Skills))
		}
	}
}

func TestDetect_ClaudeFrontmatterAndFallback(t *testing.T) {
	stubLookPath(t, map[string]string{"claude": "/fake/claude"})
	home := fakeHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".claude/skills/with-fm/SKILL.md"), `---
name: with-fm
description: Has frontmatter description
---

Body content here.
`)
	writeFile(t, filepath.Join(home, ".claude/skills/no-fm/SKILL.md"), `First line is the description.

Second paragraph should be ignored.
`)
	writeFile(t, filepath.Join(cwd, ".claude/skills/proj-skill/SKILL.md"), `---
description: project scope skill
---
`)

	clis := Detect(cwd)
	c := findCLI(clis, "claude")
	if c == nil || !c.Installed {
		t.Fatalf("claude should be installed: %+v", c)
	}
	if len(c.Skills) != 3 {
		t.Fatalf("expected 3 claude skills, got %d: %+v", len(c.Skills), c.Skills)
	}

	wf := findSkill(c.Skills, "with-fm")
	if wf == nil || wf.Description != "Has frontmatter description" || wf.Scope != ScopeUser {
		t.Errorf("with-fm wrong: %+v", wf)
	}
	nf := findSkill(c.Skills, "no-fm")
	if nf == nil || nf.Description != "First line is the description." {
		t.Errorf("no-fm fallback wrong: %+v", nf)
	}
	ps := findSkill(c.Skills, "proj-skill")
	if ps == nil || ps.Scope != ScopeProject || ps.Description != "project scope skill" {
		t.Errorf("proj-skill wrong: %+v", ps)
	}
}

func TestDetect_GeminiTomlDescription(t *testing.T) {
	stubLookPath(t, map[string]string{"gemini": "/fake/gemini"})
	home := fakeHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".gemini/commands/has-desc.toml"), `description = "Gemini cmd with desc"
prompt = "ignored"
`)
	writeFile(t, filepath.Join(home, ".gemini/commands/no-desc.toml"), `prompt = "ignored"
`)
	writeFile(t, filepath.Join(home, ".gemini/commands/notes.txt"), `should be ignored
`)

	clis := Detect(cwd)
	g := findCLI(clis, "gemini")
	if g == nil || len(g.Skills) != 2 {
		t.Fatalf("expected 2 gemini skills, got: %+v", g)
	}
	hd := findSkill(g.Skills, "has-desc")
	if hd == nil || hd.Description != "Gemini cmd with desc" {
		t.Errorf("has-desc wrong: %+v", hd)
	}
	nd := findSkill(g.Skills, "no-desc")
	if nd == nil || nd.Description != "" {
		t.Errorf("no-desc should have empty description: %+v", nd)
	}
}

func TestDetect_OpencodeAndCodexLayouts(t *testing.T) {
	stubLookPath(t, map[string]string{
		"codex":    "/fake/codex",
		"opencode": "/fake/opencode",
	})
	home := fakeHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".codex/skills/codex-skill/SKILL.md"), `---
name: codex-skill
description: codex global
---
`)
	writeFile(t, filepath.Join(home, ".config/opencode/skills/oc-skill/SKILL.md"), `---
description: opencode global
---
`)
	writeFile(t, filepath.Join(cwd, ".opencode/skills/oc-proj/SKILL.md"), `---
description: opencode project
---
`)

	clis := Detect(cwd)
	c := findCLI(clis, "codex")
	if c == nil || len(c.Skills) != 1 || c.Skills[0].Description != "codex global" {
		t.Errorf("codex wrong: %+v", c)
	}
	o := findCLI(clis, "opencode")
	if o == nil || len(o.Skills) != 2 {
		t.Fatalf("opencode expected 2 skills, got: %+v", o)
	}
	if findSkill(o.Skills, "oc-skill") == nil || findSkill(o.Skills, "oc-proj") == nil {
		t.Errorf("opencode missing skills: %+v", o.Skills)
	}
}

func TestDetect_OrderUserBeforeProjectThenAlpha(t *testing.T) {
	stubLookPath(t, map[string]string{"claude": "/fake/claude"})
	home := fakeHome(t)
	cwd := t.TempDir()

	writeFile(t, filepath.Join(home, ".claude/skills/zeta/SKILL.md"), `desc`)
	writeFile(t, filepath.Join(home, ".claude/skills/alpha/SKILL.md"), `desc`)
	writeFile(t, filepath.Join(cwd, ".claude/skills/beta/SKILL.md"), `desc`)

	clis := Detect(cwd)
	c := findCLI(clis, "claude")
	if c == nil || len(c.Skills) != 3 {
		t.Fatalf("expected 3 skills, got %+v", c)
	}
	want := []struct {
		name  string
		scope Scope
	}{
		{"alpha", ScopeUser},
		{"zeta", ScopeUser},
		{"beta", ScopeProject},
	}
	for i, w := range want {
		if c.Skills[i].Name != w.name || c.Skills[i].Scope != w.scope {
			t.Errorf("skill[%d] = %+v, want name=%s scope=%s", i, c.Skills[i], w.name, w.scope)
		}
	}
}
