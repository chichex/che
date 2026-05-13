package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// TestInterpolateInput_ReplacesPlaceholder cubre Fix 1 (#107): el helper
// reemplaza `{{INPUT}}` por el payload resuelto en el content del step
// antes de armar args. Sin esto el modelo recibe el placeholder literal y
// tiene que adivinar que stdin == lo que iba en el placeholder.
func TestInterpolateInput_ReplacesPlaceholder(t *testing.T) {
	content := "procesa estos issues:\n<<<\n{{INPUT}}\n>>>\n"
	payload := "issues:\n  - 42\n  - 43\n"
	got := interpolateInput(content, payload)
	if strings.Contains(got, "{{INPUT}}") {
		t.Errorf("interpolateInput dejo el placeholder literal: %q", got)
	}
	if !strings.Contains(got, "issues:\n  - 42\n  - 43\n") {
		t.Errorf("interpolateInput no inserto el payload: %q", got)
	}
}

// TestInterpolateInput_NoPlaceholder verifica que un content sin
// `{{INPUT}}` queda intacto (no inflamos el content con el payload por las
// dudas — el stdin sigue llevando el payload).
func TestInterpolateInput_NoPlaceholder(t *testing.T) {
	content := "/explore-flow"
	payload := "anything"
	got := interpolateInput(content, payload)
	if got != content {
		t.Errorf("interpolateInput debio devolver el content sin cambios; got=%q want=%q", got, content)
	}
}

// TestInterpolateInput_Multiple cubre el caso de varios `{{INPUT}}` en el
// mismo content (paranoid — el che-funnel.yaml hoy solo tiene uno por
// step, pero la helper debe sustituir TODOS).
func TestInterpolateInput_Multiple(t *testing.T) {
	content := "{{INPUT}} - then - {{INPUT}}"
	got := interpolateInput(content, "X")
	want := "X - then - X"
	if got != want {
		t.Errorf("interpolateInput multiple: got=%q want=%q", got, want)
	}
}

// TestBuildSpawnArgs_StepContentInterpolated verifica end-to-end que un
// step codex con `{{INPUT}}` en su content queda sustituido por el payload
// en stdin (Fix #114: codex ya no lleva el content por argv).
func TestBuildSpawnArgs_StepContentInterpolated(t *testing.T) {
	step := wizard.Step{
		CLI:     "codex",
		Kind:    wizard.KindPrompt,
		Content: "ejecuta:\n<<<\n{{INPUT}}\n>>>",
	}
	cmd, err := defaultSpawnCmd(step, "PAYLOAD-X")
	if err != nil {
		t.Fatalf("defaultSpawnCmd error: %v", err)
	}
	// Fix #114: args = [codex, exec, --json, ...] — sin content en argv.
	// Issue #142: ahora tambien se inyecta `-m <default>` cuando no hay override.
	// El content interpolado viaja por stdin.
	for _, a := range cmd.Args {
		if strings.Contains(a, "{{INPUT}}") || strings.Contains(a, "PAYLOAD-X") || strings.Contains(a, "ejecuta:") {
			t.Errorf("codex argv no debe contener content/payload, got: %q", a)
		}
	}
	// Verificar que stdin contiene el payload sustituido (no el placeholder literal).
	sr, ok := cmd.Stdin.(*strings.Reader)
	if !ok {
		t.Fatalf("cmd.Stdin no es *strings.Reader para codex")
	}
	buf := make([]byte, sr.Len())
	_, _ = sr.Read(buf)
	stdinStr := string(buf)
	if strings.Contains(stdinStr, "{{INPUT}}") {
		t.Errorf("stdin de codex contiene {{INPUT}} literal: %q", stdinStr)
	}
	if !strings.Contains(stdinStr, "PAYLOAD-X") {
		t.Errorf("stdin de codex no contiene el payload sustituido: %q", stdinStr)
	}
}

