package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chichex/che/internal/engine"
)

// fakeInvoker para los tests de cmd/run: el responder dispatcha por
// nombre del agente, y el invoker NO spawnea nada — todo en memoria.
type fakeRunInvoker struct {
	responder func(agent string) (string, engine.OutputFormat, error)
	calls     []string
}

func (f *fakeRunInvoker) Invoke(_ context.Context, agentName string, _ string) (string, engine.OutputFormat, error) {
	f.calls = append(f.calls, agentName)
	return f.responder(agentName)
}

// minimalPipelineWithEntry: pipeline JSON con entry + 3 steps. Compatible
// con el shape que carga pipeline.Manager.
const minimalPipelineWithEntry = `{
  "version": 1,
  "entry": {"agents": ["entry-agent"]},
  "steps": [
    {"name": "explore", "agents": ["claude-opus"]},
    {"name": "validate", "agents": ["claude-opus"]},
    {"name": "execute", "agents": ["claude-opus"]}
  ]
}`

// TestRun_EntryNextEjecutaTodosLosSteps: entry [next] → arranca primer
// step + corre todos.
func TestRun_EntryNextEjecutaTodosLosSteps(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"with-entry": minimalPipelineWithEntry,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			if agent == "entry-agent" {
				return "[next]", engine.FormatText, nil
			}
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "with-entry", "", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v\nout=%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "entry: agent=entry-agent") {
		t.Errorf("output no incluye entry: %q", got)
	}
	if !strings.Contains(got, "step[0]: explore") {
		t.Errorf("output no muestra step[0]: %q", got)
	}
	if !strings.Contains(got, "step[2]: execute") {
		t.Errorf("output no muestra step[2]: %q", got)
	}
	if !strings.Contains(got, "completed") {
		t.Errorf("output no marca completed: %q", got)
	}
}

// TestRun_EntryGotoSaltaSteps: entry [goto: validate] → motor empieza
// en validate, salta explore.
func TestRun_EntryGotoSaltaSteps(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"with-entry": minimalPipelineWithEntry,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			if agent == "entry-agent" {
				return "[goto: validate]", engine.FormatText, nil
			}
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "with-entry", "", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "start=validate") {
		t.Errorf("output no muestra start=validate: %q", got)
	}
	if strings.Contains(got, "step[0]: explore") {
		t.Errorf("explore no debería correr; output=%q", got)
	}
	if !strings.Contains(got, "validate") {
		t.Errorf("output no muestra validate: %q", got)
	}
}

// TestRun_EntryStopExitNoEsError: entry [stop] termina con StopReason
// EntryStop pero NO devuelve error técnico (es un outcome legítimo del
// pipeline, no un error del CLI).
func TestRun_EntryStopExitNoEsError(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"with-entry": minimalPipelineWithEntry,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			if agent == "entry-agent" {
				return "[stop]", engine.FormatText, nil
			}
			t.Fatalf("ningún step debería correr después de entry [stop]; agent=%q", agent)
			return "", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "with-entry", "", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v (entry stop no debería ser error técnico)", err)
	}
	got := out.String()
	if !strings.Contains(got, string(engine.StopReasonEntryStop)) {
		t.Errorf("output no muestra StopReasonEntryStop: %q", got)
	}
}

// TestRun_FromBypasaaEntry: --from validate hace que el entry NO corra
// y el pipeline arranque en validate.
func TestRun_FromBypasaaEntry(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"with-entry": minimalPipelineWithEntry,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			if agent == "entry-agent" {
				t.Fatalf("entry no debería correr cuando se pasó --from")
				return "", engine.FormatText, nil
			}
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "with-entry", "validate", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "entry bypassed") {
		t.Errorf("output no muestra 'entry bypassed': %q", got)
	}
	if strings.Contains(got, "entry: agent=") {
		t.Errorf("entry corrió aunque pasamos --from: %q", got)
	}
	if !strings.Contains(got, "step[0]: validate") {
		t.Errorf("primer step esperado=validate; output=%q", got)
	}
}

// TestRun_FromConStepDesconocidoEsError: --from ghost devuelve error
// técnico (StopReasonUnknownStep) — el caller lo ve como exit code != 0.
func TestRun_FromConStepDesconocidoEsError(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"with-entry": minimalPipelineWithEntry,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			t.Fatalf("invoker no debería llamarse con --from inválido")
			return "", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "with-entry", "ghost", "")
	if err == nil {
		t.Fatalf("runPipelineRun no devolvió error; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), string(engine.StopReasonUnknownStep)) {
		t.Errorf("err=%v want includes %q", err, engine.StopReasonUnknownStep)
	}
}

// TestRun_PipelineSinEntry: pipeline sin Entry — corre desde primer
// step, output no muestra sección entry.
func TestRun_PipelineSinEntry(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"plain": minimalPipeline,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "plain", "", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "entry: agent=") {
		t.Errorf("output incluye entry pero pipeline no tiene: %q", got)
	}
	if !strings.Contains(got, "step[0]: explore") {
		t.Errorf("output no muestra step[0]: %q", got)
	}
}

// TestRun_BuiltinFallbackSinFlags: sin pipeline arg + sin config →
// resuelve al built-in default.
func TestRun_BuiltinFallbackSinFlags(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "", "", "")
	if err != nil {
		t.Fatalf("runPipelineRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "built-in") {
		t.Errorf("output no marca built-in: %q", got)
	}
}

// TestRun_ErrorTecnicoEsExitError: invoker devuelve error técnico para
// el primer step → engine resuelve a stop+technical-error → CLI devuelve
// error.
func TestRun_ErrorTecnicoEsExitError(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"plain": minimalPipeline,
	}, "")
	wantErr := errors.New("auth failed")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			return "", engine.FormatText, wantErr
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, "plain", "", "")
	if err == nil {
		t.Fatalf("runPipelineRun no devolvió error; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), string(engine.StopReasonTechnicalError)) {
		t.Errorf("err=%v want includes %q", err, engine.StopReasonTechnicalError)
	}
}
