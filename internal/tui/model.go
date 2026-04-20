// Package tui implementa la aplicación interactiva principal de che.
//
// El loop es:
//  1. Menú principal con 6 opciones (anotar/explorar/ejecutar/cerrar/eliminar/salir).
//  2. Al elegir una opción entra a la pantalla correspondiente.
//  3. Al terminar un flow (éxito o error), vuelve al menú con un toast.
package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	closing "github.com/chichex/che/internal/flow/close"
	"github.com/chichex/che/internal/flow/execute"
	"github.com/chichex/che/internal/flow/explore"
	"github.com/chichex/che/internal/flow/idea"
	"github.com/chichex/che/internal/flow/iterate"
	"github.com/chichex/che/internal/flow/validate"
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
	screenValidateLoading
	screenValidateSelect
	screenValidateValidators
	screenValidateRunning
	screenIterateLoading
	screenIterateSelect
	screenIterateRunning
	screenCloseLoading
	screenCloseSelect
	screenCloseRunning
	screenResult
)

// maxValidatorsPerAgent define cuántas instancias del mismo agente se
// pueden seleccionar. El design permite repetir tipo (ej: codex×2); le
// ponemos tope 2 para mantener la suma razonable (1-3 validadores en total).
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
	{label: "Validar", key: "4", action: screenValidateLoading},
	{label: "Iterar", key: "5", action: screenIterateLoading},
	{label: "Cerrar", key: "6", action: screenCloseLoading},
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

	// ctx es el context raíz del TUI (signal.NotifyContext en tui.Run).
	// Cada flow en background se corre sobre un subcontext derivado —
	// cancelRun permite cancelar solo la corrida activa sin afectar el
	// context raíz, y una cancelación del raíz cascadea a todas las
	// corridas. Los subcommandos (CLI) tienen su propio ctx separado.
	ctx       context.Context
	cancelRun context.CancelFunc
	// quittingAfterCleanup se setea cuando pedimos shutdown (Ctrl+C durante
	// execute o señal externa): en vez de matar la UI de una, cancelamos el
	// flow y esperamos a que el done msg llegue para asegurar que el
	// cleanup local (label rollback, worktree remove, branch local)
	// terminó antes de salir.
	quittingAfterCleanup bool

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

	// selector de validate: lista de PRs abiertos + panel de validadores.
	validateCandidates     []validate.Candidate
	validateCursor         int
	validateChosenRef      string
	validateChosenURL      string
	validateValidatorCursor int
	validateValidatorCount  map[validate.Agent]int

	// selector de close: dos grupos — Ready (sin verdict bloqueante) y
	// Blocked (changes-requested / needs-human). Ambos se muestran; el
	// cursor es un índice global sobre la concatenación Ready+Blocked.
	// No hay panel de validadores — close usa opus por diseño.
	closeReady     []validate.Candidate
	closeBlocked   []validate.Candidate
	closeCursor    int
	closeChosenRef string
	closeChosenURL string

	// selector de iterate: lista de PRs con validated:changes-requested
	// (los que piden que opus aplique cambios).
	iterateCandidates []validate.Candidate
	iterateCursor     int
	iterateChosenRef  string
	iterateChosenURL  string

	// resultado final
	resultLines []string
	resultOK    bool
}

