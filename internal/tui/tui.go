// Package tui renderiza el menu principal de che cuando se invoca sin
// subcomando. Es una TUI bubbletea minima: lista las acciones disponibles,
// captura una seleccion (flechas + enter, o digito directo), y devuelve la
// Action elegida al caller. La ejecucion de cada accion vive afuera —
// este paquete solo elige.
package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Action identifica una entrada del menu. El caller (cmd/root) la consume
// para decidir que rutina invocar.
type Action string

const (
	ActionRunPipelines Action = "run-pipelines"
	ActionSeePipelines Action = "see-pipelines"
	ActionSeeSkills    Action = "see-skills"
	ActionExit         Action = "exit"
)

type item struct {
	digit  string
	label  string
	action Action
}

var menu = []item{
	{digit: "1", label: "Run pipelines", action: ActionRunPipelines},
	{digit: "2", label: "See pipelines", action: ActionSeePipelines},
	{digit: "3", label: "See skills", action: ActionSeeSkills},
	{digit: "0", label: "Exit", action: ActionExit},
}

type model struct {
	cursor   int
	selected *item
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "q", "esc":
		exit := menu[len(menu)-1]
		m.selected = &exit
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(menu)-1 {
			m.cursor++
		}
	case "enter", " ":
		sel := menu[m.cursor]
		m.selected = &sel
		return m, tea.Quit
	default:
		for i, it := range menu {
			if it.digit == key.String() {
				m.cursor = i
				sel := menu[i]
				m.selected = &sel
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D7FF"))
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Bold(true)
	itemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2"))
	digitStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
)

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("che") + "\n\n")
	for i, it := range menu {
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("> "+it.digit+". "+it.label) + "\n")
			continue
		}
		b.WriteString("  " + digitStyle.Render(it.digit+".") + " " + itemStyle.Render(it.label) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("↑/↓ navigate · enter select · 0-3 jump · q quit") + "\n")
	return b.String()
}

// Run levanta el menu interactivo. Devuelve la Action elegida o ActionExit
// si el usuario salio (ctrl+c, q, esc, o item 0). El error solo aparece si
// bubbletea no pudo arrancar (p.ej. stdout no es TTY).
func Run() (Action, error) {
	final, err := tea.NewProgram(model{}).Run()
	if err != nil {
		return ActionExit, err
	}
	m, ok := final.(model)
	if !ok || m.selected == nil {
		return ActionExit, nil
	}
	return m.selected.action, nil
}
