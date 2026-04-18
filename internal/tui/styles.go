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
			Italic(true).
			Padding(1, 2)

	textareaBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorPrimary).
			Padding(0, 1).
			Margin(0, 2)

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(1, 2).
			Margin(1, 2)
)
