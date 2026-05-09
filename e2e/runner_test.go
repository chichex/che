package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestRunner_R0H1Skeleton cubre H1 del flow del runner (wire-up): enter
// sobre un row ready en `My pipelines` debe disparar la pantalla skeleton del
// runner. esc vuelve al lister; q sale total. Sin tocar disco — no debe
// existir ~/.che/runs/ post-run.
//
// Nombre del test sigue el patron `TestRunner_R<screen>H<story><caso>`:
// R0 porque el trigger del wire-up vive en el lister (R0 segun el doc); el
// test arranca ahi y verifica que el skeleton del runner se renderiza.
func TestRunner_R0H1Skeleton(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Pre-armar un yaml ready (sin bloque status). readyYAML viene de
	// wizard_h8_h9_h10_test.go en el mismo package y devuelve un pipeline
	// con un step text/claude — alcanza para que IsValid pase con la
	// deteccion de la harness (claude esta symlinkeado a chefake).
	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"runner-h1.yaml": readyYAML("Runner H1"),
	})

	p := env.StartPTY()
	defer p.Close()

	// Menu → My pipelines.
	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1 (My pipelines): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Runner H1", 3*time.Second) {
		t.Fatalf("ready entry never rendered\n%s", p.Since(mark))
	}

	// enter sobre el row ready → runner skeleton.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run · Runner H1", 3*time.Second) {
		t.Fatalf("runner skeleton never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "runner pendiente") {
		t.Errorf("expected 'runner pendiente' placeholder; got:\n%s", p.Since(mark))
	}

	// esc vuelve al lister (no sale del proceso).
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}

	// Sin tocar disco: no debe existir ~/.che/runs/ — H1 es solo wire-up.
	runsDir := filepath.Join(env.HomeDir, ".che", "runs")
	if _, err := os.Stat(runsDir); !os.IsNotExist(err) {
		t.Errorf("expected ~/.che/runs/ to not exist, stat err=%v", err)
	}

	// esc desde el lister vuelve al menu.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (lister→menu): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
}

// TestRunner_R0H1QuitFromSkeleton cubre la rama complementaria: q desde el
// runner skeleton sale total (no vuelve al lister, no vuelve al menu). Es la
// otra mitad del wire-up — esc vs q son las dos transiciones declaradas en
// los criterios de aceptacion de H1.
func TestRunner_R0H1QuitFromSkeleton(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"runner-h1-quit.yaml": readyYAML("Runner Quit"),
	})

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Runner Quit", 3*time.Second) {
		t.Fatalf("ready entry never rendered\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run · Runner Quit", 3*time.Second) {
		t.Fatalf("runner skeleton never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}

	// Sin disco tocado tampoco en este flow.
	runsDir := filepath.Join(env.HomeDir, ".che", "runs")
	if _, err := os.Stat(runsDir); !os.IsNotExist(err) {
		t.Errorf("expected ~/.che/runs/ to not exist, stat err=%v", err)
	}
}
