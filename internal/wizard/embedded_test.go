package wizard

import (
	"strings"
	"testing"
)

// TestBuiltins_ParseAndStructure verifica que cada builtin parsea y tiene
// la forma minima esperada. No corremos IsValid aca porque depende de la
// deteccion de CLIs (el harness de tests no instala claude/codex/gemini).
// IsValid se cubre con TestBuiltins_IsValidWithMockedCLIs abajo.
func TestBuiltins_ParseAndStructure(t *testing.T) {
	bs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins() error: %v", err)
	}
	if len(bs) == 0 {
		t.Fatal("Builtins() devolvio vacio — esperabamos al menos che-funnel")
	}

	wantSlugs := map[string]bool{"che-funnel": true}
	for _, b := range bs {
		if !wantSlugs[b.Slug] {
			t.Errorf("slug inesperado: %q", b.Slug)
		}
		if len(b.Source) == 0 {
			t.Errorf("%s: Source vacio", b.Slug)
		}
		if b.Pipeline.Name == "" {
			t.Errorf("%s: Pipeline.Name vacio", b.Slug)
		}
		if len(b.Pipeline.Steps) == 0 {
			t.Errorf("%s: Pipeline.Steps vacio", b.Slug)
		}
	}
}

func TestBuiltins_CheFunnelShape(t *testing.T) {
	bs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins() error: %v", err)
	}
	var cf *BuiltinPipeline
	for i := range bs {
		if bs[i].Slug == "che-funnel" {
			cf = &bs[i]
			break
		}
	}
	if cf == nil {
		t.Fatal("che-funnel no encontrado en builtins")
	}

	wantSteps := []string{"idea", "explore", "execute", "close"}
	if got := len(cf.Pipeline.Steps); got != len(wantSteps) {
		t.Fatalf("steps len: got %d want %d", got, len(wantSteps))
	}
	for i, want := range wantSteps {
		if got := cf.Pipeline.Steps[i].Name; got != want {
			t.Errorf("step[%d].Name: got %q want %q", i, got, want)
		}
	}

	// explore y execute deben tener validator (la "iteracion" del che
	// clasico vive ahi via max_loops).
	for _, idx := range []int{1, 2} {
		s := cf.Pipeline.Steps[idx]
		if s.Validator == nil {
			t.Errorf("step[%d] (%s): validator esperado no nil", idx, s.Name)
			continue
		}
		if s.MaxLoops <= 0 {
			t.Errorf("step[%d] (%s): max_loops esperado > 0, got %d", idx, s.Name, s.MaxLoops)
		}
	}

	// idea es el primer step y NO puede usar previous_output.
	if cf.Pipeline.Steps[0].Input == InputPreviousOutput {
		t.Errorf("step[0] (idea): input=previous_output no es valido en step 0")
	}
}

func TestBuiltins_IsValidWithMockedCLIs(t *testing.T) {
	prev := detectInstalledCLIs
	t.Cleanup(func() { detectInstalledCLIs = prev })
	detectInstalledCLIs = func() []string { return []string{"claude", "codex", "gemini", "opencode"} }

	bs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins() error: %v", err)
	}
	for _, b := range bs {
		if err := IsValid(b.Pipeline); err != nil {
			t.Errorf("%s: IsValid error: %v", b.Slug, err)
		}
	}
}

// TestBuiltins_SourceIsRoundtripParseable garantiza que Source es el YAML
// crudo y no una serializacion derivada (necesario para copy-on-edit:
// cuando escribimos el archivo al FS, el usuario tiene que ver el mismo
// contenido que ve el binario).
func TestBuiltins_SourceIsRoundtripParseable(t *testing.T) {
	bs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins() error: %v", err)
	}
	for _, b := range bs {
		p, err := Unmarshal(b.Source)
		if err != nil {
			t.Errorf("%s: re-Unmarshal del Source fallo: %v", b.Slug, err)
			continue
		}
		if p.Name != b.Pipeline.Name {
			t.Errorf("%s: roundtrip Name mismatch: %q vs %q", b.Slug, p.Name, b.Pipeline.Name)
		}
		if len(p.Steps) != len(b.Pipeline.Steps) {
			t.Errorf("%s: roundtrip Steps len mismatch: %d vs %d", b.Slug, len(p.Steps), len(b.Pipeline.Steps))
		}
	}

	// Sanity: el header del YAML va a empezar con un comentario "che-funnel".
	// Si rompemos el orden o lo regeneramos perderiamos el comment block.
	for _, b := range bs {
		if b.Slug == "che-funnel" {
			head := string(b.Source)
			if !strings.Contains(head, "che-funnel") {
				t.Errorf("che-funnel: Source no contiene el slug en el header")
			}
		}
	}
}
