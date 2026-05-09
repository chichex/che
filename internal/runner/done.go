package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// updateDone maneja teclas de R4 (terminal verde). H10 cubre el set
// completo del doc:
//
//   - enter / esc       → vuelve al lister (R0). El refresh del chip "last
//     run" lo hace el lister al renderear (lee el manifest del run que
//     acabamos de cerrar).
//   - q / ctrl+c        → salida total de che.
//   - y                 → suspende TUI + abre $EDITOR sobre el result.yaml
//     del ultimo step (output efectivo del pipeline).
//   - l                 → suspende TUI + abre $PAGER sobre el stdout.log
//     del ultimo step.
//
// y/l no transicionan — al volver del editor/pager, la pantalla R4 sigue
// activa para que el usuario pueda enter/esc cuando quiera.
func (m RunModel) updateDone(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "enter", "esc":
		m.exitApp = false
		return m, tea.Quit
	case "y":
		path := m.lastResultPath()
		if path == "" {
			return m, nil
		}
		return m, openInEditorCmd(path)
	case "l":
		path := m.lastStdoutPath()
		if path == "" {
			return m, nil
		}
		return m, openInPagerCmd(path)
	}
	return m, nil
}

// updateFailed maneja teclas de RF (terminal rojo / cancelado amarillo).
// Mismas teclas que R4 + r retry + l pager (H10):
//
//   - enter / esc       → vuelve al lister (R0). Igual que R4.
//   - q / ctrl+c        → salida total.
//   - l                 → suspende TUI + abre $PAGER sobre el stderr.log
//     del step que fallo. Para runs cancelled (donde stderr puede estar
//     vacio porque se interrumpio el subprocess antes de emitir nada), cae
//     a stdout.log del mismo step.
//   - r                 → re-disparar el pipeline desde R1 con el input
//     pre-cargado. Marca la flag retry para que el caller (Run loop) lo
//     procese — el doc fija "crea un run-id nuevo".
func (m RunModel) updateFailed(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "enter", "esc":
		m.exitApp = false
		return m, tea.Quit
	case "r":
		// retry → cerramos el program emitiendo el flag; runner.Run lo
		// detecta y arma una nueva pasada con el input ya resuelto. El
		// run viejo queda intacto en disco como historial (la barrera
		// del doc: "crea un run-id nuevo").
		m.retryRequested = true
		m.exitApp = false
		return m, tea.Quit
	case "l":
		path := m.failedLogPath()
		if path == "" {
			return m, nil
		}
		return m, openInPagerCmd(path)
	}
	return m, nil
}

// updateDoneEditorReturn / updateFailedEditorReturn son no-ops: tea.ExecProcess
// ya restauro el estado del program antes de emitir el msg, asi que el
// re-render del View() siguiente alcanza para que la pantalla vuelva a
// pintarse. Existen como handlers explicitos para que el switch del Update
// no se quede sin caso conocido y emita un fallback "tecla desconocida".
func (m RunModel) updateDoneEditorReturn(_ editorReturnedMsg) (tea.Model, tea.Cmd) {
	return m, nil
}

func (m RunModel) updateFailedEditorReturn(_ editorReturnedMsg) (tea.Model, tea.Cmd) {
	return m, nil
}

// viewDone renderea el resumen verde del run completo. Sigue el mockup del
// doc (R4): titulo, duracion, lista de steps, path al run dir + result.yaml
// del ultimo step.
func (m RunModel) viewDone() string {
	var b strings.Builder
	crumb := append(runnerCrumb(m.Pipeline.Name), "Done")
	b.WriteString(breadcrumb(crumb...))
	b.WriteString("  ")
	// Chip "✓ done" verde apenas a la derecha del breadcrumb — sustituye
	// al "Run completo · <name>" verde que tenia antes (mismo color, misma
	// senal de exito sin pisar el header del breadcrumb).
	b.WriteString(okStyle.Render("✓ done"))
	b.WriteString("\n\n")

	if len(m.Steps) > 0 {
		first := m.Steps[0]
		dur := totalDuration(m.Steps)
		b.WriteString(dimStyle.Render(fmt.Sprintf("duracion: %s", dur)))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("run id: %s", m.RunID)))
		b.WriteString("\n\n")

		for _, s := range m.Steps {
			line := fmt.Sprintf("  %d. %s    %s    %s",
				s.Idx,
				s.Name,
				s.FinishedAt.Sub(s.StartedAt).Round(time.Millisecond),
				okStyle.Render("✓"))
			b.WriteString(line)
			b.WriteString("\n")
		}
		_ = first
		b.WriteString("\n")
		lastIdx := m.Steps[len(m.Steps)-1].Idx
		resultPath := filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.result.yaml", lastIdx))
		b.WriteString(dimStyle.Render("output final: " + resultPath))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	// H10: el doc fija el footer completo del R4 — y abre result.yaml en
	// $EDITOR; l abre stdout.log en $PAGER; enter/esc vuelven al lister.
	b.WriteString(hintStyle.Render("enter / esc volver al menu · y abrir result.yaml · l abrir log · q / ctrl+c salir"))
	b.WriteString("\n")
	return b.String()
}

