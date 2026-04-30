package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// TestPipelineShow_HumanSummary: pipeline on-disk, sin --json. Debe
// imprimir header con name+path, mostrar version, listar steps y agentes.
func TestPipelineShow_HumanSummary(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	if err := runPipelineShow(&out, mgr, "fast", false); err != nil {
		t.Fatalf("runPipelineShow: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"pipeline: fast",
		"fast.json",
		"version: 1",
		"explore",
		"claude-opus",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("falta %q en output: %q", want, got)
		}
	}
}

// TestPipelineShow_JSON: con --json escupe JSON canónico parseable +
// igual al pipeline cargado.
func TestPipelineShow_JSON(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	if err := runPipelineShow(&out, mgr, "fast", true); err != nil {
		t.Fatalf("runPipelineShow: %v", err)
	}
	var got pipeline.Pipeline
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json invalid: %v\noutput: %s", err, out.String())
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if len(got.Steps) != 1 || got.Steps[0].Name != "explore" {
		t.Errorf("Steps drift: %+v", got.Steps)
	}
}

// TestPipelineShow_BuiltinDefault: sin archivo, name=default → cae al
// built-in.
func TestPipelineShow_BuiltinDefault(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	if err := runPipelineShow(&out, mgr, "default", false); err != nil {
		t.Fatalf("runPipelineShow: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "built-in") {
		t.Errorf("no marca built-in: %q", got)
	}
	// El built-in declara "validate_issue" entre los steps; sirve como
	// canario de drift vs `pipeline.Default()`.
	if !strings.Contains(got, "validate_issue") {
		t.Errorf("no muestra steps del built-in: %q", got)
	}
}

// TestPipelineShow_Missing: nombre no existente, no es "default" → error.
func TestPipelineShow_Missing(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	err := runPipelineShow(&out, mgr, "nope", false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error no menciona el nombre: %v", err)
	}
}
