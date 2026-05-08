package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestWizard_S1PersistsDraft valida H2 sobre el flujo H3: completar S1
// (name + description) y avanzar con ctrl+s lleva a S2; volver a S1 con
// esc deja el archivo con status.stage=info y los campos de S1 intactos.
func TestWizard_S1PersistsDraft(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\nout:\n%s", p.Snapshot())
	}

	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	// Tipear el nombre. Foco arranca en Nombre.
	if err := p.Send("Triage Checkout Flow"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	// Ir a Descripcion con tab.
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab: %v", err)
	}
	if err := p.Send("Toma una metrica anomala"); err != nil {
		t.Fatalf("send desc: %v", err)
	}

	// Avanzar con ctrl+s — en H3 esto lleva a S2 (no mas placeholder).
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("never reached S2\n%s", p.Since(mark))
	}

	// Volver a S1 via esc — H2 invariant: name + description quedan
	// guardados; status revierte a stage=info.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("never returned to S1\n%s", p.Since(mark))
	}

	// Verificar el archivo en HOME sandbox.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "triage-checkout-flow.yaml")
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected draft file at %s, got err=%v", expected, err)
	}
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	// El `name` del YAML guarda lo tipeado por el usuario (espacios,
	// case, etc.). El slug se usa solo para el filename.
	for _, want := range []string{
		"status:",
		"stage: info",
		"name: Triage Checkout Flow",
		"description: Toma una metrica anomala",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in draft YAML; got:\n%s", want, body)
		}
	}

	// Volver al menu via SC keep.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	// El draft sigue en disco tras volver al menu sin discard.
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("draft should remain on disk after keep: err=%v", err)
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nout:\n%s", res.ExitCode, res.Stdout)
	}
}

// TestWizard_S1Collision valida que un segundo intento con el mismo
// nombre dispara el modal "el nombre ya existe", y que cancelar conserva
// el archivo previo intacto.
func TestWizard_S1Collision(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Pre-armar un draft existente: simulamos que ya hubo un primer run.
	dir := filepath.Join(env.HomeDir, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	preExisting := filepath.Join(dir, "demo.yaml")
	original := []byte("status:\n  stage: info\n  last_saved_at: 2026-01-01T00:00:00Z\nname: demo\ndescription: original\n")
	if err := os.WriteFile(preExisting, original, 0o600); err != nil {
		t.Fatalf("write pre-existing: %v", err)
	}

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("demo"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "ya existe", 3*time.Second) {
		t.Fatalf("collision modal never opened\n%s", p.Since(mark))
	}

	// Elegir "elegir otro" (cancelar) → volvemos a S1, archivo intacto.
	mark = p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("did not return to S1\n%s", p.Since(mark))
	}

	got, err := os.ReadFile(preExisting)
	if err != nil {
		t.Fatalf("read after cancel: %v", err)
	}
	if !strings.Contains(string(got), "description: original") {
		t.Errorf("pre-existing file got mutated:\n%s", got)
	}

	// Salir via SC keep (sin path, no toca disco).
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened\n%s", p.Since(mark))
	}
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}

	mark = p.Mark()
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