// New construye el modelo inicial. version es el tag con el que se buildeó
// el binario (ej. "0.0.8"). El repo y la branch se detectan en el momento.
// ctx es el context raíz (cancelable por señal); si es nil, se usa
// context.Background() — los tests de snapshot del model no lo necesitan.
func New(version string, ctx context.Context) Model {
	ta := textarea.New()
	ta.Placeholder = "Contame la idea — puede ser multilínea. Ctrl+D para enviar, Esc para cancelar."
	ta.CharLimit = 5000
	ta.SetWidth(70)
	ta.SetHeight(8)
	ta.ShowLineNumbers = false
	if ctx == nil {
		ctx = context.Background()
	}
	return Model{
		screen:   screenMenu,
		cursor:   0,
		textarea: ta,
		version:  version,
		repo:     detectRepo(),
		branch:   detectBranch(),
		ctx:      ctx,
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
type validateCandidatesLoadedMsg struct {
	items []validate.Candidate
	err   error
}
type validateDoneMsg struct {
	code   validate.ExitCode
	stdout string
	stderr string
}
type closeCandidatesLoadedMsg struct {
	ready   []validate.Candidate
	blocked []validate.Candidate
	err     error
}
type closeDoneMsg struct {
	code   closing.ExitCode
	stdout string
	stderr string
}
type iterateCandidatesLoadedMsg struct {
	items []validate.Candidate
	err   error
}
type iterateDoneMsg struct {
	code   iterate.ExitCode
	stdout string
	stderr string
}
type resumeInspectedMsg struct {
	ref        string
	agent      explore.Agent
	validators []explore.Validator
	err        error
}

// shutdownMsg llega cuando el context raíz se cancela (SIGINT/SIGTERM desde
// fuera del TUI). La UI la interpreta igual que Ctrl+C durante un run en
// curso: cancela la corrida activa y espera a que el done msg llegue antes
// de salir, para que el cleanup local del flow corra síncrono.
type shutdownMsg struct{}

type tickMsg time.Time

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case shutdownMsg:
		// Señal externa (SIGTERM/SIGINT fuera de la TUI). Si hay un run
		// activo con cancelRun seteado, cancelamos y marcamos
		// quittingAfterCleanup: cuando llegue el *DoneMsg salimos. Si no
		// hay run activo, salimos de una.
		if m.cancelRun != nil {
			m.cancelRun()
			m.quittingAfterCleanup = true
			return m, nil
		}
		return m, tea.Quit

	case progressMsg:
		m.runLog = appendLog(m.runLog, msg.line)
		// seguimos escuchando el channel
		return m, waitForMsg(m.progressCh)

	case flowDoneMsg:
		return m.afterDone(m.finishRun(int(msg.code), msg.code == idea.ExitOK, msg.stdout, msg.stderr))

	case exploreDoneMsg:
		return m.afterDone(m.finishRun(int(msg.code), msg.code == explore.ExitOK, msg.stdout, msg.stderr))

	case executeDoneMsg:
		return m.afterDone(m.finishRun(int(msg.code), msg.code == execute.ExitOK, msg.stdout, msg.stderr))

	case validateDoneMsg:
		return m.afterDone(m.finishRun(int(msg.code), msg.code == validate.ExitOK, msg.stdout, msg.stderr))

	case closeDoneMsg:
		return m.finishRun(int(msg.code), msg.code == closing.ExitOK, msg.stdout, msg.stderr), nil

	case iterateDoneMsg:
		return m.finishRun(int(msg.code), msg.code == iterate.ExitOK, msg.stdout, msg.stderr), nil

	case iterateCandidatesLoadedMsg:
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
				"No hay PRs con validated:changes-requested en este repo.",
				"Corré `che validate <pr>` y si pide cambios, volvé acá.",
			}
			return m, nil
		}
		m.iterateCandidates = msg.items
		m.iterateCursor = 0
		m.screen = screenIterateSelect
		return m, nil

	case closeCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.ready)+len(msg.blocked) == 0 {
			m.screen = screenResult
			m.resultOK = false
			m.resultLines = []string{
				"No hay PRs abiertos que cerrar en este repo.",
				"Abrí un PR con `che execute` o validá uno existente antes.",
			}
			return m, nil
		}
		m.closeReady = msg.ready
		m.closeBlocked = msg.blocked
		m.closeCursor = 0
		m.screen = screenCloseSelect
		return m, nil

	case validateCandidatesLoadedMsg:
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
				"No hay PRs abiertos en este repo.",
				"Abrí un PR antes (por ej. con `che execute`) y volvé a intentar.",
			}
			return m, nil
		}
		m.validateCandidates = msg.items
		m.validateCursor = 0
		m.screen = screenValidateSelect
		return m, nil

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
		if m.screen == screenIdeaRunning || m.screen == screenExploreRunning ||
			m.screen == screenExecuteRunning || m.screen == screenValidateRunning ||
			m.screen == screenCloseRunning || m.screen == screenIterateRunning {
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
	// Liberar el cancel del run — el ctx del run ya quedó resuelto y la
	// próxima corrida crea el suyo.
	m.cancelRun = nil
	return m
}

