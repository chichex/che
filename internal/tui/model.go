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
	"github.com/chichex/che/internal/output"
)

type screen int

const (
	screenMenu screen = iota
	screenIdeaInput
	screenIdeaRunning
	screenExploreLoading
	screenExploreSelect
	screenExploreAgent
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

// maxValidatorsTotal es el tope absoluto de validadores disparables en una
// tanda por validate. El stepper 0..N por agente respeta este cap global —
// si la suma ya es 3, intentar incrementar es no-op.
const maxValidatorsTotal = 3

// buildValidatorList traduce un mapa de counts a una lista ordenada de
// validadores con Instance 1..N, respetando el orden canónico de agents.
// Genérico para compartirse entre los 3 flows de validators (explore,
// execute, validate) sin acoplar tipos; mk es el constructor concreto que
// cada flow provee para envolver (Agent, instance) en su propio Validator.
func buildValidatorList[A comparable, V any](agents []A, counts map[A]int, mk func(a A, inst int) V) []V {
	var out []V
	for _, a := range agents {
		n := counts[a]
		for i := 1; i <= n; i++ {
			out = append(out, mk(a, i))
		}
	}
	return out
}

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

	// selector de explore: lista de ideas sin explorar. No hay lista de
	// "resume" — con el flow simplificado explore no pausa más para input
	// humano.
	exploreNew     []explore.Candidate
	exploreCursor  int
	exploreLoadErr error

	// selector de agente ejecutor. El stepper actual es Loading → Select →
	// Agent → Running; no hay panel de validadores (explore ya no dispara
	// validadores automáticamente).
	exploreChosenRef   string
	exploreAgentIdx    int
	exploreChosenAgent explore.Agent

	// selector de execute: lista de issues en status:plan, seguido de
	// selector de ejecutor. Sin panel de validadores — execute tampoco
	// dispara validadores automáticamente.
	executeCandidates  []execute.Candidate
	executeCursor      int
	executeChosenRef   string
	executeAgentIdx    int
	executeChosenAgent execute.Agent

	// selector de validate: dos listas (planes pendientes + PRs abiertos)
	// con cursor unificado (0..len(plans)-1 → planes, resto → PRs), seguido
	// del panel de validadores (stepper 0..N por agente).
	validatePlans []validate.PlanCandidate
	validatePRs   []validate.Candidate
	validateCursor int
	// validateLoad* tracking: el loader dispara dos comandos paralelos
	// (plans + PRs). Transicionamos a Select solo cuando los dos recibidos.
	validatePlansLoaded bool
	validatePRsLoaded   bool
	validatePlansErr    error
	validatePRsErr      error
	validateChosenRef       string
	validateChosenURL       string
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

	// selector de iterate: dos listas (planes con plan-validated:changes-
	// requested + PRs con validated:changes-requested) con cursor unificado.
	iteratePlans []validate.PlanCandidate
	iteratePRs   []validate.Candidate
	iterateCursor int
	iteratePlansLoaded bool
	iteratePRsLoaded   bool
	iteratePlansErr    error
	iteratePRsErr      error
	iterateChosenRef   string
	iterateChosenURL   string

	// resultado final
	resultLines []string
	resultKind  resultKind
}

// resultKind distingue el tipo de pantalla final:
//   - resultInfo (zero value) — empty state informativo (sin items que
//     mostrar, acción no aplicable). NO es error.
//   - resultSuccess — el flow terminó OK.
//   - resultError — error real: exit code no-OK, o fetch que falló.
type resultKind int

