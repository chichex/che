package wizard

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updateSaved es el handler de S4. enter / esc cierran el wizard volviendo
// al menu (exitApp=false). ctrl+c pasa a ser salida total directa: el
// archivo ya esta guardado como ready, no hay nada que rescatar.
func (m model) updateSaved(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "enter", "esc", " ":
		m.exitApp = false
		return m, tea.Quit
	case "ctrl+c":
		m.exitApp = true
		return m, tea.Quit
	}
	return m, nil
}

// viewSaved renderiza S4. Render minimal — el usuario ya termino, no
// queremos competir visualmente con el menu al que vuelve.
func (m model) viewSaved() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Create pipeline", "Pipeline guardado"))
	b.WriteString("\n\n")
	b.WriteString(mutedItem.Render(m.pipeline.Name))
	b.WriteString("\n")
	if m.path != "" {
		b.WriteString(dimStyle.Render(m.path))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("enter / esc volver al menu"))
	b.WriteString("\n")
	return b.String()
}
