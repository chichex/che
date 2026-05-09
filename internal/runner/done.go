package runner

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// updateDone maneja teclas de R4 (terminal verde). H4 minimal: enter / esc
// vuelven al lister; q / ctrl+c salen total. y / l (abrir result.yaml o
// stderr.log en $EDITOR / $PAGER) son out of scope de H4 — quedan como
// TODOs para H10 segun el doc.
func (m RunModel) updateDone(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "enter", "esc":
		m.exitApp = false
		return m, tea.Quit
	}
	return m, nil
}

// updateFailed maneja teclas de RF (terminal rojo / cancelado amarillo).
// Mismas teclas que R4. r (retry desde R1) tambien queda como TODO H10.
func (m RunModel) updateFailed(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "enter", "esc":
		m.exitApp = false
		return m, tea.Quit
	}
	return m, nil
}

// viewDone renderea el resumen verde del run completo. Sigue el mockup del
// doc (R4): titulo, duracion, lista de steps, path al run dir + result.yaml
// del ultimo step.
func (m RunModel) viewDone() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}
	var b strings.Builder
	b.WriteString(okStyle.Render("Run completo · " + name))
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
	b.WriteString(hintStyle.Render("enter / esc volver al menu · q / ctrl+c salir"))
	b.WriteString("\n")
	return b.String()
}

// viewFailed renderea el resumen rojo (o amarillo si fue cancel). Marca
// el step que fallo, muestra el exit_code y un dump de las ultimas lineas
// del stderr (el doc fija las "ultimas 20 lineas").
func (m RunModel) viewFailed() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}
	var b strings.Builder

	// Detectar si fue cancel para usar tono amarillo.
	cancelled := false
	for _, s := range m.Steps {
		if s.Status == StepStatusCancelled {
			cancelled = true
			break
		}
	}
	if cancelled {
		b.WriteString(warnStyle.Render("Run cancelado · " + name))
	} else {
		b.WriteString(errorStyle.Render("Run fallo · " + name))
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
	b.WriteString(hintStyle.Render("enter / esc volver al menu · q / ctrl+c salir"))
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
