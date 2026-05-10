package runner

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/wizard"
)

// TestViewInput_WrapsLongTextAtTerminalWidth verifica que el input multilinea
// de R1 (kind=text) envuelve un prompt largo cuando hay un terminalWidth que
// permite calcular un inner > 0, y que con terminalWidth=0 (tests headless /
// pre-WindowSizeMsg) el render queda intacto — backward-compat con tests sin
// pty del paquete.
func TestViewInput_WrapsLongTextAtTerminalWidth(t *testing.T) {
	long := strings.Repeat("a", 200)
	buf := newTextBuffer(true)
	for _, r := range long {
		buf.insertRune(r)
	}

	build := func(termWidth int) RunModel {
		return RunModel{
			Pipeline: wizard.Pipeline{Name: "test"},
			inputUI: inputUIState{
				kind:    wizard.InputText,
				textBuf: buf,
			},
			terminalWidth: termWidth,
		}
	}

	noWrap := build(0).viewInput()
	wrapped := build(40).viewInput()

	if strings.Count(wrapped, "\n") <= strings.Count(noWrap, "\n") {
		t.Fatalf("expected wrapped output to contain extra newlines vs no-wrap; got noWrap=%d wrapped=%d", strings.Count(noWrap, "\n"), strings.Count(wrapped, "\n"))
	}
}
