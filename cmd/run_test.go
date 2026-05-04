package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chichex/che/internal/engine"
	"github.com/chichex/che/internal/runner"
)

// fakeRunInvoker para los tests de cmd/run: el responder dispatcha por
// nombre del agente, y el invoker NO spawnea nada — todo en memoria.
type fakeRunInvoker struct {
	responder func(agent string) (string, engine.OutputFormat, error)
	mu        sync.Mutex
	calls     []string
}

func (f *fakeRunInvoker) Invoke(_ context.Context, agentName string, _ string) (string, engine.OutputFormat, error) {
	f.mu.Lock()
	f.calls = append(f.calls, agentName)
	f.mu.Unlock()
	return f.responder(agentName)
}

func (f *fakeRunInvoker) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
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

// autoArgs es un atajo: arma runRunArgs en modo auto-loop con el
// pipeline + fromStep dados.
func autoArgs(pipelineFlag, fromStep string) runRunArgs {
	return runRunArgs{
		pipelineFlag: pipelineFlag,
		fromStep:     fromStep,
		mode:         runner.ModeAuto,
	}
}

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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("with-entry", ""))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("with-entry", ""))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("with-entry", ""))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("with-entry", "validate"))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("with-entry", "ghost"))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("plain", ""))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("", ""))
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
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("plain", ""))
	if err == nil {
		t.Fatalf("runPipelineRun no devolvió error; out=%q", out.String())
	}
	if !strings.Contains(err.Error(), string(engine.StopReasonTechnicalError)) {
		t.Errorf("err=%v want includes %q", err, engine.StopReasonTechnicalError)
	}
}

// --- pickModeAndSelector (PR9a) ---

// TestPickModeAndSelector_AutoYManualMutex: ambos flags juntos = error.
func TestPickModeAndSelector_AutoYManualMutex(t *testing.T) {
	_, _, err := pickModeAndSelector(true, true, true, &bytes.Buffer{})
	if err == nil {
		t.Fatal("se esperaba error por flags mutex")
	}
	if !strings.Contains(err.Error(), "mutuamente excluyentes") {
		t.Errorf("error sin la frase clave: %v", err)
	}
}

// TestPickModeAndSelector_ManualSinTTYError: --manual sin TTY se
// rechaza temprano.
func TestPickModeAndSelector_ManualSinTTYError(t *testing.T) {
	_, _, err := pickModeAndSelector(false, false, true, &bytes.Buffer{})
	if err == nil {
		t.Fatal("se esperaba error por --manual sin tty")
	}
	if !strings.Contains(err.Error(), "TTY") {
		t.Errorf("error sin mención de TTY: %v", err)
	}
}

// TestPickModeAndSelector_AutoExplicito: --auto con TTY igual fuerza
// auto-loop (override del default interactivo).
func TestPickModeAndSelector_AutoExplicito(t *testing.T) {
	mode, sel, err := pickModeAndSelector(true, true, false, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mode != runner.ModeAuto {
		t.Errorf("mode=%q want auto-loop", mode)
	}
	got, _ := sel("step1", []string{"a", "b"})
	if len(got) != 2 {
		t.Errorf("selector no es AutoSelector (devolvió %d agentes)", len(got))
	}
}

// TestPickModeAndSelector_NoTTYDefaultAuto: sin flags y sin TTY =
// auto-loop (modo scripteable / dash / CI).
func TestPickModeAndSelector_NoTTYDefaultAuto(t *testing.T) {
	mode, sel, err := pickModeAndSelector(false, false, false, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mode != runner.ModeAuto {
		t.Errorf("mode=%q want auto-loop", mode)
	}
	got, _ := sel("step1", []string{"a", "b", "c"})
	if len(got) != 3 {
		t.Errorf("selector no es AutoSelector (devolvió %d agentes)", len(got))
	}
}

// TestPickModeAndSelector_TTYDefaultManual: sin flags y con TTY =
// manual (PromptSelector). No invocamos al selector porque abriría
// bubbletea — sólo verificamos la rama.
func TestPickModeAndSelector_TTYDefaultManual(t *testing.T) {
	mode, sel, err := pickModeAndSelector(true, false, false, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mode != runner.ModeManual {
		t.Errorf("mode=%q want manual", mode)
	}
	if sel == nil {
		t.Error("selector nil en modo manual")
	}
}

// --- selector subset filtering (PR9a) ---

// TestRun_SelectorManualFiltraSubset: con un selector manual que
// devuelve sólo el último agente, el motor invoca a ese (no al primero
// canónico).
func TestRun_SelectorManualFiltraSubset(t *testing.T) {
	const pipe = `{
  "version": 1,
  "steps": [
    {"name": "validate_pr", "agents": ["code-reviewer-strict", "code-reviewer-security", "claude-opus"]}
  ]
}`
	mgr, _ := pipelineFixture(t, map[string]string{"work": pipe}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			return "[next]", engine.FormatText, nil
		},
	}
	manualSel := func(stepName string, agents []string) ([]string, error) {
		return []string{agents[len(agents)-1]}, nil // último: claude-opus
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, manualSel, runRunArgs{
		pipelineFlag: "work",
		mode:         runner.ModeManual,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	calls := inv.Calls()
	if got := len(calls); got != 1 {
		t.Fatalf("invocaciones=%d want 1", got)
	}
	if calls[0] != "claude-opus" {
		t.Errorf("motor invocó %q want claude-opus (subset manual)", calls[0])
	}
}

// TestRun_SelectorCancelEsExitOk: si el selector devuelve
// ErrSelectionCancelled, runPipelineRun devuelve nil (exit 0) e
// imprime el mensaje en stderr.
func TestRun_SelectorCancelEsExitOk(t *testing.T) {
	const pipe = `{
  "version": 1,
  "steps": [
    {"name": "explore", "agents": ["claude-opus"]}
  ]
}`
	mgr, _ := pipelineFixture(t, map[string]string{"work": pipe}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			t.Fatal("invoker no debería haberse llamado tras cancelación")
			return "", engine.FormatText, nil
		},
	}
	cancelSel := func(string, []string) ([]string, error) {
		return nil, runner.ErrSelectionCancelled
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, cancelSel, runRunArgs{
		pipelineFlag: "work",
		mode:         runner.ModeManual,
	})
	if err != nil {
		t.Fatalf("err: %v want nil (cancel es exit 0)", err)
	}
	if !strings.Contains(errOut.String(), "cancelado por el usuario") {
		t.Errorf("stderr sin mensaje de cancelación: %q", errOut.String())
	}
}

// TestRun_BannerIncluyeMode: el banner debe incluir el modo (auto-loop
// / manual) — lo consumen los tests de scripts y la doc.
func TestRun_BannerIncluyeMode(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"plain": minimalPipeline,
	}, "")
	inv := &fakeRunInvoker{
		responder: func(agent string) (string, engine.OutputFormat, error) {
			return "[next]", engine.FormatText, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runPipelineRun(context.Background(), &out, &errOut, mgr, inv, runner.AutoSelector, autoArgs("plain", ""))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(out.String(), "mode:     auto-loop") {
		t.Errorf("banner sin 'mode:     auto-loop': %q", out.String())
	}
}
