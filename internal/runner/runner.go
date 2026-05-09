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
// y arranca un bubbletea program que renderiza la screen skeleton de H1.
//
// Devuelve exitApp=true si el usuario pidio salida total (q / ctrl+c), false
// si volvio al lister (esc). En H1 no se toca disco fuera del Load — sin
// run-dir, sin manifest, sin subprocess. Esos llegan en H2+.
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
		Screen:   ScreenSkeleton,
		Pipeline: p,
		path:     path,
	}

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

// Init satisface tea.Model. La screen skeleton de H1 no tiene side-effects al
// arrancar (sin tickers, sin async load, sin subprocess) — devolver nil es lo
// correcto.
func (m RunModel) Init() tea.Cmd { return nil }

// Update reacciona a teclas. H1 solo maneja esc (volver al lister) y q/ctrl+c
// (salida total). El resto se ignora.
func (m RunModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
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

// View renderiza la screen skeleton: titulo "Run · <name>" + mensaje
// "runner pendiente" + hint con teclas. Mismo estilo que el resto de las
// pantallas TUI (titleStyle pink, hintStyle dim) — paleta dracula consistente.
func (m RunModel) View() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("Run · " + name))
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("runner pendiente — H2+ implementa input/preflight/running/done"))
	b.WriteString("\n\n")
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
)
