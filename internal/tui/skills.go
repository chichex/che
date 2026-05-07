// skills.go renderiza la pantalla "See skills" del menu principal: drilldown
// de tres niveles (CLIs -> skills del CLI -> detalle del skill). Es solo
// lectura — no invoca skills, no escribe archivos. La deteccion vive en
// internal/skills.
package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/skills"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type skillsLevel int

const (
	levelCLIs skillsLevel = iota
	levelSkills
	levelDetail
)

type skillsModel struct {
	clis     []skills.CLI
	level    skillsLevel
	cursor   int
	cliIdx   int // CLI activo cuando level >= levelSkills
	width    int // ancho del terminal; lo refrescamos via tea.WindowSizeMsg
	status   string
	statusOK bool // tinte del status: true=ok, false=error
	exitApp  bool // true => el caller no debe re-mostrar el menu principal
}

func (m skillsModel) Init() tea.Cmd { return nil }

func (m skillsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "q":
		// Exit total: cuando el caller vea exitApp=true, no re-muestra el menu
		// principal. Para "volver al menu" alcanza con esc.
		m.exitApp = true
		return m, tea.Quit
	case "esc", "left", "h", "backspace":
		if m.level == levelCLIs {
			// Esc en el nivel raiz no tiene "atras" dentro de skills —
			// salimos del program y dejamos que el caller re-muestre el menu.
			return m, tea.Quit
		}
		return m.back().clearStatus(), nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m.clearStatus(), nil
	case "down", "j":
		if max := m.maxCursor(); m.cursor < max {
			m.cursor++
		}
		return m.clearStatus(), nil
	case "enter", " ", "right", "l":
		return m.forward().clearStatus(), nil
	case "o":
		return m.openCurrent(), nil
	}
	return m, nil
}

// clearStatus borra el ultimo feedback de "open" cuando el usuario navega.
// Sin esto, el status quedaria pegado pidiendo al lector preguntarse de
// que skill venia.
func (m skillsModel) clearStatus() skillsModel {
	m.status = ""
	m.statusOK = false
	return m
}

// openCurrent dispara `code <source>` para el skill apuntado por el cursor.
// Solo aplica en levelSkills y levelDetail (en levelCLIs no hay un skill
// elegido). Si `code` no esta en el PATH, dejamos un mensaje en status para
// que el usuario instale o configure el shell command.
func (m skillsModel) openCurrent() skillsModel {
	if m.level == levelCLIs {
		return m
	}
	skills := m.clis[m.cliIdx].Skills
	if len(skills) == 0 || m.cursor >= len(skills) {
		return m
	}
	source := skills[m.cursor].Source
	if _, err := exec.LookPath("code"); err != nil {
		m.status = "VS Code not found in PATH (open VS Code → Cmd+Shift+P → \"Shell Command: Install 'code' command\")"
		m.statusOK = false
		return m
	}
	// Start (no Run) deja el proceso despegado del TUI: code es un wrapper
	// que abre la GUI y vuelve enseguida, asi que no bloquea ni roba TTY.
	if err := exec.Command("code", source).Start(); err != nil {
		m.status = "failed to launch VS Code: " + err.Error()
		m.statusOK = false
		return m
	}
	m.status = "opened in VS Code: " + source
	m.statusOK = true
	return m
}

// maxCursor devuelve el indice valido mas alto del nivel actual. En
// levelDetail compartimos bounds con levelSkills — asi up/down browsea
// entre skills sin tener que volver al listado.
func (m skillsModel) maxCursor() int {
	switch m.level {
	case levelCLIs:
		return len(m.clis) - 1
	case levelSkills, levelDetail:
		return len(m.clis[m.cliIdx].Skills) - 1
	}
	return 0
}

func (m skillsModel) forward() skillsModel {
	switch m.level {
	case levelCLIs:
		// Solo entramos si el CLI esta instalado y tiene al menos un skill —
		// si no, no hay nada que mostrar y un nivel vacio confunde.
		c := m.clis[m.cursor]
		if !c.Installed || len(c.Skills) == 0 {
			return m
		}
		m.cliIdx = m.cursor
		m.level = levelSkills
		m.cursor = 0
	case levelSkills:
		if len(m.clis[m.cliIdx].Skills) == 0 {
			return m
		}
		m.level = levelDetail
	}
	return m
}

func (m skillsModel) back() skillsModel {
	switch m.level {
	case levelDetail:
		m.level = levelSkills
	case levelSkills:
		m.level = levelCLIs
		m.cursor = m.cliIdx
		m.cliIdx = 0
	}
	return m
}

var (
	skillsTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D7FF"))
	skillsCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Bold(true)
	skillsItemStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2"))
	skillsMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	skillsHintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
	skillsBadgeOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("#50FA7B"))
	skillsBadgeMiss   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555"))
	skillsBadgeUser   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD"))
	skillsBadgeProj   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	skillsHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F8F8F2"))
)

func (m skillsModel) View() string {
	switch m.level {
	case levelCLIs:
		return m.viewCLIs()
	case levelSkills:
		return m.viewSkills()
	case levelDetail:
		return m.viewDetail()
	}
	return ""
}

