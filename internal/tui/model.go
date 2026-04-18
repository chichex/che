// Package tui implementa la aplicación interactiva principal de che.
//
// El loop es:
//   1. Menú principal con 6 opciones (anotar/explorar/ejecutar/cerrar/eliminar/salir).
//   2. Al elegir una opción entra a la pantalla correspondiente.
//   3. Al terminar un flow (éxito o error), vuelve al menú con un toast.
package tui

import (
	"bytes"
	"fmt"
	"strings"

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
	key      string // atajo numérico
	disabled bool
	action   screen // pantalla a la que saltar
}

var menuItems = []menuItem{
	{label: "Anotar una idea nueva", key: "1", action: screenIdeaInput},
	{label: "Explorar un issue existente", key: "2", disabled: true},
	{label: "Ejecutar un plan", key: "3", disabled: true},
	{label: "Cerrar una idea", key: "4", disabled: true},
	{label: "Eliminar una idea", key: "5", disabled: true},
	{label: "Salir", key: "6"},
}

// Model es el estado raíz del TUI.
type Model struct {
	screen   screen
	cursor   int
	textarea textarea.Model

	// resultado del último flow corrido
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

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case ideaFlowDoneMsg:
		m.resultLines = msg.lines
		m.resultOK = msg.ok
		m.screen = screenResult
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
		// ignorar input mientras corre
		return m, nil
	case screenResult:
		return m.handleResultKey(msg)
	}
	return m, nil
}

func (m Model) handleMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// Shortcuts numéricos
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
	// "Salir"
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
		m.screen = screenIdeaRunning
		return m, runIdeaCmd(text)
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m Model) handleResultKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	}
	// Cualquier tecla vuelve al menú.
	m.screen = screenMenu
	m.resultLines = nil
	return m, nil
}

// View es la render function.
func (m Model) View() string {
	switch m.screen {
	case screenMenu:
		return renderMenu(m)
	case screenIdeaInput:
		return renderIdeaInput(m)
	case screenIdeaRunning:
		return renderIdeaRunning()
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
	sb.WriteString(subtitleStyle.Render("Escribilo como un commit message: claro y accionable."))
	sb.WriteString("\n")
	sb.WriteString(textareaBorder.Render(m.textarea.View()))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+D envía · Esc vuelve al menú · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderIdeaRunning() string {
	return titleStyle.Render("Procesando…") + "\n" +
		subtitleStyle.Render("Consultando a claude y creando los issues…") + "\n"
}

func renderResult(m Model) string {
	var sb strings.Builder
	if m.resultOK {
		sb.WriteString(successStyle.Render("✓ Listo"))
	} else {
		sb.WriteString(errorStyle.Render("✗ Error"))
	}
	sb.WriteString("\n\n")
	for _, line := range m.resultLines {
		sb.WriteString("  " + line + "\n")
	}
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("presioná cualquier tecla para volver al menú"))
	sb.WriteString("\n")
	return sb.String()
}

// ---- comandos async para Bubble Tea ----

type ideaFlowDoneMsg struct {
	ok    bool
	lines []string
}

// runIdeaCmd invoca el flow en background y emite un ideaFlowDoneMsg con
// el resultado cuando termina.
func runIdeaCmd(text string) tea.Cmd {
	return func() tea.Msg {
		var stdout, stderr bytes.Buffer
		code := idea.Run(text, &stdout, &stderr)
		lines := collectLines(stdout.String(), stderr.String())
		if code == idea.ExitOK {
			return ideaFlowDoneMsg{ok: true, lines: lines}
		}
		return ideaFlowDoneMsg{ok: false, lines: append(lines, fmt.Sprintf("(exit %d)", int(code)))}
	}
}

func collectLines(stdout, stderr string) []string {
	var out []string
	for _, block := range []string{stdout, stderr} {
		for _, l := range strings.Split(block, "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// Run lanza el TUI y bloquea hasta que el usuario cierre.
func Run() error {
	p := tea.NewProgram(New(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
