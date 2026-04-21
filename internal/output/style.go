package output

import "github.com/charmbracelet/lipgloss"

// Paleta Dracula.
//
// KEEP IN SYNC with internal/tui/styles.go. Se duplica aqui adrede para
// evitar ciclo de imports (tui ya importa flow, y flow va a importar
// internal/output).
var (
	colorPrimary = lipgloss.Color("#00D7FF") // cyan brillante
	colorAccent  = lipgloss.Color("#FF79C6") // magenta — labels, numeros
	colorSuccess = lipgloss.Color("#50FA7B") // verde — OK
	colorError   = lipgloss.Color("#FF5555") // rojo — errores
	colorMuted   = lipgloss.Color("#6272A4") // gris — hints, separadores
	colorText    = lipgloss.Color("#F8F8F2") // off-white — texto normal
	colorWarn    = lipgloss.Color("#F1FA8C") // amarillo Dracula — warnings
)

// Simbolos por Level. Matchean la convencion preexistente de doctor.go.
const (
	symInfo    = "▸"
	symStep    = "·"
	symSuccess = "✓"
	symWarn    = "⚠"
	symError   = "✗"
)

// Estilos lipgloss por componente. Se inicializan sin renderer asociado;
// el writerSink los aplica con su propio renderer (que respeta TTY y
// NO_COLOR).
var (
	styleInfo    = lipgloss.NewStyle().Foreground(colorText)
	styleStep    = lipgloss.NewStyle().Foreground(colorMuted)
	styleSuccess = lipgloss.NewStyle().Foreground(colorSuccess)
	styleWarn    = lipgloss.NewStyle().Foreground(colorWarn)
	styleError   = lipgloss.NewStyle().Foreground(colorError).Bold(true)

	styleNumber    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleLabels    = lipgloss.NewStyle().Foreground(colorAccent)
	styleURL       = lipgloss.NewStyle().Foreground(colorMuted).Underline(true)
	styleMuted     = lipgloss.NewStyle().Foreground(colorMuted)
	styleAgent     = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	styleVerdictOK = lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	styleVerdictKO = lipgloss.NewStyle().Foreground(colorError).Bold(true)
)

// symbolFor devuelve el simbolo del Level.
func symbolFor(lv Level) string {
	switch lv {
	case LevelInfo:
		return symInfo
	case LevelStep:
		return symStep
	case LevelSuccess:
		return symSuccess
	case LevelWarn:
		return symWarn
	case LevelError:
		return symError
	}
	return symInfo
}

// styleForLevel devuelve el estilo del texto base segun Level.
func styleForLevel(lv Level) lipgloss.Style {
	switch lv {
	case LevelInfo:
		return styleInfo
	case LevelStep:
		return styleStep
	case LevelSuccess:
		return styleSuccess
	case LevelWarn:
		return styleWarn
	case LevelError:
		return styleError
	}
	return styleInfo
}
