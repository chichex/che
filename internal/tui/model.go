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
	"github.com/chichex/che/internal/flow/execute"
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
	screenExploreAgent
	screenExploreValidators
	screenExploreRunning
	screenExecuteLoading
	screenExecuteSelect
	screenExecuteAgent
	screenExecuteRunning
	screenResult
)

// maxValidatorsPerAgent define cuántas instancias del mismo agente se
// pueden seleccionar. El design permite repetir tipo (ej: codex×2); le
// ponemos tope 2 para mantener la suma razonable (2-3 validadores en total).
const maxValidatorsPerAgent = 2

// validatorAgentDescriptions son los textos cortos que se muestran al lado
// de cada checkbox. Orden pensado para diversidad: Opus primero (mismo
// ejecutor por default), después los otros dos.
var validatorAgentDescriptions = map[explore.Agent]string{
	explore.AgentOpus:   "Claude Opus — si ya es tu ejecutor, sumarlo da menos diversidad",
	explore.AgentCodex:  "Codex CLI — segunda mirada técnica, criterio distinto",
	explore.AgentGemini: "Gemini CLI — tercera mirada, útil para diversidad",
}

type menuItem struct {
	label    string
	key      string
	disabled bool
	action   screen
}

var menuItems = []menuItem{
	{label: "Idea nueva", key: "1", action: screenIdeaInput},
	{label: "Explorar", key: "2", action: screenExploreLoading},
	{label: "Ejecutar", key: "3", action: screenExecuteLoading},
	{label: "Cerrar", key: "4", disabled: true},
	{label: "Eliminar", key: "5", disabled: true},
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

	// selector de explore: dos listas — ideas sin explorar + issues
	// awaiting-human para reanudar. El cursor es un índice global sobre la
	// concatenación new+resume; exploreTargets arma esa vista unificada.
	exploreNew     []explore.Candidate
	exploreResume  []explore.Candidate
	exploreCursor  int
	exploreLoadErr error

	// selector de agente ejecutor + checkbox de validadores
	exploreChosenRef       string
	exploreAgentIdx        int
	exploreChosenAgent     explore.Agent
	exploreValidatorCursor int
	exploreValidatorCount  map[explore.Agent]int

	// selector de execute: lista de issues en status:plan.
	executeCandidates []execute.Candidate
	executeCursor     int
	executeChosenRef  string
	executeAgentIdx   int
	executeChosenAgent execute.Agent

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
	newItems    []explore.Candidate
	resumeItems []explore.Candidate
	err         error
}
type executeCandidatesLoadedMsg struct {
	items []execute.Candidate
	err   error
}
type executeDoneMsg struct {
	code   execute.ExitCode
	stdout string
	stderr string
}
type resumeInspectedMsg struct {
	ref        string
	agent      explore.Agent
	validators []explore.Validator
	err        error
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

	case executeDoneMsg:
		return m.finishRun(int(msg.code), msg.code == execute.ExitOK, msg.stdout, msg.stderr), nil

	case executeCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.items) == 0 {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{
				"No hay issues con label ct:plan + status:plan listos para ejecutar.",
				"Primero corré `che explore <issue>` sobre un issue en status:idea.",
			}
			return m, nil
		}
		m.executeCandidates = msg.items
		m.executeCursor = 0
		m.screen = screenExecuteSelect
		return m, nil

	case resumeInspectedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"error leyendo run anterior: " + msg.err.Error()}
			return m, nil
		}
		m.exploreChosenRef = msg.ref
		m.exploreAgentIdx = indexOfAgent(msg.agent)
		// Pre-seleccionar validators del run anterior en el mapa de counts.
		counts := map[explore.Agent]int{}
		for _, v := range msg.validators {
			counts[v.Agent]++
		}
		m.exploreValidatorCount = counts
		m.exploreValidatorCursor = 0
		m.screen = screenExploreAgent
		return m, nil

	case exploreCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.newItems) == 0 && len(msg.resumeItems) == 0 {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{
				"No hay issues con label ct:plan listos para explorar.",
				"Tampoco hay issues en pausa esperando tu respuesta.",
			}
			return m, nil
		}
		m.exploreNew = msg.newItems
		m.exploreResume = msg.resumeItems
		m.exploreCursor = 0
		m.screen = screenExploreSelect
		return m, nil

	case tickMsg:
		if m.screen == screenIdeaRunning || m.screen == screenExploreRunning || m.screen == screenExecuteRunning {
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
	case screenIdeaRunning, screenExploreRunning, screenExploreLoading, screenExecuteRunning, screenExecuteLoading:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil
	case screenExploreSelect:
		return m.handleExploreSelectKey(msg)
	case screenExploreAgent:
		return m.handleExploreAgentKey(msg)
	case screenExploreValidators:
		return m.handleExploreValidatorsKey(msg)
	case screenExecuteSelect:
		return m.handleExecuteSelectKey(msg)
	case screenExecuteAgent:
		return m.handleExecuteAgentKey(msg)
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
		m.exploreNew = nil
		m.exploreResume = nil
		m.exploreCursor = 0
		m.exploreLoadErr = nil
		return m, loadExploreCandidatesCmd()
	case screenExecuteLoading:
		m.screen = screenExecuteLoading
		m.executeCandidates = nil
		m.executeCursor = 0
		return m, loadExecuteCandidatesCmd()
	}
	return m, nil
}

func loadExecuteCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		items, err := execute.ListCandidates()
		return executeCandidatesLoadedMsg{items: items, err: err}
	}
}

func (m Model) handleExecuteSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.executeCandidates)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.executeCandidates = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if total == 0 {
			return m, nil
		}
		m.executeCursor = (m.executeCursor - 1 + total) % total
		return m, nil
	case "down", "j":
		if total == 0 {
			return m, nil
		}
		m.executeCursor = (m.executeCursor + 1) % total
		return m, nil
	case "enter":
		if total == 0 {
			return m, nil
		}
		chosen := m.executeCandidates[m.executeCursor]
		m.executeChosenRef = fmt.Sprint(chosen.Number)
		m.executeAgentIdx = 0
		m.screen = screenExecuteAgent
		return m, nil
	}
	return m, nil
}

func (m Model) handleExecuteAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenExecuteSelect
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.executeAgentIdx = (m.executeAgentIdx - 1 + len(execute.ValidAgents)) % len(execute.ValidAgents)
		return m, nil
	case "down", "j":
		m.executeAgentIdx = (m.executeAgentIdx + 1) % len(execute.ValidAgents)
		return m, nil
	case "enter":
		m.executeChosenAgent = execute.ValidAgents[m.executeAgentIdx]
		return m.startExecuteFlow(m.executeChosenRef, m.executeChosenAgent)
	}
	for i := range execute.ValidAgents {
		if k == fmt.Sprint(i+1) {
			m.executeChosenAgent = execute.ValidAgents[i]
			return m.startExecuteFlow(m.executeChosenRef, m.executeChosenAgent)
		}
	}
	return m, nil
}

// startExecuteFlow arranca execute.Run en background sobre el issue elegido
// con el agente seleccionado y sin validadores (default 'none' en la TUI —
// el usuario interactivo típicamente quiere ver el PR listo, los validadores
// se pueden disparar después desde CLI si hace falta).
func (m Model) startExecuteFlow(issueRef string, agent execute.Agent) (tea.Model, tea.Cmd) {
	m.screen = screenExecuteRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := execute.Run(issueRef, execute.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			Agent:  agent,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- executeDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

func loadExploreCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		newItems, err := explore.ListCandidates()
		if err != nil {
			return exploreCandidatesLoadedMsg{err: err}
		}
		resumeItems, err := explore.ListAwaiting()
		if err != nil {
			return exploreCandidatesLoadedMsg{err: err}
		}
		return exploreCandidatesLoadedMsg{newItems: newItems, resumeItems: resumeItems}
	}
}