// afterDone decide si después de un finishRun tenemos que cerrar la UI
// (cuando venimos de un shutdown pedido) o volver al menú / pantalla de
// resultado normal. Factorizado para no repetir la lógica en cada *DoneMsg.
func (m Model) afterDone(newModel Model) (tea.Model, tea.Cmd) {
	if newModel.quittingAfterCleanup {
		return newModel, tea.Quit
	}
	return newModel, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenMenu:
		return m.handleMenuKey(msg)
	case screenIdeaInput:
		return m.handleIdeaInputKey(msg)
	case screenIdeaRunning, screenExploreRunning, screenExploreLoading, screenExecuteRunning, screenExecuteLoading,
		screenValidateRunning, screenValidateLoading, screenCloseRunning, screenCloseLoading,
		screenIterateRunning, screenIterateLoading:
		if msg.String() == "ctrl+c" {
			// Si hay un run activo con cancel asociado (caso execute),
			// cancelamos y esperamos al done msg para que el cleanup
			// local termine síncrono antes de cerrar la UI. Para los
			// otros flows el cancel no está armado y salimos directo.
			if m.cancelRun != nil {
				m.cancelRun()
				m.quittingAfterCleanup = true
				return m, nil
			}
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
	case screenValidateSelect:
		return m.handleValidateSelectKey(msg)
	case screenValidateValidators:
		return m.handleValidateValidatorsKey(msg)
	case screenCloseSelect:
		return m.handleCloseSelectKey(msg)
	case screenIterateSelect:
		return m.handleIterateSelectKey(msg)
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
	case screenValidateLoading:
		m.screen = screenValidateLoading
		m.validateCandidates = nil
		m.validateCursor = 0
		return m, loadValidateCandidatesCmd()
	case screenCloseLoading:
		m.screen = screenCloseLoading
		m.closeReady = nil
		m.closeBlocked = nil
		m.closeCursor = 0
		return m, loadCloseCandidatesCmd()
	case screenIterateLoading:
		m.screen = screenIterateLoading
		m.iterateCandidates = nil
		m.iterateCursor = 0
		return m, loadIterateCandidatesCmd()
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

// loadValidateCandidatesCmd lista PRs abiertos del repo vía gh.
func loadValidateCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		items, err := validate.ListOpenPRs()
		return validateCandidatesLoadedMsg{items: items, err: err}
	}
}

func (m Model) handleValidateSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.validateCandidates)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.validateCandidates = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if total == 0 {
			return m, nil
		}
		m.validateCursor = (m.validateCursor - 1 + total) % total
		return m, nil
	case "down", "j":
		if total == 0 {
			return m, nil
		}
		m.validateCursor = (m.validateCursor + 1) % total
		return m, nil
	case "enter":
		if total == 0 {
			return m, nil
		}
		chosen := m.validateCandidates[m.validateCursor]
		m.validateChosenRef = fmt.Sprint(chosen.Number)
		m.validateChosenURL = chosen.URL
		// Default: opus=1 (coherente con flag default).
		m.validateValidatorCount = map[validate.Agent]int{validate.AgentOpus: 1}
		m.validateValidatorCursor = 0
		m.screen = screenValidateValidators
		return m, nil
	}
	return m, nil
}

func (m Model) handleValidateValidatorsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch k {
	case "esc":
		m.screen = screenValidateSelect
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.validateValidatorCursor = (m.validateValidatorCursor - 1 + len(validate.ValidAgents)) % len(validate.ValidAgents)
		return m, nil
	case "down", "j":
		m.validateValidatorCursor = (m.validateValidatorCursor + 1) % len(validate.ValidAgents)
		return m, nil
	case " ", "space", "x":
		a := validate.ValidAgents[m.validateValidatorCursor]
		m.validateValidatorCount[a] = (m.validateValidatorCount[a] + 1) % (maxValidatorsPerAgent + 1)
		return m, nil
	case "enter":
		validators := validateValidatorsFromCounts(m.validateValidatorCount)
		total := len(validators)
		// validate REQUIERE al menos 1 validador (a diferencia de explore).
		if total < 1 || total > 3 {
			return m, nil
		}
		return m.startValidateFlow(m.validateChosenRef, validators)
	}
	return m, nil
}

// validateValidatorsFromCounts traduce el mapa de counts del TUI a una lista
// de validate.Validator con instance correcto en el orden canónico.
func validateValidatorsFromCounts(counts map[validate.Agent]int) []validate.Validator {
	var out []validate.Validator
	for _, a := range validate.ValidAgents {
		n := counts[a]
		for i := 1; i <= n; i++ {
			out = append(out, validate.Validator{Agent: a, Instance: i})
		}
	}
	return out
}

// loadIterateCandidatesCmd lista PRs con validated:changes-requested —
// los que pidieron cambios y están esperando que iterate los aplique.
func loadIterateCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		items, err := iterate.ListIterable()
		return iterateCandidatesLoadedMsg{items: items, err: err}
	}
}

