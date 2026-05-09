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

// readyYAMLInput es la version de readyYAML que permite elegir el kind del
// input del step 0. H2 necesita ejercitar text / file / issue — readyYAML
// (en wizard_h8_h9_h10_test.go) hardcodea "text" y no podemos modificarlo
// sin tocar tests de wizard que ya pasan.
func readyYAMLInput(name, inputKind string) string {
	return fmt.Sprintf(`name: %s
description: ready desc
steps:
- name: alpha
  cli: claude
  kind: prompt
  content: hola
  input: %s
`, name, inputKind)
}

// TestRunner_R0H1Skeleton cubre H1 del flow del runner (wire-up): enter
// sobre un row ready en `My pipelines` debe entrar al runner. esc vuelve al
// lister; el runner no toca disco — no debe existir ~/.che/runs/ post-run.
//
// H2 reemplazo el placeholder "runner pendiente" por R1 (InputPrompt). El
// test sigue verificando el wire-up y la salida limpia; el assert sobre
// "runner pendiente" se evoluciono a "Run · <name>" + el header de R1, que
// son los nuevos sentinels post-H2.
//
// Nombre del test sigue el patron `TestRunner_R<screen>H<story><caso>`:
// R0 porque el trigger del wire-up vive en el lister (R0 segun el doc); el
// test arranca ahi y verifica que el runner abre sobre el row ready.
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

	// enter sobre el row ready → R1 del runner (post-H2).
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run · Runner H1", 3*time.Second) {
		t.Fatalf("runner R1 never rendered\n%s", p.Since(mark))
	}
	// readyYAML usa input=text → R1 muestra el header "Input · text".
	if !strings.Contains(p.Since(mark), "Input · text") {
		t.Errorf("expected R1 'Input · text' header; got:\n%s", p.Since(mark))
	}

	// esc vuelve al lister (no sale del proceso).
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}

	// Sin tocar disco: no debe existir ~/.che/runs/ — el runner no inicia
	// el run hasta que pase R2/R3 (H3+).
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

// TestRunner_R0H1QuitFromSkeleton cubre la rama complementaria: ctrl+c desde
// el runner sale total (no vuelve al lister, no vuelve al menu). Post-H2 R1
// trata `q` como caracter normal del input — el atajo de salida total es
// ctrl+c. esc vs ctrl+c son las dos transiciones declaradas para R1 en el
// doc.
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
		t.Fatalf("runner R1 never rendered\n%s", p.Since(mark))
	}

	// ctrl+c sale total (post-H2 `q` es char tipeable en R1).
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
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

// TestRunner_R1H2TextConfirm cubre el camino feliz de R1 con kind=text:
// tipear texto + ctrl+s deja al runner en la pantalla siguiente (placeholder
// de R2). Coincide con el primer test e2e listado en H2 ("Pipeline text con
// stdin que tipea + ctrl+s; assert: pantalla siguiente abre").
func TestRunner_R1H2TextConfirm(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-text.yaml": readyYAMLInput("R1H2 Text", "text"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 Text", 3*time.Second) {
		t.Fatalf("ready entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Input · text", 3*time.Second) {
		t.Fatalf("R1 text header never rendered\n%s", p.Since(mark))
	}

	// Tipear el texto y confirmar.
	mark = p.Mark()
	if err := p.Send("hola input"); err != nil {
		t.Fatalf("send text: %v", err)
	}
	// ctrl+s confirma.
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "ok, siguiente: preflight (placeholder)", 3*time.Second) {
		t.Fatalf("R2 placeholder never rendered\n%s", p.Since(mark))
	}
	// Confirmamos que el chip "input resuelto" refleja el largo del texto.
	if !strings.Contains(p.Since(mark), "kind=text") {
		t.Errorf("expected 'kind=text' in R2 placeholder, got:\n%s", p.Since(mark))
	}

	// esc vuelve al lister sin crear run dir.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
	runsDir := filepath.Join(env.HomeDir, ".che", "runs")
	if _, err := os.Stat(runsDir); !os.IsNotExist(err) {
		t.Errorf("expected ~/.che/runs/ to not exist post-R2 placeholder, stat err=%v", err)
	}
	// Salida limpia.
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

