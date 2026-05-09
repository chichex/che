package e2e_test

import (
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestTUI_MenuRoutesItem2ToWizard valida que digit 2 abra el wizard
// (S1 PipelineInfo en H2+) y que el modal de cancel devuelva el control
// al menu sin crashear.
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
	// S1 muestra "paso 1/3" y los labels Nombre/Descripcion.
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 PipelineInfo never rendered\nsince mark:\n%s", p.Since(mark))
	}

	// esc en S1 sin path skipea el modal SC (no hay nada que keep ni
	// que discardear): cae directo al menu.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after S1 esc\nsince mark:\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
}

// TestTUI_MenuItem1OpensMyPipelines valida H9: digit 1 abre la pantalla
// "My pipelines"; con HOME limpio renderiza el placeholder vacio. q sale
// con exit 0.
func TestTUI_MenuItem1OpensMyPipelines(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "no pipelines yet", 3*time.Second) {
		t.Fatalf("My pipelines screen never rendered\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
}
