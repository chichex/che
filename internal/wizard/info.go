package wizard

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// updateInfo maneja teclas en S1.
func (m model) updateInfo(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		// q tambien aparece en la doc como trigger de SC, pero S1 tiene
		// inputs siempre focused y el usuario puede querer "queue" como
		// nombre — la dejamos como caracter normal. esc / ctrl+c siguen
		// siendo el camino de salida.
		return m.openCancel(ScreenInfo)
	case "esc":
		return m.openCancel(ScreenInfo)
	case "tab", "down":
		m.focus = nextFocus(m.focus)
		return m, nil
	case "shift+tab", "up":
		m.focus = prevFocus(m.focus)
		return m, nil
	case "ctrl+s":
		return m.tryAdvance()
	case "enter":
		// enter en el ultimo campo (Description) avanza a S2 — equivale
		// a ctrl+s. En los campos previos solo cyclea foco. Para newline
		// dentro del textarea de description: shift+enter, alt+enter o
		// ctrl+j (depende del terminal).
		if m.focus == FocusDescription {
			return m.tryAdvance()
		}
		m.focus = nextFocus(m.focus)
		return m, nil
	}

	return m.passKeyToFocused(key)
}

// passKeyToFocused entrega la tecla al input actualmente con foco. Si el
// input la consume, ya esta; si no, ignoramos.
func (m model) passKeyToFocused(key tea.KeyMsg) (model, tea.Cmd) {
	keyStr := key.String()
	switch m.focus {
	case FocusName:
		m.nameInput.HandleKey(keyStr, key.Runes)
	case FocusDescription:
		m.descInput.HandleKey(keyStr, key.Runes)
	}
	// borrar errMsg cuando el usuario empieza a tipear de nuevo
	if m.errMsg != "" {
		m.errMsg = ""
	}
	return m, nil
}

// tryAdvance es el handler de "siguiente" (ctrl+s) en S1. Valida nombre
// + slug, detecta colisiones, persiste el archivo con status:info, y
// pasa al placeholder de S2.
func (m model) tryAdvance() (model, tea.Cmd) {
	name := strings.TrimSpace(m.nameInput.Value())
	if name == "" {
		m.errMsg = "el nombre no puede estar vacio"
		return m, nil
	}
	slug := Slug(name)
	if slug == "" {
		m.errMsg = "el nombre debe contener al menos una letra o numero"
		return m, nil
	}

	path, err := PathFor(m.homeDir, slug)
	if err != nil {
		m.errMsg = "no se pudo resolver HOME: " + err.Error()
		return m, nil
	}

	// Si el path ya existe Y no es nuestro path actual (re-save del
	// mismo draft), abrimos el modal de colision.
	if path != m.path {
		exists, err := Exists(path)
		if err != nil {
			m.errMsg = "no se pudo chequear el archivo: " + err.Error()
			return m, nil
		}
		if exists {
			m.collisionCursor = CollisionOverwrite
			m.screen = ScreenCollision
			return m, nil
		}
	}

	m.path = path
	m.pipeline.Name = name
	m.pipeline.Description = strings.TrimSpace(m.descInput.Value())
	m.pipeline.Status = &Status{
		Stage:       StageInfo,
		LastSavedAt: time.Now(),
	}

	if err := Save(m.path, m.pipeline); err != nil {
		m.errMsg = "no se pudo guardar: " + err.Error()
		return m, nil
	}

	m.errMsg = ""
	// Entrar a S2 con step 0 mode=create. enterStepCreate persiste de
	// nuevo con status.stage=step para que el archivo refleje donde
	// estamos al instante.
	return m.enterStepCreate(0)
}

// confirmOverwrite es la rama "sobreescribir" del modal de colision —
// fuerza el save al path existente.
func (m model) confirmOverwrite() (model, tea.Cmd) {
	name := strings.TrimSpace(m.nameInput.Value())
	slug := Slug(name)
	path, err := PathFor(m.homeDir, slug)
	if err != nil {
		m.errMsg = "no se pudo resolver HOME: " + err.Error()
		m.screen = ScreenInfo
		return m, nil
	}
	m.path = path
	m.pipeline.Name = name
	m.pipeline.Description = strings.TrimSpace(m.descInput.Value())
	m.pipeline.Status = &Status{
		Stage:       StageInfo,
		LastSavedAt: time.Now(),
	}
	// El usuario eligio sobreescribir — limpiar steps del archivo previo
	// para evitar que persistan en la sesion del wizard nuevo.
	m.pipeline.Steps = nil
	if err := Save(m.path, m.pipeline); err != nil {
		m.errMsg = "no se pudo guardar: " + err.Error()
		m.screen = ScreenInfo
		return m, nil
	}
	m.errMsg = ""
	return m.enterStepCreate(0)
}

func nextFocus(f FieldFocus) FieldFocus {
	if f == FocusName {
		return FocusDescription
	}
	return FocusName
}

func prevFocus(f FieldFocus) FieldFocus {
	return nextFocus(f) // solo dos campos, prev y next son lo mismo
}

// viewInfo renderiza S1.
func (m model) viewInfo() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Create pipeline · paso 1/3"))
	b.WriteString("\n\n")

	b.WriteString(renderLabeledField("Nombre", m.nameInput, m.focus == FocusName))
	b.WriteString("\n")
	b.WriteString(renderLabeledField("Descripcion", m.descInput, m.focus == FocusDescription))
	b.WriteString("\n")

	if m.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("✗ " + m.errMsg))
		b.WriteString("\n")
	}

	if m.path != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(fmt.Sprintf("draft: %s", m.path)))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("enter/tab/↓ siguiente · shift+tab/↑ anterior · ctrl+s guardar · esc cancelar"))
	b.WriteString("\n")
	return b.String()
}

// renderLabeledField imprime label + caja con borde por encima/abajo del
// contenido. La caja deja en claro donde empieza y donde termina el input,
// evitando que el placeholder se confunda con texto ya tipeado.
func renderLabeledField(label string, t textInput, focused bool) string {
	style := inputBoxBorder
	labelText := labelStyle.Render(label)
	if focused {
		style = inputBoxBorderFocus
		labelText = labelText + dimStyle.Render("  ← foco")
	}
	return labelText + "\n" + style.Render(t.view(focused))
}
