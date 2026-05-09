// aigen.go renderea la pantalla "Crear pipeline con IA": pide nombre +
// descripcion al usuario, arma un prompt con el schema de che + reglas de
// "prompt sano" (internal/aiprompt), lo copia al portapapeles via pbcopy /
// xclip / wl-copy / clip.exe (internal/clipboard), y lo muestra en
// pantalla con scroll para que el usuario pueda releerlo o copiarlo
// manualmente si el clipboard del sistema no estuvo disponible.
//
// Decisiones:
//   - Es un programa bubbletea separado (mismo patron que skills.go) — el
//     caller (cmd/root) lo invoca tras seleccionar la opcion de menu y
//     vuelve a re-mostrar el menu al cerrar.
//   - tea.WithAltScreen() en el caller — congruente con el resto del TUI
//     post-fix de breadcrumbs.
//   - El prompt se copia AUTOMATICAMENTE al entrar a la pantalla de
//     output (tras ctrl+s en la de inputs). Si el clipboard falla, lo
//     surfaceamos en un toast pero NO bloqueamos: el texto sigue visible
//     para copia manual.
package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/chichex/che/internal/aiprompt"
	"github.com/chichex/che/internal/clipboard"
)

// aigenScreen identifica el sub-screen activo dentro del program. Empezamos
// en aigenInput (los 2 campos), pasamos a aigenOutput tras ctrl+s.
type aigenScreen int

const (
	aigenInput aigenScreen = iota
	aigenOutput
)

// aigenFocus indica que campo de aigenInput tiene el foco.
type aigenFocus int

const (
	aigenFocusName aigenFocus = iota
	aigenFocusDescription
)

type aigenModel struct {
	screen aigenScreen
	focus  aigenFocus

	nameBuf textBuffer
	descBuf textBuffer

	prompt        string // el prompt generado, listo para copiar
	clipboardErr  string // "" = exito; mensaje = clipboard no disponible
	clipboardSent bool   // true tras la primera copia (mostrar toast verde)

	scrollOffset int // scroll del bloque del prompt en la output screen
	width        int
	height       int

	exitApp bool // true = ctrl+c desde cualquier screen
}

// RunAIGen levanta el program y devuelve true si el usuario pidio salida
// total (ctrl+c). esc / enter en aigenOutput vuelven al menu (false).
func RunAIGen() (bool, error) {
	m := aigenModel{
		screen:  aigenInput,
		focus:   aigenFocusName,
		nameBuf: textBuffer{multiline: false},
		descBuf: textBuffer{multiline: true},
	}
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return false, err
	}
	mm, ok := final.(aigenModel)
	if !ok {
		return true, nil
	}
	return mm.exitApp, nil
}

func (m aigenModel) Init() tea.Cmd { return nil }

func (m aigenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		m.height = ws.Height
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch m.screen {
	case aigenInput:
		return m.updateInput(key)
	case aigenOutput:
		return m.updateOutput(key)
	}
	return m, nil
}

func (m aigenModel) updateInput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		// Sin path en disco — esc cierra y vuelve al menu (no creamos draft).
		return m, tea.Quit
	case "tab", "down":
		if m.focus == aigenFocusName {
			m.focus = aigenFocusDescription
		}
		return m, nil
	case "shift+tab", "up":
		if m.focus == aigenFocusDescription {
			m.focus = aigenFocusName
		}
		return m, nil
	case "enter":
		// enter en Nombre avanza a Descripcion. enter en Descripcion confirma.
		if m.focus == aigenFocusName {
			m.focus = aigenFocusDescription
			return m, nil
		}
		return m.confirmInputs()
	case "ctrl+s":
		return m.confirmInputs()
	case "shift+enter", "alt+enter", "ctrl+j":
		// newline literal en descripcion (mismo patron que wizard).
		if m.focus == aigenFocusDescription {
			m.descBuf.handleKey("shift+enter", []rune{'\n'})
			return m, nil
		}
		return m, nil
	}
	if m.focus == aigenFocusName {
		m.nameBuf.handleKey(key.String(), key.Runes)
	} else {
		m.descBuf.handleKey(key.String(), key.Runes)
	}
	return m, nil
}