// viewFailed renderea el resumen rojo (o amarillo si fue cancel). Marca
// el step que fallo, muestra el exit_code y un dump de las ultimas lineas
// del stderr (el doc fija las "ultimas 20 lineas").
func (m RunModel) viewFailed() string {
	var b strings.Builder

	// Detectar si fue cancel para distinguir el ultimo segmento del
	// breadcrumb + el chip de tono. El nombre del pipeline ya vive en el
	// segmento "Run · <name>", asi que no se repite en el ultimo.
	cancelled := false
	for _, s := range m.Steps {
		if s.Status == StepStatusCancelled {
			cancelled = true
			break
		}
	}
	last := "Failed"
	if cancelled {
		last = "Cancelled"
	}
	crumb := append(runnerCrumb(m.Pipeline.Name), last)
	b.WriteString(breadcrumb(crumb...))
	b.WriteString("  ")
	if cancelled {
		b.WriteString(warnStyle.Render("! cancelled"))
	} else {
		b.WriteString(errorStyle.Render("✗ failed"))
	}
	b.WriteString("\n\n")

	// Step que fallo (en H4 es siempre el step 0).
	if len(m.Steps) > 0 {
		failed := m.Steps[0]
		for _, s := range m.Steps {
			if s.Status == StepStatusFailed || s.Status == StepStatusCancelled {
				failed = s
				break
			}
		}
		head := fmt.Sprintf("step %d · %s", failed.Idx, failed.Name)
		if failed.CLI != "" {
			head += fmt.Sprintf(" (%s)", failed.CLI)
		}
		b.WriteString(head)
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("exit_code: %d   ·   duracion: %s",
			failed.ExitCode,
			failed.FinishedAt.Sub(failed.StartedAt).Round(time.Millisecond))))
		b.WriteString("\n\n")

		if m.FailedStderr != "" {
			b.WriteString(dimStyle.Render("ultimas lineas:"))
			b.WriteString("\n")
			for _, line := range tail(m.FailedStderr, 20) {
				b.WriteString("  " + errorStyle.Render(line))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		if m.RunID != "" {
			b.WriteString(dimStyle.Render("run id: " + m.RunID))
			b.WriteString("\n")
			stderrPath := filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.stderr.log", failed.Idx))
			b.WriteString(dimStyle.Render("ver logs: " + stderrPath))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	// H10: r retry (re-arma el run desde R1 con el input pre-cargado),
	// l abre el stderr.log en $PAGER, esc/enter vuelven al lister. Mismo
	// hint para failed real y cancelled — el doc no diferencia entre los
	// dos en cuanto a teclas, solo en el tono del titulo.
	b.WriteString(hintStyle.Render("enter / esc volver al menu · r retry · l abrir log · q / ctrl+c salir"))
	b.WriteString("\n")
	return b.String()
}

func totalDuration(steps []StepRun) time.Duration {
	var total time.Duration
	for _, s := range steps {
		if !s.FinishedAt.IsZero() && !s.StartedAt.IsZero() {
			total += s.FinishedAt.Sub(s.StartedAt)
		}
	}
	return total.Round(time.Millisecond)
}

// tail devuelve las ultimas n lineas de s. Si s tiene < n, las devuelve
// todas. Strings sin newlines cuentan como 1 linea.
func tail(s string, n int) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// lastResultPath devuelve el path absoluto del result.yaml del ultimo step
// del run. Vacio si no hay run dir o no hay steps. R4.y lo usa para abrir
// el "output efectivo del pipeline" en $EDITOR.
func (m RunModel) lastResultPath() string {
	if m.RunDir == "" || len(m.Steps) == 0 {
		return ""
	}
	last := m.Steps[len(m.Steps)-1]
	return filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.result.yaml", last.Idx))
}

// lastStdoutPath devuelve el path absoluto del stdout.log del ultimo step
// (R4.l → $PAGER sobre el output crudo del subprocess).
func (m RunModel) lastStdoutPath() string {
	if m.RunDir == "" || len(m.Steps) == 0 {
		return ""
	}
	last := m.Steps[len(m.Steps)-1]
	return filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.stdout.log", last.Idx))
}

// failedLogPath devuelve el path absoluto del stderr.log (o stdout.log si
// stderr no existe) del step que fallo. RF.l lo usa para abrir el log del
// fallo en $PAGER. Si no hay step fallido (caso degradado), devuelve el
// stderr del primer step.
func (m RunModel) failedLogPath() string {
	if m.RunDir == "" || len(m.Steps) == 0 {
		return ""
	}
	idx := m.Steps[0].Idx
	for _, s := range m.Steps {
		if s.Status == StepStatusFailed || s.Status == StepStatusCancelled {
			idx = s.Idx
			break
		}
	}
	stderrPath := filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.stderr.log", idx))
	// Cancelled subprocesses pueden no haber emitido stderr. Si el archivo
	// no existe, caemos a stdout.log para no abrir un pager vacio.
	if _, err := os.Stat(stderrPath); err == nil {
		return stderrPath
	}
	stdoutPath := filepath.Join(m.RunDir, fmt.Sprintf("step-%02d.stdout.log", idx))
	if _, err := os.Stat(stdoutPath); err == nil {
		return stdoutPath
	}
	// Fallback: devolvemos el stderr aunque no exista — el pager se
	// quejara con un mensaje propio, pero no panic-amos.
	return stderrPath
}
