package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// fakeLookup devuelve un agentInfo fijo para los nombres en el map; el
// resto sale como missing.
func fakeLookup(known map[string]agentInfo) func(string) (agentInfo, bool) {
	return func(name string) (agentInfo, bool) {
		a, ok := known[name]
		return a, ok
	}
}

// TestPipelineSimulate_BuiltinFallback: repo limpio, sin --pipeline →
// resuelve al built-in, source = "built-in".
func TestPipelineSimulate_BuiltinFallback(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	err := runPipelineSimulate(&out, mgr, fakeLookup(nil), "")
	if err != nil {
		t.Fatalf("runPipelineSimulate: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "built-in") {
		t.Errorf("no marca built-in: %q", got)
	}
	if !strings.Contains(got, "dry-run") {
		t.Errorf("no avisa dry-run: %q", got)
	}
}

// TestPipelineSimulate_FlagWins: flag --pipeline gana sobre el config
// default.
func TestPipelineSimulate_FlagWins(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast":     minimalPipeline,
		"thorough": minimalPipeline,
	}, "thorough")
	var out bytes.Buffer
	err := runPipelineSimulate(&out, mgr, fakeLookup(map[string]agentInfo{
		"claude-opus": {Name: "claude-opus", Model: "opus", Source: "built-in"},
	}), "fast")
	if err != nil {
		t.Fatalf("runPipelineSimulate: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "pipeline: fast") {
		t.Errorf("no resuelve flag: %q", got)
	}
	if strings.Contains(got, "thorough") {
		t.Errorf("config default leaked: %q", got)
	}
}

// TestPipelineSimulate_MissingAgent: agente no existe en el lookup → la
// tabla muestra MISSING. NO aborta.
func TestPipelineSimulate_MissingAgent(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineSimulate(&out, mgr, fakeLookup(nil), "fast")
	if err != nil {
		t.Fatalf("runPipelineSimulate: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "MISSING") {
		t.Errorf("no muestra MISSING: %q", got)
	}
	if !strings.Contains(got, "claude-opus") {
		t.Errorf("no muestra el nombre referenciado: %q", got)
	}
}

// TestPipelineSimulate_RendersAgentMetadata: agentes presentes muestran
// model + source + path en la tabla.
func TestPipelineSimulate_RendersAgentMetadata(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	err := runPipelineSimulate(&out, mgr, fakeLookup(map[string]agentInfo{
		"claude-opus": {Name: "claude-opus", Model: "opus", Source: "built-in", Path: ""},
	}), "fast")
	if err != nil {
		t.Fatalf("runPipelineSimulate: %v", err)
	}
	got := out.String()
	for _, want := range []string{"claude-opus", "opus", "built-in"} {
		if !strings.Contains(got, want) {
			t.Errorf("falta %q en output: %q", want, got)
		}
	}
}

// TestPipelineSimulate_StepsAreEnumerated: el output incluye el header
// "step[0]" (es un canario para asegurar que iteramos los steps).
func TestPipelineSimulate_StepsAreEnumerated(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	if err := runPipelineSimulate(&out, mgr, fakeLookup(nil), ""); err != nil {
		t.Fatalf("runPipelineSimulate: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "step[0]") {
		t.Errorf("no enumera steps: %q", got)
	}
}