func (m skillsModel) viewCLIs() string {
	var b strings.Builder
	b.WriteString(skillsTitleStyle.Render("Skills") + "\n")
	b.WriteString(skillsMutedStyle.Render("Skills detected across the CLIs che orchestrates.") + "\n\n")

	for i, c := range m.clis {
		var status string
		if !c.Installed {
			status = skillsBadgeMiss.Render("not installed")
		} else {
			status = skillsBadgeOK.Render(fmt.Sprintf("installed · %d skills", len(c.Skills)))
		}
		row := fmt.Sprintf("%-10s %s", c.Name, status)
		if i == m.cursor {
			b.WriteString(skillsCursorStyle.Render("> "+row) + "\n")
		} else {
			b.WriteString("  " + skillsItemStyle.Render(row) + "\n")
		}
	}

	b.WriteString("\n" + skillsHintStyle.Render("↑/↓ navigate · enter open · esc back · q quit") + "\n")
	return b.String()
}

func (m skillsModel) viewSkills() string {
	c := m.clis[m.cliIdx]
	var b strings.Builder
	b.WriteString(skillsTitleStyle.Render("Skills · "+c.Name) + "\n")
	b.WriteString(skillsMutedStyle.Render(c.BinPath) + "\n\n")

	if len(c.Skills) == 0 {
		b.WriteString(skillsMutedStyle.Render("(no skills found)") + "\n")
	}
	// Reservamos ~40 cols para gutter+badge+nombre antes de la descripcion;
	// el resto va para desc. En terminales muy angostos omitimos desc en vez
	// de truncar a algo ininteligible.
	descBudget := m.width - 40
	for i, s := range c.Skills {
		badge := scopeBadge(s.Scope)
		row := fmt.Sprintf("%s  %-26s", badge, s.Name)
		if descBudget > 10 && s.Description != "" {
			row += " " + skillsMutedStyle.Render(truncate(s.Description, descBudget))
		}
		if i == m.cursor {
			b.WriteString(skillsCursorStyle.Render("> ") + row + "\n")
		} else {
			b.WriteString("  " + row + "\n")
		}
	}

	b.WriteString(m.renderStatus())
	b.WriteString("\n" + skillsHintStyle.Render("↑/↓ navigate · enter detail · o open in VS Code · esc back · q quit") + "\n")
	return b.String()
}

func (m skillsModel) viewDetail() string {
	c := m.clis[m.cliIdx]
	s := c.Skills[m.cursor]
	var b strings.Builder
	b.WriteString(skillsTitleStyle.Render("Skill · "+c.Name+" · "+s.Name) + "\n\n")
	b.WriteString(skillsHeaderStyle.Render("Scope") + "        " + scopeBadge(s.Scope) + "\n")
	b.WriteString(skillsHeaderStyle.Render("Source") + "       " + skillsMutedStyle.Render(s.Source) + "\n\n")
	if s.Description != "" {
		b.WriteString(skillsHeaderStyle.Render("Description") + "\n")
		b.WriteString(wrapStyle(m.width).Render(s.Description) + "\n")
	} else {
		b.WriteString(skillsMutedStyle.Render("(no description)") + "\n")
	}
	b.WriteString(m.renderStatus())
	b.WriteString("\n" + skillsHintStyle.Render("↑/↓ browse · o open in VS Code · esc back · q quit") + "\n")
	return b.String()
}

// renderStatus emite la linea de feedback de la ultima accion (o vacio si
// no hay nada que reportar). Se ubica entre el contenido y el hint para
// que sea visible sin pisar el listado.
func (m skillsModel) renderStatus() string {
	if m.status == "" {
		return ""
	}
	style := skillsBadgeMiss
	if m.statusOK {
		style = skillsBadgeOK
	}
	return "\n" + style.Render(m.status) + "\n"
}

// wrapStyle devuelve un style con Width seteado al ancho del terminal (con
// piso razonable y un colchon para que lipgloss no corte por el borde).
// Lipgloss hace word-wrap automatico cuando Width esta seteado.
func wrapStyle(width int) lipgloss.Style {
	w := width - 2
	if w < 40 {
		w = 40
	}
	return skillsItemStyle.Width(w)
}

func scopeBadge(scope skills.Scope) string {
	if scope == skills.ScopeProject {
		return skillsBadgeProj.Render("[project]")
	}
	return skillsBadgeUser.Render("[user]   ")
}

// truncate corta a n runas (no bytes) y agrega ellipsis. Si la cadena ya
// entra, la devuelve tal cual.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// RunSkills levanta la pantalla interactiva de skills. cwd se pasa al
// detector para resolver el scope "project". Devuelve exitApp=true si el
// usuario pidio salir totalmente (q / ctrl+c) — false significa que solo
// volvio "atras" (esc en el nivel raiz) y el caller deberia re-mostrar el
// menu principal. El error solo aparece si bubbletea no pudo arrancar.
func RunSkills(cwd string) (bool, error) {
	clis := skills.Detect(cwd)
	final, err := tea.NewProgram(skillsModel{clis: clis}).Run()
	if err != nil {
		return false, err
	}
	m, ok := final.(skillsModel)
	if !ok {
		return true, nil
	}
	return m.exitApp, nil
}