func (m Model) handleExploreSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.exploreNew) + len(m.exploreResume)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.exploreNew = nil
		m.exploreResume = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if total == 0 {
			return m, nil
		}
		m.exploreCursor = (m.exploreCursor - 1 + total) % total
		return m, nil
	case "down", "j":
		if total == 0 {
			return m, nil
		}
		m.exploreCursor = (m.exploreCursor + 1) % total
		return m, nil
	case "enter":
		if total == 0 {
			return m, nil
		}
		chosen, resume := m.exploreItemAt(m.exploreCursor)
		ref := fmt.Sprint(chosen.Number)
		if resume {
			// Reanudación: vamos a mostrar pantalla de carga mientras leemos
			// el run anterior del issue, y después paramos en agent+validators
			// con los mismos pre-seleccionados (el humano puede cambiarlos).
			m.exploreChosenRef = ref
			m.screen = screenExploreLoading
			return m, inspectResumeCmd(ref)
		}
		m.exploreChosenRef = ref
		m.exploreAgentIdx = 0
		// En modo new no hay preselect de validators — limpiamos por si quedó
		// algo de una corrida anterior.
		m.exploreValidatorCount = nil
		m.screen = screenExploreAgent
		return m, nil
	}
	return m, nil
}

// indexOfAgent devuelve el índice del agente en ValidAgents, o 0 si no se
// encuentra (default: Opus).
func indexOfAgent(a explore.Agent) int {
	for i, v := range explore.ValidAgents {
		if v == a {
			return i
		}
	}
	return 0
}

func inspectResumeCmd(ref string) tea.Cmd {
	return func() tea.Msg {
		agent, validators, err := explore.InspectResume(ref)
		return resumeInspectedMsg{ref: ref, agent: agent, validators: validators, err: err}
	}
}

// exploreItemAt devuelve el candidato en el índice global y si viene de la
// sección "resume" (awaiting-human) o "new". El orden visual siempre es
// new primero, resume después.
func (m Model) exploreItemAt(idx int) (explore.Candidate, bool) {
	if idx < len(m.exploreNew) {
		return m.exploreNew[idx], false
	}
	return m.exploreResume[idx-len(m.exploreNew)], true
}

func (m Model) handleExploreAgentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenExploreSelect
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.exploreAgentIdx = (m.exploreAgentIdx - 1 + len(explore.ValidAgents)) % len(explore.ValidAgents)
		return m, nil
	case "down", "j":
		m.exploreAgentIdx = (m.exploreAgentIdx + 1) % len(explore.ValidAgents)
		return m, nil
	case "enter":
		m.exploreChosenAgent = explore.ValidAgents[m.exploreAgentIdx]
		return m.enterValidatorsScreen(), nil
	}
	// Atajos numéricos 1..N para selección rápida del ejecutor.
	for i := range explore.ValidAgents {
		if k == fmt.Sprint(i+1) {
			m.exploreChosenAgent = explore.ValidAgents[i]
			return m.enterValidatorsScreen(), nil
		}
	}
	return m, nil
}

// enterValidatorsScreen lleva a la pantalla de validadores. Si el mapa de
// counts ya está seteado (caso de reanudación, donde lo pre-cargamos con el
// panel del run anterior), lo preserva. Si está vacío, usa el default
// codex+gemini para que el usuario arranque desde algo razonable.
func (m Model) enterValidatorsScreen() Model {
	m.exploreValidatorCursor = 0
	if len(m.exploreValidatorCount) == 0 {
		m.exploreValidatorCount = map[explore.Agent]int{
			explore.AgentCodex:  1,
			explore.AgentGemini: 1,
		}
	}
	m.screen = screenExploreValidators
	return m
}

func (m Model) handleExploreValidatorsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenExploreAgent
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.exploreValidatorCursor = (m.exploreValidatorCursor - 1 + len(explore.ValidAgents)) % len(explore.ValidAgents)
		return m, nil
	case "down", "j":
		m.exploreValidatorCursor = (m.exploreValidatorCursor + 1) % len(explore.ValidAgents)
		return m, nil
	case " ", "space", "x":
		// Cycle 0 → 1 → 2 → 0 sobre el agente bajo el cursor.
		a := explore.ValidAgents[m.exploreValidatorCursor]
		m.exploreValidatorCount[a] = (m.exploreValidatorCount[a] + 1) % (maxValidatorsPerAgent + 1)
		return m, nil
	case "enter":
		validators := validatorsFromCounts(m.exploreValidatorCount)
		total := len(validators)
		// Reglas: 0 (skip), 2 o 3 son válidos; 1 y 4+ no.
		if total != 0 && (total < 2 || total > 3) {
			return m, nil
		}
		return m.startExploreFlow(m.exploreChosenRef, m.exploreChosenAgent, validators)
	}
	return m, nil
}

