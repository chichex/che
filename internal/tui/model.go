// Package tui implementa la aplicación interactiva principal de che.
//
// El loop es:
//  1. Menú principal con 6 opciones (anotar/explorar/ejecutar/cerrar/eliminar/salir).
//  2. Al elegir una opción entra a la pantalla correspondiente.
//  3. Al terminar un flow (éxito o error), vuelve al menú con un toast.
package tui

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/flow/idea"
)

type screen int

const (
	screenMenu screen = iota
	screenIdeaInput
	screenIdeaRunning
	screenResult
)

type menuItem struct {
	label    string
	key      string
	disabled bool
	action   screen
}

var menuItems = []menuItem{
	{label: "Anotar una idea nueva", key: "1", action: screenIdeaInput},
	{label: "Explorar un issue existente", key: "2", disabled: true},
	{label: "Ejecutar un plan", key: "3", disabled: true},
	{label: "Cerrar una idea", key: "4", disabled: true},
	{label: "Eliminar una idea", key: "5", disabled: true},
	{label: "Salir", key: "6"},
}

const maxLogLines = 40

// Model es el estado raíz del TUI.
type Model struct {
	screen   screen
	cursor   int
	textarea textarea.Model

	// streaming del flow
	runStart   time.Time
	runLog     []string
	progressCh chan tea.Msg

	// resultado final
	resultLines []string
	resultOK    bool
}

// New construye el modelo inicial.
func New() Model {
	ta := textarea.New()
	ta.Placeholder = "Contame la idea — puede ser multilínea. Ctrl+D para enviar, Esc para cancelar."
	ta.CharLimit = 5000
	ta.SetWidth(70)
	ta.SetHeight(8)
	ta.ShowLineNumbers = false
	return Model{
		screen:   screenMenu,
		cursor:   0,
		textarea: ta,
	}
}

func (m Model) Init() tea.Cmd { return nil }

// ---- mensajes que fluyen desde el flow hacia el Update ----

type progressMsg struct{ line string }
type flowDoneMsg struct {
	code     idea.ExitCode
	stdout   string
	stderr   string
}
type tickMsg time.Time

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case progressMsg:
		m.runLog = appendLog(m.runLog, msg.line)
		// seguimos escuchando el channel
		return m, waitForMsg(m.progressCh)

	case flowDoneMsg:
		m.screen = screenResult
		m.resultOK = msg.code == idea.ExitOK
		// El log del run queda disponible en m.runLog; el render lo muestra
		// arriba. resultLines captura solo el stdout/stderr final (URLs o
		// errores) para que la parte "importante" quede destacada al pie.
		var lines []string
		for _, s := range splitNonEmpty(msg.stdout) {
			lines = append(lines, s)
		}
		for _, s := range splitNonEmpty(msg.stderr) {
			lines = append(lines, s)
		}
		if !m.resultOK {
			lines = append(lines, fmt.Sprintf("(exit %d)", int(msg.code)))
		}
		m.resultLines = lines
		m.progressCh = nil
		return m, nil

	case tickMsg:
		if m.screen == screenIdeaRunning {
			return m, tickCmd()
		}
		return m, nil
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenMenu:
		return m.handleMenuKey(msg)
	case screenIdeaInput:
		return m.handleIdeaInputKey(msg)
	case screenIdeaRunning:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	case screenResult:
		return m.handleResultKey(msg)
	}
	return m, nil
}

func (m Model) handleMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	for i, item := range menuItems {
		if k == item.key {
			m.cursor = i
			return m.activateCurrent()
		}
	}
	switch k {
	case "up", "k":
		m.cursor = (m.cursor - 1 + len(menuItems)) % len(menuItems)
	case "down", "j":
		m.cursor = (m.cursor + 1) % len(menuItems)
	case "enter":
		return m.activateCurrent()
	case "q", "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) activateCurrent() (tea.Model, tea.Cmd) {
	item := menuItems[m.cursor]
	if item.key == "6" {
		return m, tea.Quit
	}
	if item.disabled {
		return m, nil
	}
	switch item.action {
	case screenIdeaInput:
		m.textarea.Reset()
		m.textarea.Focus()
		m.screen = screenIdeaInput
	}
	return m, nil
}

