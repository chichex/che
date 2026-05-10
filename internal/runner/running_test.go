package runner

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/wizard"
)

// TestRenderLogPane_WrapsLongLineAtTerminalWidth verifica que las lineas del
// log pane (viewport del subprocess en R3) se wrappean al ancho del terminal
// cuando el modelo conoce un terminalWidth > 0 — antes del fix, una linea
// larga del stream del agente (ej. un text block de claude sin newlines
// internos) se cortaba contra el borde derecho del terminal.
//
// Con terminalWidth=0 (tests headless / antes del primer WindowSizeMsg) el
// render queda intacto — backward-compat con el comportamiento previo.
func TestRenderLogPane_WrapsLongLineAtTerminalWidth(t *testing.T) {
	long := "> " + strings.Repeat("palabra ", 30) + "fin"
	build := func(termWidth int) RunModel {
		rb := NewRingBuffer(10)
		rb.Append(LogLineStdout, long)
		return RunModel{
			Pipeline: wizard.Pipeline{
				Name:  "test",
				Steps: []wizard.Step{{Name: "step1", CLI: "claude", Kind: "prompt"}},
			},
			Steps:         []StepRun{{Idx: 1, Name: "step1", Status: StepStatusRunning}},
			Active:        0,
			LogFocus:      0,
			LogBuffers:    []*RingBuffer{rb},
			StickyBottom:  true,
			terminalWidth: termWidth,
		}
	}

	noWrap := renderLogPane(build(0))
	wrapped := renderLogPane(build(40))

	if strings.Count(wrapped, "\n") <= strings.Count(noWrap, "\n") {
		t.Fatalf("expected wrapped log pane to have extra newlines vs no-wrap; got noWrap=%d wrapped=%d\n--- noWrap ---\n%s\n--- wrapped ---\n%s",
			strings.Count(noWrap, "\n"), strings.Count(wrapped, "\n"), noWrap, wrapped)
	}

	// Ninguna linea del output wrappeado debe exceder el ancho util
	// (terminalWidth - margen de seguridad). Asi confirmamos que el wrap
	// efectivamente cortó la linea larga, no solo agregó newlines en otro
	// lugar.
	want := logPaneWidth(40)
	if want <= 0 {
		t.Fatalf("logPaneWidth(40) devolvio %d, esperaba > 0", want)
	}
	for _, line := range strings.Split(wrapped, "\n") {
		// Ignoramos las lineas que tienen ANSI (header con labelStyle): el
		// chequeo aplica al cuerpo del log donde el texto va raw.
		if strings.Contains(line, "\x1b[") {
			continue
		}
		if n := len([]rune(line)); n > want {
			t.Errorf("linea del log pane excede el ancho util (%d > %d): %q", n, want, line)
		}
	}
}

// TestLogPaneWidth_ZeroPreservesLegacyBehavior fija el contrato del helper:
// termWidth <= 0 devuelve 0 (signal "no wrappear"); termWidth chico (< 22)
// tambien devuelve 0 porque inner < 20 — preserva render legible en splits
// muy finos en lugar de hacer hard-break char-a-char.
func TestLogPaneWidth_ZeroPreservesLegacyBehavior(t *testing.T) {
	cases := []struct {
		termWidth int
		want      int
	}{
		{0, 0},
		{-1, 0},
		{10, 0},
		{21, 0},
		{22, 20},
		{80, 78},
	}
	for _, c := range cases {
		got := logPaneWidth(c.termWidth)
		if got != c.want {
			t.Errorf("logPaneWidth(%d) = %d, want %d", c.termWidth, got, c.want)
		}
	}
}

// TestMergeFeedbackIntoPayload_EmptyPassthrough verifica que un feedback
// vacio no toca el payload.
func TestMergeFeedbackIntoPayload_EmptyPassthrough(t *testing.T) {
	got := mergeFeedbackIntoPayload("PAYLOAD", "")
	if got != "PAYLOAD" {
		t.Errorf("expected passthrough on empty feedback, got %q", got)
	}
}