// validatorsFromCounts traduce el mapa de counts a una lista de Validators
// con instance correcto (1, 2, ...) en el orden canónico de ValidAgents.
func validatorsFromCounts(counts map[explore.Agent]int) []explore.Validator {
	var out []explore.Validator
	for _, a := range explore.ValidAgents {
		n := counts[a]
		for i := 1; i <= n; i++ {
			out = append(out, explore.Validator{Agent: a, Instance: i})
		}
	}
	return out
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
	m.exploreNew = nil
	m.exploreResume = nil
	m.executeCandidates = nil
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

// startExploreFlow arranca explore.Run en background sobre el issue elegido
// con el agente seleccionado y la lista de validators del preset. Mismo
// patrón async que startIdeaFlow.
func (m Model) startExploreFlow(issueRef string, agent explore.Agent, validators []explore.Validator) (tea.Model, tea.Cmd) {
	m.screen = screenExploreRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := explore.Run(issueRef, explore.Opts{
			Stdout:     &stdout,
			Stderr:     &stderr,
			Agent:      agent,
			Validators: validators,
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
	case screenExploreAgent:
		return renderExploreAgent(m)
	case screenExploreValidators:
		return renderExploreValidators(m)
	case screenExploreRunning:
		return renderRunning(m, "Explorando issue…", "Ctrl+C cancela")
	case screenExecuteLoading:
		return renderExecuteLoading(m)
	case screenExecuteSelect:
		return renderExecuteSelect(m)
	case screenExecuteAgent:
		return renderExecuteAgent(m)
	case screenExecuteRunning:
		return renderRunning(m, "Ejecutando issue…", "Ctrl+C cancela")
	case screenResult:
		return renderResult(m)
	}
	return ""
}

func renderExecuteLoading(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Ejecutar un issue"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando issues en status:plan…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderExecuteSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Ejecutar"))
	sb.WriteString("\n")
	total := len(m.executeCandidates)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("%d issue(s) listos para ejecutar", total)))
	sb.WriteString("\n\n")
	if total == 0 {
		sb.WriteString("  " + mutedBadge("(ninguno)") + "\n")
	} else {
		for i, c := range m.executeCandidates {
			prefix := "  "
			style := menuItemStyle
			if i == m.executeCursor {
				prefix = "▸ "
				style = menuSelectedStyle
			}
			num := menuNumberStyle.Render(fmt.Sprintf("#%d", c.Number))
			line := prefix + num + "  " + c.Title
			sb.WriteString(style.Render(line) + "\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter elige · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderExecuteAgent(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Elegí ejecutor"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("Para ejecutar el issue #%s — ¿qué agente aplica el plan?", m.executeChosenRef)))
	sb.WriteString("\n\n")
	for i, a := range execute.ValidAgents {
		prefix := "  "
		style := menuItemStyle
		if i == m.executeAgentIdx {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		num := menuNumberStyle.Render(fmt.Sprintf("%d.", i+1))
		name := strings.ToUpper(string(a)[:1]) + string(a)[1:]
		line := prefix + num + " " + name
		sb.WriteString(style.Render(line) + "\n")
	}
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · 1-3 atajo · Enter elige · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
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

var agentDescriptions = map[explore.Agent]string{
	explore.AgentOpus:   "Claude Opus — el ejecutor por defecto, balanceado.",
	explore.AgentCodex:  "Codex CLI — fuerte en código, criterio diferente al de Opus.",
	explore.AgentGemini: "Gemini CLI — tercera opinión, útil cuando querés diversidad.",
}

func renderExploreAgent(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Elegí ejecutor"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("Para explorar el issue #%s — ¿qué agente corre el análisis?", m.exploreChosenRef)))
	sb.WriteString("\n\n")

	for i, a := range explore.ValidAgents {
		prefix := "  "
		style := menuItemStyle
		if i == m.exploreAgentIdx {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		num := menuNumberStyle.Render(fmt.Sprintf("%d.", i+1))
		line := prefix + num + " " + strings.ToUpper(string(a)[:1]) + string(a)[1:]
		if desc, ok := agentDescriptions[a]; ok {
			line += "  " + comingSoonStyle.Render("— "+desc)
		}
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · 1-3 atajo · Enter elige · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderExploreValidators(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Elegí validadores"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("Issue #%s · ejecutor: %s — marcá con space (0/1/2 de cada)",
		m.exploreChosenRef, m.exploreChosenAgent)))
	sb.WriteString("\n\n")

	total := 0
	for i, a := range explore.ValidAgents {
		count := m.exploreValidatorCount[a]
		total += count
		box := renderCheckbox(count)
		prefix := "  "
		style := menuItemStyle
		if i == m.exploreValidatorCursor {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		name := strings.ToUpper(string(a)[:1]) + string(a)[1:]
		line := prefix + box + "  " + name
		if desc, ok := validatorAgentDescriptions[a]; ok {
			line += "  " + comingSoonStyle.Render("— "+desc)
		}
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString("  " + renderValidatorTotal(total) + "\n")

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Space cycle 0/1/2 · Enter manda · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// renderCheckbox genera la caja visible según el count: 0=vacía, 1=×, 2=××.
func renderCheckbox(count int) string {
	switch count {
	case 0:
		return "[  ]"
	case 1:
		return "[x ]"
	case 2:
		return "[xx]"
	}
	return fmt.Sprintf("[%d]", count)
}

// renderValidatorTotal imprime el total con un marcador que indica si el
// count es aceptable (0, 2 o 3) o inválido (1, 4+).
func renderValidatorTotal(total int) string {
	mark := "✓"
	note := ""
	valid := total == 0 || total == 2 || total == 3
	if !valid {
		mark = "✗"
		note = "  (necesitás 0 para skipear, o 2-3 para validar)"
	} else if total == 0 {
		note = "  (sin validadores — solo ejecutor)"
	}
	style := successStyle
	if !valid {
		style = errorStyle
	}
	return style.Render(fmt.Sprintf("Total: %d %s", total, mark)) + comingSoonStyle.Render(note)
}

func renderExploreSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Explorar"))
	sb.WriteString("\n")

	newCount := len(m.exploreNew)
	resumeCount := len(m.exploreResume)
	summary := fmt.Sprintf("%d sin explorar · %d en pausa para revalidar", newCount, resumeCount)
	sb.WriteString(subtitleStyle.Render(summary))
	sb.WriteString("\n\n")

	idx := 0

	if newCount > 0 {
		sb.WriteString("  " + mutedBadge("— Ideas sin explorar —") + "\n")
		for _, c := range m.exploreNew {
			sb.WriteString(exploreCandidateLine(c, idx == m.exploreCursor))
			idx++
		}
	} else {
		sb.WriteString("  " + mutedBadge("— Ideas sin explorar —") + "  " + comingSoonStyle.Render("(ninguna)") + "\n")
	}

	sb.WriteString("\n")

	if resumeCount > 0 {
		sb.WriteString("  " + mutedBadge("— Para revalidar (pausadas por input humano) —") + "\n")
		for _, c := range m.exploreResume {
			sb.WriteString(exploreCandidateLine(c, idx == m.exploreCursor))
			idx++
		}
	} else {
		sb.WriteString("  " + mutedBadge("— Para revalidar —") + "  " + comingSoonStyle.Render("(ninguna)") + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter elige · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// exploreCandidateLine renderiza un item de la lista de exploración con el
// marcador ▸ cuando el cursor está encima.
func exploreCandidateLine(c explore.Candidate, selected bool) string {
	prefix := "  "
	style := menuItemStyle
	if selected {
		prefix = "▸ "
		style = menuSelectedStyle
	}
	num := menuNumberStyle.Render(fmt.Sprintf("#%d", c.Number))
	return style.Render(prefix+num+"  "+c.Title) + "\n"
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
