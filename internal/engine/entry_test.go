package engine

import (
	"context"
	"errors"
	"testing"
)

// pipelineWithEntry arma un pipeline con un Entry y los 4 steps clásicos.
// Se usa por los tests de entry para no repetir el shape en cada test.
func pipelineWithEntry(agents ...string) Pipeline {
	if len(agents) == 0 {
		agents = []string{"entry-agent"}
	}
	p := simplePipeline()
	p.Entry = &EntrySpec{Agents: agents}
	return p
}

// TestEntry_NextArrancaPrimerStep: entry emite [next] (o no emite marker
// y queda default). El motor arranca desde el primer step y completa el
// pipeline. EntryRun queda registrado en Run.Entry.
func TestEntry_NextArrancaPrimerStep(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "input ok\n[next]", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q detail=%q", run.StopReason, run.StopDetail)
	}
	if run.Entry == nil {
		t.Fatalf("Run.Entry=nil; esperaba un EntryRun")
	}
	if run.Entry.Marker.Kind != MarkerNext {
		t.Errorf("Entry.Marker=%v want next", run.Entry.Marker.Kind)
	}
	if run.Entry.StartStep != "explore" {
		t.Errorf("Entry.StartStep=%q want explore", run.Entry.StartStep)
	}
	if len(run.Steps) != 4 {
		t.Errorf("len(Steps)=%d want 4 (todos los steps deben correr después de [next])", len(run.Steps))
	}
}

// TestEntry_DefaultNextSinMarker: invocación OK sin marker → asumir
// [next] (igual que para steps; PRD §3.b paso 5 + §5.a).
func TestEntry_DefaultNextSinMarker(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "todo bien, sin marker", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if run.Entry == nil || run.Entry.Resolved != "default-next" {
		t.Errorf("Entry.Resolved=%q want default-next", run.Entry.Resolved)
	}
	if run.Entry.StartStep != "explore" {
		t.Errorf("Entry.StartStep=%q want explore", run.Entry.StartStep)
	}
}

// TestEntry_GotoArrancaEnStepSeleccionado: entry emite [goto: validate]
// → motor arranca en validate (saltando explore).
func TestEntry_GotoArrancaEnStepSeleccionado(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		switch agent {
		case "entry-agent":
			return "[goto: validate]", FormatText, nil
		case "agent-a": // explore
			t.Fatalf("explore no debería correr cuando entry hace goto: validate")
			return "", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if run.Entry == nil || run.Entry.StartStep != "validate" {
		t.Errorf("Entry.StartStep=%q want validate", run.Entry.StartStep)
	}
	got := []string{}
	for _, s := range run.Steps {
		got = append(got, s.Step)
	}
	want := []string{"validate", "execute", "close"}
	if !equalStrings(got, want) {
		t.Errorf("steps=%v want %v", got, want)
	}
}

// TestEntry_StopRebotaInput: entry emite [stop] → motor no corre ningún
// step, Run.Stopped=true con StopReasonEntryStop. Distinguir esta razón
// de StopReasonAgentMarker es importante para que la UX (audit log) sepa
// que el rebote fue en el entry y no a mitad del pipeline.
func TestEntry_StopRebotaInput(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "input rechazado\n[stop]", FormatText, nil
		}
		t.Fatalf("ningún step debería correr después de [stop] del entry; agent=%q", agent)
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; esperaba true (entry emitió stop)")
	}
	if run.StopReason != StopReasonEntryStop {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonEntryStop)
	}
	if len(run.Steps) != 0 {
		t.Errorf("len(Steps)=%d want 0 (entry stop no debe correr steps)", len(run.Steps))
	}
	if run.Entry == nil {
		t.Fatalf("Run.Entry=nil; esperaba EntryRun aún cuando hay stop")
	}
	if run.Entry.Marker.Kind != MarkerStop {
		t.Errorf("Entry.Marker=%v want stop", run.Entry.Marker.Kind)
	}
}

// TestEntry_GotoStepDesconocido: entry hace [goto: ghost] sobre step que
// no existe → motor convierte a stop con StopReasonUnknownStep (igual
// que para steps con goto inválido — comportamiento consistente).
func TestEntry_GotoStepDesconocido(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "[goto: ghost]", FormatText, nil
		}
		t.Fatalf("ningún step debería correr cuando el entry hace goto inválido")
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonUnknownStep {
		t.Errorf("got Stopped=%v StopReason=%q want stop+unknown-step", run.Stopped, run.StopReason)
	}
	if run.Entry == nil || run.Entry.Resolved != "unknown-step" {
		t.Errorf("Entry.Resolved=%q want unknown-step", run.Entry.Resolved)
	}
}