// TestBuildSpawnArgs_CodexExcludesContent valida que buildSpawnArgs para
// codex devuelve exactamente ["exec", "--json"] sin el content en argv (Fix #114).
func TestBuildSpawnArgs_CodexExcludesContent(t *testing.T) {
	step := wizard.Step{
		CLI:     "codex",
		Kind:    wizard.KindPrompt,
		Content: "prompt largo que no debe viajar por argv",
	}
	args, err := buildSpawnArgs(step)
	if err != nil {
		t.Fatalf("buildSpawnArgs error: %v", err)
	}
	// Issue #142: codex sin step.Model recibe `-m <default>` ademas de
	// `exec --json`. Validamos que los primeros dos sigan siendo exec/--json
	// y que el content NO aparezca en ningun arg.
	if len(args) < 2 || args[0] != "exec" || args[1] != "--json" {
		t.Fatalf("buildSpawnArgs codex: expected prefix [exec --json], got %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "prompt largo") {
			t.Errorf("buildSpawnArgs codex incluye el content en argv: %q", a)
		}
	}
}

// TestDefaultSpawnCmd_CodexStdinHasContent valida que para codex, cmd.Stdin
// contiene el content interpolado y NO el payload pelado cuando el content
// embebe {{INPUT}} (Fix #114).
func TestDefaultSpawnCmd_CodexStdinHasContent(t *testing.T) {
	step := wizard.Step{
		CLI:     "codex",
		Kind:    wizard.KindPrompt,
		Content: "procesa:\n<<<\n{{INPUT}}\n>>>",
	}
	payload := "payload-pelado"
	cmd, err := defaultSpawnCmd(step, payload)
	if err != nil {
		t.Fatalf("defaultSpawnCmd error: %v", err)
	}
	sr, ok := cmd.Stdin.(*strings.Reader)
	if !ok {
		t.Fatalf("cmd.Stdin no es *strings.Reader para codex")
	}
	buf := make([]byte, sr.Len())
	_, _ = sr.Read(buf)
	stdinStr := string(buf)
	// Stdin debe contener el content (con payload interpolado inline).
	if !strings.Contains(stdinStr, "procesa:") {
		t.Errorf("stdin no contiene el content del prompt: %q", stdinStr)
	}
	if !strings.Contains(stdinStr, payload) {
		t.Errorf("stdin no contiene el payload interpolado: %q", stdinStr)
	}
	// Stdin NO debe ser SOLO el payload pelado (debe tener el content tambien).
	if stdinStr == payload {
		t.Errorf("stdin es solo el payload pelado — el content no viajo por stdin")
	}
}

// TestBuildSpawnArgs_GeminiOpencodeContentInArgv valida que gemini/opencode
// mantienen el content en argv (sin regresion por Fix #114). claude/codex se
// excluyen porque ahora pasan el prompt via stdin (ver
// TestBuildSpawnArgs_ClaudeExcludesContent y TestBuildSpawnArgs_CodexExcludesContent).
func TestBuildSpawnArgs_GeminiOpencodeContentInArgv(t *testing.T) {
	cases := []struct {
		cli     string
		kind    string
		content string
		wantArg string // fragmento que debe aparecer en algun arg
	}{
		{"gemini", wizard.KindPrompt, "mi prompt gemini", "mi prompt gemini"},
		{"opencode", wizard.KindPrompt, "mi prompt opencode", "mi prompt opencode"},
	}
	for _, tc := range cases {
		t.Run(tc.cli, func(t *testing.T) {
			step := wizard.Step{CLI: tc.cli, Kind: tc.kind, Content: tc.content}
			args, err := buildSpawnArgs(step)
			if err != nil {
				t.Fatalf("buildSpawnArgs(%s) error: %v", tc.cli, err)
			}
			found := false
			for _, a := range args {
				if strings.Contains(a, tc.wantArg) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: content %q no aparece en argv %v", tc.cli, tc.wantArg, args)
			}
		})
	}
}

