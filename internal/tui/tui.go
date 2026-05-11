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
	ActionMyPipelines    Action = "my-pipelines"
	ActionCreatePipeline Action = "create-pipeline"
	ActionAIGen          Action = "ai-gen"
	ActionSeeSkills      Action = "see-skills"
	ActionExit           Action = "exit"
)

type item struct {
	digit  string
	label  string
	action Action
}

var menu = []item{
	{digit: "1", label: "My pipelines", action: ActionMyPipelines},
	{digit: "2", label: "Create pipeline", action: ActionCreatePipeline},
	{digit: "3", label: "Crear pipeline con IA", action: ActionAIGen},
	{digit: "4", label: "See skills", action: ActionSeeSkills},
	{digit: "0", label: "Exit", action: ActionExit},
}

type model struct {
	cursor   int
	selected *item
	version  string
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
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
)

// breadcrumb arma el header "che › ... › <pantalla>" para el View() de
// cada screen del paquete tui (home menu + see skills). El ultimo
// segmento se renderea en titleStyle (cyan bold dracula); los previos +
// separadores van en dimStyle (gris dracula) para que el usuario perciba
// "donde esta" sin perder el resto del path. parts NO incluye el root
// "che" — lo prependeamos aca.
func breadcrumb(parts ...string) string {
	all := append([]string{"che"}, parts...)
	if len(all) == 1 {
		return titleStyle.Render(all[0])
	}
	sep := dimStyle.Render(" › ")
	prefix := make([]string, 0, len(all)-1)
	for _, p := range all[:len(all)-1] {
		prefix = append(prefix, dimStyle.Render(p))
	}
	return strings.Join(prefix, sep) + sep + titleStyle.Render(all[len(all)-1])
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(breadcrumb() + "\n\n")
	for i, it := range menu {
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("> "+it.digit+". "+it.label) + "\n")
			continue
		}
		b.WriteString("  " + digitStyle.Render(it.digit+".") + " " + itemStyle.Render(it.label) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("↑/↓ navigate · enter select · 0-4 jump · q quit") + "\n")
	b.WriteString(dimStyle.Render("v"+m.version) + "\n")
	return b.String()
}

// Run levanta el menu interactivo. Devuelve la Action elegida o ActionExit
// si el usuario salio (ctrl+c, q, esc, o item 0). El error solo aparece si
// bubbletea no pudo arrancar (p.ej. stdout no es TTY). version se renderiza
// como linea dim al pie del menu para que el usuario sepa que build esta
// corriendo; el caller la pasa desde cmd.Version (inyectada via ldflag).
func Run(version string) (Action, error) {
	final, err := tea.NewProgram(model{version: version}, tea.WithAltScreen()).Run()
	if err != nil {
		return ActionExit, err
	}
	m, ok := final.(model)
	if !ok || m.selected == nil {
		return ActionExit, nil
	}
	return m.selected.action, nil
}
