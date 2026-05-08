package wizard

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Init es no-op: el wizard arranca renderizando con el modelo inicial.
func (m model) Init() tea.Cmd { return nil }

// Update dispatchea al handler de la screen actual. Las transiciones
// entre screens se hacen en cada handler escribiendo m.screen.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch m.screen {
	case ScreenInfo:
		return m.updateInfo(key)
	case ScreenStep:
		return m.updateStep(key)
	case ScreenSummary:
		return m.updateSummary(key)
	case ScreenSaved:
		return m.updateSaved(key)
	case ScreenSaveChoice:
		return m.updateSaveChoice(key)
	case ScreenCancel:
		return m.updateCancel(key)
	case ScreenCollision:
		return m.updateCollision(key)
	}
	return m, nil
}

// View dispatchea al renderer de la screen actual.
func (m model) View() string {
	switch m.screen {
	case ScreenInfo:
		return m.viewInfo()
	case ScreenStep:
		return m.viewStep()
	case ScreenSummary:
		return m.viewSummary()
	case ScreenSaved:
		return m.viewSaved()
	case ScreenSaveChoice:
		return m.viewSaveChoice()
	case ScreenCancel:
		return m.viewCancel()
	case ScreenCollision:
		return m.viewCollision()
	}
	return ""
}

// newModel construye el modelo inicial con el HOME indicado. home=""
// significa "usar $HOME real"; los tests inyectan tmp dir.
func newModel(home string) model {
	return model{
		screen:    ScreenInfo,
		focus:     FocusName,
		nameInput: newSingleLine("ej: Triage checkout flow"),
		descInput: newMultiLine("ej: toma una metrica anomala y dispara un triage"),
		homeDir:   home,
	}
}

// Run levanta el wizard. Devuelve exitApp=true si el usuario pidio salida
// total (q / ctrl+c en placeholder, o "discard"/"keep" + ctrl+c en SC);
// false si el flujo termino "volviendo al menu". El error solo aparece
// si bubbletea no pudo arrancar (p.ej. stdout no es TTY).
func Run() (bool, error) {
	return runWithHome("")
}

// runWithHome es el entrypoint testeable: permite forzar HomeDir desde
// los tests sin tocar $HOME del proceso.
func runWithHome(home string) (bool, error) {
	final, err := tea.NewProgram(newModel(home)).Run()
	if err != nil {
		return false, err
	}
	m, ok := final.(model)
	if !ok {
		return true, nil
	}
	return m.exitApp, nil
}
