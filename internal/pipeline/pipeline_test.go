package pipeline

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// updateGolden permite regenerar `testdata/default.json` con
// `go test ./internal/pipeline -update`. Útil cuando se modifica Default()
// o el shape de los tipos a propósito — el test va a fallar y la flag
// regenera el golden en vez de obligar a editarlo a mano.
var updateGolden = flag.Bool("update", false, "regenerate golden files in testdata/")

const defaultGolden = "testdata/default.json"

// TestDefault_MatchesGolden chequea drift entre Default() y el JSON canónico
// del PRD §4.b. Si esta prueba rompe, una de dos cosas pasó:
//
//  1. Cambió Default() a propósito → regenerar con -update.
//  2. Cambió el shape de los tipos sin querer → revisar el diff antes de
//     regenerar.
//
// El test serializa con MarshalIndent + 2 espacios para normalizar el
// formato; cualquier diff por whitespace queda en el golden, no en runtime.
func TestDefault_MatchesGolden(t *testing.T) {
	got, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		t.Fatalf("marshal Default(): %v", err)
	}

	if *updateGolden {
		if err := os.WriteFile(defaultGolden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", defaultGolden)
		return
	}

	want, err := os.ReadFile(defaultGolden)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create)", defaultGolden, err)
	}
	// Tolerar trailing newline en el golden (editores lo agregan, json
	// no). Recortar para comparar contenido real.
	want = bytes.TrimRight(want, "\n")
	got = bytes.TrimRight(got, "\n")
	if !bytes.Equal(got, want) {
		t.Errorf("Default() drift vs %s\n--- got ---\n%s\n--- want ---\n%s",
			defaultGolden, got, want)
	}
}

// TestDefault_RoundTrip verifica que el JSON serializado se parsea de vuelta
// al mismo struct. Cuida contra bugs sutiles (typo en json tag, omitempty
// que tira un campo obligatorio, etc).
func TestDefault_RoundTrip(t *testing.T) {
	want := Default()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Pipeline
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch\n--- want ---\n%+v\n--- got ---\n%+v", want, got)
	}
}

// TestDefault_Shape pone aserciones explícitas sobre el shape canónico para
// que un drift accidental falle con error legible (no sólo "byte mismatch
// en línea N del golden"). Cubre el contrato narrativo del PRD §4.b.
func TestDefault_Shape(t *testing.T) {
	p := Default()

	if p.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", p.Version, CurrentVersion)
	}
	if p.Entry != nil {
		t.Errorf("Entry = %+v, want nil (Default no rebota inputs)", p.Entry)
	}

	wantNames := []string{
		"idea",
		"explore",
		"validate_issue",
		"execute",
		"validate_pr",
		"close",
	}
	if len(p.Steps) != len(wantNames) {
		t.Fatalf("len(Steps) = %d, want %d", len(p.Steps), len(wantNames))
	}
	for i, name := range wantNames {
		if p.Steps[i].Name != name {
			t.Errorf("Steps[%d].Name = %q, want %q", i, p.Steps[i].Name, name)
		}
		if len(p.Steps[i].Agents) == 0 {
			t.Errorf("Steps[%d] (%s) has no agents", i, name)
		}
	}
}

// TestSchemaFileExists guardrail: si alguien borra el schema (o lo mueve)
// el test falla acá en vez de en producción cuando un IDE intenta cargarlo.
// El path es relativo al root del repo — los tests se corren con cwd en
// `internal/pipeline`, así que subimos dos niveles.
func TestSchemaFileExists(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	schema := filepath.Join(repoRoot, "schemas", "pipeline.json")
	info, err := os.Stat(schema)
	if err != nil {
		t.Fatalf("stat %s: %v", schema, err)
	}
	if info.Size() == 0 {
		t.Errorf("%s is empty", schema)
	}
	// Sanity check superficial del contenido — sin validador JSON Schema en
	// stdlib, sólo verificamos que parsea como JSON y declara el $id
	// esperado. Validación profunda llega en PR3 (loader).
	raw, err := os.ReadFile(schema)
	if err != nil {
		t.Fatalf("read %s: %v", schema, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if doc["$id"] == nil {
		t.Errorf("schema missing $id")
	}
	if doc["$schema"] == nil {
		t.Errorf("schema missing $schema (meta-schema URL)")
	}
}

// TestAggregator_Validity asegura que las constantes y la lista canónica
// coinciden. Mantiene IsValid honesto si alguien agrega un Aggregator nuevo
// y se olvida de meterlo en ValidAggregators.
func TestAggregator_Validity(t *testing.T) {
	for _, a := range []Aggregator{AggregatorMajority, AggregatorUnanimous, AggregatorFirstBlocker} {
		if !a.IsValid() {
			t.Errorf("%q should be valid", a)
		}
		if a.Description() == "" {
			t.Errorf("%q should have UI description", a)
		}
	}
	if Aggregator("").IsValid() {
		t.Errorf("empty aggregator should NOT be valid")
	}
	if Aggregator("magic").IsValid() {
		t.Errorf("unknown aggregator should NOT be valid")
	}
	if len(ValidAggregators) != 3 {
		t.Errorf("ValidAggregators length = %d, want 3 (drift en presets v1)", len(ValidAggregators))
	}
}