const (
	resultInfo resultKind = iota
	resultSuccess
	resultError
)

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
	newItems []explore.Candidate
	err      error
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
// validate ahora tiene dos listas paralelas (planes pendientes + PRs
// abiertos). El loader dispara dos comandos async y la transición a Select
// espera a que ambas respuestas lleguen (o falló una y la otra cargó ok).
type validatePlansLoadedMsg struct {
	plans []validate.PlanCandidate
	err   error
}
type validatePRsLoadedMsg struct {
	prs []validate.Candidate
	err error
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
// iterate sigue la misma idea: dos listas (planes con plan-validated:
// changes-requested + PRs con validated:changes-requested).
type iteratePlansLoadedMsg struct {
	plans []validate.PlanCandidate
	err   error
}
type iteratePRsLoadedMsg struct {
	prs []validate.Candidate
	err error
}
type iterateDoneMsg struct {
	code   iterate.ExitCode
	stdout string
	stderr string
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

	case eventMsg:
		m.runLog = appendLog(m.runLog, renderEventLine(msg.ev))
		return m, waitForMsg(m.progressCh)

	case payloadMsg:
		m.runLog = appendLog(m.runLog, logLineStyle.Render(msg.line))
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

	case iteratePlansLoadedMsg:
		m.iteratePlans = msg.plans
		m.iteratePlansErr = msg.err
		m.iteratePlansLoaded = true
		return m.maybeAdvanceIterate()
	case iteratePRsLoadedMsg:
		m.iteratePRs = msg.prs
		m.iteratePRsErr = msg.err
		m.iteratePRsLoaded = true
		return m.maybeAdvanceIterate()

	case closeCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultKind = resultError
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.ready)+len(msg.blocked) == 0 {
			m.screen = screenResult
			m.resultKind = resultInfo
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

	case validatePlansLoadedMsg:
		m.validatePlans = msg.plans
		m.validatePlansErr = msg.err
		m.validatePlansLoaded = true
		return m.maybeAdvanceValidate()
	case validatePRsLoadedMsg:
		m.validatePRs = msg.prs
		m.validatePRsErr = msg.err
		m.validatePRsLoaded = true
		return m.maybeAdvanceValidate()

	case executeCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultKind = resultError
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.items) == 0 {
			m.screen = screenResult
			m.resultKind = resultInfo
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

	case exploreCandidatesLoadedMsg:
		if msg.err != nil {
			m.screen = screenResult
			m.resultKind = resultError
			m.resultLines = []string{"error: " + msg.err.Error()}
			return m, nil
		}
		if len(msg.newItems) == 0 {
			m.screen = screenResult
			m.resultKind = resultInfo
			m.resultLines = []string{
				"No hay issues con label ct:plan listos para explorar.",
				"Corré `che idea` para crear un issue primero.",
			}
			return m, nil
		}
		m.exploreNew = msg.newItems
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
	if ok {
		m.resultKind = resultSuccess
	} else {
		m.resultKind = resultError
	}
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
		m.validatePlans = nil
		m.validatePRs = nil
		m.validateCursor = 0
		m.validatePlansLoaded = false
		m.validatePRsLoaded = false
		m.validatePlansErr = nil
		m.validatePRsErr = nil
		return m, tea.Batch(loadValidatePlansCmd(), loadValidatePRsCmd())
	case screenCloseLoading:
		m.screen = screenCloseLoading
		m.closeReady = nil
		m.closeBlocked = nil
		m.closeCursor = 0
		return m, loadCloseCandidatesCmd()
	case screenIterateLoading:
		m.screen = screenIterateLoading
		m.iteratePlans = nil
		m.iteratePRs = nil
		m.iterateCursor = 0
		m.iteratePlansLoaded = false
		m.iteratePRsLoaded = false
		m.iteratePlansErr = nil
		m.iteratePRsErr = nil
		return m, tea.Batch(loadIteratePlansCmd(), loadIteratePRsCmd())
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

// loadValidatePlansCmd lista issues en status:plan sin plan-validated:approve.
func loadValidatePlansCmd() tea.Cmd {
	return func() tea.Msg {
		plans, err := validate.ListPlanCandidates()
		return validatePlansLoadedMsg{plans: plans, err: err}
	}
}

// loadValidatePRsCmd lista PRs abiertos del repo vía gh.
func loadValidatePRsCmd() tea.Cmd {
	return func() tea.Msg {
		prs, err := validate.ListOpenPRs()
		return validatePRsLoadedMsg{prs: prs, err: err}
	}
}

// maybeAdvanceValidate transiciona a screenValidateSelect cuando los dos
// loaders respondieron. Política de errores: si ambas listas fallaron, vamos
// a resultError con los dos mensajes; si una falló y la otra devolvió items,
// seguimos con la que funcionó (el error de la otra se drop); si las dos
// cargaron OK pero ambas vacías, empty state informativo.
func (m Model) maybeAdvanceValidate() (tea.Model, tea.Cmd) {
	if !m.validatePlansLoaded || !m.validatePRsLoaded {
		return m, nil
	}
	// Ambas fallaron → error.
	if m.validatePlansErr != nil && m.validatePRsErr != nil {
		m.screen = screenResult
		m.resultKind = resultError
		m.resultLines = []string{
			"error cargando planes: " + m.validatePlansErr.Error(),
			"error cargando PRs: " + m.validatePRsErr.Error(),
		}
		return m, nil
	}
	// Si una falló pero la otra devolvió items, igual seguimos (la que falló
	// aparece como "(sin ítems)"). Si una falló y la otra no tiene items,
	// reportamos el error.
	if len(m.validatePlans)+len(m.validatePRs) == 0 {
		if m.validatePlansErr != nil || m.validatePRsErr != nil {
			m.screen = screenResult
			m.resultKind = resultError
			var lines []string
			if m.validatePlansErr != nil {
				lines = append(lines, "error cargando planes: "+m.validatePlansErr.Error())
			}
			if m.validatePRsErr != nil {
				lines = append(lines, "error cargando PRs: "+m.validatePRsErr.Error())
			}
			m.resultLines = lines
			return m, nil
		}
		m.screen = screenResult
		m.resultKind = resultInfo
		m.resultLines = []string{
			"No hay planes ni PRs para validar en este repo.",
			"Explorá un issue con `che explore` o abrí un PR con `che execute`.",
		}
		return m, nil
	}
	m.validateCursor = 0
	m.screen = screenValidateSelect
	return m, nil
}

// validateItemAt devuelve el item seleccionado según el cursor unificado.
// Si idx < len(plans), es un plan (isPR=false); si >= len(plans), es un PR.
// Devuelve (number, url, isPR).
func (m Model) validateItemAt(idx int) (int, string, bool) {
	if idx < len(m.validatePlans) {
		p := m.validatePlans[idx]
		return p.Number, p.URL, false
	}
	c := m.validatePRs[idx-len(m.validatePlans)]
	return c.Number, c.URL, true
}

func (m Model) handleValidateSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.validatePlans) + len(m.validatePRs)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.validatePlans = nil
		m.validatePRs = nil
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
		num, url, _ := m.validateItemAt(m.validateCursor)
		m.validateChosenRef = fmt.Sprint(num)
		m.validateChosenURL = url
		// Default: opus=1 (coherente con flag default).
		m.validateValidatorCount = map[validate.Agent]int{validate.AgentOpus: 1}
		m.validateValidatorCursor = 0
		m.screen = screenValidateValidators
		return m, nil
	}
	return m, nil
}