// TestMergeFeedbackIntoPayload_RawVerdictNotPrepended cubre Fix 3 (#107):
// si el feedback recibido es solo el fallback raw `"verdict: <token>"`, el
// bloque "FEEDBACK del validator" NO se prependea — el modelo no puede
// hacer nada con esa instruccion pelada. Defensa adicional al flag
// RawFeedbackOnly (que normalmente bloquea esto antes en el caller).
func TestMergeFeedbackIntoPayload_RawVerdictNotPrepended(t *testing.T) {
	cases := []string{
		"verdict: changes_requested",
		"verdict: fail",
		"verdict: needs_human",
		"  verdict: changes_requested  ",
	}
	for _, fb := range cases {
		got := mergeFeedbackIntoPayload("PAYLOAD", fb)
		if got != "PAYLOAD" {
			t.Errorf("expected raw verdict feedback %q to be suppressed; got=%q", fb, got)
		}
	}
}

// TestMergeFeedbackIntoPayload_RealFeedbackPrepended verifica que feedback
// real (no el fallback raw) si se prependea al payload.
func TestMergeFeedbackIntoPayload_RealFeedbackPrepended(t *testing.T) {
	got := mergeFeedbackIntoPayload("PAYLOAD", "corregi el edge case X")
	if !strings.Contains(got, "FEEDBACK del validator") {
		t.Errorf("expected feedback block prepended, got %q", got)
	}
	if !strings.Contains(got, "corregi el edge case X") {
		t.Errorf("expected feedback content in output, got %q", got)
	}
	if !strings.HasSuffix(got, "PAYLOAD") {
		t.Errorf("expected payload preserved at the end, got %q", got)
	}
}

// TestIsRawVerdictFallback_Patterns cubre cada caso del detector heuristico
// usado como red de seguridad en mergeFeedbackIntoPayload.
func TestIsRawVerdictFallback_Patterns(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"verdict: changes_requested", true},
		{"verdict: fail", true},
		{"verdict: ok", true},
		{"verdict: needs_human", true},
		{"  verdict: changes_requested  ", true},
		// feedback real con espacios — no es fallback.
		{"verdict: changes_requested\ncorregi X", false},
		{"verdict: changes - explicacion", false},
		{"verdict:", false},
		{"", false},
		{"otro string", false},
		// Tras TrimSpace, "verdict: fail\n" colapsa al patron raw — el
		// detector lo trata como fallback. El test fija el contrato.
		{"verdict: fail\n", true},
		// Verdict + feedback en lineas separadas no es fallback.
		{"verdict: fail\ncorregi X", false},
	}
	for _, c := range cases {
		got := isRawVerdictFallback(c.in)
		if got != c.want {
			t.Errorf("isRawVerdictFallback(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestStepEventsPath_Rotation cubre Fix 4 (#107): cada corrida del step
// (eventsRun K, 1-based) escribe a su propio archivo
// `step-NN.events.RUN-K.jsonl`. Si eventsRun <= 0, caemos al nombre legacy
// `step-NN.events.jsonl` para compat con tests viejos.
func TestStepEventsPath_Rotation(t *testing.T) {
	cases := []struct {
		runDir    string
		idx       int
		eventsRun int
		wantSubs  string
	}{
		{"/tmp/run-X", 1, 1, "step-01.events.RUN-01.jsonl"},
		{"/tmp/run-X", 3, 2, "step-03.events.RUN-02.jsonl"},
		{"/tmp/run-X", 1, 0, "step-01.events.jsonl"},
		{"/tmp/run-X", 1, -1, "step-01.events.jsonl"},
	}
	for _, c := range cases {
		got := stepEventsPath(c.runDir, c.idx, c.eventsRun)
		if !strings.HasSuffix(got, c.wantSubs) {
			t.Errorf("stepEventsPath(%q, %d, %d) = %q, want suffix %q",
				c.runDir, c.idx, c.eventsRun, got, c.wantSubs)
		}
	}
}