// TestBuildSpawnArgs_ClaudeExcludesContent valida que claude (igual que codex
// post-Fix #114) NO lleva el content en argv — el prompt viaja por stdin.
// Tambien valida que se inyecta `--model opus` por default.
func TestBuildSpawnArgs_ClaudeExcludesContent(t *testing.T) {
	step := wizard.Step{
		CLI:     "claude",
		Kind:    wizard.KindPrompt,
		Content: "prompt largo que no debe viajar por argv",
	}
	args, err := buildSpawnArgs(step)
	if err != nil {
		t.Fatalf("buildSpawnArgs error: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "prompt largo") {
			t.Errorf("buildSpawnArgs claude incluye el content en argv: %q", a)
		}
	}
	// Debe pasar --model opus por default (sin step.Model declarado).
	wantPairs := map[string]string{"--model": "opus"}
	for flag, val := range wantPairs {
		idx := -1
		for i, a := range args {
			if a == flag {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(args) || args[idx+1] != val {
			t.Errorf("buildSpawnArgs claude: expected %s %s en argv, got %v", flag, val, args)
		}
	}
}

// TestBuildSpawnArgs_ClaudeHonorsStepModel valida que el campo `model:` del
// step claude se traduce a `--model <X>` (override del default opus).
func TestBuildSpawnArgs_ClaudeHonorsStepModel(t *testing.T) {
	step := wizard.Step{
		CLI:     "claude",
		Kind:    wizard.KindPrompt,
		Content: "p",
		Model:   "sonnet",
	}
	args, err := buildSpawnArgs(step)
	if err != nil {
		t.Fatalf("buildSpawnArgs error: %v", err)
	}
	idx := -1
	for i, a := range args {
		if a == "--model" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(args) || args[idx+1] != "sonnet" {
		t.Errorf("buildSpawnArgs claude: expected --model sonnet, got %v", args)
	}
}

// TestDefaultSpawnCmd_ClaudeLargePayload verifica que un step claude con un
// payload sintetico de >= 512 KB arma el cmd sin error, los args quedan
// acotados (sin el payload en argv), y stdin contiene el payload completo
// (via content interpolado con {{INPUT}}) — Fix #114 extendido a claude.
func TestDefaultSpawnCmd_ClaudeLargePayload(t *testing.T) {
	largePayload := strings.Repeat("A", 512*1024)
	step := wizard.Step{
		CLI:     "claude",
		Kind:    wizard.KindPrompt,
		Content: "analiza:\n<<<\n{{INPUT}}\n>>>",
	}
	cmd, err := defaultSpawnCmd(step, largePayload)
	if err != nil {
		t.Fatalf("defaultSpawnCmd con payload grande error: %v", err)
	}
	for _, a := range cmd.Args {
		if len(a) > 1000 {
			t.Errorf("arg demasiado largo (%d bytes) — el payload viajo por argv", len(a))
		}
	}
	sr, ok := cmd.Stdin.(*strings.Reader)
	if !ok {
		t.Fatalf("cmd.Stdin no es *strings.Reader para claude")
	}
	if sr.Len() < 512*1024 {
		t.Errorf("stdin demasiado corto (%d bytes) — el payload no esta completo en stdin", sr.Len())
	}
}

// TestDefaultSpawnCmd_CodexLargePayload verifica que un step codex con un
// payload sintetico de >= 512 KB arma el cmd sin error, los args quedan
// acotados (sin el payload en argv), y stdin contiene el payload completo
// (via content interpolado con {{INPUT}}) — Fix #114.
func TestDefaultSpawnCmd_CodexLargePayload(t *testing.T) {
	// Payload sintetico de 512 KB.
	largePayload := strings.Repeat("A", 512*1024)
	step := wizard.Step{
		CLI:     "codex",
		Kind:    wizard.KindPrompt,
		Content: "analiza:\n<<<\n{{INPUT}}\n>>>",
	}
	cmd, err := defaultSpawnCmd(step, largePayload)
	if err != nil {
		t.Fatalf("defaultSpawnCmd con payload grande error: %v", err)
	}
	// Args deben ser acotados: [codex, exec, --json, -m, <default>] — sin el payload.
	// Issue #142 sumo `-m <default>` por step.
	for _, a := range cmd.Args {
		if len(a) > 1000 {
			t.Errorf("arg demasiado largo (%d bytes) — el payload viajo por argv", len(a))
		}
	}
	// Stdin debe contener el payload completo.
	sr, ok := cmd.Stdin.(*strings.Reader)
	if !ok {
		t.Fatalf("cmd.Stdin no es *strings.Reader para codex")
	}
	if sr.Len() < 512*1024 {
		t.Errorf("stdin demasiado corto (%d bytes) — el payload no esta completo en stdin", sr.Len())
	}
}

// TestRunStep_RotatesEventsAcrossReruns cubre Fix 4 (#107) end-to-end: dos
// invocaciones consecutivas de runStep con EventsRun=1 y EventsRun=2
// producen DOS archivos events.jsonl distintos
// (`step-01.events.RUN-01.jsonl` y `step-01.events.RUN-02.jsonl`), cada
// uno con el contenido de su corrida. Antes del fix, la segunda
// invocacion truncaba el archivo unico `step-01.events.jsonl` y perdiamos
// la traza de la primera vuelta.
//
// Usamos un fake claude que emite una linea de stream-json distinta por
// corrida (`PAYLOAD-RUN-K`) para poder distinguir cada archivo.
func TestRunStep_RotatesEventsAcrossReruns(t *testing.T) {
	prev := spawnCmdFn
	t.Cleanup(func() { spawnCmdFn = prev })

	// El runner abre events.jsonl solo si step.CLI == "claude", asi que
	// declaramos un step claude y mockeamos spawnCmdFn para devolver un
	// `printf` que emita exactamente UN evento stream-json valido — la
	// goroutine teeStream lo va a parsear con parser.ForCLI("claude") y
	// appendear a events.jsonl. El contenido del evento varia por payload
	// para distinguir las dos corridas en disco.
	spawnCmdFn = func(step wizard.Step, payload string) (*exec.Cmd, error) {
		// Stream-json evento "system" con un campo arbitrario que lleva
		// el payload — alcanza para que el parser de claude lo trate
		// como evento valido y lo appendee crudo a events.jsonl.
		ev := fmt.Sprintf(`{"type":"system","subtype":"init","payload":"%s"}`, strings.TrimSpace(payload))
		return exec.Command("/bin/sh", "-c", "printf '%s\\n' '"+ev+"'"), nil
	}

	runDir := t.TempDir()
	step := wizard.Step{
		CLI:     "claude",
		Kind:    wizard.KindPrompt,
		Content: "/test",
	}

	// Helper: arranca runStep y drena el lineCh hasta el stepDoneMsg.
	drainOnce := func(eventsRun int, payload string) {
		state := &runState{requestCancel: make(chan struct{}, 1)}
		cmd := runStep(step, payload, runDir, 1, eventsRun, state)
		if cmd == nil {
			t.Fatalf("runStep devolvio cmd nil (eventsRun=%d)", eventsRun)
		}
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("timeout esperando stepDoneMsg (eventsRun=%d)", eventsRun)
			default:
			}
			msg := cmd()
			if _, ok := msg.(stepDoneMsg); ok {
				return
			}
			cmd = waitForLine(state.lineCh)
		}
	}

	drainOnce(1, "PAYLOAD-RUN-1")
	drainOnce(2, "PAYLOAD-RUN-2")

	// Ambos archivos deben existir, cada uno con el payload de su corrida.
	for k := 1; k <= 2; k++ {
		path := filepath.Join(runDir, fmt.Sprintf("step-01.events.RUN-%02d.jsonl", k))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("RUN-%02d events file ausente: %v", k, err)
		}
		want := fmt.Sprintf("PAYLOAD-RUN-%d", k)
		if !strings.Contains(string(raw), want) {
			t.Errorf("RUN-%02d events file no contiene %q; got:\n%s", k, want, string(raw))
		}
		// Defensa cruzada: el otro payload NO debe aparecer en este archivo.
		other := fmt.Sprintf("PAYLOAD-RUN-%d", 3-k)
		if strings.Contains(string(raw), other) {
			t.Errorf("RUN-%02d events file mezcla la otra corrida (%q); got:\n%s", k, other, string(raw))
		}
	}

	// El archivo legacy `step-01.events.jsonl` (sin sufijo) NO debe
	// crearse cuando eventsRun > 0 — el fix garantiza paths versionados.
	if _, err := os.Stat(filepath.Join(runDir, "step-01.events.jsonl")); !os.IsNotExist(err) {
		t.Errorf("legacy events.jsonl NO debe existir cuando se rota; stat err=%v", err)
	}
}