// handleValidateValidatorsKey maneja el stepper 0..N por agente:
//   - ↑/↓ (k/j): cambia el agente seleccionado (cursor vertical).
//   - ←/- (decrement): resta 1 al count del agente actual, con piso 0.
//   - →/+ (increment): suma 1 al count, con el cap global maxValidatorsTotal.
//     Si la suma ya es 3, no-op (feedback visual vía total con indicador).
//   - Enter: arranca el flow si total > 0; rechaza (no-op) si total == 0
//     para dar feedback temprano — el flow lo rechazaría igual con
//     ExitSemantic, pero mejor no dejar pasar.
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
	case "left", "h", "-":
		a := validate.ValidAgents[m.validateValidatorCursor]
		if m.validateValidatorCount[a] > 0 {
			m.validateValidatorCount[a]--
		}
		return m, nil
	case "right", "l", "+", "=":
		a := validate.ValidAgents[m.validateValidatorCursor]
		if validatorTotal(m.validateValidatorCount) < maxValidatorsTotal {
			m.validateValidatorCount[a]++
		}
		return m, nil
	case "enter":
		validators := validateValidatorsFromCounts(m.validateValidatorCount)
		total := len(validators)
		// validate REQUIERE al menos 1 validador (a diferencia de explore).
		if total < 1 || total > maxValidatorsTotal {
			return m, nil
		}
		return m.startValidateFlow(m.validateChosenRef, validators)
	}
	return m, nil
}

