package engine

import (
	"context"
	"errors"
	"testing"
)

// fakeInvoker es un Invoker programable: el test setea cómo responder a
// cada agente y registra cuántas veces se invocó cada uno. Permite
// verificar el flow del engine sin spawnear procesos reales.
type fakeInvoker struct {
	// responder se llama por cada Invoke. Recibe el agente y la cuenta
	// de llamadas previas a ESE agente (0-based). Devuelve el output, el
	// formato y un error técnico opcional.
	responder func(agent string, callIdx int) (string, OutputFormat, error)
	// calls cuenta llamadas por agente para que los tests puedan
	// verificar "se llamó al executor 3 veces, al validator 1".
	calls map[string]int
}

func newFakeInvoker(fn func(agent string, callIdx int) (string, OutputFormat, error)) *fakeInvoker {
	return &fakeInvoker{
		responder: fn,
		calls:     map[string]int{},
	}
}

func (f *fakeInvoker) Invoke(ctx context.Context, agent string, input string) (string, OutputFormat, error) {
	idx := f.calls[agent]
	f.calls[agent] = idx + 1
	return f.responder(agent, idx)
}

func simplePipeline() Pipeline {
	return Pipeline{
		Steps: []Step{
			{Name: "explore", Agents: []string{"agent-a"}},
			{Name: "validate", Agents: []string{"agent-b"}},
			{Name: "execute", Agents: []string{"agent-c"}},
			{Name: "close", Agents: []string{"agent-d"}},
		},
	}
}

func TestRun_LinealConNext(t *testing.T) {
	// Cada step emite [next]; el motor camina los 4 steps en orden.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "ok\n[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true, esperaba false; reason=%q detail=%q", run.StopReason, run.StopDetail)
	}
	if got, want := len(run.Steps), 4; got != want {
		t.Fatalf("len(Steps)=%d want %d; runs=%+v", got, want, run.Steps)
	}
	expectedSteps := []string{"explore", "validate", "execute", "close"}
	for i, s := range run.Steps {
		if s.Step != expectedSteps[i] {
			t.Errorf("step %d: name=%q want %q", i, s.Step, expectedSteps[i])
		}
		if s.Marker.Kind != MarkerNext {
			t.Errorf("step %d: marker=%v want next", i, s.Marker.Kind)
		}
	}
	if run.Transitions != 4 {
		t.Errorf("Transitions=%d want 4", run.Transitions)
	}
}

func TestRun_DefaultNextSinMarcador(t *testing.T) {
	// PRD §3.b paso 5: invocación exitosa sin marker → default [next].
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "todo bien, sin marker explícito", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q; esperaba completar todo el pipeline con default [next]", run.StopReason)
	}
	for _, s := range run.Steps {
		if s.Resolved != "default-next" {
			t.Errorf("step %q: Resolved=%q want default-next", s.Step, s.Resolved)
		}
		if s.Marker.Kind != MarkerNext {
			t.Errorf("step %q: marker=%v want next", s.Step, s.Marker.Kind)
		}
	}
}

func TestRun_StopExplicito(t *testing.T) {
	// El primer step emite [stop] → motor termina ahí.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "agent-a" {
			return "[stop]", FormatText, nil
		}
		t.Fatalf("invoker no debería llamar a %q después de [stop]", agent)
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; esperaba true")
	}
	if run.StopReason != StopReasonAgentMarker {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonAgentMarker)
	}
	if len(run.Steps) != 1 {
		t.Errorf("len(Steps)=%d want 1; runs=%+v", len(run.Steps), run.Steps)
	}
}

func TestRun_GotoForward(t *testing.T) {
	// explore emite [goto: execute]. Saltar validate y arrancar en execute.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		switch agent {
		case "agent-a": // explore
			return "[goto: execute]", FormatText, nil
		case "agent-c": // execute
			return "[next]", FormatText, nil
		case "agent-d": // close
			return "[next]", FormatText, nil
		}
		t.Fatalf("agente inesperado %q (validate no debería correr)", agent)
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	got := []string{}
	for _, s := range run.Steps {
		got = append(got, s.Step)
	}
	want := []string{"explore", "execute", "close"}
	if !equalStrings(got, want) {
		t.Errorf("steps recorridos=%v want %v", got, want)
	}
}

func TestRun_GotoBackward_LoopCap(t *testing.T) {
	// validate siempre emite [goto: explore]; explore siempre [next].
	// El motor entra en loop validate↔explore hasta cap=20.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		switch agent {
		case "agent-a":
			return "[next]", FormatText, nil
		case "agent-b":
			return "[goto: explore]", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; esperaba true (loop cap)")
	}
	if run.StopReason != StopReasonLoopCap {
		t.Errorf("StopReason=%q want %q (detail=%q)", run.StopReason, StopReasonLoopCap, run.StopDetail)
	}
	// El motor llega exactamente a MaxTransitions y ahí corta antes de
	// invocar el siguiente step. La cantidad de StepRun puede ser
	// MaxTransitions (loop fluido) o algo cercano si el cap se chequea
	// al inicio de cada vuelta.
	if run.Transitions != MaxTransitions {
		t.Errorf("Transitions=%d want %d", run.Transitions, MaxTransitions)
	}
}

func TestRun_GotoStepDesconocido(t *testing.T) {
	// PRD §3.c: "Step destino inválido → error explícito + [stop] con
	// razón 'step destino desconocido'".
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "[goto: nonexistent_step]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; esperaba true")
	}
	if run.StopReason != StopReasonUnknownStep {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonUnknownStep)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("len(Steps)=%d want 1", len(run.Steps))
	}
	last := run.Steps[0]
	if last.Resolved != "unknown-step" {
		t.Errorf("Resolved=%q want unknown-step", last.Resolved)
	}
	if last.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop (el motor convierte goto inválido a stop)", last.Marker.Kind)
	}
}