// findFlagValue busca un flag (ej. "-m" o "--model") en argv y devuelve el
// valor inmediatamente siguiente. Devuelve "" si el flag no aparece o no
// tiene valor (defensive — no deberia pasar en los tests porque siempre
// emitimos pares).
func findFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestBuildSpawnArgs_CodexInjectsModelFlag valida que codex recibe `-m <X>`
// cuando hay default o override declarado (issue #142).
func TestBuildSpawnArgs_CodexInjectsModelFlag(t *testing.T) {
	cases := []struct {
		name      string
		stepModel string
		wantModel string
	}{
		{"default cuando no se declara", "", "gpt-5.5"},
		{"override valido", "gpt-5.4", "gpt-5.4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := wizard.Step{
				CLI:     "codex",
				Kind:    wizard.KindPrompt,
				Content: "p",
				Model:   tc.stepModel,
			}
			args, err := buildSpawnArgs(step)
			if err != nil {
				t.Fatalf("buildSpawnArgs error: %v", err)
			}
			got := findFlagValue(args, "-m")
			if got != tc.wantModel {
				t.Errorf("buildSpawnArgs codex: expected -m %s, got %v", tc.wantModel, args)
			}
		})
	}
}

// TestBuildSpawnArgs_GeminiInjectsModelFlag valida que gemini recibe `-m <X>`
// cuando hay default o override declarado, tanto para kind=prompt como
// kind=skill.
func TestBuildSpawnArgs_GeminiInjectsModelFlag(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		stepModel string
		wantModel string
	}{
		{"prompt default", wizard.KindPrompt, "", "gemini-2.5-pro"},
		{"prompt override", wizard.KindPrompt, "gemini-2.5-flash", "gemini-2.5-flash"},
		{"skill default", wizard.KindSkill, "", "gemini-2.5-pro"},
		{"skill override", wizard.KindSkill, "gemini-2.5-flash-lite", "gemini-2.5-flash-lite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := wizard.Step{
				CLI:     "gemini",
				Kind:    tc.kind,
				Content: "x",
				Model:   tc.stepModel,
			}
			args, err := buildSpawnArgs(step)
			if err != nil {
				t.Fatalf("buildSpawnArgs error: %v", err)
			}
			got := findFlagValue(args, "-m")
			if got != tc.wantModel {
				t.Errorf("buildSpawnArgs gemini: expected -m %s, got %v", tc.wantModel, args)
			}
		})
	}
}

