package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_DefaultRoundTrip valida que el JSON canónico del PRD §4.b
// (que también es el golden de Default()) carga limpio. Cierre del loop
// types/loader: si Default() y el loader están desincronizados, esto
// rompe primero — antes que cualquier flow que invoque pipelines.
func TestLoad_DefaultRoundTrip(t *testing.T) {
	p, err := Load("testdata/default.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	if p.Version != want.Version {
		t.Errorf("Version = %d, want %d", p.Version, want.Version)
	}
	if len(p.Steps) != len(want.Steps) {
		t.Fatalf("len(Steps) = %d, want %d", len(p.Steps), len(want.Steps))
	}
	for i := range p.Steps {
		if p.Steps[i].Name != want.Steps[i].Name {
			t.Errorf("Steps[%d].Name = %q, want %q", i, p.Steps[i].Name, want.Steps[i].Name)
		}
	}
}

// TestLoadBytes_RejectsUnknownVersion: la spec dice "rechazar versiones
// desconocidas pidiendo upgrade" (PRD §4.a). El error tiene que ser
// suficientemente explícito para que el usuario sepa qué hacer.
func TestLoadBytes_RejectsUnknownVersion(t *testing.T) {
	raw := []byte(`{"version": 2, "steps": [{"name":"x","agents":["claude-opus"]}]}`)
	_, err := LoadBytes(raw)
	if err == nil {
		t.Fatal("expected error for version=2")
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "version" {
		t.Errorf("Field = %q, want version", le.Field)
	}
	if !strings.Contains(le.Reason, "upgrade") {
		t.Errorf("Reason = %q, expected to mention 'upgrade'", le.Reason)
	}
}

// TestLoadBytes_RejectsZeroSteps: los pipelines vacíos no tienen sentido
// (no hay nada que correr). El loader debe rechazar como decisión
// explícita, no por accidente del schema.
func TestLoadBytes_RejectsZeroSteps(t *testing.T) {
	raw := []byte(`{"version": 1, "steps": []}`)
	_, err := LoadBytes(raw)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "steps" {
		t.Errorf("Field = %q, want steps", le.Field)
	}
}

// TestLoadBytes_SyntaxErrorWithLineColumn verifica el contrato clave
// del PR3: "errores con línea + campo via json.SyntaxError.Offset".
// Construimos un JSON con una coma de más al inicio de la 3a línea y
// chequeamos que LoadError.Line apunte ahí.
func TestLoadBytes_SyntaxErrorWithLineColumn(t *testing.T) {
	raw := []byte(`{
  "version": 1,
  "steps":, [
    {"name": "x", "agents": ["claude-opus"]}
  ]
}`)
	_, err := LoadBytes(raw)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Line != 3 {
		t.Errorf("Line = %d, want 3 (the comma is on line 3)", le.Line)
	}
	if le.Column < 1 {
		t.Errorf("Column = %d, want >= 1", le.Column)
	}
	if !strings.Contains(le.Reason, "invalid JSON") {
		t.Errorf("Reason = %q, expected 'invalid JSON' prefix", le.Reason)
	}
}

// TestLoad_AttachesPath asegura que `Load(path)` rellena `Path` en el
// LoadError aunque el error venga del decoder. El call site formatea
// "<path>:<line>:<col>: <reason>" y depende de los 3 campos.
func TestLoad_AttachesPath(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "broken.json")
	if err := os.WriteFile(bad, []byte("{ bogus"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(bad)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Path != bad {
		t.Errorf("Path = %q, want %q", le.Path, bad)
	}
	// Un Error() bien formado tiene que incluir el path al inicio.
	if !strings.HasPrefix(le.Error(), bad) {
		t.Errorf("Error() = %q, want prefix %q", le.Error(), bad)
	}
}

// TestLoadBytes_TypeErrorReportsField: el schema declara `version: 1`
// como const numeric. Si alguien manda un string, queremos que el error
// nombre el campo culpable (typeErr.Field), no sólo "JSON inválido".
func TestLoadBytes_TypeErrorReportsField(t *testing.T) {
	raw := []byte(`{"version": "uno", "steps": [{"name":"x","agents":["a"]}]}`)
	_, err := LoadBytes(raw)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field == "" {
		t.Errorf("Field = %q, want non-empty (json type error has Field)", le.Field)
	}
}

// TestLoadBytes_RejectsUnknownField espeja el `additionalProperties:
// false` del schema: campos extra son típicamente typos (`agnts` vs
// `agents`) que el strict mode atrapa antes de que el motor los ignore
// silenciosamente.
func TestLoadBytes_RejectsUnknownField(t *testing.T) {
	raw := []byte(`{"version": 1, "steps": [{"name":"x","agents":["a"]}], "bogus": true}`)
	_, err := LoadBytes(raw)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "bogus" {
		t.Errorf("Field = %q, want bogus", le.Field)
	}
}

// TestLoadBytes_TrailingData: `{...}{...}` no es un pipeline válido,
// indica concatenación accidental o un export raro. dec.More() detecta
// el segundo objeto y debería abortar.
func TestLoadBytes_TrailingData(t *testing.T) {
	raw := []byte(`{"version": 1, "steps": [{"name":"x","agents":["a"]}]} {"junk":1}`)
	_, err := LoadBytes(raw)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if !strings.Contains(le.Reason, "trailing data") {
		t.Errorf("Reason = %q, expected 'trailing data'", le.Reason)
	}
}

// TestLoadError_Error verifica el formato compuesto del mensaje:
// "<path>:<line>:<col>: field \"X\": <reason>". Útil para que diff de
// otros tests no falle por un cambio cosmético — fijamos el shape.
func TestLoadError_Error(t *testing.T) {
	cases := []struct {
		name string
		le   *LoadError
		want string
	}{
		{
			name: "full",
			le:   &LoadError{Path: "/p/x.json", Line: 3, Column: 7, Field: "steps[0].name", Reason: "bad"},
			want: `/p/x.json:3:7: field "steps[0].name": bad`,
		},
		{
			name: "no-col",
			le:   &LoadError{Path: "/p/x.json", Line: 3, Reason: "bad"},
			want: "/p/x.json:3: bad",
		},
		{
			name: "field-only",
			le:   &LoadError{Field: "version", Reason: "bad"},
			want: `field "version": bad`,
		},
		{
			name: "reason-only",
			le:   &LoadError{Reason: "bad"},
			want: "bad",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.le.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOffsetToLineCol cubre el cálculo manual de line/col que usa
// LoadError. El gotcha es: encoding/json a veces devuelve Offset =
// len(data) cuando el error es "EOF inesperado", y el helper tiene que
// no panickear con out-of-range.
func TestOffsetToLineCol(t *testing.T) {
	data := []byte("ab\ncd\nef")
	cases := []struct {
		off       int
		wantLine  int
		wantCol   int
	}{
		{0, 1, 1},
		{1, 1, 2},
		{2, 1, 3},  // posición del \n
		{3, 2, 1},  // arranque de "cd"
		{6, 3, 1},  // arranque de "ef"
		{8, 3, 3},  // fin del buffer
		{99, 3, 3}, // out-of-range, debe clamparse
		{-1, 0, 0}, // negativo → desconocido
	}
	for _, tc := range cases {
		gotL, gotC := offsetToLineCol(data, tc.off)
		if gotL != tc.wantLine || gotC != tc.wantCol {
			t.Errorf("offsetToLineCol(%d) = (%d,%d), want (%d,%d)",
				tc.off, gotL, gotC, tc.wantLine, tc.wantCol)
		}
	}
}
