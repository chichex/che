// Package wizard renderiza el flujo "Create pipeline" del menu principal.
// H1 es solo el skeleton: una pantalla placeholder para validar que el
// routing desde el menu funciona. La logica real (S1 PipelineInfo, persist
// a ~/.che/pipelines, etc.) llega en H2+.
package wizard

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type model struct {
	exitApp bool
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		return m, tea.Quit
	}
	return m, nil
}

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D7FF"))
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	hintStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
)

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Create pipeline") + "\n\n")
	b.WriteString(pendingStyle.Render("wizard pendiente") + "\n")
	b.WriteString("\n" + hintStyle.Render("esc back · q quit") + "\n")
	return b.String()
}

// Run levanta el skeleton del wizard. Devuelve exitApp=true si el usuario
// pidio salida total (q / ctrl+c); false si solo volvio "atras" (esc) y el
// caller deberia re-mostrar el menu principal. El error solo aparece si
// bubbletea no pudo arrancar (p.ej. stdout no es TTY).
func Run() (bool, error) {
	final, err := tea.NewProgram(model{}).Run()
	if err != nil {
		return false, err
	}
	m, ok := final.(model)
	if !ok {
		return true, nil
	}
	return m.exitApp, nil
}
