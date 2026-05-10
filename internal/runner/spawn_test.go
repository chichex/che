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
// step con `{{INPUT}}` en su content queda sustituido por el payload en
// los args del subprocess. Es el contrato real de Fix 1.
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
	// args = [codex, exec, --json, <content sustituido>]
	if len(cmd.Args) < 4 {
		t.Fatalf("expected >= 4 args, got %d: %#v", len(cmd.Args), cmd.Args)
	}
	contentArg := cmd.Args[3]
	if strings.Contains(contentArg, "{{INPUT}}") {
		t.Errorf("step.Content paso al subprocess con {{INPUT}} literal: %q", contentArg)
	}
	if !strings.Contains(contentArg, "PAYLOAD-X") {
		t.Errorf("step.Content no contiene el payload sustituido: %q", contentArg)
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
