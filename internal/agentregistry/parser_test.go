package agentregistry

import (
	"errors"
	"strings"
	"testing"
)

// TestParse_Canonical es el contract test contra la doc oficial de
// Claude Code (testdata/contract/canonical.md). Si Claude Code rota el
// formato, esto rompe — y los discover/list que dependen del parser
// con él. El fixture es la versión pinneada del PRD §2.a.
func TestParse_Canonical(t *testing.T) {
	fm, err := ParseFile("testdata/contract/canonical.md")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "plan-reviewer-strict" {
		t.Errorf("Name = %q, want plan-reviewer-strict", fm.Name)
	}
	if !strings.HasPrefix(fm.Description, "Valida plans con foco") {
		t.Errorf("Description = %q, want prefix 'Valida plans con foco'", fm.Description)
	}
	if fm.Model != "opus" {
		t.Errorf("Model = %q, want opus", fm.Model)
	}
}

// TestParse_TolerantToUnknownFields verifica que campos como `color`,
// `tools`, `capabilities`, `hooks`, `mcpServers`, `permissionMode` y
// hipotéticos campos futuros no rompen el parse — sólo se ignoran.
// Esto es lo que el PRD pide explícitamente como "parser tolerante a
// campos desconocidos".
func TestParse_TolerantToUnknownFields(t *testing.T) {
	fm, err := ParseFile("testdata/contract/with-unknown-fields.md")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "future-agent" {
		t.Errorf("Name = %q, want future-agent", fm.Name)
	}
	if fm.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", fm.Model)
	}
}

func TestParse_MinimalOnlyName(t *testing.T) {
	fm, err := ParseFile("testdata/contract/minimal.md")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "minimal-agent" {
		t.Errorf("Name = %q, want minimal-agent", fm.Name)
	}
	if fm.Model != "" || fm.Description != "" {
		t.Errorf("Model/Description should be empty: %+v", fm)
	}
}

func TestParse_QuotedScalars(t *testing.T) {
	fm, err := ParseFile("testdata/contract/quoted.md")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if fm.Name != "quoted-agent" {
		t.Errorf("Name = %q, want quoted-agent (sin comillas)", fm.Name)
	}
	if fm.Model != "haiku" {
		t.Errorf("Model = %q, want haiku (sin comillas)", fm.Model)
	}
	if !strings.Contains(fm.Description, "contiene un colon") {
		t.Errorf("Description perdió el contenido tras el colon: %q", fm.Description)
	}
}

func TestParse_ErrNoFrontmatter(t *testing.T) {
	_, err := ParseFile("testdata/contract/no-frontmatter.md")
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Errorf("err = %v, want ErrNoFrontmatter", err)
	}
}

func TestParse_ErrNameMissing(t *testing.T) {
	_, err := ParseFile("testdata/contract/no-name.md")
	if !errors.Is(err, ErrNameMissing) {
		t.Errorf("err = %v, want ErrNameMissing", err)
	}
}

func TestParse_FromString(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		want    Frontmatter
	}{
		{
			name: "missing closing ---",
			in:   "---\nname: foo\n",
			// Sin cierre falla — más conservador que asumir EOF como cierre.
			wantErr: true,
		},
		{
			name: "first line not ---",
			in:   "name: foo\n---\n",
			wantErr: true,
		},
		{
			name: "comment in frontmatter",
			in:   "---\n# comentario\nname: foo\nmodel: opus\n---\n",
			want: Frontmatter{Name: "foo", Model: "opus"},
		},
		{
			name: "indented field is ignored (nested array)",
			in:   "---\nname: foo\ntools:\n  - Read\n  - Bash\n---\n",
			want: Frontmatter{Name: "foo"},
		},
		{
			name: "list item at top level is ignored",
			in:   "---\nname: foo\n- bar\n---\n",
			want: Frontmatter{Name: "foo"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, err := Parse(strings.NewReader(c.in))
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got %+v", fm)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fm != c.want {
				t.Errorf("fm = %+v, want %+v", fm, c.want)
			}
		})
	}
}