// TestWizard_S1DiscardRemovesDraft valida la rama discard del modal SC:
// tras escribir el nombre + ctrl+s para crear el archivo, esc + "discard"
// lo elimina.
func TestWizard_S1DiscardRemovesDraft(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("ToDelete"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("never reached S2\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "todelete.yaml")
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("draft missing pre-discard: %v", err)
	}

	// esc en S2 vuelve a S1; segundo esc abre SC.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (S2→S1): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("never returned to S1\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (S1→SC): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened from S1\n%s", p.Since(mark))
	}
	// "2" → discard.
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(expected); os.IsNotExist(err) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Errorf("draft still exists after discard: err=%v", err)
	}

	mark = p.Mark()
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		// best-effort: si el menu no rerenders en este wait, igual el
		// proceso debe terminar al mandar q
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H3CreateFirstStep cubre los criterios de H3 + flujo H6: S1 → S2
// step 1 mode=create con kind=prompt → ctrl+s → S3 (resumen) → ctrl+s → S4
// (ready) → enter → menu. El YAML final NO trae bloque status (ready).
func TestWizard_S2H3CreateFirstStep(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	// S1: nombre + descripcion.
	if err := p.Send("Demo H3"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab: %v", err)
	}
	if err := p.Send("step uno demo"); err != nil {
		t.Fatalf("send desc: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// S2: focus arranca en name. Tipear, tab x3 hasta content (cli y kind
	// quedan en default claude/prompt — el harness instala claude como
	// fake binary, asi que la pill se considera installed).
	if err := p.Send("collect-signals"); err != nil {
		t.Fatalf("send step name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab #%d: %v", i, err)
		}
	}
	// Tipear el prompt.
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send content: %v", err)
	}
	// tab → input (default text). ctrl+s lleva a S3.
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab→input: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered after step save\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered after pipeline save\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after enter\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h3.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"name: Demo H3",
		"steps:",
		"- name: collect-signals",
		"cli: claude",
		"kind: prompt",
		"content: hola",
		"input: text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}
	// Pipeline ready post-H6: el bloque status fue stripeado.
	for _, unwanted := range []string{"status:", "stage:", "step_mode:", "step_idx:"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("ready pipeline should not contain %q; got:\n%s", unwanted, body)
		}
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H4ValidatorOn cubre H4 con el toggle de validator encendido:
// completar S2 + toggle on + dejar defaults (cli=claude, kind=prompt,
// max_loops=3, on_max_loops=fail) + content del validator escrito a mano,
// ctrl+s para guardar. El YAML resultante debe traer el bloque validator
// con sus 3 sub-keys + max_loops + on_max_loops.
func TestWizard_S2H4ValidatorOn(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	// S1
	if err := p.Send("Demo H4 On"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// S2 — Name → CLI → Kind → Content
	if err := p.Send("collect-signals"); err != nil {
		t.Fatalf("send step name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab #%d: %v", i, err)
		}
	}
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send step content: %v", err)
	}

	// Content → Input → ValToggle.
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab→Input: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab→ValToggle: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Validar este step", 3*time.Second) {
		t.Fatalf("validator toggle never rendered\n%s", p.Since(mark))
	}

	// Toggle on con "y".
	mark = p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Bloque validator", 3*time.Second) {
		t.Fatalf("validator block never appeared after toggle\n%s", p.Since(mark))
	}

	// ValToggle → ValCLI (default claude) → ValKind (default prompt) →
	// ValContent.
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab into validator block #%d: %v", i, err)
		}
	}
	if err := p.Send("revisa output"); err != nil {
		t.Fatalf("send validator content: %v", err)
	}

	// Defaults max_loops=3 / on_max_loops=fail — no tocar.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h4-on.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"- name: collect-signals",
		"cli: claude",
		"kind: prompt",
		"content: hola",
		"input: text",
		"validator:",
		"content: revisa output",
		"max_loops: 3",
		"on_max_loops: fail",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H4ValidatorOff cubre H4 con el toggle apagado (default):