// validatorTotal suma los counts del mapa. Helper para el cap global del
// stepper (maxValidatorsTotal) — evita recomputar en cada tecla.
func validatorTotal(counts map[validate.Agent]int) int {
	total := 0
	for _, n := range counts {
		total += n
	}
	return total
}

// validateValidatorsFromCounts traduce el mapa de counts del TUI a una lista
// de validate.Validator con instance correcto en el orden canónico.
func validateValidatorsFromCounts(counts map[validate.Agent]int) []validate.Validator {
	return buildValidatorList(validate.ValidAgents, counts, func(a validate.Agent, inst int) validate.Validator {
		return validate.Validator{Agent: a, Instance: inst}
	})
}

// loadIteratePlansCmd lista issues con plan-validated:changes-requested
// — planes que pidieron cambios y esperan que iterate reescriba el plan.
func loadIteratePlansCmd() tea.Cmd {
	return func() tea.Msg {
		plans, err := iterate.ListIterablePlanCandidates()
		return iteratePlansLoadedMsg{plans: plans, err: err}
	}
}

// loadIteratePRsCmd lista PRs con validated:changes-requested — los que
// pidieron cambios y esperan que iterate los aplique sobre el diff.
func loadIteratePRsCmd() tea.Cmd {
	return func() tea.Msg {
		prs, err := iterate.ListIterable()
		return iteratePRsLoadedMsg{prs: prs, err: err}
	}
}

// maybeAdvanceIterate es el hermano de maybeAdvanceValidate: transiciona a
// screenIterateSelect cuando ambos loaders respondieron, con la misma
// política de errores (ambos fallaron = error; una falló pero la otra tiene
// items = seguimos; ambos vacíos = empty state informativo).
func (m Model) maybeAdvanceIterate() (tea.Model, tea.Cmd) {
	if !m.iteratePlansLoaded || !m.iteratePRsLoaded {
		return m, nil
	}
	if m.iteratePlansErr != nil && m.iteratePRsErr != nil {
		m.screen = screenResult
		m.resultKind = resultError
		m.resultLines = []string{
			"error cargando planes: " + m.iteratePlansErr.Error(),
			"error cargando PRs: " + m.iteratePRsErr.Error(),
		}
		return m, nil
	}
	if len(m.iteratePlans)+len(m.iteratePRs) == 0 {
		if m.iteratePlansErr != nil || m.iteratePRsErr != nil {
			m.screen = screenResult
			m.resultKind = resultError
			var lines []string
			if m.iteratePlansErr != nil {
				lines = append(lines, "error cargando planes: "+m.iteratePlansErr.Error())
			}
			if m.iteratePRsErr != nil {
				lines = append(lines, "error cargando PRs: "+m.iteratePRsErr.Error())
			}
			m.resultLines = lines
			return m, nil
		}
		m.screen = screenResult
		m.resultKind = resultInfo
		m.resultLines = []string{
			"No hay planes ni PRs pidiendo cambios.",
			"Corré `che validate` sobre un plan o PR y si pide cambios, volvé acá.",
		}
		return m, nil
	}
	m.iterateCursor = 0
	m.screen = screenIterateSelect
	return m, nil
}

// iterateItemAt devuelve el item seleccionado según el cursor unificado.
// Si idx < len(plans), es un plan; si >= len(plans), es un PR. Devuelve
// (number, url, isPR).
func (m Model) iterateItemAt(idx int) (int, string, bool) {
	if idx < len(m.iteratePlans) {
		p := m.iteratePlans[idx]
		return p.Number, p.URL, false
	}
	c := m.iteratePRs[idx-len(m.iteratePlans)]
	return c.Number, c.URL, true
}

