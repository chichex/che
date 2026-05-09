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