// confirmInputs valida que ambos campos tengan algo, arma el prompt y
// transiciona a aigenOutput intentando copiar al portapapeles.
func (m aigenModel) confirmInputs() (tea.Model, tea.Cmd) {
	if strings.TrimSpace(m.nameBuf.value()) == "" {
		// Sin nombre no avanzamos — el helper igual lo manejaria, pero
		// preferimos no generar un prompt con "<sin nombre>" cuando el
		// usuario solo se olvido de tipear.
		m.focus = aigenFocusName
		return m, nil
	}
	m.prompt = aiprompt.Build(m.nameBuf.value(), m.descBuf.value())
	m.screen = aigenOutput
	m.scrollOffset = 0
	if err := clipboard.Copy(m.prompt); err != nil {
		m.clipboardErr = err.Error()
		m.clipboardSent = false
	} else {
		m.clipboardErr = ""
		m.clipboardSent = true
	}
	return m, nil
}

func (m aigenModel) updateOutput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.exitApp = true
		return m, tea.Quit
	case "esc", "enter", "q":
		return m, tea.Quit
	case "y":
		// Re-copiar (por si el usuario cambio de portapapeles entre apps).
		if err := clipboard.Copy(m.prompt); err != nil {
			m.clipboardErr = err.Error()
			m.clipboardSent = false
		} else {
			m.clipboardErr = ""
			m.clipboardSent = true
		}
		return m, nil
	case "up", "k":
		if m.scrollOffset > 0 {
			m.scrollOffset--
		}
		return m, nil
	case "down", "j":
		m.scrollOffset++
		return m, nil
	case "g":
		m.scrollOffset = 0
		return m, nil
	}
	return m, nil
}

func (m aigenModel) View() string {
	switch m.screen {
	case aigenInput:
		return m.viewInput()
	case aigenOutput:
		return m.viewOutput()
	}
	return ""
}

func (m aigenModel) viewInput() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Crear pipeline con IA"))
	b.WriteString("\n\n")
	b.WriteString(aigenDimStyle.Render("Decinos nombre + objetivo. Te devolvemos un prompt sanitizado para pegar en tu cliente de IA."))
	b.WriteString("\n\n")

	b.WriteString(aigenLabelStyle.Render("Nombre"))
	if m.focus == aigenFocusName {
		b.WriteString(aigenDimStyle.Render("  ← foco"))
	}
	b.WriteString("\n")
	b.WriteString(aigenInputBox(m.focus == aigenFocusName).Render(m.nameBuf.viewInline(m.focus == aigenFocusName, "ej: triage-flow")))
	b.WriteString("\n\n")

	b.WriteString(aigenLabelStyle.Render("Descripcion"))
	if m.focus == aigenFocusDescription {
		b.WriteString(aigenDimStyle.Render("  ← foco · shift+enter / alt+enter para newline"))
	}
	b.WriteString("\n")
	b.WriteString(aigenInputBox(m.focus == aigenFocusDescription).Render(m.descBuf.viewInline(m.focus == aigenFocusDescription, "ej: dispara un triage cuando llega una metrica anomala")))
	b.WriteString("\n\n")

	b.WriteString(aigenHintStyle.Render("enter siguiente · ctrl+s generar · esc volver al menu · ctrl+c salir"))
	b.WriteString("\n")
	return b.String()
}

func (m aigenModel) viewOutput() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Crear pipeline con IA", "Prompt generado"))
	b.WriteString("\n")
	if m.clipboardSent {
		b.WriteString(aigenOKStyle.Render("✓ copiado al portapapeles"))
	} else if m.clipboardErr != "" {
		b.WriteString(aigenWarnStyle.Render("⚠ no se pudo copiar al portapapeles (" + m.clipboardErr + ") — copialo manualmente abajo"))
	}
	b.WriteString("\n\n")

	b.WriteString(aigenDimStyle.Render("Pegalo en tu cliente de IA preferido (claude.ai, ChatGPT, etc) y guardá el YAML que te devuelva en ~/.che/pipelines/<slug>.yaml"))
	b.WriteString("\n\n")

	b.WriteString(aigenRenderScroll(m.prompt, m.scrollOffset, m.height))
	b.WriteString("\n")
	b.WriteString(aigenHintStyle.Render("↑/↓ scroll · g top · y copiar otra vez · esc/enter volver al menu · ctrl+c salir"))
	b.WriteString("\n")
	return b.String()
}