func (m Model) handleIterateSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.iteratePlans) + len(m.iteratePRs)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.iteratePlans = nil
		m.iteratePRs = nil
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
		num, url, _ := m.iterateItemAt(m.iterateCursor)
		m.iterateChosenRef = fmt.Sprint(num)
		m.iterateChosenURL = url
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
		var stderr bytes.Buffer
		code := iterate.Run(prRef, iterate.Opts{
			Stdout: newStdoutLineWriter(ch),
			Out:    output.New(&eventSink{ch: ch}),
		})
		ch <- iterateDoneMsg{code: code, stdout: "", stderr: stderr.String()}
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
		var stderr bytes.Buffer
		code := closing.Run(prRef, closing.Opts{
			Stdout: newStdoutLineWriter(ch),
			Out:    output.New(&eventSink{ch: ch}),
		})
		ch <- closeDoneMsg{code: code, stdout: "", stderr: stderr.String()}
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
		var stderr bytes.Buffer
		code := validate.Run(prRef, validate.Opts{
			Stdout:     newStdoutLineWriter(ch),
			Out:        output.New(&eventSink{ch: ch}),
			Validators: validators,
		})
		ch <- validateDoneMsg{code: code, stdout: "", stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// startExecuteFlow arranca execute.Run en background sobre el issue elegido
// con el agente seleccionado.
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
		var stderr bytes.Buffer
		code := execute.Run(issueRef, execute.Opts{
			Stdout: newStdoutLineWriter(ch),
			Out:    output.New(&eventSink{ch: ch}),
			Agent:  agent,
			Ctx:    runCtx,
		})
		ch <- executeDoneMsg{code: code, stdout: "", stderr: stderr.String()}
	}(m.progressCh, runCtx)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

func loadExploreCandidatesCmd() tea.Cmd {
	return func() tea.Msg {
		newItems, err := explore.ListCandidates()
		if err != nil {
			return exploreCandidatesLoadedMsg{err: err}
		}
		return exploreCandidatesLoadedMsg{newItems: newItems}
	}
}

func (m Model) handleExploreSelectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	total := len(m.exploreNew)
	switch k {
	case "esc":
		m.screen = screenMenu
		m.exploreNew = nil
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
		chosen := m.exploreNew[m.exploreCursor]
		m.exploreChosenRef = fmt.Sprint(chosen.Number)
		m.exploreAgentIdx = 0
		m.screen = screenExploreAgent
		return m, nil
	}
	return m, nil
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
		return m.startExploreFlow(m.exploreChosenRef, m.exploreChosenAgent)
	}
	// Atajos numéricos 1..N para selección rápida del ejecutor.
	for i := range explore.ValidAgents {
		if k == fmt.Sprint(i+1) {
			m.exploreChosenAgent = explore.ValidAgents[i]
			return m.startExploreFlow(m.exploreChosenRef, m.exploreChosenAgent)
		}
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
	m.exploreNew = nil
	m.executeCandidates = nil
	m.validatePlans = nil
	m.validatePRs = nil
	m.closeReady = nil
	m.closeBlocked = nil
	m.iteratePlans = nil
	m.iteratePRs = nil
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
		var stderr bytes.Buffer
		code := idea.Run(text, idea.Opts{
			Stdout: newStdoutLineWriter(ch),
			Out:    output.New(&eventSink{ch: ch}),
		})
		ch <- flowDoneMsg{code: code, stdout: "", stderr: stderr.String()}
	}(m.progressCh)

	return m, tea.Batch(waitForMsg(m.progressCh), tickCmd())
}

