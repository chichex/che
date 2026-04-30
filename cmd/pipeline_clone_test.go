package cmd

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// TestParseReplaceRules_OK: shape válido old=new.
func TestParseReplaceRules_OK(t *testing.T) {
	got, err := parseReplaceRules([]string{"a=b", "c=d"})
	if err != nil {
		t.Fatalf("parseReplaceRules: %v", err)
	}
	want := map[string]string{"a": "b", "c": "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseReplaceRules_NoEquals: falta el "=".
func TestParseReplaceRules_NoEquals(t *testing.T) {
	_, err := parseReplaceRules([]string{"abc"})
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "old=new") {
		t.Errorf("error no menciona el formato esperado: %v", err)
	}
}

// TestParseReplaceRules_EmptyOld: old vacío rechazado.
func TestParseReplaceRules_EmptyOld(t *testing.T) {
	_, err := parseReplaceRules([]string{"=value"})
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
}

// TestParseReplaceRules_DuplicateConflict: dos entries con mismo old y
// distinto new → error.
func TestParseReplaceRules_DuplicateConflict(t *testing.T) {
	_, err := parseReplaceRules([]string{"a=b", "a=c"})
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
}

// TestApplyReplacements_StepAndEntry: sustituye en steps + entry.agents,
// preserva nombres de step + aggregator.
func TestApplyReplacements_StepAndEntry(t *testing.T) {
	src := pipeline.Pipeline{
		Version: 1,
		Entry: &pipeline.Entry{
			Agents:     []string{"claude-opus", "guard"},
			Aggregator: pipeline.AggregatorUnanimous,
		},
		Steps: []pipeline.Step{
			{Name: "explore", Agents: []string{"claude-opus"}},
			{Name: "validate", Agents: []string{"claude-opus", "reviewer"}, Aggregator: pipeline.AggregatorMajority},
		},
	}
	got := applyReplacements(src, map[string]string{"claude-opus": "claude-sonnet"})
	if got.Entry.Agents[0] != "claude-sonnet" || got.Entry.Agents[1] != "guard" {
		t.Errorf("entry.agents drift: %v", got.Entry.Agents)
	}
	if got.Entry.Aggregator != pipeline.AggregatorUnanimous {
		t.Errorf("entry.aggregator drift")
	}
	if got.Steps[0].Agents[0] != "claude-sonnet" {
		t.Errorf("step[0].agents[0] drift: %v", got.Steps[0].Agents)
	}
	if got.Steps[1].Agents[0] != "claude-sonnet" || got.Steps[1].Agents[1] != "reviewer" {
		t.Errorf("step[1].agents drift: %v", got.Steps[1].Agents)
	}
	if got.Steps[1].Aggregator != pipeline.AggregatorMajority {
		t.Errorf("step[1].aggregator drift")
	}
	// El src no se mutó (deep equal contra clone manual).
	if src.Entry.Agents[0] != "claude-opus" {
		t.Errorf("src.Entry.Agents mutado")
	}
}

// TestPipelineClone_DefaultToFast: ejemplo del PRD §1.c.
//
//	che pipeline clone default fast --replace claude-opus=claude-sonnet
func TestPipelineClone_DefaultToFast(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	err := runPipelineClone(&out, mgr, "default", "fast", []string{"claude-opus=claude-sonnet"}, false)
	if err != nil {
		t.Fatalf("runPipelineClone: %v", err)
	}
	dest := filepath.Join(root, ".che", "pipelines", "fast.json")
	got, err := pipeline.Load(dest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Cada step que tenía claude-opus debe ahora tener claude-sonnet.
	// Al menos el primer step (idea) debe haber cambiado.
	if got.Steps[0].Agents[0] != "claude-sonnet" {
		t.Errorf("step[0].agents[0] = %q, want claude-sonnet", got.Steps[0].Agents[0])
	}
	// Los nombres de step se preservan.
	if got.Steps[0].Name != "idea" {
		t.Errorf("step[0].name = %q, want idea", got.Steps[0].Name)
	}
}

// TestPipelineClone_ConflictWithoutForce: dst ya existe, sin --force →
// error.
func TestPipelineClone_ConflictWithoutForce(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"src": minimalPipeline,
		"dst": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineClone(&out, mgr, "src", "dst", nil, false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "ya existe") {
		t.Errorf("error no menciona conflicto: %v", err)
	}
}

// TestPipelineClone_SrcEqDst: clone a sí mismo rechazado.
func TestPipelineClone_SrcEqDst(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"x": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineClone(&out, mgr, "x", "x", nil, false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
}

// TestPipelineClone_BadReplace_RejectsEmptyAgent: --replace
// claude-opus="" produce un pipeline inválido (agent vacío) — el
// Validate post-replace debe rechazarlo y clone NO debe escribir el
// archivo.
func TestPipelineClone_BadReplace_RejectsEmptyAgent(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	err := runPipelineClone(&out, mgr, "default", "broken", []string{"claude-opus="}, false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	dest := filepath.Join(root, ".che", "pipelines", "broken.json")
	if _, statErr := pipeline.Load(dest); statErr == nil {
		t.Errorf("clone escribió archivo aunque el resultado era inválido")
	}
}
