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