// completar S2 sin tocar el toggle, ctrl+s. El YAML no debe traer
// validator / max_loops / on_max_loops.
func TestWizard_S2H4ValidatorOff(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("Demo H4 Off"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("collect-signals"); err != nil {
		t.Fatalf("send step name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab #%d: %v", i, err)
		}
	}
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab→Input: %v", err)
	}

	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h4-off.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"- name: collect-signals",
		"cli: claude",
		"kind: prompt",
		"content: hola",
		"input: text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"validator:", "max_loops:", "on_max_loops:"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("validator off should omit %q; got:\n%s", unwanted, body)
		}
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H5LoopTwoSteps cubre H5 con el flujo "+ agregar step":
// completar S2 step 1 con input=text, ctrl+n para avanzar a step 2,
// completar step 2 dejando el default input=previous_output, ctrl+s. El
// YAML resultante debe traer 2 steps; steps[1].input == "previous_output".
func TestWizard_S2H5LoopTwoSteps(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	// S1
	if err := p.Send("Demo H5 Loop"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// Step 1: nombre + cli/kind default + content "alpha" + input=text default.
	if err := p.Send("collect"); err != nil {
		t.Fatalf("send step1 name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab #%d: %v", i, err)
		}
	}
	if err := p.Send("alpha"); err != nil {
		t.Fatalf("send step1 content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("send tab→Input step1: %v", err)
	}

	// ctrl+n para pushear step 1 + entrar a step 2 mode=create.
	mark = p.Mark()
	if err := p.Send("\x0e"); err != nil {
		t.Fatalf("send ctrl+n: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 2 (create)", 3*time.Second) {
		t.Fatalf("step 2 never rendered after ctrl+n\n%s", p.Since(mark))
	}

	// Step 2: foco arranca en name. Default input=previous_output (idx>=1).
	if err := p.Send("digest"); err != nil {
		t.Fatalf("send step2 name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab step2 #%d: %v", i, err)
		}
	}
	if err := p.Send("beta"); err != nil {
		t.Fatalf("send step2 content: %v", err)
	}

	// ctrl+s — guarda step 2 + va a S3.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save step2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h5-loop.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)

	// Asserts globales: 2 steps, segundo con input=previous_output.
	for _, want := range []string{
		"- name: collect",
		"content: alpha",
		"input: text",
		"- name: digest",
		"content: beta",
		"input: previous_output",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}
	// Heuristica: el YAML debe tener exactamente 2 entradas "- name:" en
	// la lista de steps. Si vemos 3+ algo se duplico.
	if got := strings.Count(body, "- name:"); got != 2 {
		t.Errorf("expected 2 steps in YAML, got %d:\n%s", got, body)
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H5BackFromStep2 cubre la rama "atras" de H5: tras ctrl+n
// desde step 1, esc en step 2 mode=create vuelve a step 1 mode=edit con
// los campos pre-cargados. Modificar el name + ctrl+s deja un YAML con 1
// solo step (el modificado): step 2 mode=create se descarta porque nunca
// llego a pushearse a pipeline.Steps.
func TestWizard_S2H5BackFromStep2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("Demo H5 Back"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// Step 1 completo.
	if err := p.Send("first"); err != nil {
		t.Fatalf("send step1 name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("send tab #%d: %v", i, err)
		}
	}
	if err := p.Send("alpha"); err != nil {
		t.Fatalf("send step1 content: %v", err)
	}

	// ctrl+n → push step 1, entrar a step 2 mode=create.
	mark = p.Mark()
	if err := p.Send("\x0e"); err != nil {
		t.Fatalf("send ctrl+n: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 2 (create)", 3*time.Second) {
		t.Fatalf("step 2 never rendered\n%s", p.Since(mark))
	}

	// Tipear algo en step 2 que NO debe persistir (mode=create se
	// descarta al ir atras).
	if err := p.Send("ghost-step"); err != nil {
		t.Fatalf("send step2 name: %v", err)
	}

	// esc → vuelta a step 1 mode=edit.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (edit)", 3*time.Second) {
		t.Fatalf("never returned to step 1 mode=edit\n%s", p.Since(mark))
	}

	// El name del step 1 debe estar pre-cargado: agregamos sufijo para
	// verificar que la modificacion persiste. Foco arranca en name.
	if err := p.Send("-renamed"); err != nil {
		t.Fatalf("send rename suffix: %v", err)
	}

	// ctrl+s — guarda step 1 actualizado, va a S3.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save step1 edit: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h5-back.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "- name: first-renamed") {
		t.Errorf("expected step 1 renamed; got:\n%s", body)
	}
	if strings.Contains(body, "ghost-step") {
		t.Errorf("step 2 buffer should not persist; got:\n%s", body)
	}
	if got := strings.Count(body, "- name:"); got != 1 {
		t.Errorf("expected exactly 1 step in YAML, got %d:\n%s", got, body)
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2H3BackToInfo cubre la rama "atras" de H3: en S2 step 1
// mode=create, esc revierte status.stage a info y descarta lo tipeado del
// step. El archivo persiste con name+description pero sin steps.
func TestWizard_S2H3BackToInfo(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}
	if err := p.Send("Back Demo"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// Tipear algo en el name del step para verificar que se descarta.
	if err := p.Send("ghost"); err != nil {
		t.Fatalf("send ghost: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("never returned to S1\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "back-demo.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "stage: info") {
		t.Errorf("expected stage: info after back; got:\n%s", body)
	}
	if strings.Contains(body, "stage: step") {
		t.Errorf("stage should not be step after back; got:\n%s", body)
	}
	if strings.Contains(body, "steps:") {
		t.Errorf("steps section should be absent after back; got:\n%s", body)
	}
	if strings.Contains(body, "ghost") {
		t.Errorf("ghost step name should not be persisted; got:\n%s", body)
	}

	// Salir limpio via SC keep.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (S1→SC): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after keep\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S2YAMLCombinations es un end-to-end "ancho": construye un
// pipeline de 3 steps que ejercita la mayor combinacion posible de campos
// del wizard, y verifica que el YAML final tenga sentido.
//
// Cobertura del happy-path (en orden):
//
//	step 1 | prompt | claude | input=text            | sin validator
//	step 2 | skill  | claude | input=previous_output | validator cross-CLI (codex/prompt, max=5, on=continue)
//	step 3 | prompt | codex  | input=pr              | validator self-CLI (codex/prompt, max=3 default, on=fail default)
//
// Pre-seeding: drop una SKILL.md minima en HOME del sandbox para que
// internal/skills.Detect ofrezca al menos una skill cuando step 2 toca
// la toggle Kind=skill. El wizard cachea la deteccion en enterStepCreate
// del primer step, asi que el seed tiene que estar antes del PTY.
func TestWizard_S2YAMLCombinations(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Seed: ~/.claude/skills/summarize-output/SKILL.md
	skillDir := filepath.Join(env.HomeDir, ".claude", "skills", "summarize-output")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	seed := "---\nname: summarize-output\ndescription: test skill seeded by harness\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(seed), 0o600); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	// ---------- S1 ----------
	if err := p.Send("Combo Demo"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab name->desc: %v", err)
	}
	if err := p.Send("Pipeline con varias combinaciones"); err != nil {
		t.Fatalf("send desc: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S1->S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 step 1 never rendered\n%s", p.Since(mark))
	}

	// ---------- STEP 1: prompt + claude + text + sin validator ----------
	if err := p.Send("fetch-data"); err != nil {
		t.Fatalf("send step1 name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab #%d step1: %v", i, err)
		}
	}
	if err := p.Send("fetch the PR title from input"); err != nil {
		t.Fatalf("send step1 content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab content->input step1: %v", err)
	}

	mark = p.Mark()
	if err := p.Send("\x0e"); err != nil {
		t.Fatalf("ctrl+n step1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 2 (create)", 3*time.Second) {
		t.Fatalf("step 2 never rendered\n%s", p.Since(mark))
	}

	// ---------- STEP 2: skill + claude + previous_output + validator cross-CLI ----------
	if err := p.Send("summarize"); err != nil {
		t.Fatalf("send step2 name: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab name->cli step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab cli->kind step2: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("s"); err != nil {
		t.Fatalf("send s (skill) step2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "summarize-output", 3*time.Second) {
		t.Fatalf("skill picker never showed seeded skill\n%s", p.Since(mark))
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab kind->content step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab content->input step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab input->valtoggle step2: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y validator step2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Bloque validator", 3*time.Second) {
		t.Fatalf("validator block never appeared step2\n%s", p.Since(mark))
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab valtoggle->valcli step2: %v", err)
	}
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 valcli=codex step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab valcli->valkind step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab valkind->valcontent step2: %v", err)
	}
	if err := p.Send("verify the summary is under 200 chars"); err != nil {
		t.Fatalf("send valcontent step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab valcontent->valmaxloops step2: %v", err)
	}
	if err := p.Send("5"); err != nil {
		t.Fatalf("send 5 valmaxloops step2: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab valmaxloops->valonmax step2: %v", err)
	}
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 valonmax=continue step2: %v", err)
	}

	mark = p.Mark()
	if err := p.Send("\x0e"); err != nil {
		t.Fatalf("ctrl+n step2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 3 (create)", 3*time.Second) {
		t.Fatalf("step 3 never rendered\n%s", p.Since(mark))
	}

	// ---------- STEP 3: prompt + codex + pr + validator self-CLI defaults ----------
	if err := p.Send("report"); err != nil {
		t.Fatalf("send step3 name: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab name->cli step3: %v", err)
	}
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 cli=codex step3: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab into content step3 #%d: %v", i, err)
		}
	}
	if err := p.Send("produce final report"); err != nil {
		t.Fatalf("send step3 content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab content->input step3: %v", err)
	}
	if err := p.Send("3"); err != nil {
		t.Fatalf("send 3 input=pr step3: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab input->valtoggle step3: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y validator step3: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Bloque validator", 3*time.Second) {
		t.Fatalf("validator block never appeared step3\n%s", p.Since(mark))
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab into valcontent step3 #%d: %v", i, err)
		}
	}
	if err := p.Send("check format"); err != nil {
		t.Fatalf("send valcontent step3: %v", err)
	}

	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s final step3: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}

	// ---------- ASSERTS ----------
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "combo-demo.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)

	for _, want := range []string{
		"name: Combo Demo",
		"description: Pipeline con varias combinaciones",
		"steps:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}

	if got := strings.Count(body, "- name:"); got != 3 {
		t.Errorf("expected 3 steps, got %d:\n%s", got, body)
	}

	for _, want := range []string{
		"- name: fetch-data",
		"fetch the PR title from input",
		"input: text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("step1 missing %q:\n%s", want, body)
		}
	}

	for _, want := range []string{
		"- name: summarize",
		"kind: skill",
		"content: summarize-output",
		"input: previous_output",
		"validator:",
		"verify the summary is under 200 chars",
		"max_loops: 5",
		"on_max_loops: continue",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("step2 missing %q:\n%s", want, body)
		}
	}

	for _, want := range []string{
		"- name: report",
		"produce final report",
		"input: pr",
		"check format",
		"max_loops: 3",
		"on_max_loops: fail",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("step3 missing %q:\n%s", want, body)
		}
	}

	// Conteos cruzados.
	if got := strings.Count(body, "cli: codex"); got < 3 {
		t.Errorf("expected ≥3 occurrences of 'cli: codex' (step3 + 2 validators), got %d:\n%s", got, body)
	}
	if got := strings.Count(body, "cli: claude"); got != 2 {
		t.Errorf("expected exactly 2 occurrences of 'cli: claude' (step1+step2), got %d:\n%s", got, body)
	}
	if got := strings.Count(body, "validator:"); got != 2 {
		t.Errorf("expected 2 validators, got %d:\n%s", got, body)
	}

	// Step 1 NO debe traer claves de validator (toggle off).
	step1Block := body
	if i := strings.Index(body, "- name: summarize"); i > 0 {
		step1Block = body[:i]
	}
	for _, unwanted := range []string{"validator:", "max_loops:", "on_max_loops:"} {
		if strings.Contains(step1Block, unwanted) {
			t.Errorf("step1 should not have %q (validator off):\n%s", unwanted, step1Block)
		}
	}

	// Pipeline ready post-H6: el bloque status fue stripeado al guardar
	// desde S3 (ctrl+s). Cualquier residuo indicaria que el wizard no
	// llego a finalizar.
	for _, unwanted := range []string{"status:", "stage:", "step_mode:", "step_idx:"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("ready pipeline should not contain %q; got:\n%s", unwanted, body)
		}
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S3SummarySavesReady cubre el happy-path de H6: completar S1 + S2
// + entrar al modal "Step listo" via enter en el ultimo foco + elegir
// "finalizar pipeline" → S3 → ctrl+s → S4 → enter → menu. El YAML final
// debe ser ready (sin bloque status) y conservar name + steps.
func TestWizard_S3SummarySavesReady(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("Demo H6 Ready"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// Step 1 con campos minimos. Llegamos al ultimo foco (ValToggle) por tab.
	if err := p.Send("collect"); err != nil {
		t.Fatalf("send step name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab #%d: %v", i, err)
		}
	}
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→input: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→valtoggle: %v", err)
	}

	// enter en el ultimo foco abre el modal "Step listo".
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter (open modal): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Step listo", 3*time.Second) {
		t.Fatalf("save-choice modal never opened\n%s", p.Since(mark))
	}

	// "finalizar pipeline" = opcion 2 en mode=create.
	mark = p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 (finalizar): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered after modal finalizar\n%s", p.Since(mark))
	}

	// Mientras seguimos en S3 el archivo debe traer status.stage=summary.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h6-ready.yaml")
	{
		data, err := os.ReadFile(expected)
		if err != nil {
			t.Fatalf("read draft pre-save: %v", err)
		}
		if !strings.Contains(string(data), "stage: summary") {
			t.Errorf("expected stage: summary in S3 draft; got:\n%s", data)
		}
	}

	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}

	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"name: Demo H6 Ready",
		"steps:",
		"- name: collect",
		"content: hola",
		"input: text",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in YAML; got:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"status:", "stage:", "step_mode:", "step_idx:", "last_saved_at"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("ready pipeline should not contain %q; got:\n%s", unwanted, body)
		}
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter (S4→menu): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after S4\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S3InvalidStays cubre la rama invalida de H6: pipeline lleva
// validator gemini, y para cuando el usuario toca ctrl+s en S3 gemini ya
// no esta en PATH (la skill desinstalo entremedio). IsValid falla → S3
// sigue visible con el error inline → el archivo NO se ready-fica.
//
// Usamos un binario fake llamado `gemini-real` que apunta al chefake
// (idem fakeIdentities) y un symlink "gemini" hacia el. Al desactivar
// el segundo entre S2 y S3, el wizard sigue funcionando (skillsCache
// ya quedo lleno desde el primer enterStepCreate) pero IsValid corre
// detectInstalledCLIs en vivo y reporta gemini missing.
func TestWizard_S3InvalidStays(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("Demo H6 Invalid"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	// Step 1: cli=claude (default), kind=prompt + validator gemini cross-CLI.
	if err := p.Send("first"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab #%d: %v", i, err)
		}
	}
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→input: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→valtoggle: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Bloque validator", 3*time.Second) {
		t.Fatalf("validator block never appeared\n%s", p.Since(mark))
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→valcli: %v", err)
	}
	// gemini = pill 3 (claude/codex/gemini/opencode).
	if err := p.Send("3"); err != nil {
		t.Fatalf("send 3 valcli=gemini: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→valkind: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→valcontent: %v", err)
	}
	if err := p.Send("verifica formato"); err != nil {
		t.Fatalf("send valcontent: %v", err)
	}

	// Antes de ctrl+s, removemos gemini de PATH. Asi cuando IsValid corre
	// en S3 ya lo ve missing.
	env.RemoveFake("gemini")

	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}

	// ctrl+s en S3 → IsValid falla con gemini missing → seguimos en S3.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save pipeline: %v", err)
	}
	// Sigue en S3: el header del paso 3/3 sigue visible y el error
	// "no esta instalado" aparece. Damos tiempo a que el render incluya
	// el mensaje de error.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := p.Since(mark)
		if strings.Contains(snap, "no se puede guardar") && strings.Contains(snap, "gemini") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := p.Since(mark)
	if !strings.Contains(snap, "no se puede guardar") {
		t.Errorf("expected validation error banner; got:\n%s", snap)
	}
	if !strings.Contains(snap, "gemini") {
		t.Errorf("expected gemini in error; got:\n%s", snap)
	}

	// Archivo NO debe ser ready: el bloque status sigue presente.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h6-invalid.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "status:") {
		t.Errorf("expected draft to keep status: block (not ready); got:\n%s", body)
	}
	if !strings.Contains(body, "stage: summary") {
		t.Errorf("expected stage: summary; got:\n%s", body)
	}

	// Salir limpio via ctrl+c → SC keep → menu → q.
	mark = p.Mark()
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1 (keep): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after keep\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S3BackToS2 cubre la rama "esc" de H6: en S3, esc vuelve a S2
// sobre el ultimo step en mode=edit. Una segunda esc (en mode=edit) debe
// volver a S3 sin guardar. Como salimos via SC keep desde S3, el archivo
// final debe traer stage=summary (es draft, no ready).
func TestWizard_S3BackToS2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	p := env.StartPTY()
	defer p.Close()

	if !p.WaitForOutput(t, "Create pipeline", 3*time.Second) {
		t.Fatalf("menu never rendered\n%s", p.Snapshot())
	}
	mark := p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 3*time.Second) {
		t.Fatalf("S1 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("Demo H6 Back"); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}

	if err := p.Send("only"); err != nil {
		t.Fatalf("send step name: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := p.Send("\t"); err != nil {
			t.Fatalf("tab #%d: %v", i, err)
		}
	}
	if err := p.Send("hola"); err != nil {
		t.Fatalf("send content: %v", err)
	}
	if err := p.Send("\t"); err != nil {
		t.Fatalf("tab→input: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}

	// esc en S3 → S2 mode=edit del ultimo step.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (edit)", 3*time.Second) {
		t.Fatalf("never returned to S2 mode=edit\n%s", p.Since(mark))
	}

	// Una segunda esc en mode=edit vuelve a S3 sin guardar (los buffers
	// de stepEdit se descartan, pero pipeline.Steps[0] mantiene el
	// contenido original).
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc 2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("never returned to S3\n%s", p.Since(mark))
	}

	// Salir via ctrl+c → SC keep desde S3.
	mark = p.Mark()
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC modal never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1 (keep): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered after keep\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h6-back.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "stage: summary") {
		t.Errorf("expected stage: summary in keep'd draft; got:\n%s", body)
	}
	if !strings.Contains(body, "- name: only") {
		t.Errorf("expected step persisted; got:\n%s", body)
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}
