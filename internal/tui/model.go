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
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/flow/explore"
	"github.com/chichex/che/internal/flow/idea"
)

type screen int

const (
	screenMenu screen = iota
	screenIdeaInput
	screenIdeaRunning
	screenExploreLoading
	screenExploreSelect
	screenExploreRunning
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
	{label: "Explorar un issue existente", key: "2", action: screenExploreLoading},
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

	// contexto mostrado en el header
	version string
	repo    string
	branch  string

	// streaming del flow
	runStart   time.Time
	runLog     []string
	progressCh chan tea.Msg

	// selector de explore: lista de issues candidatos + cursor propio
	exploreCandidates []explore.Candidate
	exploreCursor     int
	exploreLoadErr    error

	// resultado final
	resultLines []string
	resultOK    bool
}

// New construye el modelo inicial. version es el tag con el que se buildeó
// el binario (ej. "0.0.8"). El repo y la branch se detectan en el momento.
func New(version string) Model {
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
		version:  version,
		repo:     detectRepo(),
		branch:   detectBranch(),
	}
}

func detectRepo() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimSuffix(url, ".git")
	switch {
	case strings.HasPrefix(url, "https://github.com/"):
		return strings.TrimPrefix(url, "https://github.com/")
	case strings.HasPrefix(url, "git@github.com:"):
		return strings.TrimPrefix(url, "git@github.com:")
	}
	return url
}

func detectBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (m Model) Init() tea.Cmd { return nil }

// ---- mensajes que fluyen desde el flow hacia el Update ----

type progressMsg struct{ line string }
type flowDoneMsg struct {
	code   idea.ExitCode
	stdout string
	stderr string
}
type exploreDoneMsg struct {
	code   explore.ExitCode
	stdout string
	stderr string
}
type exploreCandidatesLoadedMsg struct {
	items []explore.Candidate
	err   error
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
		return m.finishRun(int(msg.code), msg.code == idea.ExitOK, msg.stdout, msg.stderr), nil

	case exploreDoneMsg:
		return m.finishRun(int(msg.code), msg.code == explore.ExitOK, msg.stdout, msg.stderr), nil

	case exploreCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.items) == 0 {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"No hay issues con label ct:plan listos para explorar."}
			return m, nil
		}
		m.exploreCandidates = msg.items
		m.exploreCursor = 0
		m.screen = screenExploreSelect
		return m, nil

	case tickMsg:
		if m.screen == screenIdeaRunning || m.screen == screenExploreRunning {
			return m, tickCmd()
		}
		return m, nil
	}
	return m, nil
}

// finishRun centraliza la transición de un flow running → screenResult,
// compartida por idea y explore. resultLines captura el stdout/stderr final
// (URLs o errores); m.runLog se muestra arriba para preservar el contexto.
func (m Model) finishRun(exitCode int, ok bool, stdout, stderr string) Model {
	m.screen = screenResult
	m.resultOK = ok
	var lines []string
	lines = append(lines, splitNonEmpty(stdout)...)
	lines = append(lines, splitNonEmpty(stderr)...)
	if !ok {
		lines = append(lines, fmt.Sprintf("(exit %d)", exitCode))
	}
	m.resultLines = lines
	m.progressCh = nil
	return m
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenMenu:
		return m.handleMenuKey(msg)
	case screenIdeaInput:
		return m.handleIdeaInputKey(msg)
	case screenIdeaRunning, screenExploreRunning, screenExploreLoading:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	case screenExploreSelect:
		return m.handleExploreSelectKey(msg)
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
		return m, nil
	case screenExploreLoading:
		m.screen = screenExploreLoading
		m.exploreCandidates = nil
		m.exploreCursor = 0
		m.exploreLoadErr = nil
		return m, loadExploreCandidatesCmd()
	}
	return m, nil
}

func loadExploreCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		items, err := explore.ListCandidates()
		return exploreCandidatesLoadedMsg{items: items, err: err}
	}
}

func (m Model) handleExploreSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenMenu
		m.exploreCandidates = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.exploreCursor = (m.exploreCursor - 1 + len(m.exploreCandidates)) % len(m.exploreCandidates)
		return m, nil
	case "down", "j":
		m.exploreCursor = (m.exploreCursor + 1) % len(m.exploreCandidates)
		return m, nil
	case "enter":
		if len(m.exploreCandidates) == 0 {
			return m, nil
		}
		chosen := m.exploreCandidates[m.exploreCursor]
		return m.startExploreFlow(fmt.Sprint(chosen.Number))
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
	m.exploreCandidates = nil
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

// startExploreFlow arranca explore.Run en background sobre el issue elegido.
// Usa el mismo patrón que startIdeaFlow: channel para progress, goroutine que
// corre el flow, y un tick para mostrar elapsed time.
func (m Model) startExploreFlow(issueRef string) (tea.Model, tea.Cmd) {
	m.screen = screenExploreRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := explore.Run(issueRef, explore.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- exploreDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
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
		return renderRunning(m, "Procesando idea…", "Ctrl+C cancela")
	case screenExploreLoading:
		return renderExploreLoading(m)
	case screenExploreSelect:
		return renderExploreSelect(m)
	case screenExploreRunning:
		return renderRunning(m, "Explorando issue…", "Ctrl+C cancela")
	case screenResult:
		return renderResult(m)
	}
	return ""
}

func renderMenu(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("che — workflow estandarizado con agentes de IA"))
	sb.WriteString("\n")
	sb.WriteString(contextLineStyle.Render(formatContext(m)))
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

// formatContext arma la línea "v0.0.8 · chichex/demo · main". Omite partes
// vacías (ej. si estás fuera de un repo git).
func formatContext(m Model) string {
	parts := []string{}
	if m.version != "" {
		parts = append(parts, accentBadge("v"+m.version))
	}
	if m.repo != "" {
		parts = append(parts, primaryBadge(m.repo))
	} else {
		parts = append(parts, mutedBadge("(sin repo git)"))
	}
	if m.branch != "" {
		parts = append(parts, mutedBadge(m.branch))
	}
	return strings.Join(parts, " · ")
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

// renderRunning es el render compartido para flows en ejecución (idea +
// explore): título + elapsed + log de progreso + hint.
func renderRunning(m Model, title, hint string) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render(title))
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
	sb.WriteString(hintStyle.Render(hint))
	sb.WriteString("\n")
	return sb.String()
}

func renderExploreLoading(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Explorar un issue"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando issues con label ct:plan…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderExploreSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Explorar un issue"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("%d issue(s) listos para explorar — elegí uno:", len(m.exploreCandidates))))
	sb.WriteString("\n\n")

	for i, c := range m.exploreCandidates {
		prefix := "  "
		style := menuItemStyle
		if i == m.exploreCursor {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		num := menuNumberStyle.Render(fmt.Sprintf("#%d", c.Number))
		line := prefix + num + "  " + c.Title
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter elige · Esc vuelve · Ctrl+C sale"))
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

// Run lanza el TUI y bloquea hasta que el usuario cierre. version se muestra
// en el header del menú (típicamente cmd.Version).
func Run(version string) error {
	p := tea.NewProgram(New(version), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
