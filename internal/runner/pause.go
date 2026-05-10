// pause.go implementa el modal RP (Paused) de H7. Se abre cuando el
// validator de un step agoto max_loops y on_max_loops=pause: el subprocess
// del validator ya no esta vivo (esperamos decision humana — los outputs ya
// estan escritos en disco). El modal se renderea ENCIMA de R3 (sigue el
// patron del cancel modal — no es un screen aparte).
//
// Las 3 opciones del modal:
//   - continuar: aceptar el ultimo output del step, pasar al siguiente.
//     Manifest registra final_verdict: human-override.
//   - retry:     reset del contador y otro intento (consume max_loops nuevos).
//   - abort:     equivalente a on_max_loops: fail. Va a RF.
//
// El layout sigue el mockup del doc (3 opciones verticales con cursor `>`,
// el feedback del ultimo verdict arriba para que el humano decida con
// contexto).
package runner

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updatePauseModal maneja teclas mientras RP esta visible. up/down navega
// las 3 opciones; enter las dispara. esc NO cierra el modal — el doc lo
// deja abierto hasta que el humano decida. ctrl+c cae a abort (equivalente
// a fail).
//
// Los msgs no-tea.KeyMsg que pueden llegar mientras el modal esta abierto:
//   - validatorDoneMsg "rezagado": ya consumido al gatillar el modal,
//     defensive — lo descartamos (state.done=true ya).
//   - stepLineMsg / stepDoneMsg: no esperados (el step ya termino antes
//     del validator) — los descartamos defensivamente para no quedarnos
//     colgados.
func (m RunModel) updatePauseModal(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		// Defensive: cualquier msg no-key durante RP lo ignoramos. El
		// canal del runState ya esta cerrado (validator termino) — no
		// re-issuamos waitForLine.
		return m, nil
	}
	if m.PauseModal == nil {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.PauseModal.Choice > PauseChoiceContinue {
			m.PauseModal.Choice--
		}
		return m, nil
	case "down", "j":
		if m.PauseModal.Choice < PauseChoiceAbort {
			m.PauseModal.Choice++
		}
		return m, nil
	case "ctrl+c":
		// ctrl+c durante el pause cae a abort (equivalente a fail).
		m.PauseModal.Choice = PauseChoiceAbort
		return m.resolvePauseChoice()
	case "enter", " ":
		return m.resolvePauseChoice()
	}
	return m, nil
}

// resolvePauseChoice aplica la decision del humano segun PauseModal.Choice
// y devuelve el (model, cmd) listo para la siguiente transicion. Las 3
// ramas:
//
//   - continuar: registra final_verdict=human-override en el StepRun + cierra
//     el manifest snapshot del step y avanza al siguiente (o cae
//     a R4 si era el ultimo).
//   - retry:     reset del LoopsRun a 0 (pero NO de FinalVerdict — queda
//     vacio para que el loop arranque limpio). Re-spawnea el step
//     desde cero, reseteando ring buffer + StartedAt.
//   - abort:     final_verdict=fail + manifest cancelled como en
//     on_max_loops=fail. Va a RF.
func (m RunModel) resolvePauseChoice() (tea.Model, tea.Cmd) {
	if m.PauseModal == nil {
		return m, nil
	}
	idx := m.PauseModal.StepIdx
	if idx < 0 || idx >= len(m.Steps) {
		// Defensive: idx fuera de rango — caemos a abort.
		m.PauseModal = nil
		m.Screen = ScreenFailed
		m.FailedStderr = "pause modal con stepIdx fuera de rango"
		return m, nil
	}
	choice := m.PauseModal.Choice
	feedback := m.PauseModal.LastFeedback
	m.PauseModal = nil

	switch choice {
	case PauseChoiceContinue:
		// Aceptar el ultimo output como bueno. Step sigue done +
		// final_verdict=human-override.
		if m.Steps[idx].Validator != nil {
			m.Steps[idx].Validator.FinalVerdict = FinalVerdictHumanOverride
		}
		m.Steps[idx].Status = StepStatusDone
		return m.advanceAfterValidator(idx, m.Steps[idx].FinishedAt)

	case PauseChoiceRetry:
		// Reset del contador del validator + re-spawn del step. El
		// LoopsRun vuelve a 0 (la siguiente ronda contara desde 1) y
		// FinalVerdict queda vacio. El feedback se preserva como
		// LastFeedback para que el step lo pueda incorporar (loop
		// sintetico cuenta como "fail" para re-payload).
		if m.Steps[idx].Validator != nil {
			m.Steps[idx].Validator.LoopsRun = 0
			m.Steps[idx].Validator.FinalVerdict = ""
			// LastFeedback se mantiene — el wizard puede querer que
			// el siguiente run del step lo vea (mismo criterio que
			// un fail normal con loops < max).
			_ = feedback // ya esta en m.Steps[idx].Validator.LastFeedback
		}
		return rerunStepWithFeedback(m, idx)

	case PauseChoiceAbort:
		// Equivalente a on_max_loops=fail: final_verdict=fail, RF.
		if m.Steps[idx].Validator != nil {
			m.Steps[idx].Validator.FinalVerdict = FinalVerdictFail
		}
		m.Steps[idx].Status = StepStatusFailed
		m.FailedStderr = "validator agoto max_loops · usuario eligio abort\n" + feedback
		_ = closeManifest(m.RunDir, baseManifestForRun(m, m.Steps[idx].StartedAt), ManifestStatusFailed, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		return m, nil
	}
	return m, nil
}

// viewPauseModal renderea el modal RP. Layout: borde lipgloss + titulo +
// ultimo feedback (truncado a ~12 lineas para no romper el viewport) + 3
// opciones con cursor.
func viewPauseModal(m RunModel) string {
	var b strings.Builder
	if m.PauseModal == nil {
		return ""
	}
	b.WriteString(warnStyle.Render("┌─ Validator agoto max_loops ───────────────────┐"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("ultimo feedback del validator:"))
	b.WriteString("\n")
	feedback := strings.TrimSpace(m.PauseModal.LastFeedback)
	if feedback == "" {
		feedback = "(sin feedback)"
	}
	for _, line := range tail(feedback, 12) {
		b.WriteString("  ")
		b.WriteString(dimStyle.Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	options := []struct {
		choice PauseChoice
		label  string
	}{
		{PauseChoiceContinue, "continuar (acepta el ultimo output)"},
		{PauseChoiceRetry, "retry (resetea el contador y reintenta)"},
		{PauseChoiceAbort, "abort (cae a fail · RF)"},
	}
	for _, opt := range options {
		marker := "  "
		text := opt.label
		if opt.choice == m.PauseModal.Choice {
			marker = "> "
			text = pickerSelected.Render(text)
		}
		b.WriteString(marker)
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ elegir · enter confirmar"))
	b.WriteString("\n")
	return b.String()
}

