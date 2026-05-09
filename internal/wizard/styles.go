package wizard

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// wrapText envuelve cada linea logica de s a un ancho maximo de w columnas
// (chars). Prefiere romper en espacios; si una palabra es mas larga que w
// hace hard break por char. Newlines existentes se preservan — cada linea
// logica se wrappea independiente. w <= 0 devuelve s sin tocar (uso para
// "todavia no se que ancho tiene la terminal").
func wrapText(s string, w int) string {
	if w <= 0 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapLine(line, w)...)
	}
	return strings.Join(out, "\n")
}

func wrapLine(line string, w int) []string {
	runes := []rune(line)
	if len(runes) <= w {
		return []string{line}
	}
	var lines []string
	for len(runes) > w {
		breakAt := -1
		for i := w; i > 0; i-- {
			if runes[i-1] == ' ' {
				breakAt = i - 1
				break
			}
		}
		if breakAt <= 0 {
			lines = append(lines, string(runes[:w]))
			runes = runes[w:]
			continue
		}
		lines = append(lines, strings.TrimRight(string(runes[:breakAt]), " "))
		runes = runes[breakAt+1:]
	}
	if len(runes) > 0 {
		lines = append(lines, string(runes))
	}
	return lines
}

// breadcrumb arma el header "che › ... › <pantalla>" para el View() de cada
// screen del wizard. El ultimo segmento se renderea en titleStyle (cyan
// bold dracula) — la pantalla actual destaca; los segmentos previos +
// separadores van en dimStyle (gris dracula) para que el ojo aterrice en
// el ultimo. parts NO debe incluir el root "che" — lo prependeamos aca
// para que toda la TUI hable la misma jerarquia sin que cada screen tenga
// que recordarlo.
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

// Paleta dracula-ish, alineada con internal/tui. La duplicamos para
// evitar import circular cuando wizard quiera estilar errores propios sin
// pasar por tui.
var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D7FF"))
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2")).Bold(true)
	hintStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555")).Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	selectedItem = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Bold(true)
	// selectedOff es la pill "seleccionada pero la fila no tiene foco". Sin
	// este tono dedicado, mutedItem (#F8F8F2) se confunde con el texto
	// normal y solo los corchetes distinguen visualmente la pill — bajo
	// contraste cuando la fila vecina (no enfocada tampoco) tiene pills
	// blancas tambien. Purple dracula (#BD93F9) le da peso sin competir
	// con el pink de "en foco".
	selectedOff = lipgloss.NewStyle().Foreground(lipgloss.Color("#BD93F9")).Bold(true)
	mutedItem   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2"))
	modalBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#BD93F9")).Padding(1, 2)

	// Cajas de input: borde redondo siempre, color fuerte cuando tiene
	// foco. Asi se ve "esto es un input vacio" vs "esto es texto".
	inputBoxBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#44475A")).
			Foreground(lipgloss.Color("#F8F8F2")).
			Padding(0, 1)
	inputBoxBorderFocus = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#FF79C6")).
				Foreground(lipgloss.Color("#F8F8F2")).
				Padding(0, 1)

	// validatorBoxStyle envuelve el bloque del validator (B2) cuando esta
	// on. El border-left + indent sirven de senal visual fuerte de "esto
	// es opcional / sub-bloque del step", asi el usuario no confunde
	// los CLI/Kind del validator con los del step principal.
	validatorBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), false, false, false, true).
				BorderForeground(lipgloss.Color("#BD93F9")).
				PaddingLeft(2)
)

// cursorBlock es el caracter usado como cursor del textInput. Bloquito
// solido — visible sobre cualquier fondo y compatible con la mayoria de
// terminales monospace.
const cursorBlock = "▎"

var placeholderStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#6272A4")).
	Italic(true).
	Faint(true)

func placeholderText(s string) string {
	return placeholderStyle.Render(s)
}