func (m Model) handleIterateSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.iterateCandidates)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.iterateCandidates = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if total == 0 {
			return m, nil
		}
		m.iterateCursor = (m.iterateCursor - 1 + total) % total
		return m, nil
	case "down", "j":
		if total == 0 {
			return m, nil
		}
		m.iterateCursor = (m.iterateCursor + 1) % total
		return m, nil
	case "enter":
		if total == 0 {
			return m, nil
		}
		chosen := m.iterateCandidates[m.iterateCursor]
		m.iterateChosenRef = fmt.Sprint(chosen.Number)
		m.iterateChosenURL = chosen.URL
		return m.startIterateFlow(m.iterateChosenRef)
	}
	return m, nil
}

// startIterateFlow arranca iterate.Run en background sobre el PR elegido.
// Sin selector de agente — iterate usa opus hardcoded.
func (m Model) startIterateFlow(prRef string) (tea.Model, tea.Cmd) {
	m.screen = screenIterateRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := iterate.Run(prRef, iterate.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- iterateDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// loadCloseCandidatesCmd lista PRs abiertos agrupados en ready/blocked
// por verdict. Ambos grupos se muestran al usuario — close no esconde
// nada; la agrupación es solo visual.
func loadCloseCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		groups, err := closing.ListCloseable()
		return closeCandidatesLoadedMsg{ready: groups.Ready, blocked: groups.Blocked, err: err}
	}
}

// closeItemAt devuelve el candidate en el índice global (sobre la
// concatenación Ready+Blocked) y si viene del grupo blocked.
func (m Model) closeItemAt(idx int) (validate.Candidate, bool) {
	if idx < len(m.closeReady) {
		return m.closeReady[idx], false
	}
	return m.closeBlocked[idx-len(m.closeReady)], true
}

func (m Model) handleCloseSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.closeReady) + len(m.closeBlocked)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.closeReady = nil
		m.closeBlocked = nil
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if total == 0 {
			return m, nil
		}
		m.closeCursor = (m.closeCursor - 1 + total) % total
		return m, nil
	case "down", "j":
		if total == 0 {
			return m, nil
		}
		m.closeCursor = (m.closeCursor + 1) % total
		return m, nil
	case "enter":
		if total == 0 {
			return m, nil
		}
		chosen, _ := m.closeItemAt(m.closeCursor)
		m.closeChosenRef = fmt.Sprint(chosen.Number)
		m.closeChosenURL = chosen.URL
		return m.startCloseFlow(m.closeChosenRef)
	}
	return m, nil
}

