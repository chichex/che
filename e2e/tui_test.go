package e2e_test

import (
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestTUI_MenuRoutesItem2ToWizard valida H1: digit 2 abre el wizard
// skeleton, esc vuelve al menu, q sale con exit 0.
func TestTUI_MenuRoutesItem2ToWizard(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered Create pipeline label\nout:\n%s", p.Snapshot())
	}
	if !strings.Contains(p.Snapshot(), "My pipelines") {
		t.Fatalf("menu missing My pipelines label\nout:\n%s", p.Snapshot())
	}

	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "wizard pendiente", 3*time.Second) {
		t.Fatalf("wizard skeleton never rendered\nout:\n%s", p.Snapshot())
	}

	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	// Tras esc el wizard cierra y el loop de root.go re-muestra el menu.
	// Si el menu no redibuja "Create pipeline" en este tramo, el routing
	// "back" esta roto.
	if !p.WaitForOutputSince(t, mark, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never re-rendered after esc\nsince mark:\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
}

// TestTUI_MenuItem1IsNotImplemented valida H1: digit 1 selecciona
// "My pipelines" y dispara el placeholder con exit 1.
func TestTUI_MenuItem1IsNotImplemented(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "My pipelines", 3*time.Second) {
		t.Fatalf("menu never rendered My pipelines label\nout:\n%s", p.Snapshot())
	}

	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	res := p.Wait(t, 3*time.Second)

	if res.ExitCode != 1 {
		t.Fatalf("expected exit 1, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
	for _, want := range []string{"My pipelines", "not implemented"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("expected output to contain %q\nout:\n%s", want, res.Stdout)
		}
	}
}
