package runner

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

func samplePipeline() pipeline.Pipeline {
	return pipeline.Pipeline{
		Version: pipeline.CurrentVersion,
		Steps: []pipeline.Step{
			{Name: "explore", Agents: []string{"claude-opus", "plan-reviewer-strict"}},
			{Name: "execute", Agents: []string{"claude-opus"}},
			{Name: "validate_pr", Agents: []string{"code-reviewer-strict", "code-reviewer-security", "claude-opus"}},
		},
	}
}

// TestAutoSelector_DevuelveListaCompleta: el selector default no
// filtra nada y respeta el orden canónico — equivale a "modo
// auto-loop".
func TestAutoSelector_DevuelveListaCompleta(t *testing.T) {
	got, err := AutoSelector("step1", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestAutoSelector_CopiaDefensiva: el selector devuelve una copia para
// que mutar el resultado no contamine al pipeline.
func TestAutoSelector_CopiaDefensiva(t *testing.T) {
	in := []string{"a", "b"}
	out, _ := AutoSelector("step1", in)
	out[0] = "MUTATED"
	if in[0] != "a" {
		t.Errorf("AutoSelector mutó el slice de entrada: in=%v", in)
	}
}

// TestResolveSelections_NilSelectorEsAuto: pasar nil equivale a usar
// AutoSelector — todos los agentes en cada step.
func TestResolveSelections_NilSelectorEsAuto(t *testing.T) {
	p := samplePipeline()
	sels, err := ResolveSelections(p, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got, want := len(sels), 3; got != want {
		t.Fatalf("len(sels)=%d want %d", got, want)
	}
	for _, s := range p.Steps {
		got := sels[s.Name]
		if !equalStringSlice(got, s.Agents) {
			t.Errorf("step %q: got %v want %v", s.Name, got, s.Agents)
		}
	}
}

// TestResolveSelections_SubsetValido: el selector devuelve un subset
// estricto y la validación pasa.
func TestResolveSelections_SubsetValido(t *testing.T) {
	p := samplePipeline()
	sel := func(stepName string, agents []string) ([]string, error) {
		// Sólo el primer agente de cada step.
		return []string{agents[0]}, nil
	}
	sels, err := ResolveSelections(p, sel)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := sels["explore"]; !equalStringSlice(got, []string{"claude-opus"}) {
		t.Errorf("explore: got %v", got)
	}
	if got := sels["validate_pr"]; !equalStringSlice(got, []string{"code-reviewer-strict"}) {
		t.Errorf("validate_pr: got %v", got)
	}
}

// TestResolveSelections_SubsetVacioRechazado: un selector que
// devuelve [] sobre un step con agentes declarados se rechaza con un
// error que apunta al step.
func TestResolveSelections_SubsetVacioRechazado(t *testing.T) {
	p := samplePipeline()
	sel := func(stepName string, agents []string) ([]string, error) {
		return nil, nil
	}
	_, err := ResolveSelections(p, sel)
	if err == nil {
		t.Fatal("se esperaba error por subset vacío")
	}
	if !strings.Contains(err.Error(), `"explore"`) {
		t.Errorf("error sin step name: %v", err)
	}
}

// TestResolveSelections_AgenteFantasiaRechazado: un selector que
// devuelve un agente no declarado se rechaza con la lista de válidos.
func TestResolveSelections_AgenteFantasiaRechazado(t *testing.T) {
	p := samplePipeline()
	sel := func(stepName string, agents []string) ([]string, error) {
		return []string{"agent-fantasma"}, nil
	}
	_, err := ResolveSelections(p, sel)
	if err == nil {
		t.Fatal("se esperaba error por agente no declarado")
	}
	if !strings.Contains(err.Error(), "agent-fantasma") {
		t.Errorf("error sin nombre del agente fantasma: %v", err)
	}
	if !strings.Contains(err.Error(), "claude-opus") {
		t.Errorf("error sin lista de válidos: %v", err)
	}
}

// TestResolveSelections_DuplicadoRechazado: el mismo agente repetido
// en la selección se rechaza para no ejecutar dos veces.
func TestResolveSelections_DuplicadoRechazado(t *testing.T) {
	p := samplePipeline()
	sel := func(stepName string, agents []string) ([]string, error) {
		return []string{"claude-opus", "claude-opus"}, nil
	}
	_, err := ResolveSelections(p, sel)
	if err == nil {
		t.Fatal("se esperaba error por duplicado")
	}
	if !strings.Contains(err.Error(), "duplicado") {
		t.Errorf("error sin la palabra 'duplicado': %v", err)
	}
}

// TestResolveSelections_CancelacionPropagada: si el selector devuelve
// ErrSelectionCancelled, ResolveSelections lo propaga y no sigue
// preguntando por los siguientes steps.
func TestResolveSelections_CancelacionPropagada(t *testing.T) {
	p := samplePipeline()
	calls := 0
	sel := func(stepName string, agents []string) ([]string, error) {
		calls++
		return nil, ErrSelectionCancelled
	}
	_, err := ResolveSelections(p, sel)
	if err != ErrSelectionCancelled {
		t.Fatalf("err=%v want ErrSelectionCancelled", err)
	}
	if calls != 1 {
		t.Errorf("se preguntaron %d steps; se esperaba parar al primero", calls)
	}
}

// equalStringSlice compara dos slices respetando orden. Helper local.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
