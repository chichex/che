package e2e_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// writeFakeEditor escribe un script bash en t.TempDir() y lo deja
// ejecutable. body es el cuerpo del script (sin shebang). Devuelve el path
// absoluto, listo para usar como $EDITOR.
//
// El editor recibe el path del archivo como $1; el body decide si reescribe,
// patchea, o falla.
func writeFakeEditor(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	full := "#!/bin/sh\nset -e\n" + body + "\n"
	if err := os.WriteFile(path, []byte(full), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	return path
}

// h8BuildOneStepPipeline empuja al wizard hasta S3 con un pipeline de 1
// step. Reusa el mismo flujo que el resto de tests S3 — name + un step con
// content + ctrl+s al ultimo foco para abrir el modal y elegir "finalizar".
func h8BuildOneStepPipeline(t *testing.T, p *harness.PTYRun, name string) {
	t.Helper()
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
	if err := p.Send(name); err != nil {
		t.Fatalf("send name: %v", err)
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S1→S2: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1 (create)", 3*time.Second) {
		t.Fatalf("S2 never rendered\n%s", p.Since(mark))
	}
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
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save step: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 3*time.Second) {
		t.Fatalf("S3 never rendered\n%s", p.Since(mark))
	}
}

// TestWizard_S3H8EditorMutates cubre H8 happy path: en S3, `y` lanza el
// editor (un script que reescribe el archivo con un name nuevo); al volver,
// la TUI muestra el nombre actualizado y el archivo persiste con el cambio.
func TestWizard_S3H8EditorMutates(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	editor := writeFakeEditor(t, "edit-mutator.sh", `cat > "$1" <<'EOF'
status:
  stage: summary
  last_saved_at: 2026-01-01T00:00:00Z
name: Edited By Editor
description: post-edit
steps:
  - name: collect
    cli: claude
    kind: prompt
    content: hola
    input: text
EOF`)
	env.SetEnv("EDITOR", editor)

	p := env.StartPTY()
	defer p.Close()

	h8BuildOneStepPipeline(t, p, "Demo H8 Mutate")

	// y → suspende TUI, ejecuta editor, vuelve. Esperamos a que el name
	// nuevo aparezca en el render de S3 (la fila "Pipeline: ...").
	mark := p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Edited By Editor", 5*time.Second) {
		t.Fatalf("S3 did not reflect the edited name\n%s", p.Since(mark))
	}

	// Archivo en disco refleja el cambio + sigue draft (stage=summary).
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "demo-h8-mutate.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "name: Edited By Editor") {
		t.Errorf("expected edited name in YAML; got:\n%s", body)
	}
	if !strings.Contains(body, "stage: summary") {
		t.Errorf("expected stage: summary (still draft) in YAML; got:\n%s", body)
	}

	// ctrl+s → ready-fica el pipeline con el name nuevo; archivo final no
	// debe contener bloque status.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}
	data, err = os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	body = string(data)
	if !strings.Contains(body, "name: Edited By Editor") {
		t.Errorf("expected edited name in ready YAML; got:\n%s", body)
	}
	if strings.Contains(body, "status:") {
		t.Errorf("ready pipeline should not have status block; got:\n%s", body)
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter S4→menu: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestWizard_S3H8EditorInvalidYAML cubre H8 rama de error: el editor escribe
// YAML invalido. Tras volver, S3 muestra error inline y el modelo previo en
// RAM queda intacto (ctrl+s sigue ready-ficando el pipeline original).
func TestWizard_S3H8EditorInvalidYAML(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	editor := writeFakeEditor(t, "edit-broken.sh", `printf 'key: [unterminated\n' > "$1"`)
	env.SetEnv("EDITOR", editor)

	p := env.StartPTY()
	defer p.Close()

	h8BuildOneStepPipeline(t, p, "Demo H8 Invalid")

	mark := p.Mark()
	if err := p.Send("y"); err != nil {
		t.Fatalf("send y: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "no se pudo releer", 5*time.Second) {
		t.Fatalf("expected reload error banner in S3; got:\n%s", p.Since(mark))
	}
	// El modelo previo sigue intacto: el name original "Demo H8 Invalid"
	// se sigue mostrando en S3.
	if !strings.Contains(p.Snapshot(), "Demo H8 Invalid") {
		t.Errorf("expected original name to remain in S3 after editor error; snapshot:\n%s", p.Snapshot())
	}

	// Salir limpio: ctrl+c → SC keep → menu → q.
	mark = p.Mark()
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("ctrl+c: %v", err)
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
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// preArmPipelinesDir crea ~/.che/pipelines/ y escribe los YAML que recibe.
// Cada entrada de files es path-relativo-al-dir → contenido.
func preArmPipelinesDir(t *testing.T, home string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(home, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return dir
}

// readyYAML devuelve el body de un pipeline ready (sin status) con un step.
func readyYAML(name string) string {
	return fmt.Sprintf(`name: %s
description: ready desc
steps:
- name: alpha
  cli: claude
  kind: prompt
  content: hola
  input: text
`, name)
}

// draftYAMLSummary devuelve el body de un draft en stage=summary con un
// step. lastSaved es un RFC3339 — los tests lo eligen para forzar orden.
func draftYAMLSummary(name, lastSaved string) string {
	return fmt.Sprintf(`status:
  stage: summary
  last_saved_at: %s
name: %s
description: draft desc
steps:
- name: alpha
  cli: claude
  kind: prompt
  content: pre-edit
  input: text
`, lastSaved, name)
}

// draftYAMLStep devuelve un draft en stage=step con un step ya pusheado y
// el wizard apuntando a step idx en mode dado.
func draftYAMLStep(name, lastSaved string, stepIdx int, mode string) string {
	return fmt.Sprintf(`status:
  stage: step
  step_idx: %d
  step_mode: %s
  last_saved_at: %s
name: %s
description: draft desc
steps:
- name: alpha
  cli: claude
  kind: prompt
  content: pre-edit
  input: text
`, stepIdx, mode, lastSaved, name)
}

// TestPipelinesList_H9Renders cubre H9: 2 ready + 2 drafts pre-armados se
// renderizan en una sola lista, con chips correctos y orden por fecha desc.
func TestPipelinesList_H9Renders(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"alpha-ready.yaml":   readyYAML("Alpha Ready"),
		"beta-ready.yaml":    readyYAML("Beta Ready"),
		"gamma-draft.yaml":   draftYAMLSummary("Gamma Draft", "2030-01-01T00:00:00Z"),
		"delta-draft.yaml":   draftYAMLStep("Delta Draft", "2030-01-02T00:00:00Z", 0, "edit"),
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
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("My pipelines never rendered\n%s", p.Since(mark))
	}

	snap := p.Since(mark)
	for _, want := range []string{
		"Alpha Ready",
		"Beta Ready",
		"Gamma Draft",
		"Delta Draft",
		"[ready]",
		"[draft]",
	} {
		if !strings.Contains(snap, want) {
			t.Errorf("expected %q in list render; got:\n%s", want, snap)
		}
	}

	// Orden: Delta Draft (2030-01-02) > Gamma Draft (2030-01-01) > readys
	// (mtime de hoy, posterior a 2030 si el reloj de la maquina del CI
	// esta en 2026 — por las dudas no asseteo orden absoluto, pero si
	// "Delta Draft" aparece antes que "Gamma Draft" en la lista).
	deltaIdx := strings.Index(snap, "Delta Draft")
	gammaIdx := strings.Index(snap, "Gamma Draft")
	if deltaIdx < 0 || gammaIdx < 0 {
		t.Fatalf("missing expected names in render:\n%s", snap)
	}
	if deltaIdx > gammaIdx {
		t.Errorf("expected Delta Draft (newer) to render before Gamma Draft; got snap:\n%s", snap)
	}

	// Sub-label "en paso N de M" / "en resumen" / etc.
	if !strings.Contains(snap, "en resumen") && !strings.Contains(snap, "editando step") {
		t.Errorf("expected at least one draft sub-label; got:\n%s", snap)
	}

	// q sale.
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10ResumeDraft cubre H10 resume: enter sobre un draft
// stage=summary entra al wizard arrancando en S3 con el step pre-cargado;
// ctrl+s ready-fica el archivo.
func TestPipelinesList_H10ResumeDraft(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"resume-draft.yaml": draftYAMLSummary("Resume Draft", "2030-01-01T00:00:00Z"),
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
	if !p.WaitForOutputSince(t, mark, "Resume Draft", 3*time.Second) {
		t.Fatalf("My pipelines never showed entry\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 5*time.Second) {
		t.Fatalf("wizard did not resume in S3\n%s", p.Since(mark))
	}
	// El step pre-cargado aparece en S3.
	if !strings.Contains(p.Since(mark), "alpha") {
		t.Errorf("expected pre-loaded step in S3 render; got:\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s save pipeline: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}

	// Archivo final sin status.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "resume-draft.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "name: Resume Draft") {
		t.Errorf("expected name preserved; got:\n%s", body)
	}
	if strings.Contains(body, "status:") {
		t.Errorf("ready pipeline should not have status block; got:\n%s", body)
	}

	// S4 enter → vuelve al lister, no al menu, porque arrancamos desde
	// el lister.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter S4: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after save\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10EditReady cubre H10 edit ready: `e` sobre un pipeline
// ready re-introduce status.stage=summary, abre el wizard en S3, y ctrl+s
// vuelve a ready-ficarlo (sin status).
func TestPipelinesList_H10EditReady(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"edit-ready.yaml": readyYAML("Edit Ready"),
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
	if !p.WaitForOutputSince(t, mark, "Edit Ready", 3*time.Second) {
		t.Fatalf("ready entry not rendered\n%s", p.Since(mark))
	}

	// e sobre un ready → S3 mode=edit.
	mark = p.Mark()
	if err := p.Send("e"); err != nil {
		t.Fatalf("send e: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 5*time.Second) {
		t.Fatalf("S3 never opened from edit ready\n%s", p.Since(mark))
	}

	// ctrl+s en S3 → ready-fica de nuevo.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("ctrl+s S3: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Pipeline guardado", 3*time.Second) {
		t.Fatalf("S4 never rendered\n%s", p.Since(mark))
	}

	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "edit-ready.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if strings.Contains(string(data), "status:") {
		t.Errorf("ready pipeline should not have status block; got:\n%s", data)
	}

	// Salir.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter S4: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10EditReadyNoChangesStaysReady cubre el caso "abrir
// un ready, no tocar nada, salir": el archivo en disco debe seguir ready
// (sin status) — entrar a edit-ready no debe convertirlo en draft por el
// solo hecho de abrirlo.
func TestPipelinesList_H10EditReadyNoChangesStaysReady(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"untouched.yaml": readyYAML("Untouched Ready"),
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
	if !p.WaitForOutputSince(t, mark, "Untouched Ready", 3*time.Second) {
		t.Fatalf("ready entry not rendered\n%s", p.Since(mark))
	}

	// e → S3 mode=edit, sin tocar nada.
	mark = p.Mark()
	if err := p.Send("e"); err != nil {
		t.Fatalf("send e: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 5*time.Second) {
		t.Fatalf("S3 never opened\n%s", p.Since(mark))
	}

	// esc → SC → 1 (keep).
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1 (keep): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}

	// El archivo sigue ready: sin bloque status, contenido intacto.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "untouched.yaml")
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "status:") {
		t.Errorf("ready pipeline turned draft after no-op edit; got:\n%s", body)
	}
	if !strings.Contains(body, "name: Untouched Ready") {
		t.Errorf("expected name preserved; got:\n%s", body)
	}
	// El chip del lister debe seguir mostrando [ready].
	if !strings.Contains(p.Since(mark), "[ready]") {
		t.Errorf("expected [ready] chip after no-op edit; got:\n%s", p.Since(mark))
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10EditReadyDiscardRestores cubre el caso "abro un
// ready con e, no toco nada (o toco algo), elijo discard": el archivo en
// disco vuelve a su estado ready original — discard en edit-ready significa
// "tirar mis cambios", no "borrar el pipeline".
func TestPipelinesList_H10EditReadyDiscardRestores(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	original := readyYAML("Survives Discard")
	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"survives.yaml": original,
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
	if !p.WaitForOutputSince(t, mark, "Survives Discard", 3*time.Second) {
		t.Fatalf("ready entry not rendered\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("e"); err != nil {
		t.Fatalf("send e: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "paso 3/3", 5*time.Second) {
		t.Fatalf("S3 never opened\n%s", p.Since(mark))
	}

	// esc → SC → la label de "discard" debe decir "discard changes" en
	// este flow. "2" elige discard.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "discard changes", 3*time.Second) {
		t.Fatalf("expected 'discard changes' label in SC under edit-ready; got:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 (discard): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after discard\n%s", p.Since(mark))
	}

	// Archivo debe existir y ser identico al original.
	expected := filepath.Join(env.HomeDir, ".che", "pipelines", "survives.yaml")
	got, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read after discard: %v (file should still exist)", err)
	}
	if string(got) != original {
		t.Errorf("expected file restored to original ready content;\nwant:\n%s\ngot:\n%s", original, got)
	}

	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10Delete cubre H10 delete: d → modal → "1" confirma →
// archivo desaparece del disco y de la lista.
func TestPipelinesList_H10Delete(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"to-del.yaml":   readyYAML("To Delete"),
		"to-keep.yaml":  readyYAML("To Keep"),
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
	if !p.WaitForOutputSince(t, mark, "To Delete", 3*time.Second) {
		t.Fatalf("To Delete not rendered\n%s", p.Since(mark))
	}

	// La lista esta ordenada por mtime; ambos readys tienen mtime ahora,
	// asi que el orden depende del sort stable. Voy a localizar "To Delete"
	// con flechas: el cursor arranca en 0.
	snap := p.Since(mark)
	delIdx := strings.Index(snap, "To Delete")
	keepIdx := strings.Index(snap, "To Keep")
	if delIdx < 0 || keepIdx < 0 {
		t.Fatalf("missing entries; snap:\n%s", snap)
	}
	if delIdx > keepIdx {
		// "To Delete" esta segundo → bajar el cursor.
		if err := p.Send("j"); err != nil {
			t.Fatalf("send j: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// d → modal.
	mark = p.Mark()
	if err := p.Send("d"); err != nil {
		t.Fatalf("send d: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Borrar pipeline", 3*time.Second) {
		t.Fatalf("delete modal never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("1"); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "borrado", 3*time.Second) {
		t.Fatalf("toast 'borrado' not seen\n%s", p.Since(mark))
	}

	// Archivos en disco.
	if _, err := os.Stat(filepath.Join(env.HomeDir, ".che", "pipelines", "to-del.yaml")); !os.IsNotExist(err) {
		t.Errorf("expected to-del.yaml to be deleted, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.HomeDir, ".che", "pipelines", "to-keep.yaml")); err != nil {
		t.Errorf("expected to-keep.yaml to remain: %v", err)
	}

	// Salir limpio.
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestPipelinesList_H10CorruptStatusFallback cubre H10 status corrupto:
// step_idx fuera de rango → wizard arranca en S1 con warning visible
// "fuera de rango". El usuario puede salir sin perder el archivo.
func TestPipelinesList_H10CorruptStatusFallback(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	corrupt := `status:
  stage: step
  step_idx: 999
  step_mode: edit
  last_saved_at: 2030-01-01T00:00:00Z
name: Corrupt Draft
description: bad status
steps:
- name: alpha
  cli: claude
  kind: prompt
  content: hola
  input: text
`
	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"corrupt-draft.yaml": corrupt,
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
	if !p.WaitForOutputSince(t, mark, "Corrupt Draft", 3*time.Second) {
		t.Fatalf("corrupt draft entry not rendered\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// Fallback: arranca en S1 con warning.
	if !p.WaitForOutputSince(t, mark, "paso 1/3", 5*time.Second) {
		t.Fatalf("S1 fallback never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "fuera de rango", 3*time.Second) {
		t.Fatalf("expected step_idx warning in S1; got:\n%s", p.Since(mark))
	}

	// Salir limpio via ctrl+c → SC → "2" (discard). discard borra el
	// archivo, al volver al lister la lista queda vacia.
	mark = p.Mark()
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("ctrl+c: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Salir del wizard", 3*time.Second) {
		t.Fatalf("SC never opened\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("2"); err != nil {
		t.Fatalf("send 2 (discard): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after discard\n%s", p.Since(mark))
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
}
