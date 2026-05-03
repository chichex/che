package runner

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// PromptSelector construye un Selector que abre un wizard bubbletea
// multi-check para CADA step, mostrando los agentes preseleccionados
// (default: todos) y dejando al usuario togglear con espacio /
// confirmar con enter.
//
// Pensado para `che run` en TTY. Los tests del paquete usan un
// Selector inline (no este wizard) — la cobertura de la UI bubbletea
// va por su propio test cuando exista (PR9b traerá el test golden
// del frame).
//
// `out` es el writer a donde el programa de bubbletea rendea la UI
// (típicamente os.Stderr — stdout queda libre para el output de los
// agentes). Si out es nil, se usa os.Stderr internamente vía
// tea.WithOutput(nil) que cae al default.
//
// Cancelación: Esc o Ctrl+C devuelven ErrSelectionCancelled.
func PromptSelector(out io.Writer) Selector {
	return func(stepName string, agents []string) ([]string, error) {
		if len(agents) == 0 {
			// Step sin agentes: nada que preguntar. Devolvemos vacío
			// y el runner lo maneja (StopReasonNoAgents).
			return nil, nil
		}
		// Default: todos preseleccionados (PRD §3.e).
		checked := make([]bool, len(agents))
		for i := range checked {
			checked[i] = true
		}
		m := selectorModel{
			stepName: stepName,
			agents:   agents,
			checked:  checked,
			cursor:   0,
		}
		opts := []tea.ProgramOption{
			// Sin alt-screen: el wizard es inline (mantiene visible
			// el contexto de los steps anteriores).
		}
		if out != nil {
			opts = append(opts, tea.WithOutput(out))
		}
		p := tea.NewProgram(m, opts...)
		final, err := p.Run()
		if err != nil {
			return nil, fmt.Errorf("selector tui: %w", err)
		}
		fm, ok := final.(selectorModel)
		if !ok {
			return nil, fmt.Errorf("selector tui: modelo final inesperado")
		}
		if fm.cancelled {
			return nil, ErrSelectionCancelled
		}
		// `picked` evita shadow del parámetro `out` (writer del wizard).
		picked := make([]string, 0, len(agents))
		for i, a := range fm.agents {
			if fm.checked[i] {
				picked = append(picked, a)
			}
		}
		return picked, nil
	}
}

// selectorModel es el modelo bubbletea del wizard multi-check.
//
// Diseño minimalista: una columna de checkboxes [x]/[ ] + nombre del
// agente, con cursor (▸) y header con el nombre del step. Sin
// dependencia del package internal/tui (que es el TUI completo de che)
// para que el runner sea standalone.
type selectorModel struct {
	stepName  string
	agents    []string
	checked   []bool
	cursor    int
	cancelled bool
	done      bool
}

// Init satisface tea.Model. No tenemos comandos iniciales (el wizard
// es 100% reactivo a teclas).
func (m selectorModel) Init() tea.Cmd { return nil }

// Update implementa el handler de teclas:
//   - up/k, down/j: mueve el cursor.
//   - space: toggle el checkbox actual.
//   - a: marca todos.
//   - n: desmarca todos.
//   - enter: confirma y cierra (rechaza si 0 marcados).
//   - esc, ctrl+c, q: cancela (sale con ErrSelectionCancelled).
func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.agents)-1 {
				m.cursor++
			}
		case " ":
			m.checked[m.cursor] = !m.checked[m.cursor]
		case "a":
			for i := range m.checked {
				m.checked[i] = true
			}
		case "n":
			for i := range m.checked {
				m.checked[i] = false
			}
		case "enter":
			// Rechazo silencioso si 0 marcados — el header del view
			// muestra el contador para feedback. El usuario tiene que
			// marcar al menos uno o cancelar.
			any := false
			for _, c := range m.checked {
				if c {
					any = true
					break
				}
			}
			if !any {
				return m, nil
			}
			m.done = true
			return m, tea.Quit
		case "esc", "ctrl+c", "q":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

var (
	selectorTitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#00D7FF")).
				Bold(true)
	selectorHintStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#6272A4")).
				Italic(true)
	selectorCursorStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FF79C6")).
				Bold(true)
	selectorCheckedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#50FA7B"))
)

// View rendea el wizard. Layout:
//
//	step "explore" — elegí los agentes a correr
//	  ▸ [x] claude-opus
//	    [ ] plan-reviewer-strict
//
//	espacio toggle · a todos · n ninguno · enter confirma · esc cancela · marcados: 1/2
func (m selectorModel) View() string {
	var sb strings.Builder
	sb.WriteString(selectorTitleStyle.Render(fmt.Sprintf("step %q — elegí los agentes a correr", m.stepName)))
	sb.WriteString("\n\n")
	for i, a := range m.agents {
		cursor := "  "
		if i == m.cursor {
			cursor = selectorCursorStyle.Render("▸ ")
		}
		box := "[ ]"
		if m.checked[i] {
			box = selectorCheckedStyle.Render("[x]")
		}
		sb.WriteString(cursor + box + " " + a + "\n")
	}
	count := 0
	for _, c := range m.checked {
		if c {
			count++
		}
	}
	sb.WriteString("\n")
	sb.WriteString(selectorHintStyle.Render(fmt.Sprintf(
		"espacio toggle · a todos · n ninguno · enter confirma · esc cancela · marcados: %d/%d",
		count, len(m.agents))))
	sb.WriteString("\n")
	return sb.String()
}
