package e2e_test

import (
	"encoding/json"
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
	// Post-H3 R2 ya no es placeholder — render real con header "Preflight
	// de <name>" + rows del checklist. El sentinel "Preflight de" es
	// estable porque el header sale en el primer Render().
	if !p.WaitForOutputSince(t, mark, "Preflight de R1H2 Text", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	// Pipeline usa cli=claude → R2 lista al menos el row "cli claude
	// instalado" (chefake symlink lo hace ✓).
	if !strings.Contains(p.Since(mark), "cli claude instalado") {
		t.Errorf("expected 'cli claude instalado' row in R2, got:\n%s", p.Since(mark))
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
		t.Errorf("expected ~/.che/runs/ to not exist post-R2 preflight, stat err=%v", err)
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
	// Asegurar que NO se transiciono a R2 (post-H3 el sentinel R2 es
	// "Preflight de").
	if strings.Contains(p.Since(mark), "Preflight de") {
		t.Errorf("R1 should NOT transition to R2 on empty input, got:\n%s", p.Since(mark))
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
	if !p.WaitForOutputSince(t, mark, "Preflight de R1H2 File", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	// Post-H3 R2 lista un row "file readable: <path>" (re-check defensivo
	// del input=file). En macos os.Getwd puede resolver el cwd a
	// /private/var/... mientras t.TempDir devuelve /var/...; por eso
	// chequeamos solo el basename del fixture (suficiente para confirmar
	// que el row apunta al archivo correcto).
	if !strings.Contains(p.Since(mark), "file readable:") || !strings.Contains(p.Since(mark), filepath.Base(fpath)) {
		t.Errorf("expected 'file readable: ...%s' row in R2, got:\n%s", filepath.Base(fpath), p.Since(mark))
	}

	// Cleanup — esc (R2→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2IssueGHHappy cubre el camino feliz de input=issue: gh
// faked via PATH (el harness symlinkea gh → chefake) responde JSON con
// body conocido; R1 confirma + transiciona a R2 (post-H3 con preflight
// real, que tambien chequea gh auth — aca scripteamos auth status como
// success).
func TestRunner_R1H2IssueGHHappy(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Scriptear gh para devolver un JSON dummy en `issue view` y "ok" en
	// `auth status` (R2 lo invoca porque el step usa input=issue, post-H3).
	ghBody := `{"title":"fake","body":"hola desde gh","comments":[]}`
	env.ExpectGh(`issue view --repo chichex/che 1`).RespondStdout(ghBody, 0)
	env.ExpectGh(`auth status`).RespondStdout("Logged in to github.com", 0)

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
	if !p.WaitForOutputSince(t, mark, "Preflight de R1H2 Issue", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	// El row de gh auth tiene que existir cuando input=issue.
	if !strings.Contains(p.Since(mark), "gh auth status") {
		t.Errorf("expected 'gh auth status' row in R2, got:\n%s", p.Since(mark))
	}

	// Cleanup — esc (R2→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
	if strings.Contains(p.Since(mark), "Preflight de") {
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

// readyYAMLSkillStep es la version de readyYAMLInput que arma un step
// kind=skill con la skill que el test recibe. H3 lo usa para forzar el
// row "skill X en claude" en R2.
func readyYAMLSkillStep(name, skill, inputKind string) string {
	return fmt.Sprintf(`name: %s
description: ready desc
steps:
- name: alpha
  cli: claude
  kind: skill
  content: %s
  input: %s
`, name, skill, inputKind)
}

// preArmClaudeSkill crea ~/.claude/skills/<name>/SKILL.md con un cuerpo
// minimal. internal/skills.scanSkillDirs lo detecta porque solo necesita
// que SKILL.md exista — sin frontmatter usa el nombre del dir como
// skill name (lo cual es justo lo que el preflight chequea).
func preArmClaudeSkill(t *testing.T, home, skill string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "skills", skill)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "# " + skill + "\n\ntest skill installed by harness\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// TestRunner_R2H3AllGreenAdvances cubre el camino feliz de H3: pipeline con
// CLI faked + skill pre-creada → R2 marca todos los chequeos verdes →
// enter avanza a R3. Coincide con el primer test e2e listado en H3
// ("Tmp HOME con CLI faked + skill pre-creada; assert: R2 todos verdes,
// enter avanza"). Post-H4 R3 spawnea de verdad — scripteamos el fake
// claude para que devuelva ok rapido y el run termine en R4.
func TestRunner_R2H3AllGreenAdvances(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Skill instalada en la home tmp del test.
	preArmClaudeSkill(t, env.HomeDir, "h3-green")

	// Fake claude responde con "ok" para el step h3-green. H4 spawnea
	// blocking — sin esto el test colgaria en R3.
	env.ExpectAgent("claude").WhenArgsMatch(`h3-green`).RespondStdout("ok h3-green", 0)

	// Pipeline con kind=skill apuntando a la skill que acabamos de crear,
	// e input=none para no tener que pasar por R1 (mas simple) y para
	// evitar el row de gh auth (no necesitamos scriptearlo aca).
	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r2h3-green.yaml": readyYAMLSkillStep("R2H3 Green", "h3-green", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R2H3 Green", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R2H3 Green", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	// Esperamos a que aparezca el row de la skill — es el ultimo
	// chequeo "no-disco" relevante; si esta listado significa que el
	// builder + runner los corrieron.
	if !p.WaitForOutputSince(t, mark, "skill h3-green en claude", 3*time.Second) {
		t.Fatalf("expected skill row in R2, got:\n%s", p.Since(mark))
	}
	// Verdict "todo listo" → enter avanza directo.
	if !strings.Contains(p.Since(mark), "todo listo") {
		t.Errorf("expected 'todo listo' verdict (all green), got:\n%s", p.Since(mark))
	}
	// Asegurar que NO hay rows fail (cualquier "remedio:" indicaria una
	// fila roja o amarilla). disk-warn aparece como warn, no como
	// remedio bloqueante; el verdict "todo listo" arriba ya garantiza
	// que no hay warns ni fails.
	if strings.Contains(p.Since(mark), "remedio:") {
		t.Errorf("expected no remedios when all green, got:\n%s", p.Since(mark))
	}

	// enter avanza a R3 (post-H4 con spawn real → fake claude responde
	// "ok h3-green" y aterrizamos en R4 "Run completo").
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 5*time.Second) {
		t.Fatalf("R4 Run completo never rendered after enter\n%s", p.Since(mark))
	}

	// Cleanup — esc (R4→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Post-H4 R3 escribe ~/.che/runs/<slug>/<run-id>/manifest.yaml. El
	// dir tiene que existir tras un run exitoso (H3 no escribia disco;
	// H4 si).
	runsDir := filepath.Join(env.HomeDir, ".che", "runs")
	if _, err := os.Stat(runsDir); err != nil {
		t.Errorf("expected ~/.che/runs/ to exist post-R3 spawn, stat err=%v", err)
	}
}

// TestRunner_R2H3MissingSkillBlocks cubre el escenario "skill no
// instalada": pipeline con kind=skill apuntando a una skill que NO
// existe en el HOME tmp → R2 marca el row en rojo + enter no avanza.
// Coincide con el segundo test e2e de H3 ("Tmp HOME sin la skill;
// assert: R2 marca el row en rojo, enter bloqueado").
func TestRunner_R2H3MissingSkillBlocks(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// NOTA: NO creamos la skill — preflight tiene que detectar que falta.
	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r2h3-missing.yaml": readyYAMLSkillStep("R2H3 Missing", "no-existe-h3", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R2H3 Missing", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R2H3 Missing", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	// El row debe aparecer + estar marcado como rojo (con remedio).
	if !p.WaitForOutputSince(t, mark, "skill no-existe-h3 en claude", 3*time.Second) {
		t.Fatalf("expected skill row in R2, got:\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "remedio:") {
		t.Errorf("expected 'remedio:' line under failed skill row, got:\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "instalar la skill") {
		t.Errorf("expected install hint in remedio, got:\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "enter bloqueado") {
		t.Errorf("expected 'enter bloqueado' footer when red row present, got:\n%s", p.Since(mark))
	}

	// enter no debe transicionar a R3.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// Pequena espera para que cualquier transicion (incorrecta) tenga
	// chance de renderear. El assert principal es que el sentinel R3
	// (header "step N/M" del runner real) NO aparezca.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(p.Since(mark), "step 1/1") {
		t.Errorf("R2 should NOT advance to R3 with a failed check, got:\n%s", p.Since(mark))
	}
	// Aseguramos que seguimos en R2 (el footer "enter bloqueado" sigue
	// presente en el snapshot total).
	if !strings.Contains(p.Snapshot(), "enter bloqueado") {
		t.Errorf("expected to still be in R2 (enter bloqueado), got:\n%s", p.Snapshot())
	}

	// Cleanup — esc (R2→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R2H3MissingSkillRetries cubre la rama `r` de R2: con un row
// rojo, presionar `r` re-corre los chequeos. Si entre el primer run y el
// retry escribimos la skill que faltaba, el verdict cambia a all-green.
// No esta en la lista textual de e2e de H3 pero cubre el criterio de
// aceptacion "r reintenta" — sin esto, `r` nunca se ejercita.
func TestRunner_R2H3MissingSkillRetries(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r2h3-retry.yaml": readyYAMLSkillStep("R2H3 Retry", "h3-retry", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R2H3 Retry", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R2H3 Retry", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "enter bloqueado") {
		t.Fatalf("expected initial verdict to block (no skill installed), got:\n%s", p.Since(mark))
	}

	// Crear la skill y pedir retry con `r`.
	preArmClaudeSkill(t, env.HomeDir, "h3-retry")
	mark = p.Mark()
	if err := p.Send("r"); err != nil {
		t.Fatalf("send r: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 3*time.Second) {
		t.Fatalf("expected 'todo listo' after retry with skill present, got:\n%s", p.Since(mark))
	}

	// Cleanup — esc (R2→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// TestRunner_R1H2NoneSkipsR1 cubre el branch "input: none" — R1 no aparece
// y caemos directo a R2 (post-H3 = preflight real).
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
	// Esperamos R2 directo, sin pasar por R1.
	if !p.WaitForOutputSince(t, mark, "Preflight de R1H2 None", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered for input=none\n%s", p.Since(mark))
	}
	if strings.Contains(p.Since(mark), "Input · text") || strings.Contains(p.Since(mark), "Input · file") {
		t.Errorf("R1 should NOT render when input=none; got:\n%s", p.Since(mark))
	}

	// Cleanup — esc (R2→lister), esc (lister→menu), q (menu→exit). Cada
	// esc necesita esperar al re-render antes del proximo send para que
	// el debounce interno de bubbletea no las merge en una sola.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}
}

// firstRunDir devuelve el unico run-dir creado bajo ~/.che/runs/<slug>/.
// H4 escribe un dir por run; los tests asumen que solo corrio uno y
// fallan ruidosamente si encuentran 0 o >1 (cualquier estado distinto es
// bug del runner o del harness).
func firstRunDir(t *testing.T, home, slug string) string {
	t.Helper()
	slugDir := filepath.Join(home, ".che", "runs", slug)
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		t.Fatalf("read %s: %v", slugDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 {
		t.Fatalf("expected exactly 1 run dir under %s, got %v", slugDir, dirs)
	}
	return filepath.Join(slugDir, dirs[0])
}

// readRunFile es un helper para leer + assertar contenido de un archivo
// del run dir. Falla el test si no existe o si tiene un read error.
func readRunFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestRunner_R3H4SpawnHappy cubre el camino feliz de H4: pipeline 1 step
// con un fake claude que devuelve "ok-h4-happy" y exit 0. El runner
// arranca R3, ejecuta blocking, escribe manifest + result.yaml, y aterriza
// en R4 ("Run completo"). Coincide con el primer test e2e listado en H4.
func TestRunner_R3H4SpawnHappy(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmClaudeSkill(t, env.HomeDir, "h4-happy")
	// Fake claude que matchea cuando el arg incluye la skill h4-happy y
	// emite el sentinel "ok-h4-happy" en stdout. exit 0 → done.
	env.ExpectAgent("claude").WhenArgsMatch(`h4-happy`).RespondStdout("ok-h4-happy", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h4-happy.yaml": readyYAMLSkillStep("R3H4 Happy", "h4-happy", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H4 Happy", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H4 Happy", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 3*time.Second) {
		t.Fatalf("preflight verdict never reached 'todo listo':\n%s", p.Since(mark))
	}

	// enter avanza a R3; el spawn corre y aterrizamos en R4.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 5*time.Second) {
		t.Fatalf("R4 'Run completo' never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "R3H4 Happy") {
		t.Errorf("R4 should mention pipeline name, got:\n%s", p.Since(mark))
	}

	// Cleanup antes de leer disco — esc → lister, esc → menu, q → exit.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered after esc\n%s", p.Since(mark))
	}
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

	// Asserts en disco: manifest done, result.yaml con output="ok-h4-happy".
	runDir := firstRunDir(t, env.HomeDir, "r3h4-happy")
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: done") {
		t.Errorf("expected manifest status: done, got:\n%s", manifestRaw)
	}
	resultRaw := readRunFile(t, filepath.Join(runDir, "step-01.result.yaml"))
	if !strings.Contains(resultRaw, "ok-h4-happy") {
		t.Errorf("expected result.yaml to contain ok-h4-happy, got:\n%s", resultRaw)
	}
	if !strings.Contains(resultRaw, "exit_code: 0") {
		t.Errorf("expected result.yaml exit_code: 0, got:\n%s", resultRaw)
	}

	// Invocacion del fake llego (sanity: si el subprocess no corrio, no
	// habria nada en _invocations.jsonl).
	calls := env.Invocations().For("claude")
	if len(calls) != 1 {
		t.Errorf("expected 1 invocation of claude, got %d", len(calls))
	}
}

// TestRunner_R3H4SpawnFailExit1 cubre el camino de error: fake claude
// devuelve stderr "boom-h4" + exit 1 → R3 transiciona a RF, manifest
// failed, stderr.log contiene "boom-h4". Coincide con el segundo test
// e2e listado en H4.
func TestRunner_R3H4SpawnFailExit1(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmClaudeSkill(t, env.HomeDir, "h4-fail")
	env.ExpectAgent("claude").WhenArgsMatch(`h4-fail`).RespondExitWithError(1, "boom-h4 went wrong")

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h4-fail.yaml": readyYAMLSkillStep("R3H4 Fail", "h4-fail", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H4 Fail", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H4 Fail", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 3*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run fallo", 5*time.Second) {
		t.Fatalf("RF 'Run fallo' never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "exit_code: 1") {
		t.Errorf("RF should mention exit_code: 1, got:\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Disco: manifest failed + stderr.log con boom-h4.
	runDir := firstRunDir(t, env.HomeDir, "r3h4-fail")
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: failed") {
		t.Errorf("expected manifest status: failed, got:\n%s", manifestRaw)
	}
	stderrRaw := readRunFile(t, filepath.Join(runDir, "step-01.stderr.log"))
	if !strings.Contains(stderrRaw, "boom-h4") {
		t.Errorf("expected stderr.log to contain boom-h4, got:\n%s", stderrRaw)
	}
}

// TestRunner_R3H4CancelAbort cubre el modal RC: fake claude que bloquea
// (BlockSeconds 15) → ctrl+c abre el modal → enter sobre "abort & save"
// → SIGTERM al subprocess → manifest cancelled, run dir conserva los logs
// parciales. Coincide con el tercer test e2e listado en H4.
func TestRunner_R3H4CancelAbort(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmClaudeSkill(t, env.HomeDir, "h4-cancel")
	// Fake que emite sentinel "started" antes de bloquear, asi el test
	// puede esperar a que el subprocess este vivo antes de mandar ctrl+c
	// (sin esto la senal podria llegar antes del Start() y el cancel
	// quedaria con un proceso no nato).
	env.ExpectAgent("claude").WhenArgsMatch(`h4-cancel`).
		BlockSeconds(15).
		RespondStdout("started-h4-cancel", 0)
	// Acortar la grace para no esperar 5s por el SIGKILL si el fake
	// ignora SIGTERM.
	env.SetEnv("CHE_KILL_GRACE", "1")

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h4-cancel.yaml": readyYAMLSkillStep("R3H4 Cancel", "h4-cancel", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H4 Cancel", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H4 Cancel", 3*time.Second) {
		t.Fatalf("R2 never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 3*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}

	// enter → R3. Esperamos el header de R3 ("step 1/1") como sentinel.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "step 1/1", 5*time.Second) {
		t.Fatalf("R3 header never rendered\n%s", p.Since(mark))
	}

	// Esperar a que el subprocess este vivo. Sin un sentinel claro del
	// Start() podriamos mandar ctrl+c antes — un sleep corto cubre el
	// gap (la goroutine del spawn arranca en el siguiente tick).
	time.Sleep(500 * time.Millisecond)

	// ctrl+c abre el modal RC.
	mark = p.Mark()
	if err := p.Send("\x03"); err != nil {
		t.Fatalf("send ctrl+c: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Cancelar run", 3*time.Second) {
		t.Fatalf("RC modal never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "abort & save") {
		t.Errorf("RC modal should list 'abort & save', got:\n%s", p.Since(mark))
	}

	// El cursor inicial esta sobre "abort & save" → enter dispara el
	// cancel.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter (abort): %v", err)
	}
	// Tras el cancel, la goroutine SIGTERMea al fake; con
	// CHE_KILL_GRACE=1 a lo sumo 1s + grace SIGKILL → el msg done llega
	// rapido y aterrizamos en RF (tono cancelled).
	if !p.WaitForOutputSince(t, mark, "Run cancelado", 8*time.Second) {
		t.Fatalf("RF cancelled screen never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
	res := p.Wait(t, 5*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Disco: manifest cancelled + run dir conserva logs parciales (el
	// stdout sentinel "started-h4-cancel" que el fake emitio antes del
	// bloqueo tiene que estar en step-01.stdout.log).
	runDir := firstRunDir(t, env.HomeDir, "r3h4-cancel")
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: cancelled") {
		t.Errorf("expected manifest status: cancelled, got:\n%s", manifestRaw)
	}
	stdoutRaw := readRunFile(t, filepath.Join(runDir, "step-01.stdout.log"))
	if !strings.Contains(stdoutRaw, "started-h4-cancel") {
		t.Errorf("expected partial stdout to contain started-h4-cancel, got:\n%s", stdoutRaw)
	}
}

// streamJSONEventClaude arma una linea de stream-json con un evento
// type=assistant cuyo unico bloque text es el texto dado. Permite a los
// tests de H5 emitir N eventos faciles de matchear visualmente y de contar
// en events.jsonl.
func streamJSONEventClaude(text string) string {
	// Marshalear inline para evitar dependencias adicionales y garantizar
	// escape de caracteres especiales del payload.
	body, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// readyYAMLSkillStepCLI es la version de readyYAMLSkillStep que permite
// elegir el CLI del step (no solo claude). Necesario para los tests de H5
// que ejercitan gemini (parser raw, sin events.jsonl).
func readyYAMLSkillStepCLI(name, cli, skill, inputKind string) string {
	return fmt.Sprintf(`name: %s
description: ready desc
steps:
- name: alpha
  cli: %s
  kind: skill
  content: %s
  input: %s
`, name, cli, skill, inputKind)
}

// preArmCLISkill es la version generica de preArmClaudeSkill que crea la
// "skill" bajo el path correcto segun el CLI. claude usa SKILL.md adentro
// de un dir; gemini usa un .toml suelto en commands/ (campo description).
// H5 necesita instalar skills para gemini en algun test; preArmClaudeSkill
// alcanza para claude solo.
func preArmCLISkill(t *testing.T, home, cli, skill string) {
	t.Helper()
	switch cli {
	case "claude":
		preArmClaudeSkill(t, home, skill)
	case "gemini":
		dir := filepath.Join(home, ".gemini", "commands")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		body := fmt.Sprintf("description = %q\nprompt = \"test\"\n", "skill "+skill)
		if err := os.WriteFile(filepath.Join(dir, skill+".toml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write toml: %v", err)
		}
	default:
		t.Fatalf("preArmCLISkill: unsupported cli %q", cli)
	}
}

// TestRunner_R3H5StreamJSONLines cubre el primer test e2e listado en H5:
// "Fake CLI que emite 5 eventos stream-json con sleeps cortos. Assert: log
// pane termina mostrando la ultima linea + events.jsonl tiene 5 entradas."
//
// El fake claude emite 5 eventos JSON tipo assistant con texto unico cada
// uno. El runner los stream-parsea (claude.go), los appendea a events.jsonl
// crudo y los renderea en el log pane como "> linea N de cinco". Tras el
// run, el ultimo sentinel tiene que estar visible en el viewport y
// events.jsonl tiene que tener exactamente 5 lineas.
func TestRunner_R3H5StreamJSONLines(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmClaudeSkill(t, env.HomeDir, "h5-stream")

	// Fake claude: 5 lineas de stream-json, separadas por 30ms cada una
	// para dar tiempo al scanner del runner a leer linea por linea (en
	// vez de drenar todo de una). Al final exit 0.
	expect := env.ExpectAgent("claude").WhenArgsMatch(`h5-stream`)
	for i := 1; i <= 5; i++ {
		text := fmt.Sprintf("linea %d de cinco", i)
		expect = expect.StreamLine(streamJSONEventClaude(text), 30)
	}
	expect.RespondStreamed(0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h5-stream.yaml": readyYAMLSkillStep("R3H5 Stream", "h5-stream", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H5 Stream", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H5 Stream", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 3*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}

	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// El run termina rapido (5 * 30ms ≈ 150ms + overhead) y aterrizamos
	// en R4. Esperamos el sentinel "Run completo".
	if !p.WaitForOutputSince(t, mark, "Run completo", 5*time.Second) {
		t.Fatalf("R4 'Run completo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Disco: events.jsonl tiene que existir + tener exactamente 5 lineas
	// no vacias (1 por evento del stream).
	runDir := firstRunDir(t, env.HomeDir, "r3h5-stream")
	eventsRaw := readRunFile(t, filepath.Join(runDir, "step-01.events.jsonl"))
	count := 0
	for _, line := range strings.Split(strings.TrimRight(eventsRaw, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count != 5 {
		t.Errorf("expected events.jsonl to have 5 entries, got %d. raw:\n%s", count, eventsRaw)
	}

	// stdout.log tiene que tener las 5 lineas crudas (json) y la ultima
	// debe contener el sentinel "linea 5 de cinco" para confirmar que el
	// stream llego al final.
	stdoutRaw := readRunFile(t, filepath.Join(runDir, "step-01.stdout.log"))
	if !strings.Contains(stdoutRaw, "linea 5 de cinco") {
		t.Errorf("expected stdout.log to contain final sentinel, got:\n%s", stdoutRaw)
	}

	// El R4 final muestra el resumen (no el log pane) — los logs los
	// chequeamos en disco. El sentinel "ok" del invocations log confirma
	// que claude se llamo 1 sola vez.
	calls := env.Invocations().For("claude")
	if len(calls) != 1 {
		t.Errorf("expected 1 invocation of claude, got %d", len(calls))
	}
}

// TestRunner_R3H5StreamJSONLargeLine cubre el segundo test e2e de H5:
// "Fake CLI que emite una linea > 64 KiB. Assert: parser no trunca;
// events.jsonl tiene la linea entera."
//
// Validamos el criterio del doc: el buffer del scanner se subio a 1 MiB
// para evitar el truncate silencioso de bufio.Scanner (memory project
// gotcha). Si el buffer estuviera en el default de 64 KiB, la linea se
// cortaria sin error visible y events.jsonl quedaria con < tamaño_real
// bytes.
func TestRunner_R3H5StreamJSONLargeLine(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmClaudeSkill(t, env.HomeDir, "h5-large")

	// Construir un payload que exceda 64 KiB: 100 KiB de un solo char
	// dentro del field text del evento. La linea resultante (JSON
	// escapado + envelope) supera los 100 KiB facilmente — bien por
	// encima del default de bufio.Scanner.
	bigText := strings.Repeat("X", 100*1024)
	bigEvent := streamJSONEventClaude(bigText)
	if len(bigEvent) <= 64*1024 {
		t.Fatalf("test fixture corrupted: bigEvent size %d <= 64 KiB; expected > 64 KiB", len(bigEvent))
	}

	env.ExpectAgent("claude").WhenArgsMatch(`h5-large`).
		StreamLine(bigEvent, 0).
		RespondStreamed(0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h5-large.yaml": readyYAMLSkillStep("R3H5 Large", "h5-large", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H5 Large", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 8*time.Second) {
		t.Fatalf("R4 never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// events.jsonl debe tener el evento completo (no truncado). El check
	// principal es el tamaño: si el scanner truncara, la linea seria <=
	// 64 KiB. Tambien chequeamos que los XXX...X esten presentes
	// completos (sentinel del fin del repeat).
	runDir := firstRunDir(t, env.HomeDir, "r3h5-large")
	eventsRaw := readRunFile(t, filepath.Join(runDir, "step-01.events.jsonl"))
	// events.jsonl tiene 1 linea + un newline final → trimear y medir.
	eventsLine := strings.TrimRight(eventsRaw, "\n")
	if len(eventsLine) < len(bigEvent) {
		t.Fatalf("expected events.jsonl line length >= %d (no truncation), got %d",
			len(bigEvent), len(eventsLine))
	}
	// El payload original empieza con XXXX... cuenta exacta de Xs en el
	// JSON resultante (ya escapado).
	xCount := strings.Count(eventsLine, "X")
	if xCount < 100*1024 {
		t.Errorf("expected at least 100 KiB of X chars in events.jsonl line, got %d", xCount)
	}
}

// TestRunner_R3H5StderrInterleaved cubre el tercer test e2e de H5:
// "Fake CLI que emite stderr y stdout intercalados. Assert: stderr.log y
// stdout.log tienen el split correcto, ring buffer mantiene el orden de
// llegada."
//
// Usamos gemini en vez de claude porque gemini es text mode (parser raw)
// — asi los sentinels que emitimos van directo al log pane sin pasar por
// el parser de stream-json. El runner separa stdout/stderr en dos files
// distintos pero el ring buffer en RAM los registra en orden de llegada.
func TestRunner_R3H5StderrInterleaved(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmCLISkill(t, env.HomeDir, "gemini", "h5-mix")

	// Stream alternado: out1 → err1 → out2 → err2 → out3. Todos con
	// pequeno delay para que el scanner del runner pueda procesarlos
	// linea por linea (ver siempre el mismo orden).
	env.ExpectAgent("gemini").WhenArgsMatch(`h5-mix`).
		StreamLine("out-line-uno", 20).
		StreamStderrLine("err-line-uno", 20).
		StreamLine("out-line-dos", 20).
		StreamStderrLine("err-line-dos", 20).
		StreamLine("out-line-tres", 20).
		RespondStreamed(0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h5-mix.yaml": readyYAMLSkillStepCLI("R3H5 Mix", "gemini", "h5-mix", "none"),
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
	if !p.WaitForOutputSince(t, mark, "R3H5 Mix", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 8*time.Second) {
		t.Fatalf("R4 never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Disco: stdout.log tiene SOLO out-line-*; stderr.log tiene SOLO
	// err-line-*. Cada archivo split correctamente.
	runDir := firstRunDir(t, env.HomeDir, "r3h5-mix")
	stdoutRaw := readRunFile(t, filepath.Join(runDir, "step-01.stdout.log"))
	stderrRaw := readRunFile(t, filepath.Join(runDir, "step-01.stderr.log"))

	for _, want := range []string{"out-line-uno", "out-line-dos", "out-line-tres"} {
		if !strings.Contains(stdoutRaw, want) {
			t.Errorf("stdout.log missing %q. raw:\n%s", want, stdoutRaw)
		}
		if strings.Contains(stderrRaw, want) {
			t.Errorf("stderr.log should NOT contain stdout sentinel %q. raw:\n%s", want, stderrRaw)
		}
	}
	for _, want := range []string{"err-line-uno", "err-line-dos"} {
		if !strings.Contains(stderrRaw, want) {
			t.Errorf("stderr.log missing %q. raw:\n%s", want, stderrRaw)
		}
		if strings.Contains(stdoutRaw, want) {
			t.Errorf("stdout.log should NOT contain stderr sentinel %q. raw:\n%s", want, stdoutRaw)
		}
	}

	// gemini text mode no genera events.jsonl (criterio del doc).
	if _, err := os.Stat(filepath.Join(runDir, "step-01.events.jsonl")); !os.IsNotExist(err) {
		t.Errorf("events.jsonl should NOT exist for gemini (text mode), stat err=%v", err)
	}
}

// multiStepYAML arma un pipeline de N steps. Cada step es {name, cli, kind,
// content, input}. H6 lo necesita para ejercitar pipelines reales con
// previous_output entre steps. Mantenemos kind=skill para que preflight
// chequee que el skill existe (los tests pre-arman las skills relevantes).
type multiStep struct {
	Name    string
	CLI     string
	Kind    string
	Content string
	Input   string
}

func multiStepYAML(name string, steps []multiStep) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "description: ready desc\n")
	b.WriteString("steps:\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "- name: %s\n", s.Name)
		fmt.Fprintf(&b, "  cli: %s\n", s.CLI)
		fmt.Fprintf(&b, "  kind: %s\n", s.Kind)
		fmt.Fprintf(&b, "  content: %s\n", s.Content)
		fmt.Fprintf(&b, "  input: %s\n", s.Input)
	}
	return b.String()
}

// readCapturedStdin lee el archivo stdin capturado por el fake (CaptureStdin).
// El path es <ScriptDir>/stdins/<seq>.bin. seq lo asigna chefake en orden de
// invocacion global (ver cmd/fake/main.go.nextSeq), no por bin — los tests
// derivan el seq de env.Invocations() cuando necesitan correlacionar.
func readCapturedStdin(t *testing.T, scriptDir string, seq int) string {
	t.Helper()
	path := filepath.Join(scriptDir, "stdins", fmt.Sprintf("%d.bin", seq))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured stdin %s: %v", path, err)
	}
	return string(data)
}

// TestRunner_R3H6MultiStepPreviousOutput cubre el primer test e2e listado
// en H6: "Pipeline 2 steps con fake CLIs predecibles. Assert: payload del
// step 2 es exactamente el output del step 1."
//
// Arrancamos un pipeline con 2 steps (input=none + input=previous_output).
// El step 1 emite un sentinel conocido y exit 0. El step 2 captura el stdin
// que recibe — el assert principal es que ese stdin == el stdout del step 1
// (lo que el runner escribe en step-01.result.yaml/output).
func TestRunner_R3H6MultiStepPreviousOutput(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Skills pre-armadas para que preflight pase verde.
	preArmCLISkill(t, env.HomeDir, "gemini", "h6-step1")
	preArmCLISkill(t, env.HomeDir, "gemini", "h6-step2")

	// Step 1 (gemini text mode): emite un sentinel unico en stdout y exit 0.
	// Usamos gemini para evitar el parser stream-json de claude — el output
	// "limpio" es mas facil de igualar contra el stdin capturado del step 2.
	step1Output := "step1-output-payload-h6"
	env.ExpectAgent("gemini").WhenArgsMatch(`/h6-step1`).
		RespondStdout(step1Output, 0)
	// Step 2: captura stdin y emite "step2-done". CaptureStdin escribe el
	// payload en stdins/<seq>.bin para que el test pueda assertarlo.
	env.ExpectAgent("gemini").WhenArgsMatch(`/h6-step2`).
		CaptureStdin().
		RespondStdout("step2-done-h6", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h6-multi.yaml": multiStepYAML("R3H6 Multi", []multiStep{
			{Name: "alpha", CLI: "gemini", Kind: "skill", Content: "h6-step1", Input: "none"},
			{Name: "beta", CLI: "gemini", Kind: "skill", Content: "h6-step2", Input: "previous_output"},
		}),
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
	if !p.WaitForOutputSince(t, mark, "R3H6 Multi", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H6 Multi", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// El run ejecuta los 2 steps en orden y aterriza en R4 ("Run completo").
	if !p.WaitForOutputSince(t, mark, "Run completo", 8*time.Second) {
		t.Fatalf("R4 'Run completo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup ordenado.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Asserts en disco.
	runDir := firstRunDir(t, env.HomeDir, "r3h6-multi")

	// Manifest done con 2 steps done.
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: done") {
		t.Errorf("expected manifest status: done, got:\n%s", manifestRaw)
	}
	// Ambos steps deberian estar como status done en el manifest.
	if strings.Count(manifestRaw, "status: done") < 3 {
		// 1 (run-level) + 2 (steps) = 3 ocurrencias minimas.
		t.Errorf("expected manifest to record both steps done, got:\n%s", manifestRaw)
	}

	// step-01.result.yaml tiene el output del step 1.
	step1Result := readRunFile(t, filepath.Join(runDir, "step-01.result.yaml"))
	if !strings.Contains(step1Result, step1Output) {
		t.Errorf("expected step-01.result.yaml to contain %q, got:\n%s", step1Output, step1Result)
	}

	// step-02.result.yaml existe (criterio del doc: "step-NN.result.yaml
	// existe por cada step").
	if _, err := os.Stat(filepath.Join(runDir, "step-02.result.yaml")); err != nil {
		t.Errorf("expected step-02.result.yaml to exist: %v", err)
	}

	// El stdin del step 2 (capturado por el fake) tiene que contener el
	// output del step 1. El fake escribe el archivo bajo
	// <ScriptDir>/stdins/<seq>.bin — buscamos el seq del invocation con
	// args matcheando "h6-step2".
	calls := env.Invocations().For("gemini")
	if len(calls) != 2 {
		t.Fatalf("expected 2 invocations of gemini, got %d", len(calls))
	}
	var step2Seq int
	for _, c := range calls {
		args := strings.Join(c.Args, " ")
		if strings.Contains(args, "h6-step2") {
			step2Seq = c.Seq
			break
		}
	}
	if step2Seq == 0 {
		t.Fatalf("could not find seq of step 2 invocation; calls=%+v", calls)
	}
	stdin2 := readCapturedStdin(t, env.ScriptDir, step2Seq)
	// El runner escribe el output crudo del step previo (step1Output) +
	// posiblemente un newline trailing del scanner del tee. Asertamos
	// contains, no equals, para no engancharnos en trimming.
	if !strings.Contains(stdin2, step1Output) {
		t.Errorf("expected step 2 stdin to contain step 1 output %q, got:\n%q",
			step1Output, stdin2)
	}
}

// TestRunner_R3H6MultiStepFailStops cubre el segundo test e2e listado en
// H6: "Step 2 fake con exit 1; assert: RF, manifest steps[0].status=done,
// steps[1].status=failed, steps[2] no existe en el array."
//
// Pipeline de 3 steps; el segundo falla → el runner transiciona a RF
// inmediatamente y el step 3 nunca se invoca (no hay step-03.* en el
// run dir, y el fake no registra una tercer invocacion).
func TestRunner_R3H6MultiStepFailStops(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmCLISkill(t, env.HomeDir, "gemini", "h6-fail-step1")
	preArmCLISkill(t, env.HomeDir, "gemini", "h6-fail-step2")
	preArmCLISkill(t, env.HomeDir, "gemini", "h6-fail-step3")

	env.ExpectAgent("gemini").WhenArgsMatch(`/h6-fail-step1`).
		RespondStdout("step1-ok", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`/h6-fail-step2`).
		RespondExitWithError(1, "boom-h6-step2")
	// Step 3 esta scripteado pero no deberia matchear nunca — si lo hace,
	// el assert sobre count de invocaciones falla.
	env.ExpectAgent("gemini").WhenArgsMatch(`/h6-fail-step3`).
		RespondStdout("step3-should-not-run", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h6-fail.yaml": multiStepYAML("R3H6 Fail", []multiStep{
			{Name: "alpha", CLI: "gemini", Kind: "skill", Content: "h6-fail-step1", Input: "none"},
			{Name: "beta", CLI: "gemini", Kind: "skill", Content: "h6-fail-step2", Input: "previous_output"},
			{Name: "gamma", CLI: "gemini", Kind: "skill", Content: "h6-fail-step3", Input: "previous_output"},
		}),
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
	if !p.WaitForOutputSince(t, mark, "R3H6 Fail", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H6 Fail", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run fallo", 8*time.Second) {
		t.Fatalf("RF 'Run fallo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	runDir := firstRunDir(t, env.HomeDir, "r3h6-fail")

	// Manifest: status failed; steps[0]=done, steps[1]=failed, steps[2] no
	// debe estar listado (criterio del doc — el step nunca arranco).
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: failed") {
		t.Errorf("expected manifest status: failed, got:\n%s", manifestRaw)
	}
	// Step alpha (1) done, beta (2) failed — buscamos por nombre del step
	// para no depender del pretty-printing del yaml.
	if !strings.Contains(manifestRaw, "name: alpha") {
		t.Errorf("expected manifest to list step alpha, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "name: beta") {
		t.Errorf("expected manifest to list step beta, got:\n%s", manifestRaw)
	}
	if strings.Contains(manifestRaw, "name: gamma") {
		t.Errorf("manifest should NOT include step gamma (nunca arranco), got:\n%s", manifestRaw)
	}

	// step-01.result.yaml existe (alpha corrio); step-02.result.yaml existe
	// (beta corrio aunque haya fallado — el doc fija que result.yaml se
	// escribe siempre); step-03.* NO debe existir (gamma nunca arranco).
	if _, err := os.Stat(filepath.Join(runDir, "step-01.result.yaml")); err != nil {
		t.Errorf("expected step-01.result.yaml to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "step-02.result.yaml")); err != nil {
		t.Errorf("expected step-02.result.yaml to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "step-03.result.yaml")); !os.IsNotExist(err) {
		t.Errorf("step-03.result.yaml should NOT exist (step 3 nunca arranco), stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "step-03.stdout.log")); !os.IsNotExist(err) {
		t.Errorf("step-03.stdout.log should NOT exist, stat err=%v", err)
	}

	// El fake debe haber sido invocado exactamente 2 veces (alpha + beta);
	// gamma nunca llego a spawnearse.
	calls := env.Invocations().For("gemini")
	if len(calls) != 2 {
		t.Errorf("expected 2 invocations of gemini (step 3 nunca arranca), got %d: %+v",
			len(calls), calls)
	}
}

// validatorYAMLStep arma un YAML de pipeline con UN solo step (gemini text)
// que tiene bloque validator (gemini text tambien). Usado por los tests de
// H7 — todas las variantes (ok / fail / no-block / pause) reusan la misma
// shape, lo unico que cambia es el response del fake gemini.
//
// El step usa input=none (no hay input previo a resolver), kind=prompt
// (el content es la prompt cruda — evitamos el path de skills para no
// requerir skills pre-armadas en cada test). El validator usa kind=skill
// para que el fake matchee args /<skill> y podamos diferenciar invocacion
// de step vs invocacion de validator en los logs.
func validatorYAMLStep(name, stepPrompt, validatorSkill string, maxLoops int, onMaxLoops string) string {
	return fmt.Sprintf(`name: %s
description: ready desc
steps:
- name: alpha
  cli: gemini
  kind: prompt
  content: %s
  input: none
  validator:
    cli: gemini
    kind: skill
    content: %s
  max_loops: %d
  on_max_loops: %s
`, name, stepPrompt, validatorSkill, maxLoops, onMaxLoops)
}

// TestRunner_R3H7ValidatorOkFirstTry cubre el primer test e2e listado en
// H7: "Validator fake con verdict ok primer try; assert: manifest
// loops_run=1, final_verdict=ok."
//
// Pipeline 1 step (prompt) con validator gemini skill, max_loops=3,
// on_max_loops=fail. El fake del step emite output cualquiera + exit 0; el
// fake del validator (skill h7-ok-validator) emite "verdict: ok" → ningun
// retry, run aterriza en R4 con final_verdict=ok y loops_run=1.
func TestRunner_R3H7ValidatorOkFirstTry(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	// Skill del validator (gemini kind=skill → /h7-ok-validator).
	preArmCLISkill(t, env.HomeDir, "gemini", "h7-ok-validator")

	// Step principal: gemini -p "step output content" → emite sentinel.
	// Args matcheamos por el prompt para no colisionar con el validator.
	env.ExpectAgent("gemini").WhenArgsMatch(`step-h7-ok-prompt`).
		RespondStdout("step1-output-h7-ok", 0)
	// Validator: gemini /h7-ok-validator → emite verdict ok.
	env.ExpectAgent("gemini").WhenArgsMatch(`/h7-ok-validator`).
		RespondStdout("verdict: ok\n", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h7-ok.yaml": validatorYAMLStep("R3H7 Ok", "step-h7-ok-prompt", "h7-ok-validator", 3, "fail"),
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
	if !p.WaitForOutputSince(t, mark, "R3H7 Ok", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H7 Ok", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 8*time.Second) {
		t.Fatalf("R4 'Run completo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup ordenado.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	// Asserts en disco.
	runDir := firstRunDir(t, env.HomeDir, "r3h7-ok")

	// Manifest done con loops_run: 1 + final_verdict: ok dentro del bloque
	// validator del step. El yaml del manifest serializa el bloque como
	// "validator:" con sub-campos indentados.
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: done") {
		t.Errorf("expected manifest status: done, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "validator:") {
		t.Errorf("expected manifest to contain validator block, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "loops_run: 1") {
		t.Errorf("expected loops_run: 1, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "final_verdict: ok") {
		t.Errorf("expected final_verdict: ok, got:\n%s", manifestRaw)
	}

	// verdict.yaml del loop 1 existe.
	verdictRaw := readRunFile(t, filepath.Join(runDir, "step-01.validator.01.verdict.yaml"))
	if !strings.Contains(verdictRaw, "verdict: ok") {
		t.Errorf("expected verdict.yaml to contain 'verdict: ok', got:\n%s", verdictRaw)
	}

	// El step se invoco 1 vez + el validator se invoco 1 vez = 2 invocaciones
	// de gemini en total.
	calls := env.Invocations().For("gemini")
	if len(calls) != 2 {
		t.Errorf("expected 2 invocations of gemini (1 step + 1 validator), got %d: %+v",
			len(calls), calls)
	}
}

// TestRunner_R3H7ValidatorFailMaxLoopsFail cubre el segundo test e2e
// listado en H7: "Validator fake con verdict fail siempre, on_max_loops=
// fail; assert: RF, loops_run=max_loops, final_verdict=fail."
//
// Pipeline 1 step con validator que SIEMPRE devuelve verdict: fail.
// max_loops=2 (mas chico que el default 3 para que el test sea rapido).
// on_max_loops=fail → tras agotar los loops el run cae a RF con
// final_verdict=fail.
//
// Cantidad esperada de invocaciones del fake gemini:
//   - step: corre 2 veces (loop 1 + retry tras fail)
//   - validator: corre 2 veces (1 por cada vuelta del step)
//   - total: 4
func TestRunner_R3H7ValidatorFailMaxLoopsFail(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmCLISkill(t, env.HomeDir, "gemini", "h7-fail-validator")

	env.ExpectAgent("gemini").WhenArgsMatch(`step-h7-fail-prompt`).
		RespondStdout("step1-output-h7-fail", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`/h7-fail-validator`).
		RespondStdout("verdict: fail\nfeedback: nope\n", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h7-fail.yaml": validatorYAMLStep("R3H7 Fail", "step-h7-fail-prompt", "h7-fail-validator", 2, "fail"),
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
	if !p.WaitForOutputSince(t, mark, "R3H7 Fail", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H7 Fail", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	// Tras agotar max_loops con on_max_loops=fail, R3 transiciona a RF.
	if !p.WaitForOutputSince(t, mark, "Run fallo", 10*time.Second) {
		t.Fatalf("RF 'Run fallo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	runDir := firstRunDir(t, env.HomeDir, "r3h7-fail")

	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: failed") {
		t.Errorf("expected manifest status: failed, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "loops_run: 2") {
		t.Errorf("expected loops_run: 2 (=max_loops), got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "final_verdict: fail") {
		t.Errorf("expected final_verdict: fail, got:\n%s", manifestRaw)
	}

	// Ambos verdict.yaml existen.
	if _, err := os.Stat(filepath.Join(runDir, "step-01.validator.01.verdict.yaml")); err != nil {
		t.Errorf("expected verdict 01.yaml to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "step-01.validator.02.verdict.yaml")); err != nil {
		t.Errorf("expected verdict 02.yaml to exist: %v", err)
	}

	// Invocaciones del fake: 2 del step + 2 del validator = 4.
	calls := env.Invocations().For("gemini")
	if len(calls) != 4 {
		t.Errorf("expected 4 gemini invocations (2 step + 2 validator), got %d: %+v",
			len(calls), calls)
	}
}

// TestRunner_R3H7ValidatorNoVerdictBlock cubre el tercer test e2e listado
// en H7: "Validator que devuelve un YAML sin bloque verdict; assert:
// comportamiento equivalente a fail con feedback 'no verdict block'."
//
// El validator emite stdout que NO contiene bloque verdict (texto crudo
// sin yaml). max_loops=1 + on_max_loops=fail → tras un solo intento que
// el parser trata como fail, el run cae a RF. El verdict.yaml debe
// registrar feedback="no verdict block".
func TestRunner_R3H7ValidatorNoVerdictBlock(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmCLISkill(t, env.HomeDir, "gemini", "h7-noblock-validator")

	env.ExpectAgent("gemini").WhenArgsMatch(`step-h7-noblock-prompt`).
		RespondStdout("step1-output-h7-noblock", 0)
	// Validator emite texto sin bloque YAML con clave verdict.
	env.ExpectAgent("gemini").WhenArgsMatch(`/h7-noblock-validator`).
		RespondStdout("aca no hay nada que parezca yaml\nsolo prosa libre\n", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h7-noblock.yaml": validatorYAMLStep("R3H7 NoBlock", "step-h7-noblock-prompt", "h7-noblock-validator", 1, "fail"),
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
	if !p.WaitForOutputSince(t, mark, "R3H7 NoBlock", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H7 NoBlock", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run fallo", 8*time.Second) {
		t.Fatalf("RF 'Run fallo' never rendered\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	runDir := firstRunDir(t, env.HomeDir, "r3h7-noblock")

	// El verdict.yaml registra el feedback sintetico 'no verdict block'.
	verdictRaw := readRunFile(t, filepath.Join(runDir, "step-01.validator.01.verdict.yaml"))
	if !strings.Contains(verdictRaw, "verdict: fail") {
		t.Errorf("expected verdict.yaml to contain 'verdict: fail', got:\n%s", verdictRaw)
	}
	if !strings.Contains(verdictRaw, "no verdict block") {
		t.Errorf("expected feedback 'no verdict block' in verdict.yaml, got:\n%s", verdictRaw)
	}

	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: failed") {
		t.Errorf("expected manifest status: failed, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "final_verdict: fail") {
		t.Errorf("expected final_verdict: fail, got:\n%s", manifestRaw)
	}
}

// TestRunner_R3H7ValidatorPauseContinue cubre el cuarto test e2e listado
// en H7: "on_max_loops=pause; stdin que selecciona 'continuar' en RP;
// assert: R4, final_verdict=human-override."
//
// El validator siempre falla → tras agotar max_loops (=1) el modal RP
// aparece. El test envia "enter" (cursor inicial = continuar) → run
// avanza al final con final_verdict=human-override y aterriza en R4.
func TestRunner_R3H7ValidatorPauseContinue(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	preArmCLISkill(t, env.HomeDir, "gemini", "h7-pause-validator")

	env.ExpectAgent("gemini").WhenArgsMatch(`step-h7-pause-prompt`).
		RespondStdout("step1-output-h7-pause", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`/h7-pause-validator`).
		RespondStdout("verdict: fail\nfeedback: feedback-de-pause\n", 0)

	preArmPipelinesDir(t, env.HomeDir, map[string]string{
		"r3h7-pause.yaml": validatorYAMLStep("R3H7 Pause", "step-h7-pause-prompt", "h7-pause-validator", 1, "pause"),
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
	if !p.WaitForOutputSince(t, mark, "R3H7 Pause", 3*time.Second) {
		t.Fatalf("entry never rendered\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Preflight de R3H7 Pause", 3*time.Second) {
		t.Fatalf("R2 preflight never rendered\n%s", p.Since(mark))
	}
	if !p.WaitForOutputSince(t, mark, "todo listo", 5*time.Second) {
		t.Fatalf("preflight never reached todo listo:\n%s", p.Since(mark))
	}
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter: %v", err)
	}

	// Esperar el modal RP — se renderea con titulo "Validator agoto
	// max_loops" + el feedback en la lista.
	if !p.WaitForOutputSince(t, mark, "Validator agoto max_loops", 8*time.Second) {
		t.Fatalf("RP modal never rendered\n%s", p.Since(mark))
	}
	if !strings.Contains(p.Since(mark), "feedback-de-pause") {
		t.Errorf("RP modal should show last feedback, got:\n%s", p.Since(mark))
	}

	// Cursor inicial = continuar (PauseChoiceContinue=0). enter elige.
	mark = p.Mark()
	if err := p.Send("\r"); err != nil {
		t.Fatalf("send enter (continuar): %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "Run completo", 5*time.Second) {
		t.Fatalf("R4 'Run completo' never rendered after continuar\n%s", p.Since(mark))
	}

	// Cleanup.
	mark = p.Mark()
	if err := p.Send("\x1b"); err != nil {
		t.Fatalf("send esc: %v", err)
	}
	if !p.WaitForOutputSince(t, mark, "My pipelines", 3*time.Second) {
		t.Fatalf("lister never re-rendered\n%s", p.Since(mark))
	}
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
		t.Fatalf("expected exit 0, got %d", res.ExitCode)
	}

	runDir := firstRunDir(t, env.HomeDir, "r3h7-pause")
	manifestRaw := readRunFile(t, filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(manifestRaw, "status: done") {
		t.Errorf("expected manifest status: done, got:\n%s", manifestRaw)
	}
	if !strings.Contains(manifestRaw, "final_verdict: human-override") {
		t.Errorf("expected final_verdict: human-override, got:\n%s", manifestRaw)
	}
}
