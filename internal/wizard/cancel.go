package wizard

import (
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// openCancel abre el modal SC. returnTo es la screen a la que volvemos si
// el usuario elige "back".
func (m model) openCancel(returnTo Screen) (model, tea.Cmd) {
	m.cancelReturn = returnTo
	m.cancelCursor = CancelBack // default seguro: no perder progreso
	m.screen = ScreenCancel
	return m, nil
}

// updateCancel maneja teclas dentro del modal SC.
func (m model) updateCancel(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "esc":
		// segundo esc = "back" (volver sin tocar)
		m.screen = m.cancelReturn
		return m, nil
	case "ctrl+c":
		// segundo ctrl+c = exit total. No tocamos disco; el archivo
		// queda con la ultima version persistida (si la habia).
		m.exitApp = true
		return m, tea.Quit
	case "up", "k":
		if m.cancelCursor > CancelKeep {
			m.cancelCursor--
		}
		return m, nil
	case "down", "j":
		if m.cancelCursor < CancelBack {
			m.cancelCursor++
		}
		return m, nil
	case "1":
		m.cancelCursor = CancelKeep
		return m.applyCancelChoice()
	case "2":
		m.cancelCursor = CancelDiscard
		return m.applyCancelChoice()
	case "3":
		m.cancelCursor = CancelBack
		return m.applyCancelChoice()
	case "enter", " ":
		return m.applyCancelChoice()
	}
	return m, nil
}

// applyCancelChoice ejecuta la opcion seleccionada.
func (m model) applyCancelChoice() (model, tea.Cmd) {
	switch m.cancelCursor {
	case CancelKeep:
		// Save final sincronico antes de salir. Si todavia no hay path
		// (S1 sin completar nombre), no hay nada que guardar — ya
		// volvemos al menu.
		//
		// Skip si el contenido en RAM ya coincide con el archivo en disco
		// (ignorando el bloque status / LastSavedAt). Esto evita que un
		// keep "vacio" pase un ready a draft cuando el usuario abrio
		// edit-ready, no toco nada, y aprieta esc — el archivo en disco
		// sigue ready, nuestro modelo en RAM tiene status.stage=summary
		// sembrado por RunEditReady. Sin este chequeo el Save aca lo
		// pasaria a draft sin justificativo (no hubo cambios reales).
		if m.path != "" {
			existing, lerr := Load(m.path)
			if lerr == nil && pipelinesEquivalentContent(existing, m.pipeline) {
				m.exitApp = false
				return m, tea.Quit
			}
			if m.pipeline.Status == nil {
				m.pipeline.Status = &Status{Stage: StageInfo}
			}
			m.pipeline.Status.LastSavedAt = time.Now()
			if err := Save(m.path, m.pipeline); err != nil {
				m.errMsg = "no se pudo guardar: " + err.Error()
				m.screen = m.cancelReturn
				return m, nil
			}
		}
		// volver al menu
		m.exitApp = false
		return m, tea.Quit
	case CancelDiscard:
		if m.path != "" {
			if m.originalReadySnapshot != nil {
				// edit-ready: "discard" = tirar mis cambios y volver al
				// estado ready original. Restauramos el archivo desde el
				// snapshot que tomamos al entrar (vale incluso si los
				// handlers persistieron un draft entre medio).
				if err := os.WriteFile(m.path, m.originalReadySnapshot, 0o600); err != nil {
					m.errMsg = "no se pudo restaurar el archivo: " + err.Error()
					m.screen = m.cancelReturn
					return m, nil
				}
			} else {
				// Resto de flows (creacion nueva, resume de draft):
				// discard borra el archivo, como dice la label.
				_ = Delete(m.path)
			}
		}
		m.exitApp = false
		return m, tea.Quit
	case CancelBack:
		m.screen = m.cancelReturn
		return m, nil
	}
	return m, nil
}

// viewCancel renderiza el modal SC.
func (m model) viewCancel() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Salir del wizard"))
	b.WriteString("\n\n")
	b.WriteString("¿Que querés hacer con el progreso actual?\n\n")

	discardLabel := "discard & exit"
	discardHint := "borra el archivo si existe"
	if m.originalReadySnapshot != nil {
		// En edit-ready la semantica es "tirar mis cambios", no "borrar
		// el pipeline" — la label tiene que reflejarlo o el usuario que
		// abrio un ready solo para mirar borra el archivo por accidente.
		discardLabel = "discard changes & exit"
		discardHint = "vuelve el archivo a su estado ready original"
	}
	options := []struct {
		choice CancelChoice
		digit  string
		label  string
		hint   string
	}{
		{CancelKeep, "1", "keep & exit", "guarda como draft, aparece en \"My pipelines\""},
		{CancelDiscard, "2", discardLabel, discardHint},
		{CancelBack, "3", "back", "volver y seguir editando"},
	}
	for _, o := range options {
		line := "  " + o.digit + ". " + o.label + "  " + dimStyle.Render(o.hint)
		if m.cancelCursor == o.choice {
			line = selectedItem.Render("> " + o.digit + ". " + o.label) + "  " + dimStyle.Render(o.hint)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter confirmar · esc volver"))
	b.WriteString("\n")
	return modalBorder.Render(b.String())
}

// updateCollision maneja teclas dentro del modal de colision de slug.
func (m model) updateCollision(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.screen = ScreenInfo
		return m, nil
	case "ctrl+c":
		return m.openCancel(ScreenInfo)
	case "up", "k", "left":
		m.collisionCursor = CollisionOverwrite
		return m, nil
	case "down", "j", "right":
		m.collisionCursor = CollisionCancel
		return m, nil
	case "1":
		m.collisionCursor = CollisionOverwrite
		return m.applyCollision()
	case "2":
		m.collisionCursor = CollisionCancel
		return m.applyCollision()
	case "enter", " ":
		return m.applyCollision()
	}
	return m, nil
}

func (m model) applyCollision() (model, tea.Cmd) {
	switch m.collisionCursor {
	case CollisionOverwrite:
		return m.confirmOverwrite()
	case CollisionCancel:
		m.screen = ScreenInfo
		return m, nil
	}
	return m, nil
}

func (m model) viewCollision() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("El nombre ya existe"))
	b.WriteString("\n\n")
	slug := Slug(m.nameInput.Value())
	b.WriteString("Ya hay un pipeline en ")
	b.WriteString(dimStyle.Render("~/.che/pipelines/" + slug + ".yaml"))
	b.WriteString(".\n\n")

	options := []struct {
		choice CollisionChoice
		digit  string
		label  string
		hint   string
	}{
		{CollisionOverwrite, "1", "sobreescribir", "se pierde el contenido previo"},
		{CollisionCancel, "2", "elegir otro", "volver a S1 a editar el nombre"},
	}
	for _, o := range options {
		line := "  " + o.digit + ". " + o.label + "  " + dimStyle.Render(o.hint)
		if m.collisionCursor == o.choice {
			line = selectedItem.Render("> " + o.digit + ". " + o.label) + "  " + dimStyle.Render(o.hint)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter confirmar · esc volver"))
	b.WriteString("\n")
	return modalBorder.Render(b.String())
}