func TestRun_ErrorTecnicoEsStop(t *testing.T) {
	// PRD §3.b paso 4: "Si la invocación falla (timeout, error de red,
	// exit code distinto de 0 del binario, crash) → trata el outcome
	// como [stop] automático".
	wantErr := errors.New("claude: exit 1: auth failed")
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "", FormatText, wantErr
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; esperaba true (error técnico → stop)")
	}
	if run.StopReason != StopReasonTechnicalError {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonTechnicalError)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("len(Steps)=%d want 1", len(run.Steps))
	}
	stepRun := run.Steps[0]
	if !errors.Is(stepRun.Err, wantErr) {
		t.Errorf("step.Err=%v want %v", stepRun.Err, wantErr)
	}
	if stepRun.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", stepRun.Marker.Kind)
	}
}

func TestRun_StreamJSONFormat(t *testing.T) {
	// Cuando el invoker reporta FormatStreamJSON, el motor parsea con
	// ParseStreamMarker en vez de ParseMarker.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		// Stream válido con un evento `result` final que emite [next].
		stream := `{"type":"system","subtype":"init"}` + "\n" +
			`{"type":"result","subtype":"success","result":"todo OK\n[next]"}`
		return stream, FormatStreamJSON, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if len(run.Steps) != 4 {
		t.Errorf("len(Steps)=%d want 4 (todos avanzan con [next] en stream-json)", len(run.Steps))
	}
}

func TestRun_StreamJSONSinResultCierraEnNext(t *testing.T) {
	// PRD §3.c "Si el último output no es texto (LLM cerró con tool_use
	// sin response final), default [next]".
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		// Stream sin event `result` — solo tool_use.
		stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
		return stream, FormatStreamJSON, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	for _, s := range run.Steps {
		if s.Resolved != "default-next" {
			t.Errorf("step %q: Resolved=%q want default-next (stream sin result → default [next])", s.Step, s.Resolved)
		}
	}
}

func TestRun_PipelineVacio(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		t.Fatalf("invoker no debería llamarse con pipeline vacío")
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), Pipeline{}, inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonEmptyPipeline {
		t.Errorf("got Stopped=%v StopReason=%q want stop+empty-pipeline", run.Stopped, run.StopReason)
	}
}

func TestRun_StepSinAgentes(t *testing.T) {
	p := Pipeline{
		Steps: []Step{
			{Name: "explore", Agents: []string{"agent-a"}},
			{Name: "broken", Agents: nil},
		},
	}
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), p, inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonNoAgents {
		t.Errorf("got Stopped=%v StopReason=%q want stop+no-agents", run.Stopped, run.StopReason)
	}
}

func TestRun_NilInvoker(t *testing.T) {
	_, err := RunPipeline(context.Background(), simplePipeline(), nil, Options{})
	if !errors.Is(err, ErrInvokerNil) {
		t.Errorf("err=%v want ErrInvokerNil", err)
	}
}

func TestRun_EntryStepOverride(t *testing.T) {
	// Arrancar desde "execute" (saltando explore + validate).
	// PR5d expondrá esto como `che run --from <step>`.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		switch agent {
		case "agent-c", "agent-d":
			return "[next]", FormatText, nil
		}
		t.Fatalf("agente inesperado %q (entry=execute, no debería llamar a %q)", agent, agent)
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{EntryStep: "execute"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	got := []string{}
	for _, s := range run.Steps {
		got = append(got, s.Step)
	}
	want := []string{"execute", "close"}
	if !equalStrings(got, want) {
		t.Errorf("steps=%v want %v", got, want)
	}
}

func TestRun_EntryStepDesconocido(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		t.Fatalf("invoker no debería llamarse con entry inválido")
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{EntryStep: "ghost"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonUnknownStep {
		t.Errorf("got Stopped=%v StopReason=%q want stop+unknown-step", run.Stopped, run.StopReason)
	}
}

func TestRun_ContextCancelado(t *testing.T) {
	// Si el ctx se cancela antes de la primera invocación, el motor
	// detiene ahí mismo con stop técnico.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		t.Fatalf("invoker no debería llamarse con ctx cancelado")
		return "", FormatText, nil
	})
	run, err := RunPipeline(ctx, simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonTechnicalError {
		t.Errorf("got Stopped=%v StopReason=%q want stop+technical-error", run.Stopped, run.StopReason)
	}
}

func TestRun_LoopGotoCuentaTransitions(t *testing.T) {
	// Diseño del cap: cada step ejecutado cuenta como una transición.
	// Goto 5 veces para atrás antes de avanzar — verifica que el motor
	// no llega al cap si el loop converge.
	bouncesLeft := 5
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		switch agent {
		case "agent-a":
			return "[next]", FormatText, nil
		case "agent-b":
			if bouncesLeft > 0 {
				bouncesLeft--
				return "[goto: explore]", FormatText, nil
			}
			return "[next]", FormatText, nil
		default:
			return "[next]", FormatText, nil
		}
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	// explore + 5*(validate→explore) + validate + execute + close = 1 + 10 + 1 + 1 + 1 = 14
	if run.Transitions != 14 {
		t.Errorf("Transitions=%d want 14", run.Transitions)
	}
	if run.Transitions >= MaxTransitions {
		t.Errorf("Transitions=%d not < cap=%d", run.Transitions, MaxTransitions)
	}
}

func equalStrings(a, b []string) bool {
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