// startExploreFlow arranca explore.Run en background sobre el issue elegido
// con el agente seleccionado. Mismo patrón async que startIdeaFlow.
func (m Model) startExploreFlow(issueRef string, agent explore.Agent) (tea.Model, tea.Cmd) {
	m.screen = screenExploreRunning
	m.runStart = time.Now()
	m.runLog = []string{}
	m.progressCh = make(chan tea.Msg, 64)

	go func(ch chan<- tea.Msg) {
		var stderr bytes.Buffer
		code := explore.Run(issueRef, explore.Opts{
			Stdout: newStdoutLineWriter(ch),
			Out:    output.New(&eventSink{ch: ch}),
			Agent:  agent,
		})
		ch <- exploreDoneMsg{code: code, stdout: "", stderr: stderr.String()}
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
	sb.WriteString(titleStyle.Render("Validar"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando planes pendientes y PRs abiertos…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

// renderValidateSelect muestra dos listas separadas (planes pendientes +
// PRs abiertos) con cursor unificado. Índices 0..len(plans)-1 son planes;
// el resto son PRs. El render mantiene el orden visual (planes arriba, PRs
// abajo) y ubica el marcador ▸ en el item correspondiente al cursor global.
func renderValidateSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Validar — elegí qué validar"))
	sb.WriteString("\n")
	totalPlans := len(m.validatePlans)
	totalPRs := len(m.validatePRs)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf(
		"%d plan(es) pendiente(s) · %d PR(s) abierto(s)", totalPlans, totalPRs)))
	sb.WriteString("\n\n")

	// Planes pendientes (primer grupo; cursor 0..len(plans)-1).
	sb.WriteString("  " + mutedBadge("— Planes pendientes —") + "\n")
	if totalPlans == 0 {
		sb.WriteString("  " + comingSoonStyle.Render("(sin ítems)") + "\n")
	} else {
		for i, p := range m.validatePlans {
			selected := i == m.validateCursor
			sb.WriteString(planCandidateLine(p, selected))
		}
	}

	sb.WriteString("\n")

	// PRs abiertos (segundo grupo; cursor len(plans)..total-1).
	sb.WriteString("  " + mutedBadge("— PRs abiertos —") + "\n")
	if totalPRs == 0 {
		sb.WriteString("  " + comingSoonStyle.Render("(sin ítems)") + "\n")
	} else {
		for i, c := range m.validatePRs {
			selected := (i + totalPlans) == m.validateCursor
			sb.WriteString(prValidateCandidateLine(c, selected))
		}
	}

	sb.WriteString("\n")
	if totalPlans+totalPRs == 0 {
		sb.WriteString(hintStyle.Render(
			"no hay planes ni PRs para validar — explorá algo primero con `che explore` · Esc vuelve"))
	} else {
		sb.WriteString(hintStyle.Render("↑/↓ navega · Enter elige · Esc vuelve · Ctrl+C sale"))
	}
	sb.WriteString("\n")
	return sb.String()
}

// planCandidateLine renderea un item de la lista de planes pendientes.
// Mantenemos el formato minimal (número + título) porque PlanCandidate no
// trae autor/draft/closes como los PRs.
func planCandidateLine(p validate.PlanCandidate, selected bool) string {
	prefix := "  "
	style := menuItemStyle
	if selected {
		prefix = "▸ "
		style = menuSelectedStyle
	}
	num := menuNumberStyle.Render(fmt.Sprintf("#%d", p.Number))
	return style.Render(prefix+num+"  "+p.Title) + "\n"
}

// prValidateCandidateLine renderea un PR en el selector de validate. Mismo
// formato que el render anterior (draft badge, closes, author), pero como
// helper separado para que la lista combinada de planes+PRs sea legible.
func prValidateCandidateLine(c validate.Candidate, selected bool) string {
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

// renderValidateValidators rendea el panel de validadores como un stepper
// 0..N por agente. El indicador de total aparece arriba (Total: X / 3) para
// que el cap global sea obvio; el selector lleva ▸ al agente actual y ←→
// ajustan su count.
func renderValidateValidators(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Panel de validadores"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf(
		"Ref #%s — al menos 1, máximo %d en total",
		m.validateChosenRef, maxValidatorsTotal)))
	sb.WriteString("\n\n")

	total := validatorTotal(m.validateValidatorCount)
	sb.WriteString("  " + renderValidateStepperTotal(total) + "\n\n")

	for i, a := range validate.ValidAgents {
		count := m.validateValidatorCount[a]
		box := renderStepper(count)
		prefix := "  "
		style := menuItemStyle
		if i == m.validateValidatorCursor {
			prefix = "▸ "
			style = menuSelectedStyle
		}
		name := padRight(strings.ToUpper(string(a)[:1])+string(a)[1:], 8)
		line := prefix + name + " " + box
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
	sb.WriteString(hintStyle.Render(
		"↑/↓ elegí agente · ←/→ (o -/+) ajustá · Enter empezar · Esc volver · Ctrl+C sale"))
	sb.WriteString("\n")
	return sb.String()
}

// renderStepper rendea el box [ N ] del stepper. Mantenemos padding fijo
// (tres chars entre los brackets) para que el layout no se desalinee.
func renderStepper(count int) string {
	return fmt.Sprintf("[ %d ]", count)
}

