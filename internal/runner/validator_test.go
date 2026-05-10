package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// wizardStepWithValidator construye un wizard.Step con bloque validator
// seteado al cli dado. Usado por los unit tests de newValidatorRun /
// parseVerdict para no depender del shape exacto del wizard.
func wizardStepWithValidator(cli string, maxLoops int, onMaxLoops string) wizard.Step {
	return wizard.Step{
		Name:    "test",
		CLI:     "gemini",
		Kind:    wizard.KindPrompt,
		Content: "noop",
		Input:   wizard.InputNone,
		Validator: &wizard.Validator{
			CLI:     cli,
			Kind:    wizard.KindPrompt,
			Content: "validate",
		},
		MaxLoops:   maxLoops,
		OnMaxLoops: onMaxLoops,
	}
}

// TestParseVerdict_OK cubre el camino feliz: el validator emite un bloque
// YAML simple con verdict: ok. parseVerdict devuelve Status="ok", feedback
// vacio.
func TestParseVerdict_OK(t *testing.T) {
	stdout := `verdict: ok
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected status %q, got %q", VerdictOk, v.Status)
	}
	if v.Feedback != "" {
		t.Errorf("expected empty feedback, got %q", v.Feedback)
	}
}

// TestParseVerdict_FailWithFeedback cubre el path tipico: verdict fail +
// feedback multilinea.
func TestParseVerdict_FailWithFeedback(t *testing.T) {
	stdout := `Aca el output del modelo con razonamiento previo.

verdict: fail
feedback: |
  faltan tests para el caso vacio
  ademas el formato del output esta mal
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected status %q, got %q", VerdictFail, v.Status)
	}
	if !strings.Contains(v.Feedback, "faltan tests") {
		t.Errorf("expected feedback to contain 'faltan tests', got %q", v.Feedback)
	}
}

// TestParseVerdict_NoBlock cubre el criterio del doc: "sin bloque verdict
// → fail + feedback: 'no verdict block'".
func TestParseVerdict_NoBlock(t *testing.T) {
	stdout := `lorem ipsum dolor sit amet
sin nada que parezca yaml
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected status fail, got %q", v.Status)
	}
	if v.Feedback != "no verdict block" {
		t.Errorf("expected feedback 'no verdict block', got %q", v.Feedback)
	}
}

// TestParseVerdict_Empty cubre el edge case de stdout vacio.
func TestParseVerdict_Empty(t *testing.T) {
	v := parseVerdict("")
	if v.Status != VerdictFail {
		t.Errorf("expected status fail, got %q", v.Status)
	}
	if v.Feedback != "no verdict block" {
		t.Errorf("expected feedback 'no verdict block', got %q", v.Feedback)
	}
}

// TestParseVerdict_LastBlockWins cubre el criterio del doc: "busca el
// ultimo bloque YAML con clave verdict". Si el modelo emitio 2 bloques
// (un borrador y uno final), gana el final.
func TestParseVerdict_LastBlockWins(t *testing.T) {
	stdout := `verdict: ok

intermedio que descartamos

verdict: fail
feedback: cambio de opinion
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected last block (fail) to win, got %q", v.Status)
	}
	if !strings.Contains(v.Feedback, "cambio de opinion") {
		t.Errorf("expected feedback from last block, got %q", v.Feedback)
	}
}

// TestParseVerdict_NormalizesUnknownToFail cubre el normalizer defensivo:
// cualquier valor distinto de "ok" se mapea a "fail".
func TestParseVerdict_NormalizesUnknownToFail(t *testing.T) {
	stdout := `verdict: maybe
feedback: estoy confundido
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected unknown verdict to map to fail, got %q", v.Status)
	}
}

// TestParseVerdict_SeparatorYAML cubre el caso donde el modelo separo
// bloques con `---` (separador yaml estandar). El parser tiene que
// tratarlo como divisor de bloques.
func TestParseVerdict_SeparatorYAML(t *testing.T) {
	stdout := `algo previo
---
verdict: ok
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected status ok across yaml separator, got %q", v.Status)
	}
}

// TestParseVerdict_ApproveAliasOK cubre el alias 3-vias del che-funnel:
// `verdict: approve` se canonicaliza a VerdictOk para que el loop avance
// igual que con `verdict: ok`.
func TestParseVerdict_ApproveAliasOK(t *testing.T) {
	stdout := `verdict: approve
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected approve to map to ok, got %q", v.Status)
	}
	if v.Feedback != "" {
		t.Errorf("expected empty feedback for ok, got %q", v.Feedback)
	}
}

// TestParseVerdict_ChangesRequestedMapsToFail cubre el otro extremo del
// alias 3-vias: changes_requested cae a fail y, sin feedback explicito,
// se preserva el token original como Feedback para el modal RP.
func TestParseVerdict_ChangesRequestedMapsToFail(t *testing.T) {
	stdout := `verdict: changes_requested
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected changes_requested to map to fail, got %q", v.Status)
	}
	if v.Feedback != "verdict: changes_requested" {
		t.Errorf("expected feedback to preserve original token, got %q", v.Feedback)
	}
}