// TestEntry_ErrorTecnicoEsStop: error técnico del invoker para el entry
// → stop con StopReasonTechnicalError (mismo trato que para steps).
func TestEntry_ErrorTecnicoEsStop(t *testing.T) {
	wantErr := errors.New("entry: claude exit 1")
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "", FormatText, wantErr
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonTechnicalError {
		t.Errorf("got Stopped=%v StopReason=%q want stop+technical-error", run.Stopped, run.StopReason)
	}
	if run.Entry == nil || !errors.Is(run.Entry.Err, wantErr) {
		t.Errorf("Entry.Err=%v want %v", run.Entry.Err, wantErr)
	}
}

// TestEntry_FromBypasaaEntry: cuando el caller pasa Options.EntryStep
// (= flag --from), el entry NO corre. Es el override manual del PRD
// §5.c. EntryRun queda nil para distinguirlo de "el entry corrió y
// terminó".
func TestEntry_FromBypasaaEntry(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			t.Fatalf("entry no debería correr cuando el caller pasa EntryStep (--from)")
			return "", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{EntryStep: "execute"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if run.Entry != nil {
		t.Errorf("Run.Entry=%+v; esperaba nil cuando --from bypassa el entry", run.Entry)
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

// TestEntry_PipelineSinEntry: pipeline sin Entry — el motor arranca en
// el primer step, Run.Entry queda nil. Test de regresión para asegurar
// que el entry agent NO es obligatorio (PRD §5.b).
func TestEntry_PipelineSinEntry(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			t.Fatalf("entry no debería correr cuando Pipeline.Entry == nil")
			return "", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), simplePipeline(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if run.Entry != nil {
		t.Errorf("Run.Entry=%+v; esperaba nil sin Entry", run.Entry)
	}
	if len(run.Steps) != 4 {
		t.Errorf("len(Steps)=%d want 4", len(run.Steps))
	}
}

// TestEntry_SinAgentes: defensa para EntrySpec con Agents vacío (config
// inválido escapó al validator). Stop con StopReasonEntryNoAgents.
func TestEntry_SinAgentes(t *testing.T) {
	p := simplePipeline()
	p.Entry = &EntrySpec{Agents: nil}
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		t.Fatalf("invoker no debería llamarse con entry sin agentes")
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), p, inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonEntryNoAgents {
		t.Errorf("got Stopped=%v StopReason=%q want stop+entry-no-agents", run.Stopped, run.StopReason)
	}
}

// TestEntry_NoCuentaComoTransition: el entry NO debe contar como
// transición — el cap de MaxTransitions protege contra loops entre
// steps, no contra el entry (que corre una sola vez).
func TestEntry_NoCuentaComoTransition(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			return "[next]", FormatText, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Transitions != 4 {
		t.Errorf("Transitions=%d want 4 (entry no debe contar como transición)", run.Transitions)
	}
}

// TestEntry_StreamJSONFormat: cuando el invoker reporta FormatStreamJSON
// para el entry, el motor parsea con ParseStreamMarker (igual que para
// steps).
func TestEntry_StreamJSONFormat(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		if agent == "entry-agent" {
			stream := `{"type":"system","subtype":"init"}` + "\n" +
				`{"type":"result","subtype":"success","result":"input ok\n[goto: execute]"}`
			return stream, FormatStreamJSON, nil
		}
		return "[next]", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if run.Stopped {
		t.Fatalf("Stopped=true reason=%q", run.StopReason)
	}
	if run.Entry == nil || run.Entry.StartStep != "execute" {
		t.Errorf("Entry.StartStep=%q want execute (stream-json [goto: execute])", run.Entry.StartStep)
	}
}

// TestEntry_FromConPipelineEntryYStepDesconocido: --from con step que
// no existe en el pipeline → unknown-step (sin tocar el entry). El
// override manual no debe pasar por el entry incluso cuando es inválido.
func TestEntry_FromConPipelineEntryYStepDesconocido(t *testing.T) {
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		t.Fatalf("ni entry ni steps deberían correr con --from inválido")
		return "", FormatText, nil
	})
	run, err := RunPipeline(context.Background(), pipelineWithEntry(), inv, Options{EntryStep: "ghost"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped || run.StopReason != StopReasonUnknownStep {
		t.Errorf("got Stopped=%v StopReason=%q want stop+unknown-step", run.Stopped, run.StopReason)
	}
	if run.Entry != nil {
		t.Errorf("Run.Entry=%+v; --from inválido no debería invocar al entry", run.Entry)
	}
}