// padRight alinea el nombre del agente para que los steppers queden
// alineados verticalmente en el render. Los nombres son cortos (opus=4,
// codex=5, gemini=6) así que padding fijo a 8 cubre el peor caso con margen.
func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// renderValidateStepperTotal arma el header "Total: X / 3" con color según
// validez (rojo si 0 o > 3, verde si 1..3). Mismo criterio que el renderer
// anterior, pero reflejando el cap total explícito del stepper.
func renderValidateStepperTotal(total int) string {
	note := ""
	valid := total >= 1 && total <= maxValidatorsTotal
	if total == 0 {
		note = "  (elegí al menos 1)"
	} else if total >= maxValidatorsTotal {
		note = "  (cap alcanzado)"
	}
	style := successStyle
	if !valid {
		style = errorStyle
	}
	return style.Render(fmt.Sprintf("Total: %d / %d", total, maxValidatorsTotal)) +
		comingSoonStyle.Render(note)
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

func renderExploreSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Explorar"))
	sb.WriteString("\n")

	total := len(m.exploreNew)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf("%d idea(s) sin explorar", total)))
	sb.WriteString("\n\n")

	if total == 0 {
		sb.WriteString("  " + mutedBadge("(ninguna)") + "\n")
	} else {
		for i, c := range m.exploreNew {
			sb.WriteString(exploreCandidateLine(c, i == m.exploreCursor))
		}
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
	switch m.resultKind {
	case resultSuccess:
		sb.WriteString(successStyle.Render("✓ Listo"))
	case resultError:
		sb.WriteString(errorStyle.Render("✗ Error"))
	default: // resultInfo
		sb.WriteString(titleStyle.Render("Sin resultados"))
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

	// Detalle final: URLs creadas / mensaje de error / explicación del
	// empty state. El header "Resultado:" no tiene sentido para info
	// (empty state) — ahí las lineas ya son la explicación completa.
	if len(m.resultLines) > 0 {
		sb.WriteString("\n")
		if m.resultKind != resultInfo {
			sb.WriteString(subtitleStyle.Render("Resultado:"))
			sb.WriteString("\n")
		}
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
	sb.WriteString(titleStyle.Render("Iterar"))
	sb.WriteString("\n")
	sb.WriteString(subtitleStyle.Render("Buscando planes y PRs con changes-requested…"))
	sb.WriteString("\n")
	sb.WriteString(hintStyle.Render("Ctrl+C cancela"))
	sb.WriteString("\n")
	return sb.String()
}

// renderIterateSelect muestra planes + PRs con changes-requested con cursor
// unificado, mismo layout que renderValidateSelect.
func renderIterateSelect(m Model) string {
	var sb strings.Builder
	sb.WriteString(titleStyle.Render("Iterar — elegí qué iterar"))
	sb.WriteString("\n")
	totalPlans := len(m.iteratePlans)
	totalPRs := len(m.iteratePRs)
	sb.WriteString(subtitleStyle.Render(fmt.Sprintf(
		"%d plan(es) con changes-requested · %d PR(s) con changes-requested — opus aplica los cambios",
		totalPlans, totalPRs)))
	sb.WriteString("\n\n")

	sb.WriteString("  " + mutedBadge("— Planes a iterar —") + "\n")
	if totalPlans == 0 {
		sb.WriteString("  " + comingSoonStyle.Render("(sin ítems)") + "\n")
	} else {
		for i, p := range m.iteratePlans {
			selected := i == m.iterateCursor
			sb.WriteString(planCandidateLine(p, selected))
		}
	}

	sb.WriteString("\n")

	sb.WriteString("  " + mutedBadge("— PRs a iterar —") + "\n")
	if totalPRs == 0 {
		sb.WriteString("  " + comingSoonStyle.Render("(sin ítems)") + "\n")
	} else {
		for i, c := range m.iteratePRs {
			selected := (i + totalPlans) == m.iterateCursor
			sb.WriteString(closeCandidateLine(c, selected))
		}
	}

	sb.WriteString("\n")
	if totalPlans+totalPRs == 0 {
		sb.WriteString(hintStyle.Render(
			"no hay planes ni PRs pidiendo cambios — corré `che validate` primero · Esc vuelve"))
	} else {
		sb.WriteString(hintStyle.Render("↑/↓ navega · Enter dispara · Esc vuelve · Ctrl+C sale"))
	}
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