// aigenRenderScroll muestra slice del prompt segun scrollOffset acotado al
// alto del terminal — descontamos chrome (breadcrumb + toast + footer ~6
// lineas). Si terminalHeight es 0 (todavia no llego WindowSizeMsg) cae a
// 24 lineas, valor razonable para no devolver vacio.
func aigenRenderScroll(text string, offset, terminalHeight int) string {
	lines := strings.Split(text, "\n")
	visible := terminalHeight - 8
	if visible <= 0 {
		visible = 24
	}
	start := offset
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		start = len(lines) - 1
	}
	end := start + visible
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

// Estilos locales del aigen — duplicamos paleta dracula con prefijo aigen
// para no chocar con los nombres globales de tui.go.
var (
	aigenLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2")).Bold(true)
	aigenDimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	aigenHintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
	aigenOKStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#50FA7B")).Bold(true)
	aigenWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF")).Bold(true)
)

func aigenInputBox(focused bool) lipgloss.Style {
	color := lipgloss.Color("#44475A")
	if focused {
		color = lipgloss.Color("#FF79C6")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Foreground(lipgloss.Color("#F8F8F2")).
		Padding(0, 1)
}

// textBuffer es un input minimal (mismo shape que el del runner pero local
// al paquete tui — evita import cruzado y mantiene el package self-contained).
type textBuffer struct {
	runes     []rune
	cursor    int
	multiline bool
}

func (t textBuffer) value() string { return string(t.runes) }

func (t *textBuffer) handleKey(key string, runesIn []rune) {
	switch key {
	case "left":
		if t.cursor > 0 {
			t.cursor--
		}
		return
	case "right":
		if t.cursor < len(t.runes) {
			t.cursor++
		}
		return
	case "home", "ctrl+a":
		t.cursor = 0
		return
	case "end", "ctrl+e":
		t.cursor = len(t.runes)
		return
	case "backspace", "ctrl+h":
		if t.cursor > 0 {
			t.runes = append(t.runes[:t.cursor-1], t.runes[t.cursor:]...)
			t.cursor--
		}
		return
	case "delete":
		if t.cursor < len(t.runes) {
			t.runes = append(t.runes[:t.cursor], t.runes[t.cursor+1:]...)
		}
		return
	case "shift+enter", "alt+enter", "ctrl+j":
		if t.multiline {
			t.insertRune('\n')
		}
		return
	case "tab", "shift+tab", "esc", "ctrl+c", "ctrl+s", "enter", "up", "down":
		return
	}
	if len(runesIn) == 0 {
		return
	}
	for _, r := range runesIn {
		if !t.multiline && (r == '\n' || r == '\r') {
			continue
		}
		if r < 0x20 && r != '\n' {
			continue
		}
		t.insertRune(r)
	}
}

func (t *textBuffer) insertRune(r rune) {
	t.runes = append(t.runes, 0)
	copy(t.runes[t.cursor+1:], t.runes[t.cursor:])
	t.runes[t.cursor] = r
	t.cursor++
}

// viewInline rinde el buffer + cursor block + placeholder cuando vacio.
// focused=false omite el cursor (renderea texto plano o solo placeholder).
func (t textBuffer) viewInline(focused bool, placeholder string) string {
	const cursor = "▏"
	if len(t.runes) == 0 {
		if focused {
			return cursor + aigenDimStyle.Render(placeholder)
		}
		return aigenDimStyle.Render(placeholder)
	}
	if !focused {
		return string(t.runes)
	}
	if t.cursor >= len(t.runes) {
		return string(t.runes) + cursor
	}
	return string(t.runes[:t.cursor]) + cursor + string(t.runes[t.cursor:])
}
