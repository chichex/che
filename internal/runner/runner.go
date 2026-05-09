package runner

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/chichex/che/internal/wizard"
)

// Run es el entrypoint del runner. Carga el pipeline desde disco, valida el
// shape con wizard.IsValid (mismo IsValid que usa S3 del wizard al guardar),
// y arranca un bubbletea program. H2 abre directo en R1 (InputPrompt)
// segun el kind del step 0; si es "none" salta a la pantalla siguiente
// (placeholder de R2 — H3 la reemplaza por preflight real).
//
// Devuelve exitApp=true si el usuario pidio salida total (q / ctrl+c), false
// si volvio al lister (esc). H1/H2 no escriben disco fuera del Load — sin
// run-dir, sin manifest, sin subprocess. Esos llegan en H3+.
//
// Si la carga o la validacion fallan, devuelve el error sin entrar al program.
// El caller (cmd/root.go.runMyPipelines) decide como surfacearlo. En H2+ esto
// se convertira en un toast inline sobre el lister; H1 deja la decision al
// caller para no inflar el contrato.
func Run(path string) (exitApp bool, err error) {
	p, err := wizard.Load(path)
	if err != nil {
		return false, fmt.Errorf("runner: load %s: %w", path, err)
	}
	if verr := wizard.IsValid(p); verr != nil {
		return false, fmt.Errorf("runner: pipeline invalido: %w", verr)
	}

	// Retry loop (H10): si el usuario pidio `r` en RF, re-armamos el
	// runner desde R1 con el input pre-cargado. El doc fija "crea un
	// run-id nuevo" — lo logramos reseteando RunID/RunDir y dejando que
	// enterRunning genere un nuevo timestamp + dir. Cada pasada es un
	// program nuevo (state limpio) — el ResolvedPayload del input se
	// preserva entre pasadas a traves de prevInput.
	var prevInput *InputState
	for {
		m := RunModel{
			Pipeline: p,
			path:     path,
		}
		if prevInput != nil {
			m.Input = *prevInput
		}
		m = m.enterFirstScreen()
		// Si tenemos un input pre-resuelto, pre-poblamos el textBuf de R1
		// para que el usuario pueda revisarlo / modificarlo antes del
		// retry. enterFirstScreen ya inicializo el inputUI segun el kind;
		// solo necesitamos sobrescribir el contenido.
		if prevInput != nil && m.Screen == ScreenInput && prevInput.Value != "" {
			m.inputUI.textBuf.runes = []rune(prevInput.Value)
			m.inputUI.textBuf.cursor = len(m.inputUI.textBuf.runes)
		}

		final, runErr := tea.NewProgram(m, tea.WithAltScreen()).Run()
		if runErr != nil {
			return false, runErr
		}
		mm, ok := final.(RunModel)
		if !ok {
			// Tipo inesperado del program — tratamos como exit total para
			// no devolver al usuario a un loop infinito sobre el lister.
			return true, nil
		}
		if mm.retryRequested && !mm.exitApp {
			// Re-loop con el input ya resuelto (no obligamos al usuario a
			// re-tipear / re-fetchear). Si el step 0 era input=none, no
			// hay nada que pre-cargar pero igual reintentamos desde R1
			// (que sera skipeado por enterFirstScreen).
			pi := mm.Input
			prevInput = &pi
			continue
		}
		return mm.exitApp, nil
	}
}

// enterFirstScreen elige la screen inicial segun el kind del input del step 0.
// kind=none → ScreenPreflight (skip de R1, segun el doc — preflight corre
// igual). Cualquier otro kind (text/pr/issue/file/url) → ScreenInput. La
// inicializacion de inputUI vive en initInputUI para que H4+ pueda re-entrar
// a R1 desde RF (retry) sin duplicar la logica.
func (m RunModel) enterFirstScreen() RunModel {
	kind := firstInputKind(m.Pipeline)
	if kind == wizard.InputNone {
		m.Input = InputState{Kind: kind}
		return enterPreflight(m)
	}
	m.Screen = ScreenInput
	m.Input = InputState{Kind: kind}
	m.inputUI = initInputUI(kind)
	return m
}

// firstInputKind devuelve el kind del input del primer step. Si el pipeline
// no tiene steps (no deberia llegar aca — IsValid lo rechaza), devuelve
// InputNone como fallback seguro.
func firstInputKind(p wizard.Pipeline) string {
	if len(p.Steps) == 0 {
		return wizard.InputNone
	}
	k := p.Steps[0].Input
	if k == "" {
		// Step sin input declarado — tratamos como none para no bloquear
		// el run pidiendo algo que el pipeline no especifico.
		return wizard.InputNone
	}
	return k
}

// Init satisface tea.Model. Si arrancamos en R1 con el picker de gh en modo
// loading (kind=pr|issue + repo activo), disparamos el fetch async para que
// el primer frame ya muestre "Cargando..." en vez de bloquear el render
// mientras corre `gh pr list`.
func (m RunModel) Init() tea.Cmd {
	if m.Screen == ScreenInput && m.inputUI.repoMode && m.inputUI.ghLoading {
		listKind := "pr"
		if m.inputUI.kind == wizard.InputIssue {
			listKind = "issue"
		}
		return loadGHListCmd(listKind)
	}
	return nil
}