func (m Model) handleIdeaInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenMenu
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "ctrl+d":
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		return m.startIdeaFlow(text)
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m Model) handleResultKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	m.screen = screenMenu
	m.resultLines = nil
	m.runLog = nil
	return m, nil
}

// startIdeaFlow lanza el flow en background, abre un channel para mensajes
// de progreso, y programa el tick de elapsed time.
func (m Model) startIdeaFlow(text string) (tea.Model, tea.Cmd) {
	m.screen = screenIdeaRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := idea.Run(text, idea.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- flowDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// waitForMsg lee el próximo mensaje del channel y lo devuelve como tea.Msg.
// Se re-emite después de cada progressMsg para seguir escuchando.
func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if ch == nil {
			return nil
		}
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func appendLog(log []string, line string) []string {
	log = append(log, line)
	if len(log) > maxLogLines {
		log = log[len(log)-maxLogLines:]
	}
	return log
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---- render ----

func (m Model) View() string {
	switch m.screen {
	case screenMenu:
		return renderMenu(m)
	case screenIdeaInput:
		return renderIdeaInput(m)
	case screenIdeaRunning:
		return renderIdeaRunning(m)
	case screenResult:
		return renderResult(m)
	}
	return ""
}

func renderMenu(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("che — workflow estandarizado con agentes de IA"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("¿Qué querés hacer?"))
	sb.WriteString("\n\n")

	for i, item := range menuItems {
		prefix := "  "
		style := menuItemStyle
		if i == m.cursor {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		num := menuNumberStyle.Render(item.key + ".")
		line := prefix + num + " " + item.label
		if item.disabled {
			line += " " + comingSoonStyle.Render("(coming soon)")
		}
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · 1-6 atajo · Enter elige · q sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderIdeaInput(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Anotar una idea"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Escribila como un commit message: clara y accionable."))
	sb.WriteString("\n")
	sb.WriteString(textareaBorder.Render(m.textarea.View()))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+D envía · Esc vuelve al menú · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderIdeaRunning(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Procesando…"))
	sb.WriteString("\n")

	elapsed := time.Since(m.runStart).Round(time.Second)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("⏱  %s transcurridos", elapsed)))
	sb.WriteString("\n")

	if len(m.runLog) == 0 {
		sb.WriteString(hintStyle.Render("  arrancando…"))
		sb.WriteString("\n")
	} else {
		for _, line := range m.runLog {
			sb.WriteString("  " + logLineStyle.Render(line) + "\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderResult(m Model) string {
	var sb strings.Builder
	if m.resultOK {
		sb.WriteString(successStyle.Render("✓ Listo"))
	} else {
		sb.WriteString(errorStyle.Render("✗ Error"))
	}
	sb.WriteString("\n")

	// Log completo de lo que pasó durante el run — preserva contexto aunque
	// haya terminado en error.
	if len(m.runLog) > 0 {
		sb.WriteString("\n")
		sb.WriteString(subtitleStyle.Render("Log:"))
		sb.WriteString("\n")
		for _, line := range m.runLog {
			sb.WriteString("  " + logLineStyle.Render(line) + "\n")
		}
	}

	// Resumen final (URLs creadas o mensaje de error).
	if len(m.resultLines) > 0 {
		sb.WriteString("\n")
		sb.WriteString(subtitleStyle.Render("Resultado:"))
		sb.WriteString("\n")
		for _, line := range m.resultLines {
			style := logLineStyle
			if strings.HasPrefix(line, "error:") || strings.Contains(line, "(exit ") {
				style = errorStyle
			} else if strings.HasPrefix(line, "Created ") {
				style = successStyle
			}
			sb.WriteString("  " + style.Render(line) + "\n")
		}
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("presioná cualquier tecla para volver al menú"))
	sb.WriteString("\n")
	return sb.String()
}

// Run lanza el TUI y bloquea hasta que el usuario cierre.
func Run() error {
	p := tea.NewProgram(New(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
