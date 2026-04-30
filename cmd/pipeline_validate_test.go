package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// fakeHas devuelve true para los nombres en el set, false para el resto.
// Inyectable como predicado de runPipelineValidate.
func fakeHas(known ...string) func(string) bool {
	set := map[string]bool{}
	for _, k := range known {
		set[k] = true
	}
	return func(name string) bool { return set[name] }
}

// TestPipelineValidate_OK: pipeline OK + agentes presentes → ok.
func TestPipelineValidate_OK(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineValidate(&out, mgr, fakeHas("claude-opus"), "fast", false)
	if err != nil {
		t.Fatalf("runPipelineValidate: %v", err)
	}
	if !strings.Contains(out.String(), "ok") || !strings.Contains(out.String(), "fast") {
		t.Errorf("stdout no reporta ok: %q", out.String())
	}
}

// TestPipelineValidate_MissingAgent: agente referenciado no existe →
// error con field path.
func TestPipelineValidate_MissingAgent(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineValidate(&out, mgr, fakeHas(), "fast", false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "claude-opus") {
		t.Errorf("error no menciona el agente faltante: %v", err)
	}
	if !strings.Contains(err.Error(), "steps[0].agents[0]") {
		t.Errorf("error sin field path: %v", err)
	}
}

// TestPipelineValidate_SkipAgents: con --skip-agents, agentes faltantes
// no rompen.
func TestPipelineValidate_SkipAgents(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineValidate(&out, mgr, fakeHas(), "fast", true)
	if err != nil {
		t.Fatalf("runPipelineValidate --skip-agents: %v", err)
	}
}

// TestPipelineValidate_BrokenSchema: pipeline mal formado (0 steps) →
// error de schema, sin ni mirar agentes.
func TestPipelineValidate_BrokenSchema(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"broken": `{"version": 1, "steps": []}`,
	}, "")
	var out bytes.Buffer
	err := runPipelineValidate(&out, mgr, fakeHas("claude-opus"), "broken", false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "step") {
		t.Errorf("error no menciona steps: %v", err)
	}
}

// TestPipelineValidate_BuiltinDefault: validar el built-in implícito.
// En tests, los agentes referenciados (plan-reviewer-strict, etc.) no
// existen en el fake, pero si pasamos los nombres del built-in, ok.
func TestPipelineValidate_BuiltinDefault(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	// Skip agentes para no listar todos los reviewers del built-in.
	err := runPipelineValidate(&out, mgr, fakeHas(), "default", true)
	if err != nil {
		t.Fatalf("runPipelineValidate default: %v", err)
	}
	if !strings.Contains(out.String(), "default") || !strings.Contains(out.String(), "built-in") {
		t.Errorf("stdout no marca built-in: %q", out.String())
	}
}
