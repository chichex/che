package runner

import (
	"strings"
	"testing"

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