// Update dispatchea segun la screen activa. Las teclas globales (esc, q,
// ctrl+c) las maneja el handler de cada screen para poder distinguir
// "volver al lister" vs "salida total" segun el contexto (p.ej. en R1 ctrl+c
// sale total, esc vuelve al lister). H4 agrega ScreenRunning (que tambien
// procesa stepDoneMsg, no solo teclas) y los terminales R4/RF.
//
// H10: el program ahora consume tea.WindowSizeMsg (resize del terminal) y
// editorReturnedMsg (vuelta de tea.ExecProcess en R4/RF). Ambos llegan
// fuera del flujo de teclas — los procesamos antes del switch por screen.
func (m RunModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Resize del terminal: aplica a todas las screens. Solo cacheamos el
	// shape — el render decide como reflowear (R3 lo usa para dimensionar
	// el log pane; las demas screens no dependen del width/height).
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.terminalWidth = ws.Width
		m.terminalHeight = ws.Height
		// ScreenRunning igual quiere ver el msg (no hay side-effects, pero
		// si en el futuro queremos un re-layout especifico, esto lo deja
		// abierto). Hoy alcanza con el cache + un re-render natural.
		return m, nil
	}
	// editorReturnedMsg llega tras tea.ExecProcess (y/l en R4 o l en RF).
	// El handler dedicado per screen es no-op — la TUI ya volvio al frente
	// y el View() siguiente se encarga del re-render.
	if er, ok := msg.(editorReturnedMsg); ok {
		switch m.Screen {
		case ScreenDone:
			return m.updateDoneEditorReturn(er)
		case ScreenFailed:
			return m.updateFailedEditorReturn(er)
		}
		return m, nil
	}
	// ghListLoadedMsg llega cuando el goroutine de loadGHListCmd termina —
	// solo aplica a R1 con picker. Lo manejamos antes del switch por
	// screen porque es un async load, no un evento de teclado.
	if loaded, ok := msg.(ghListLoadedMsg); ok {
		if m.Screen == ScreenInput {
			m.inputUI.ghLoading = false
			if loaded.err != nil {
				m.inputUI.ghLoadErr = loaded.err.Error()
			} else {
				m.inputUI.ghEntries = loaded.items
			}
		}
		return m, nil
	}
	// ScreenRunning recibe ademas de KeyMsg el stepDoneMsg de la goroutine
	// del spawn — por eso pasamos el msg crudo, no el key.
	if m.Screen == ScreenRunning {
		return m.updateRunning(msg)
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch m.Screen {
	case ScreenInput:
		return m.updateInput(key)
	case ScreenPreflight:
		return m.updatePreflight(key)
	case ScreenDone:
		return m.updateDone(key)
	case ScreenFailed:
		return m.updateFailed(key)
	}
	// ScreenSkeleton (legacy) o cualquier otro: comportamiento heredado de
	// H1 — esc vuelve al lister, q/ctrl+c salen total.
	switch key.String() {
	case "ctrl+c", "q":
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		m.exitApp = false
		return m, tea.Quit
	}
	return m, nil
}

// View dispatchea segun la screen activa. H4 cubre R1 / R2 / R3 / R4 / RF;
// el fallback es la screen skeleton heredada de H1 (no se usa en el flow
// real pero la dejamos como red de seguridad si una transicion futura
// olvida setear Screen).
func (m RunModel) View() string {
	switch m.Screen {
	case ScreenInput:
		return m.viewInput()
	case ScreenPreflight:
		return m.viewPreflight()
	case ScreenRunning:
		return m.viewRunning()
	case ScreenDone:
		return m.viewDone()
	case ScreenFailed:
		return m.viewFailed()
	}
	return m.viewSkeleton()
}

// viewSkeleton es el render legacy de H1 — placeholder generico. Solo se
// usa como fallback de View; el flow real arranca en R1 o ScreenSecondary.
func (m RunModel) viewSkeleton() string {
	var b strings.Builder
	b.WriteString(breadcrumb(runnerCrumb(m.Pipeline.Name)...))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("runner pendiente — H3+ implementa preflight/running/done"))
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("esc volver · q salir"))
	b.WriteString("\n")
	return b.String()
}

// breadcrumb arma el header "che › My pipelines › Run · <pipeline> ›
// <screen>" para el View() de cada screen del runner. El ultimo segmento
// queda en titleStyle (cyan bold); los previos + separadores en dimStyle
// (gris dracula) para que el ojo aterrice en la pantalla actual. parts NO
// incluye el root "che" — lo prependeamos aca asi todas las screens hablan
// la misma jerarquia.
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

// runnerCrumb es el helper interno que devuelve el path comun a todas las
// screens del runner: "My pipelines › Run · <pipeline>". Cada screen le
// appendea su propio segmento final ("Input · text", "Preflight",
// "Running", "Done", "Failed", "Cancel?", "Pause"). Asi el padre del
// runner queda en un solo lugar y rename del flow no obliga a tocar 6
// archivos.
func runnerCrumb(name string) []string {
	if name == "" {
		name = "(sin nombre)"
	}
	return []string{"My pipelines", "Run · " + name}
}

// Estilos locales del runner. Duplicados de internal/wizard/styles.go por
// el mismo motivo que el wizard duplica de internal/tui: evitar import
// circular cuando quiera estilar errores propios. Paleta dracula.
var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00D7FF"))
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2")).Bold(true)
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555")).Bold(true)

	inputBoxBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF79C6")).
			Foreground(lipgloss.Color("#F8F8F2")).
			Padding(0, 1)

	pickerSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Bold(true)
	pickerNormal   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2"))

	// stderrStyle es el render de las lineas de stderr en el log pane:
	// rojo dimmed, intercalado con stdout (criterio del doc de H5).
	// Sin Bold para diferenciarse del errorStyle (usado en errores
	// fatales).
	stderrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6E6E")).Faint(true)
)