// TestBuildSpawnArgs_OpencodeNoModelFlag valida que opencode nunca recibe
// flag de modelo, ni siquiera cuando el step trae model: (que en realidad
// preflight ya rechazo — el test es defensivo sobre el contrato de spawn).
func TestBuildSpawnArgs_OpencodeNoModelFlag(t *testing.T) {
	step := wizard.Step{
		CLI:     "opencode",
		Kind:    wizard.KindPrompt,
		Content: "p",
	}
	args, err := buildSpawnArgs(step)
	if err != nil {
		t.Fatalf("buildSpawnArgs error: %v", err)
	}
	for _, a := range args {
		if a == "-m" || a == "--model" {
			t.Errorf("opencode no debe llevar flag de modelo; got args=%v", args)
		}
	}
}

// TestBuildSpawnArgs_ClaudeUsesDefaultOpus es un sanity check sobre la nueva
// implementacion via modelFor(): claude sin step.Model debe seguir pasando
// `--model opus`.
func TestBuildSpawnArgs_ClaudeUsesDefaultOpus(t *testing.T) {
	step := wizard.Step{
		CLI:     "claude",
		Kind:    wizard.KindPrompt,
		Content: "p",
	}
	args, err := buildSpawnArgs(step)
	if err != nil {
		t.Fatalf("buildSpawnArgs error: %v", err)
	}
	if got := findFlagValue(args, "--model"); got != "opus" {
		t.Errorf("expected --model opus, got %v", args)
	}
}