// TestParseVerdict_NeedsHumanMapsToFail cubre needs_human del alias
// 3-vias: tambien fail con el token preservado en feedback. running.go
// no diferencia needs_human de changes_requested (ambos disparan retry o
// pause segun on_max_loops); el token ayuda al humano en el modal.
func TestParseVerdict_NeedsHumanMapsToFail(t *testing.T) {
	stdout := `verdict: needs_human
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected needs_human to map to fail, got %q", v.Status)
	}
	if v.Feedback != "verdict: needs_human" {
		t.Errorf("expected feedback to preserve original token, got %q", v.Feedback)
	}
}

// TestParseVerdict_ChangesRequestedKeepsExplicitFeedback cubre que cuando
// el validator si emitio un feedback explicito, parseVerdict lo respeta
// y NO sobrescribe con el token original.
func TestParseVerdict_ChangesRequestedKeepsExplicitFeedback(t *testing.T) {
	stdout := `verdict: changes_requested
feedback: |
  los criterios no son testeables
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected fail, got %q", v.Status)
	}
	if !strings.Contains(v.Feedback, "los criterios no son testeables") {
		t.Errorf("expected explicit feedback preserved, got %q", v.Feedback)
	}
}

// TestParseVerdict_CodexStreamJSON cubre el shape real de codex --json:
// el verdict del agente viene enterrado dentro de un agent_message. Sin la
// extraccion stream-json, parseVerdict devuelve "no verdict block" y el
// loop nunca avanza (bug observado en el run del 2026-05-10 sobre #99).
func TestParseVerdict_CodexStreamJSON(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"abc"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"voy a validar el plan"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"gh issue view 99","aggregated_output":"verdict: ok\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_14","type":"agent_message","text":"verdict: approve"}}
{"type":"turn.completed","usage":{"input_tokens":1000}}
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected approve from codex stream-json to map to ok, got %q (feedback=%q)", v.Status, v.Feedback)
	}
}

// TestParseVerdict_CodexStreamJSON_FailWins cubre el caso donde codex
// emite un agent_message intermedio con verdict: approve pero el final
// dice changes_requested — gana el ultimo (last-block-wins se preserva
// despues de extraer texto).
func TestParseVerdict_CodexStreamJSON_FailWins(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"draft inicial: verdict: approve"}}
{"type":"item.completed","item":{"type":"agent_message","text":"verdict: changes_requested\nfeedback: |\n  los criterios no son testeables"}}
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected last codex agent_message to win, got %q", v.Status)
	}
	if !strings.Contains(v.Feedback, "criterios no son testeables") {
		t.Errorf("expected feedback from last block, got %q", v.Feedback)
	}
}

// TestParseVerdict_ClaudeStreamJSON cubre el shape de claude --output-format
// stream-json --verbose: el verdict viene en assistant.message.content[].text.
// command_executions y tool_use no deben confundir al parser.
func TestParseVerdict_ClaudeStreamJSON(t *testing.T) {
	stdout := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"reviso el plan"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"verdict: approve"}]}}
{"type":"result","subtype":"success"}
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected approve from claude stream-json to map to ok, got %q", v.Status)
	}
}

// TestParseVerdict_RawStdoutFallback cubre el camino sin stream-json
// (validator gemini/opencode raw text): si nada parece JSON, parseVerdict
// debe seguir trabajando sobre el stdout crudo como antes.
func TestParseVerdict_RawStdoutFallback(t *testing.T) {
	stdout := `Algun razonamiento del modelo en texto plano.

verdict: ok
`
	v := parseVerdict(stdout)
	if v.Status != VerdictOk {
		t.Errorf("expected ok from raw text validator, got %q", v.Status)
	}
}