// startCloseFlow arranca closing.Run en background sobre el PR elegido.
// No hay selector de agente — close usa opus hardcoded.
func (m Model) startCloseFlow(prRef string) (tea.Model, tea.Cmd) {
	m.screen = screenCloseRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := closing.Run(prRef, closing.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- closeDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// startValidateFlow arranca validate.Run en background sobre el PR elegido.
func (m Model) startValidateFlow(prRef string, validators []validate.Validator) (tea.Model, tea.Cmd) {
	m.screen = screenValidateRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stdout, stderr bytes.Buffer
		code := validate.Run(prRef, validate.Opts{
			Stdout:     &stdout,
			Stderr:     &stderr,
			Validators: validators,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- validateDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// startExecuteFlow arranca execute.Run en background sobre el issue elegido
// con el agente seleccionado y sin validadores (default 'none' en la TUI —
// el usuario interactivo típicamente quiere ver el PR listo, los validadores
// se pueden disparar después desde CLI si hace falta).
//
// Deriva un subcontext cancelable del ctx raíz y lo guarda en m.cancelRun
// para que el handler de Ctrl+C pueda cancelar solo esta corrida (dejando
// el ctx raíz vivo para las siguientes). Si llega una señal externa, el
// ctx raíz se cancela y cascadea.
func (m Model) startExecuteFlow(issueRef string, agent execute.Agent) (tea.Model, tea.Cmd) {
	m.screen = screenExecuteRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	runCtx, cancel := context.WithCancel(m.ctx)
	m.cancelRun = cancel

	go func(ch chan<- tea.Msg, runCtx context.Context) {
		var stdout, stderr bytes.Buffer
		code := execute.Run(issueRef, execute.Opts{
			Stdout: &stdout,
			Stderr: &stderr,
			Agent:  agent,
			Ctx:    runCtx,
			OnProgress: func(line string) {
				ch <- progressMsg{line: line}
			},
		})
		ch <- executeDoneMsg{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}(m.progressCh, runCtx)

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
// panel del run anterior), lo preserva. Si está vacío, usa el default opus
// para que el usuario arranque desde algo razonable.
func (m Model) enterValidatorsScreen() Model {
	m.exploreValidatorCursor = 0
	if len(m.exploreValidatorCount) == 0 {
		m.exploreValidatorCount = map[explore.Agent]int{
			explore.AgentOpus: 1,
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
		// Reglas: 0 (skip) o 1-3 son válidos; 4+ no.
		if total > 3 {
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
	m.validateCandidates = nil
	m.closeReady = nil
	m.closeBlocked = nil
	m.iterateCandidates = nil
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
	case screenValidateLoading:
		return renderValidateLoading(m)
	case screenValidateSelect:
		return renderValidateSelect(m)
	case screenValidateValidators:
		return renderValidateValidators(m)
	case screenValidateRunning:
		return renderRunning(m, "Validando PR…", "Ctrl+C cancela")
	case screenCloseLoading:
		return renderCloseLoading(m)
	case screenCloseSelect:
		return renderCloseSelect(m)
	case screenCloseRunning:
		return renderRunning(m, "Cerrando PR…", "Ctrl+C cancela")
	case screenIterateLoading:
		return renderIterateLoading(m)
	case screenIterateSelect:
		return renderIterateSelect(m)
	case screenIterateRunning:
		return renderRunning(m, "Iterando sobre findings…", "Ctrl+C cancela")
	case screenResult:
		return renderResult(m)
	}
	return ""
}

// ---- validate renders ----

func renderValidateLoading(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Validar un PR"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando PRs abiertos…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderValidateSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Validar"))
	sb.WriteString("\n")
	total := len(m.validateCandidates)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("%d PR(s) abiertos", total)))
	sb.WriteString("\n\n")
	if total == 0 {
		sb.WriteString("  " + mutedBadge("(ninguno)") + "\n")
	} else {
		for i, c := range m.validateCandidates {
			prefix := "  "
			style := menuItemStyle
			if i == m.validateCursor {
				prefix = "▸ "
				style = menuSelectedStyle
			}
			num := menuNumberStyle.Render(fmt.Sprintf("#%d", c.Number))
			draft := ""
			if c.IsDraft {
				draft = " " + mutedBadge("draft")
			}
			rel := ""
			if len(c.RelatedIssues) > 0 {
				parts := make([]string, 0, len(c.RelatedIssues))
				for _, n := range c.RelatedIssues {
					parts = append(parts, fmt.Sprintf("#%d", n))
				}
				rel = " " + mutedBadge("closes "+strings.Join(parts, ", "))
			}
			line := prefix + num + "  " + c.Title + draft + rel
			if c.Author != "" {
				line += " " + comingSoonStyle.Render("— by @"+c.Author)
			}
			sb.WriteString(style.Render(line) + "\n")
		}
	}
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter elige · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

func renderValidateValidators(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Elegí validadores"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("PR #%s — marcá con space (al menos 1, máximo 3)",
		m.validateChosenRef)))
	sb.WriteString("\n\n")

	total := 0
	for i, a := range validate.ValidAgents {
		count := m.validateValidatorCount[a]
		total += count
		box := renderCheckbox(count)
		prefix := "  "
		style := menuItemStyle
		if i == m.validateValidatorCursor {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		name := strings.ToUpper(string(a)[:1]) + string(a)[1:]
		line := prefix + box + "  " + name
		// Reusamos las descripciones de explore.validatorAgentDescriptions,
		// que están indexadas por explore.Agent — los strings subyacentes son
		// iguales así que mapeamos manualmente.
		descKey := explore.Agent(string(a))
		if desc, ok := validatorAgentDescriptions[descKey]; ok {
			line += "  " + comingSoonStyle.Render("— "+desc)
		}
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString("  " + renderValidateValidatorTotal(total) + "\n")

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Space cycle 0/1/2 · Enter manda · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// renderValidateValidatorTotal es como renderValidatorTotal pero con la regla
// de validate: el total=0 es inválido (validate sin validadores no corre).
func renderValidateValidatorTotal(total int) string {
	mark := "✓"
	note := ""
	valid := total >= 1 && total <= 3
	if total == 0 {
		mark = "✗"
		note = "  (elegí al menos 1)"
	} else if total > 3 {
		mark = "✗"
		note = "  (máximo 3 validadores)"
	}
	style := successStyle
	if !valid {
		style = errorStyle
	}
	return style.Render(fmt.Sprintf("Total: %d %s", total, mark)) + comingSoonStyle.Render(note)
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
// count es aceptable (0-3) o inválido (4+).
func renderValidatorTotal(total int) string {
	mark := "✓"
	note := ""
	valid := total >= 0 && total <= 3
	if !valid {
		mark = "✗"
		note = "  (máximo 3 validadores)"
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

// ---- close renders ----

func renderCloseLoading(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Cerrar un PR"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando PRs abiertos…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderCloseSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Cerrar"))
	sb.WriteString("\n")

	readyCount := len(m.closeReady)
	blockedCount := len(m.closeBlocked)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf(
		"%d listo(s) · %d con verdict bloqueante — opus arregla conflictos/CI y mergea",
		readyCount, blockedCount)))
	sb.WriteString("\n\n")

	idx := 0

	if readyCount > 0 {
		sb.WriteString("  " + mutedBadge("— Listos para cerrar —") + "\n")
		for _, c := range m.closeReady {
			sb.WriteString(closeCandidateLine(c, idx == m.closeCursor))
			idx++
		}
	} else {
		sb.WriteString("  " + mutedBadge("— Listos para cerrar —") + "  " + comingSoonStyle.Render("(ninguno)") + "\n")
	}

	sb.WriteString("\n")

	if blockedCount > 0 {
		sb.WriteString("  " + mutedBadge("— Con verdict bloqueante (changes-requested / needs-human) —") + "\n")
		for _, c := range m.closeBlocked {
			sb.WriteString(closeCandidateLine(c, idx == m.closeCursor))
			idx++
		}
	} else {
		sb.WriteString("  " + mutedBadge("— Con verdict bloqueante —") + "  " + comingSoonStyle.Render("(ninguno)") + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter cierra (warnea si bloqueante) · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// closeCandidateLine renderea una línea del selector de close, mismo
// estilo que los otros selectores pero con badges de draft/closes.
func closeCandidateLine(c validate.Candidate, selected bool) string {
	prefix := "  "
	style := menuItemStyle
	if selected {
		prefix = "▸ "
		style = menuSelectedStyle
	}
	num := menuNumberStyle.Render(fmt.Sprintf("#%d", c.Number))
	draft := ""
	if c.IsDraft {
		draft = " " + mutedBadge("draft")
	}
	rel := ""
	if len(c.RelatedIssues) > 0 {
		parts := make([]string, 0, len(c.RelatedIssues))
		for _, n := range c.RelatedIssues {
			parts = append(parts, fmt.Sprintf("#%d", n))
		}
		rel = " " + mutedBadge("closes "+strings.Join(parts, ", "))
	}
	line := prefix + num + "  " + c.Title + draft + rel
	if c.Author != "" {
		line += " " + comingSoonStyle.Render("— by @"+c.Author)
	}
	return style.Render(line) + "\n"
}

// ---- iterate renders ----

func renderIterateLoading(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Iterar sobre un PR"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando PRs con validated:changes-requested…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

func renderIterateSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Iterar"))
	sb.WriteString("\n")
	total := len(m.iterateCandidates)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf(
		"%d PR(s) con changes-requested — opus lee los findings y aplica los cambios",
		total)))
	sb.WriteString("\n\n")
	if total == 0 {
		sb.WriteString("  " + mutedBadge("(ninguno)") + "\n")
	} else {
		for i, c := range m.iterateCandidates {
			sb.WriteString(closeCandidateLine(c, i == m.iterateCursor))
		}
	}
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("↑/↓ navega · Enter dispara · Esc vuelve · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// Run lanza el TUI y bloquea hasta que el usuario cierre. version se muestra
// en el header del menú (típicamente cmd.Version).
//
// Instala signal.NotifyContext(SIGINT, SIGTERM) sobre context.Background().
// tea.WithoutSignalHandler desactiva el handler default de bubbletea — el
// nuestro es el único que ve las señales. En alt-screen + raw stdin, Ctrl+C
// no genera SIGINT (se lee como byte por stdin y llega al Update como
// tea.KeyMsg); las señales reales vienen de `kill -INT/TERM <pid>` fuera
// de la TUI. Una goroutine convierte el ctx.Done() a shutdownMsg para que
// el Update cancele la corrida activa y espere al cleanup antes de tea.Quit.
func Run(version string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := tea.NewProgram(New(version, ctx), tea.WithAltScreen(), tea.WithoutSignalHandler())

	go func() {
		<-ctx.Done()
		p.Send(shutdownMsg{})
	}()

	_, err := p.Run()
	return err
}
