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

	m := RunModel{
		Pipeline: p,
		path:     path,
	}
	m = m.enterFirstScreen()

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return false, err
	}
	mm, ok := final.(RunModel)
	if !ok {
		// Tipo inesperado del program — tratamos como exit total para no
		// devolver al usuario a un loop infinito sobre el lister.
		return true, nil
	}
	return mm.exitApp, nil
}

// enterFirstScreen elige la screen inicial segun el kind del input del step 0.
// kind=none → ScreenSecondary (skip de R1, segun el doc). Cualquier otro kind
// (text/pr/issue/file/url) → ScreenInput. La inicializacion de inputUI vive
// en initInputUI para que H3+ pueda re-entrar a R1 desde RF (retry) sin
// duplicar la logica.
func (m RunModel) enterFirstScreen() RunModel {
	kind := firstInputKind(m.Pipeline)
	if kind == wizard.InputNone {
		m.Screen = ScreenSecondary
		m.Input = InputState{Kind: kind}
		return m
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

// Init satisface tea.Model. Las screens H1/H2 no tienen side-effects al
// arrancar (sin tickers, sin async load, sin subprocess) — devolver nil es lo
// correcto.
func (m RunModel) Init() tea.Cmd { return nil }

// Update dispatchea segun la screen activa. Las teclas globales (esc, q,
// ctrl+c) las maneja el handler de cada screen para poder distinguir
// "volver al lister" vs "salida total" segun el contexto (p.ej. en R1 ctrl+c
// sale total, esc vuelve al lister).
func (m RunModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch m.Screen {
	case ScreenInput:
		return m.updateInput(key)
	case ScreenSecondary:
		return m.updateSecondary(key)
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

// View dispatchea segun la screen activa. H2 cubre R1 + R2-placeholder; el
// fallback es la screen skeleton heredada de H1 (no se usa en el flow real
// pero la dejamos como red de seguridad si una transicion futura olvida
// setear Screen).
func (m RunModel) View() string {
	switch m.Screen {
	case ScreenInput:
		return m.viewInput()
	case ScreenSecondary:
		return m.viewSecondary()
	}
	return m.viewSkeleton()
}

// viewSkeleton es el render legacy de H1 — placeholder generico. Solo se
// usa como fallback de View; el flow real arranca en R1 o ScreenSecondary.
func (m RunModel) viewSkeleton() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Run · " + name))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("runner pendiente — H3+ implementa preflight/running/done"))
	b.WriteString("\n\n")
	b.WriteString(hintStyle.Render("esc volver · q salir"))
	b.WriteString("\n")
	return b.String()
}

// updateSecondary maneja teclas en el placeholder de R2. esc vuelve al
// lister; q/ctrl+c sale total. enter no hace nada todavia — H3 lo va a
// usar para arrancar los chequeos de preflight.
func (m RunModel) updateSecondary(key tea.KeyMsg) (tea.Model, tea.Cmd) {
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

// viewSecondary renderiza el placeholder de R2. Texto literal "ok,
// siguiente: preflight (placeholder)" — el smoke manual de H2 lo busca,
// y los tests e2e tambien.
func (m RunModel) viewSecondary() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Run · " + name))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("ok, siguiente: preflight (placeholder)"))
	b.WriteString("\n\n")
	if m.Input.Kind != wizard.InputNone && m.Input.Kind != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("input resuelto: kind=%s · %d bytes", m.Input.Kind, len(m.Input.ResolvedPayload))))
		b.WriteString("\n\n")
	}
	b.WriteString(hintStyle.Render("esc volver · q salir"))
	b.WriteString("\n")
	return b.String()
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
)