// TestParseVerdict_StreamJSONNoVerdict cubre el caso donde el validator
// emitio JSON correcto pero ningun agent_message contiene verdict — el
// parser cae a "no verdict block" como antes.
func TestParseVerdict_StreamJSONNoVerdict(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"olvide emitir el verdict"}}
{"type":"turn.completed"}
`
	v := parseVerdict(stdout)
	if v.Status != VerdictFail {
		t.Errorf("expected fail when verdict missing, got %q", v.Status)
	}
	if v.Feedback != "no verdict block" {
		t.Errorf("expected feedback 'no verdict block', got %q", v.Feedback)
	}
}

// TestRunValidator_StreamsLinesAndEmitsDone cubre el fix del bug "el log
// pane queda vacio durante el step de validate": runValidator antes corria
// el subprocess con cmd.Run() y stdout a un buffer, asi que ningun
// stepLineMsg llegaba al lineCh — el TUI solo veia el validatorDoneMsg
// final. Ahora el validator usa el mismo pipeline streaming que runStep
// (pipes + teeStream). El test verifica que:
//
//   - se emiten N stepLineMsg con Idx==StepIdx (1-based) y Line.Text
//     matcheando cada linea del stdout del subprocess fake;
//   - el validatorDoneMsg final llega con Verdict ok y RawStdout
//     conteniendo TODO el stdout (para parseVerdict);
//   - el archivo stdout.log del validator se persiste con el contenido
//     completo.
func TestRunValidator_StreamsLinesAndEmitsDone(t *testing.T) {
	// Fake stdout multilinea — un agente "raw text" que termina con
	// "verdict: ok" para que parseVerdict devuelva ok. El blank line antes
	// del verdict block es necesario para que parseVerdict lo trate como
	// candidate YAML separado (mismo patron que el resto de los tests de
	// parseVerdict para validators raw text).
	fakeOut := "linea 1 del validator\nlinea 2 con tool use\n\nverdict: ok\n"

	prev := validatorSpawnCmdFn
	t.Cleanup(func() { validatorSpawnCmdFn = prev })
	validatorSpawnCmdFn = func(step wizard.Step, payload string) (*exec.Cmd, error) {
		// printf '%b' expande los \n del payload — emite las 3 lineas
		// del fakeOut + newlines reales. Reusamos /bin/sh -c para evitar
		// depender de un fake binario en PATH.
		return exec.Command("/bin/sh", "-c", "printf '%b' \""+fakeOut+"\""), nil
	}

	runDir := t.TempDir()
	step := wizardStepWithValidator("gemini", 1, "fail")
	state := &runState{requestCancel: make(chan struct{}, 1)}

	cmd := runValidator(step, "input payload", runDir, 1, 1, state)
	if cmd == nil {
		t.Fatal("runValidator returned nil cmd")
	}

	// Drenar lineCh: stepLineMsg... + validatorDoneMsg final. Timeout
	// generoso (2s) para no flakear en CI lento — el fake termina inmediato.
	var lines []stepLineMsg
	var done *validatorDoneMsg
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout esperando validatorDoneMsg; lineas drenadas: %d", len(lines))
		default:
		}
		msg := cmd()
		switch m := msg.(type) {
		case stepLineMsg:
			lines = append(lines, m)
			// El handler real re-issuea waitForLine para drenar la siguiente
			// linea — replicamos eso en el test.
			cmd = waitForLine(state.lineCh)
		case validatorDoneMsg:
			done = &m
			break loop
		default:
			t.Fatalf("msg inesperado: %T %+v", msg, msg)
		}
	}

	if done == nil {
		t.Fatal("no validatorDoneMsg drenado")
	}
	if done.Verdict.Status != VerdictOk {
		t.Errorf("verdict status: want %q, got %q (feedback=%q)", VerdictOk, done.Verdict.Status, done.Verdict.Feedback)
	}
	if !strings.Contains(done.RawStdout, "verdict: ok") {
		t.Errorf("RawStdout: missing verdict marker, got %q", done.RawStdout)
	}

	// Esperamos 1 stepLineMsg por linea del stdout (3 lineas en fakeOut).
	if len(lines) == 0 {
		t.Fatal("ningun stepLineMsg recibido — el log pane quedaria vacio (bug original)")
	}
	for _, l := range lines {
		if l.Idx != 1 {
			t.Errorf("stepLineMsg.Idx: want 1 (step idx), got %d", l.Idx)
		}
	}
	// Joinear los textos y verificar que las lineas del fake aparecen.
	var allText strings.Builder
	for _, l := range lines {
		allText.WriteString(l.Line.Text)
		allText.WriteString("\n")
	}
	if !strings.Contains(allText.String(), "linea 1 del validator") {
		t.Errorf("stepLineMsg lines missing first stdout line; got:\n%s", allText.String())
	}
	if !strings.Contains(allText.String(), "verdict: ok") {
		t.Errorf("stepLineMsg lines missing verdict marker; got:\n%s", allText.String())
	}

	// stdout.log del validator debe contener TODO el stdout (la persistencia
	// a disco no debe haberse roto al cambiar a streaming).
	logPath := filepath.Join(runDir, "step-01.validator.01.stdout.log")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("leyendo stdout.log: %v", err)
	}
	if !strings.Contains(string(raw), "linea 1 del validator") || !strings.Contains(string(raw), "verdict: ok") {
		t.Errorf("stdout.log incompleto, got:\n%s", string(raw))
	}
}

// TestNewValidatorRun_Defaults cubre los defaults del ValidatorRun cuando
// el yaml omite max_loops / on_max_loops (defensivo — el wizard fuerza
// valores razonables pero un yaml a mano podria omitirlos).
func TestNewValidatorRun_Defaults(t *testing.T) {
	step := wizardStepWithValidator("gemini", 0, "")
	v := newValidatorRun(step)
	if v.MaxLoops != 1 {
		t.Errorf("expected default MaxLoops=1 when missing, got %d", v.MaxLoops)
	}
	if v.OnMaxLoops != "fail" {
		t.Errorf("expected default OnMaxLoops=fail when missing, got %q", v.OnMaxLoops)
	}
	if v.LoopsRun != 0 {
		t.Errorf("expected initial LoopsRun=0, got %d", v.LoopsRun)
	}
	if v.FinalVerdict != "" {
		t.Errorf("expected empty FinalVerdict initially, got %q", v.FinalVerdict)
	}
}
