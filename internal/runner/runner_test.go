package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/chichex/che/internal/engine"
	"github.com/chichex/che/internal/pipeline"
)

// fakeInvoker registra cada Invoke (agent recibido, input) y delega a
// un responder programable. Replica el patrón de engine_test.go pero
// queda local al runner para no acoplar a su test helper.
type fakeInvoker struct {
	responder func(agent string, callIdx int) (string, engine.OutputFormat, error)
	calls     map[string]int
	agents    []string
}

func newFakeInvoker(fn func(agent string, callIdx int) (string, engine.OutputFormat, error)) *fakeInvoker {
	return &fakeInvoker{
		responder: fn,
		calls:     map[string]int{},
	}
}

func (f *fakeInvoker) Invoke(ctx context.Context, agent string, input string) (string, engine.OutputFormat, error) {
	idx := f.calls[agent]
	f.calls[agent] = idx + 1
	f.agents = append(f.agents, agent)
	return f.responder(agent, idx)
}

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

// TestBuildEnginePipeline_OrdenPreservado: la pipeline del motor
// preserva el orden canónico de agentes incluso si la selección los
// devuelve en otro orden.
func TestBuildEnginePipeline_OrdenPreservado(t *testing.T) {
	p := samplePipeline()
	// El usuario tildó security primero, luego strict — pero los
	// queremos en el orden del JSON.
	sels := Selections{
		"validate_pr": {"code-reviewer-security", "code-reviewer-strict"},
	}
	ep := BuildEnginePipeline(p, sels)
	var validatePR engine.Step
	for _, s := range ep.Steps {
		if s.Name == "validate_pr" {
			validatePR = s
		}
	}
	want := []string{"code-reviewer-strict", "code-reviewer-security"}
	if !equalStringSlice(validatePR.Agents, want) {
		t.Errorf("validate_pr agents = %v want %v", validatePR.Agents, want)
	}
}

// TestBuildEnginePipeline_StepSinOverride: steps que no están en sels
// usan la lista canónica.
func TestBuildEnginePipeline_StepSinOverride(t *testing.T) {
	p := samplePipeline()
	sels := Selections{
		"explore": {"claude-opus"},
	}
	ep := BuildEnginePipeline(p, sels)
	for _, s := range ep.Steps {
		if s.Name == "execute" {
			if !equalStringSlice(s.Agents, []string{"claude-opus"}) {
				t.Errorf("execute agents = %v want canonical", s.Agents)
			}
		}
		if s.Name == "validate_pr" {
			if len(s.Agents) != 3 {
				t.Errorf("validate_pr agents = %v want 3 (canonical)", s.Agents)
			}
		}
	}
}

