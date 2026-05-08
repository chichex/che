package wizard

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// openSaveChoice valida el step actual y, si esta OK, abre el modal de
// eleccion. Se dispara con enter sobre el ultimo foco de S2 para que el
// usuario tenga que decidir explicitamente entre "agregar otro step" y
// "finalizar pipeline" — el comportamiento previo (enter = cerrar
// wizard) cerraba de pecho sin avisar.
//
// ctrl+s y ctrl+n siguen siendo atajos directos sin modal: son ordenes
// explicitas del usuario que ya entiende lo que esta haciendo.
//
// En mode=edit el modal solo ofrece "finalizar" + "volver" (no tiene
// sentido "agregar otro" desde el medio de la lista — eso lo cubre H7
// con "+" desde S3).
func (m model) openSaveChoice() (model, tea.Cmd) {
	// Validamos antes de abrir el modal: si hay un error de buildStep, no
	// queremos preguntar — primero el usuario corrige.
	if _, err := m.buildStep(); err != nil {
		m.stepEdit.errMsg = err.Error()
		return m, nil
	}
	// default seguro: agregar otro step en mode=create (la mayoria de los
	// pipelines reales tienen >1 step), finalizar en mode=edit (no podemos
	// agregar desde el medio).
	if m.stepEdit.mode == "create" {
		m.saveCursor = SaveAddAnother
	} else {
		m.saveCursor = SaveFinish
	}
	m.screen = ScreenSaveChoice
	return m, nil
}

func (m model) updateSaveChoice(key tea.KeyMsg) (model, tea.Cmd) {
	editMode := m.stepEdit.mode == "edit"
	switch key.String() {
	case "esc":
		m.screen = ScreenStep
		return m, nil
	case "ctrl+c":
		return m.openCancel(ScreenStep)
	case "up", "k":
		if editMode {
			// solo Finish + Back; ciclar entre los 2.
			if m.saveCursor == SaveBack {
				m.saveCursor = SaveFinish
			}
			return m, nil
		}
		if m.saveCursor > SaveAddAnother {
			m.saveCursor--
		}
		return m, nil
	case "down", "j":
		if editMode {
			if m.saveCursor == SaveFinish {
				m.saveCursor = SaveBack
			}
			return m, nil
		}
		if m.saveCursor < SaveBack {
			m.saveCursor++
		}
		return m, nil
	case "1":
		if editMode {
			m.saveCursor = SaveFinish
		} else {
			m.saveCursor = SaveAddAnother
		}
		return m.applySaveChoice()
	case "2":
		if editMode {
			m.saveCursor = SaveBack
		} else {
			m.saveCursor = SaveFinish
		}
		return m.applySaveChoice()
	case "3":
		if editMode {
			// no hay opcion 3 en edit
			return m, nil
		}
		m.saveCursor = SaveBack
		return m.applySaveChoice()
	case "enter", " ":
		return m.applySaveChoice()
	}
	return m, nil
}

func (m model) applySaveChoice() (model, tea.Cmd) {
	switch m.saveCursor {
	case SaveAddAnother:
		// Equivalente a ctrl+n: solo aplica en mode=create.
		if m.stepEdit.mode != "create" {
			m.screen = ScreenStep
			return m, nil
		}
		m.screen = ScreenStep
		return m.stepSaveAndAddAnother()
	case SaveFinish:
		m.screen = ScreenStep
		return m.stepSaveAndFinish()
	case SaveBack:
		m.screen = ScreenStep
		return m, nil
	}
	return m, nil
}

func (m model) viewSaveChoice() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Step listo"))
	b.WriteString("\n\n")
	b.WriteString("¿Que querés hacer ahora?\n\n")

	editMode := m.stepEdit.mode == "edit"

	type opt struct {
		choice SaveChoice
		digit  string
		label  string
		hint   string
	}

	var options []opt
	if editMode {
		options = []opt{
			{SaveFinish, "1", "finalizar pipeline", "guarda los cambios y cierra (placeholder de S3)"},
			{SaveBack, "2", "volver a editar", "seguir tocando este step"},
		}
	} else {
		options = []opt{
			{SaveAddAnother, "1", "agregar otro step", "guarda este, abre uno nuevo a continuacion"},
			{SaveFinish, "2", "finalizar pipeline", "guarda este y cierra (placeholder de S3)"},
			{SaveBack, "3", "volver a editar", "seguir tocando este step"},
		}
	}

	for _, o := range options {
		line := "  " + o.digit + ". " + o.label + "  " + dimStyle.Render(o.hint)
		if m.saveCursor == o.choice {
			line = selectedItem.Render("> "+o.digit+". "+o.label) + "  " + dimStyle.Render(o.hint)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter confirmar · esc volver"))
	b.WriteString("\n")
	return modalBorder.Render(b.String())
}