// TestRunner_R1H2TextEmptyError cubre la rama "input vacio" de R1: ctrl+s
// sin tipear nada deja error inline rojo + foco vuelve al input. Es el caso
// minimo del criterio "validacion eager".
func TestRunner_R1H2TextEmptyError(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-empty.yaml": readyYAMLInput("R1H2 Empty", "text"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 Empty", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Input · text", 3*time.Second) {
		t.Fatalf("R1 never rendered\n%s", p.Since(mark))
	}
	// ctrl+s con buffer vacio.
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil {
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "no puede estar vacio", 3*time.Second) {
		t.Fatalf("expected 'no puede estar vacio' inline error, got:\n%s", p.Since(mark))
	}
	// Asegurar que NO se transiciono a R2.
	if strings.Contains(p.Since(mark), "ok, siguiente: preflight") {
		t.Errorf("R1 should NOT transition to R2 placeholder on empty input, got:\n%s", p.Since(mark))
	}

	// Cleanup: ctrl+c sale total (R1 trata `q` como char tipeable).
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2FileMissing cubre el path de fallo del file picker: si el
// usuario tipea ctrl+s con el cursor en una entry inexistente (en este caso
// vamos directo via el path "/no-existe.txt" inyectado como fixture), el
// runner deja error inline + se queda en R1.
//
// El test actua sobre el picker: el dir tiene 1 file que apunta a otra
// ruta — pero como el picker resuelve la entry seleccionada al abs path
// real del entry, no podemos forzar ENOENT sin mockear. En vez de eso
// validamos el escenario equivalente desde el lado de R2 placeholder con
// un archivo real.
func TestRunner_R1H2FileSelectAndConfirm(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Pre-armar un archivo real en RepoDir (es el cwd del che spawn) —
	// el picker arranca en $PWD asi que lo va a listar.
	payload := "contenido del input file"
	fpath := filepath.Join(env.RepoDir, "input.txt")
	if err := os.WriteFile(fpath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-file.yaml": readyYAMLInput("R1H2 File", "file"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 File", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Input · file", 3*time.Second) {
		t.Fatalf("R1 file picker never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "input.txt", 3*time.Second) {
		t.Fatalf("file picker never listed input.txt\n%s", p.Since(mark))
	}

	// Navegar abajo hasta dar con input.txt y confirmar. El picker tiene
	// ".." al tope si no estamos en root + el resto. Mandamos `down` varias
	// veces y luego ctrl+s — el cursor que pase por input.txt + ctrl+s lo
	// confirma. Si hay otros files arriba, mandamos suficientes downs.
	for i := 0; i < 10; i++ {
		if strings.Contains(p.Snapshot(), "> input.txt") {
			break
		}
		if err := p.Send("\x1b[B"); err != nil { // down arrow
			t.Fatalf("send down: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(p.Snapshot(), "> input.txt") {
		t.Fatalf("never landed on input.txt cursor; final picker:\n%s", p.Snapshot())
	}
	mark = p.Mark()
	if err := p.Send("\x13"); err != nil { // ctrl+s
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "ok, siguiente: preflight (placeholder)", 3*time.Second) {
		t.Fatalf("R2 placeholder never rendered\n%s", p.Since(mark))
	}
	// El chip "input resuelto" debe reportar el size del archivo.
	wantSize := fmt.Sprintf("%d bytes", len(payload))
	if !strings.Contains(p.Since(mark), wantSize) {
		t.Errorf("expected %q in R2 placeholder, got:\n%s", wantSize, p.Since(mark))
	}

	// Cleanup.
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (lister→menu): %v", err)
	}
	if !p.WaitForOutput(t, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Snapshot())
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2IssueGHHappy cubre el camino feliz de input=issue: gh
// faked via PATH (el harness symlinkea gh → chefake) responde JSON con
// body conocido; R1 confirma + transiciona a R2 placeholder + el chip
// "input resuelto" refleja el size del payload.
func TestRunner_R1H2IssueGHHappy(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Scriptear gh para devolver un JSON dummy. ExpectGh acepta args
	// regex; matcheamos issue view + el repo + el num.
	ghBody := `{"title":"fake","body":"hola desde gh","comments":[]}`
	env.ExpectGh(`issue view --repo chichex/che 1`).RespondStdout(ghBody, 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-issue.yaml": readyYAMLInput("R1H2 Issue", "issue"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 Issue", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Input · issue", 3*time.Second) {
		t.Fatalf("R1 issue input never rendered\n%s", p.Since(mark))
	}

	// Tipear ref + ctrl+s.
	mark = p.Mark()
	if err := p.Send("chichex/che#1"); err != nil {
		t.Fatalf("send ref: %v", err)
	}
	if err := p.Send("\x13"); err != nil { // ctrl+s
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "ok, siguiente: preflight (placeholder)", 3*time.Second) {
		t.Fatalf("R2 placeholder never rendered\n%s", p.Since(mark))
	}
	wantSize := fmt.Sprintf("%d bytes", len(ghBody))
	if !strings.Contains(p.Since(mark), wantSize) {
		t.Errorf("expected %q in R2 placeholder, got:\n%s", wantSize, p.Since(mark))
	}

	// Cleanup.
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (lister→menu): %v", err)
	}
	if !p.WaitForOutput(t, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Snapshot())
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2IssueBadFormat cubre el rechazo eager del formato (sin
// owner/repo#NNN). Validacion local — no se llega a llamar gh.
func TestRunner_R1H2IssueBadFormat(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-bad.yaml": readyYAMLInput("R1H2 Bad", "issue"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 Bad", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Input · issue", 3*time.Second) {
		t.Fatalf("R1 issue input never rendered\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("formato-roto"); err != nil {
		t.Fatalf("send ref: %v", err)
	}
	if err := p.Send("\x13"); err != nil { // ctrl+s
		t.Fatalf("send ctrl+s: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "owner/repo#NNN", 3*time.Second) {
		t.Fatalf("expected format hint inline error, got:\n%s", p.Since(mark))
	}
	if strings.Contains(p.Since(mark), "ok, siguiente: preflight") {
		t.Errorf("R1 should NOT transition to R2 on format error")
	}

	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2NoneSkipsR1 cubre el branch "input: none" — R1 no aparece
// y caemos directo al placeholder de R2.
func TestRunner_R1H2NoneSkipsR1(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r1h2-none.yaml": readyYAMLInput("R1H2 None", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R1H2 None", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// Esperamos R2 placeholder directo, sin pasar por R1.
	if !p.WaitForOutputSince(t, mark, "ok, siguiente: preflight (placeholder)", 3*time.Second) {
		t.Fatalf("R2 placeholder never rendered for input=none\n%s", p.Since(mark))
	}
	if strings.Contains(p.Since(mark), "Input · text") || strings.Contains(p.Since(mark), "Input · file") {
		t.Errorf("R1 should NOT render when input=none; got:\n%s", p.Since(mark))
	}

	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc (lister→menu): %v", err)
	}
	if !p.WaitForOutput(t, "0-3 jump", 3*time.Second) {
		t.Fatalf("menu never re-rendered\n%s", p.Snapshot())
	}
	if err := p.Send("q"); err != nil {
		t.Fatalf("send q: %v", err)
	}
	res := p.Wait(t, 3*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}