// TestFormatHeader_Auto: header con mode auto-loop.
func TestFormatHeader_Auto(t *testing.T) {
	got := FormatHeader("explore", ModeAuto, []string{"claude-opus"})
	want := "[step: explore · mode: auto-loop · agents: claude-opus]"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestFormatHeader_ManualMultiAgent: lista varios agentes separados
// por coma + espacio.
func TestFormatHeader_ManualMultiAgent(t *testing.T) {
	got := FormatHeader("validate_pr", ModeManual, []string{"code-reviewer-strict", "claude-opus"})
	want := "[step: validate_pr · mode: manual · agents: code-reviewer-strict, claude-opus]"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestFormatHeader_SinAgentes: caso defensivo (step roto), header
// muestra <none> en lugar de string vacío.
func TestFormatHeader_SinAgentes(t *testing.T) {
	got := FormatHeader("explore", ModeAuto, nil)
	if !strings.Contains(got, "agents: <none>") {
		t.Errorf("header sin <none>: %q", got)
	}
}

// TestRun_NoTTYPasaTodosLosAgentes: el modo auto (selector = AutoSelector)
// no filtra nada — el primer agente de cada step (que el motor invoca)
// es el primero del JSON original.
func TestRun_NoTTYPasaTodosLosAgentes(t *testing.T) {
	p := samplePipeline()
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		return "ok\n[next]", engine.FormatText, nil
	})
	var headers bytes.Buffer
	run, err := Run(context.Background(), p, inv, AutoSelector, RunOptions{
		Mode:      ModeAuto,
		HeaderOut: &headers,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if got := len(run.Steps); got != 3 {
		t.Errorf("len(Steps)=%d want 3", got)
	}
	// El motor PR5b invoca al primer agente del step. En modo auto,
	// validate_pr debe haber invocado a code-reviewer-strict (el
	// primero del JSON) — NO a un subset arbitrario.
	for _, s := range run.Steps {
		if s.Step == "validate_pr" && s.Agent != "code-reviewer-strict" {
			t.Errorf("validate_pr corrió %q want code-reviewer-strict (modo auto preserva orden canónico)", s.Agent)
		}
	}
	// Headers: uno por step, en orden, todos con mode: auto-loop.
	headerLines := splitLines(headers.String())
	if got := len(headerLines); got != 3 {
		t.Fatalf("headers=%d want 3; raw=%q", got, headers.String())
	}
	for i, line := range headerLines {
		if !strings.Contains(line, "mode: auto-loop") {
			t.Errorf("header %d sin mode auto-loop: %q", i, line)
		}
	}
	if !strings.Contains(headerLines[2], "agents: code-reviewer-strict, code-reviewer-security, claude-opus") {
		t.Errorf("header validate_pr sin lista completa: %q", headerLines[2])
	}
}

// TestRun_ManualPasaSubset: el selector reduce los agentes por step y
// el motor invoca al primero del subset.
func TestRun_ManualPasaSubset(t *testing.T) {
	p := samplePipeline()
	sel := func(stepName string, agents []string) ([]string, error) {
		// Manual: el usuario tildó sólo el último agente de cada step.
		return []string{agents[len(agents)-1]}, nil
	}
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		return "ok\n[next]", engine.FormatText, nil
	})
	var headers bytes.Buffer
	run, err := Run(context.Background(), p, inv, sel, RunOptions{
		Mode:      ModeManual,
		HeaderOut: &headers,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	// validate_pr debe haber corrido claude-opus (el último del JSON =
	// el subset de la simulación) — confirma que Run reemplazó el
	// primer agente del motor por el subset.
	for _, s := range run.Steps {
		if s.Step == "validate_pr" && s.Agent != "claude-opus" {
			t.Errorf("validate_pr corrió %q want claude-opus (subset manual)", s.Agent)
		}
	}
	// Headers: mode: manual + lista del subset (1 agente cada uno).
	headerLines := splitLines(headers.String())
	for i, line := range headerLines {
		if !strings.Contains(line, "mode: manual") {
			t.Errorf("header %d sin mode manual: %q", i, line)
		}
	}
	if !strings.Contains(headerLines[2], "agents: claude-opus]") {
		t.Errorf("header validate_pr sin agente único: %q", headerLines[2])
	}
}

// TestRun_HeaderPorStepEnOrden: el header se imprime una vez por step
// alcanzado, en orden cronológico.
func TestRun_HeaderPorStepEnOrden(t *testing.T) {
	p := samplePipeline()
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		return "[next]", engine.FormatText, nil
	})
	var headers bytes.Buffer
	_, err := Run(context.Background(), p, inv, AutoSelector, RunOptions{
		Mode:      ModeAuto,
		HeaderOut: &headers,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	lines := splitLines(headers.String())
	wantStepsInOrder := []string{"step: explore", "step: execute", "step: validate_pr"}
	if got := len(lines); got != len(wantStepsInOrder) {
		t.Fatalf("lines=%d want %d", got, len(wantStepsInOrder))
	}
	for i, want := range wantStepsInOrder {
		if !strings.Contains(lines[i], want) {
			t.Errorf("línea %d: %q no contiene %q", i, lines[i], want)
		}
	}
}

// TestRun_StopAlPrimerStepNoImprimeHeadersFalsos: si el primer step
// emite [stop], no se imprime header de los siguientes (no se
// alcanzan).
func TestRun_StopAlPrimerStepNoImprimeHeadersFalsos(t *testing.T) {
	p := samplePipeline()
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		return "[stop]", engine.FormatText, nil
	})
	var headers bytes.Buffer
	run, err := Run(context.Background(), p, inv, AutoSelector, RunOptions{
		Mode:      ModeAuto,
		HeaderOut: &headers,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("se esperaba Stopped=true")
	}
	lines := splitLines(headers.String())
	if got := len(lines); got != 1 {
		t.Errorf("headers=%d want 1 (sólo el step que sí corrió); raw=%q", got, headers.String())
	}
	if !strings.Contains(lines[0], "step: explore") {
		t.Errorf("primer header no es explore: %q", lines[0])
	}
}

// TestRun_GotoReentraEmiteHeaderNuevo: cuando un step usa goto y
// reentra al mismo step, se imprime un header nuevo (refleja la
// re-ejecución).
func TestRun_GotoReentraEmiteHeaderNuevo(t *testing.T) {
	// pipeline: explore → execute. execute emite [goto: explore]
	// la primera vez, [next] la segunda → vuelve a explore, después
	// avanza.
	p := pipeline.Pipeline{
		Version: pipeline.CurrentVersion,
		Steps: []pipeline.Step{
			{Name: "explore", Agents: []string{"agent-a"}},
			{Name: "execute", Agents: []string{"agent-b"}},
		},
	}
	inv := newFakeInvoker(func(agent string, callIdx int) (string, engine.OutputFormat, error) {
		switch agent {
		case "agent-a":
			return "[next]", engine.FormatText, nil
		case "agent-b":
			if callIdx == 0 {
				return "[goto: explore]", engine.FormatText, nil
			}
			return "[next]", engine.FormatText, nil
		}
		return "", engine.FormatText, nil
	})
	var headers bytes.Buffer
	run, err := Run(context.Background(), p, inv, AutoSelector, RunOptions{
		Mode:      ModeAuto,
		HeaderOut: &headers,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	// Esperamos: explore, execute (goto explore), explore, execute (next, fin) = 4 headers.
	lines := splitLines(headers.String())
	if got := len(lines); got != 4 {
		t.Fatalf("headers=%d want 4 (re-entrada via goto); raw=%q", got, headers.String())
	}
	wantOrder := []string{"step: explore", "step: execute", "step: explore", "step: execute"}
	for i, want := range wantOrder {
		if !strings.Contains(lines[i], want) {
			t.Errorf("línea %d: %q no contiene %q", i, lines[i], want)
		}
	}
}

// TestRun_InvokerNilDevuelveError: protege contra callers que olvidan
// pasar el invoker — mismo contrato que engine.RunPipeline.
func TestRun_InvokerNilDevuelveError(t *testing.T) {
	_, err := Run(context.Background(), samplePipeline(), nil, AutoSelector, RunOptions{})
	if err != engine.ErrInvokerNil {
		t.Errorf("err=%v want ErrInvokerNil", err)
	}
}

// TestRun_HeaderOutNilNoExplota: HeaderOut nil deshabilita la
// impresión sin afectar la corrida.
func TestRun_HeaderOutNilNoExplota(t *testing.T) {
	p := samplePipeline()
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		return "[next]", engine.FormatText, nil
	})
	run, err := Run(context.Background(), p, inv, AutoSelector, RunOptions{
		Mode: ModeAuto,
		// HeaderOut: nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if got := len(run.Steps); got != 3 {
		t.Errorf("len(Steps)=%d want 3", got)
	}
}

// TestRun_SelectorCanceladoNoInvocaMotor: Esc/Ctrl+C en el selector
// aborta sin invocar al motor.
func TestRun_SelectorCanceladoNoInvocaMotor(t *testing.T) {
	p := samplePipeline()
	called := false
	inv := newFakeInvoker(func(agent string, _ int) (string, engine.OutputFormat, error) {
		called = true
		return "[next]", engine.FormatText, nil
	})
	sel := func(string, []string) ([]string, error) {
		return nil, ErrSelectionCancelled
	}
	_, err := Run(context.Background(), p, inv, sel, RunOptions{Mode: ModeManual})
	if err != ErrSelectionCancelled {
		t.Fatalf("err=%v want ErrSelectionCancelled", err)
	}
	if called {
		t.Errorf("se invocó al motor pese a la cancelación")
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

// splitLines parte por '\n' descartando la línea vacía final que deja
// fmt.Fprintln.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
