package tui

import "github.com/charmbracelet/lipgloss"

// Paleta Dracula-esque. Probada en dark/light.
var (
	colorPrimary = lipgloss.Color("#00D7FF") // cyan brillante — selección, títulos
	colorAccent  = lipgloss.Color("#FF79C6") // magenta — labels, números
	colorSuccess = lipgloss.Color("#50FA7B") // verde — OK, creado
	colorError   = lipgloss.Color("#FF5555") // rojo — errores
	colorMuted   = lipgloss.Color("#6272A4") // gris — hints, separadores
	colorText    = lipgloss.Color("#F8F8F2") // off-white — texto normal
)

var (
	titleStyle = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true).
			Padding(1, 2, 0, 2)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Padding(0, 2, 1, 2)

	contextLineStyle = lipgloss.NewStyle().
				Padding(0, 2, 1, 2)

	menuItemStyle = lipgloss.NewStyle().
			Padding(0, 2).
			Foreground(colorText)

	menuSelectedStyle = lipgloss.NewStyle().
				Padding(0, 2).
				Foreground(colorPrimary).
				Bold(true)

	menuNumberStyle = lipgloss.NewStyle().
			Foreground(colorAccent).
			Bold(true)

	hintStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Padding(1, 2, 0, 2).
			Italic(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Bold(true)

	successStyle = lipgloss.NewStyle().
			Foreground(colorSuccess)

	comingSoonStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)

	suggestedBadgeStyle = lipgloss.NewStyle().
				Foreground(colorSuccess).
				Italic(true)

	logLineStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	textareaBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorPrimary).
			Padding(0, 1).
			Margin(0, 2)
)

// Inline badges para el header del menú. Cada "chip" con color propio.
func primaryBadge(s string) string {
	return lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(s)
}

func accentBadge(s string) string {
	return lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render(s)
}

func mutedBadge(s string) string {
	return lipgloss.NewStyle().Foreground(colorMuted).Render(s)
}

// runSubjectStyle es el padding lateral para la línea de contexto del
// header de flows en ejecución (queda pegada al título, sin gap).
var runSubjectStyle = lipgloss.NewStyle().Padding(0, 2)

// renderRunSubject arma la línea "#N — título" para el header de flows
// en ejecución. Coloreada inline (accent para el #N, text para el título)
// y truncada para no romper el layout en títulos largos.
func renderRunSubject(ref, title string) string {
	if ref == "" {
		return ""
	}
	label := accentBadge("#" + ref)
	if title == "" {
		return runSubjectStyle.Render(label)
	}
	sep := mutedBadge(" — ")
	body := lipgloss.NewStyle().Foreground(colorText).Render(truncateRunes(title, 70))
	return runSubjectStyle.Render(label + sep + body)
}

// truncateRunes corta s a max runas, agregando … si fue truncado.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
