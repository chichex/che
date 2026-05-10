package wizard

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/wizard/promptreview"
)

// promptReviewMsg llega cuando termina la corrida async de promptreview.Run
// disparada al confirmar un step con kind=prompt. El handler decide
// transicionar al modal o (si OK=true sin issues) saltearlo y guardar.
type promptReviewMsg struct {
	review promptreview.Review
	err    error
}

// runPromptReviewCmd corre promptreview.Run en un goroutine y dispatchea
// promptReviewMsg al terminar. Asi el modal puede renderear "Analizando…"
// inmediatamente sin bloquear el TUI.
func runPromptReviewCmd(prompt string) tea.Cmd {
	return func() tea.Msg {
		r, err := promptreview.Run(prompt)
		return promptReviewMsg{review: r, err: err}
	}
}

// startPromptReview abre el modal en estado loading + dispara la review.
// pendingAction es "finish" o "addanother" — define que camino tomar
// cuando el usuario apruebe (con o sin sugerencia aplicada).
func (m model) startPromptReview(pendingAction string) (model, tea.Cmd) {
	m.stepEdit.reviewLoading = true
	m.stepEdit.review = promptReviewResult{}
	m.stepEdit.reviewErr = ""
	m.stepEdit.pendingSaveAction = pendingAction
	m.screen = ScreenStepReview
	prompt := strings.TrimSpace(m.stepEdit.contentInput.Value())
	return m, runPromptReviewCmd(prompt)
}

// handlePromptReviewResult llega cuando la review async termina. Si fue
// OK (sin issues), saltamos el modal y vamos directo al save. Si hubo
// error, lo dejamos visible en el modal (el usuario decide guardar igual
// o volver). Si hay issues, el modal se queda con el resumen.
func (m model) handlePromptReviewResult(msg promptReviewMsg) (model, tea.Cmd) {
	m.stepEdit.reviewLoading = false
	if msg.err != nil {
		m.stepEdit.reviewErr = msg.err.Error()
		m.stepEdit.review = promptReviewResult{Raw: msg.review.Raw}
		// No avanzamos automaticamente — dejamos al usuario decidir
		// (puede ser un fallo de red transitorio o claude no instalado).
		return m, nil
	}
	m.stepEdit.review = promptReviewResult{
		OK:        msg.review.OK,
		Issues:    msg.review.Issues,
		Summary:   msg.review.Summary,
		Suggested: msg.review.Suggested,
		Raw:       msg.review.Raw,
	}
	if msg.review.OK && len(msg.review.Issues) == 0 {
		// Review verde: skipear el modal y persistir el step.
		return m.applyPendingSaveAction()
	}
	return m, nil
}

// updateStepReview maneja teclas dentro del modal "Review del prompt".
// Estados:
//   - loading: solo esc / ctrl+c (cancelar review, volver al step).
//   - resultado: 1=guardar igual / 2=aplicar sugerencia + guardar /
//     3=volver y editar / r=re-review / esc=volver.
func (m model) updateStepReview(key tea.KeyMsg) (model, tea.Cmd) {
	if m.stepEdit.reviewLoading {
		switch key.String() {
		case "esc", "ctrl+c":
			// Cancelar la review: volver al step. La goroutine va a seguir
			// corriendo en background — su msg se ignora porque estamos
			// fuera de ScreenStepReview cuando llegue (handlePromptReviewResult
			// solo aplica state si la screen sigue siendo Review).
			m.screen = ScreenStep
			m.stepEdit.reviewLoading = false
			return m, nil
		}
		return m, nil
	}

	switch key.String() {
	case "esc":
		m.screen = ScreenStep
		return m, nil
	case "ctrl+c":
		return m.openCancel(ScreenStep)
	case "1":
		// Guardar igual — ignorar review, persistir con el prompt actual.
		return m.applyPendingSaveAction()
	case "2":
		// Aplicar sugerencia (si la hay) + guardar.
		if m.stepEdit.review.Suggested == "" {
			return m, nil
		}
		m.stepEdit.contentInput.SetValue(m.stepEdit.review.Suggested)
		return m.applyPendingSaveAction()
	case "3":
		m.screen = ScreenStep
		return m, nil
	case "r":
		// Re-review con el prompt actual (que pudo haber cambiado si el
		// usuario fue para atras y editó).
		return m.startPromptReview(m.stepEdit.pendingSaveAction)
	}
	return m, nil
}

// applyPendingSaveAction reanuda el flow de save segun pendingSaveAction.
// Limpiamos los flags de review antes para que el siguiente step (si
// lo hay) arranque limpio.
func (m model) applyPendingSaveAction() (model, tea.Cmd) {
	action := m.stepEdit.pendingSaveAction
	m.stepEdit.pendingSaveAction = ""
	m.stepEdit.reviewLoading = false
	switch action {
	case "addanother":
		return m.stepSaveAndAddAnotherSkippingReview()
	default:
		return m.stepSaveAndFinishSkippingReview()
	}
}

// viewStepReview renderea el modal. Loading muestra "Analizando…"; con
// resultado muestra resumen + issues + sugerencia (si la hay) + opciones.
func (m model) viewStepReview() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Create pipeline", "Review del prompt"))
	b.WriteString("\n\n")

	if m.stepEdit.reviewLoading {
		b.WriteString(dimStyle.Render("  Analizando prompt con claude…"))
		b.WriteString("\n\n")
		b.WriteString(hintStyle.Render("esc cancelar"))
		b.WriteString("\n")
		return modalBorder.Render(b.String())
	}

	if m.stepEdit.reviewErr != "" {
		b.WriteString(errorStyle.Render("✗ no se pudo correr la review: " + m.stepEdit.reviewErr))
		b.WriteString("\n\n")
		b.WriteString("Opciones:\n")
		b.WriteString("  1. guardar igual (ignorar review)\n")
		b.WriteString("  3. volver y editar\n")
		b.WriteString("  r reintentar review\n")
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("1 / 3 / r · esc volver"))
		b.WriteString("\n")
		return modalBorder.Render(b.String())
	}

	if m.stepEdit.review.OK && len(m.stepEdit.review.Issues) == 0 {
		// Caso raro (el OK true se auto-saltea), pero por completitud.
		b.WriteString(okStyleWizard.Render("✓ review ok"))
		b.WriteString("\n\n")
		b.WriteString(m.stepEdit.review.Summary)
		b.WriteString("\n\n")
		b.WriteString(hintStyle.Render("1 guardar · esc volver"))
		b.WriteString("\n")
		return modalBorder.Render(b.String())
	}

	// Resumen breve + issues compactos.
	if n := len(m.stepEdit.review.Issues); n > 0 {
		b.WriteString(labelStyle.Render("Issues encontrados:"))
		b.WriteString("\n")
		for _, it := range m.stepEdit.review.Issues {
			b.WriteString("  · ")
			b.WriteString(it)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if s := strings.TrimSpace(m.stepEdit.review.Summary); s != "" {
		b.WriteString(dimStyle.Render(WrapText(s, ContentInnerWidth(m.width))))
		b.WriteString("\n\n")
	}
	if s := strings.TrimSpace(m.stepEdit.review.Suggested); s != "" {
		b.WriteString(labelStyle.Render("Sugerencia:"))
		b.WriteString("\n")
		b.WriteString(inputBoxBorder.Render(WrapText(s, ContentInnerWidth(m.width))))
		b.WriteString("\n\n")
	}

	b.WriteString("Opciones:\n")
	b.WriteString("  1. guardar igual (ignorar review)\n")
	if m.stepEdit.review.Suggested != "" {
		b.WriteString("  2. aplicar sugerencia y guardar\n")
	}
	b.WriteString("  3. volver y editar\n")
	b.WriteString("  r re-review (tras editar)\n")
	b.WriteString("\n")
	hint := "1 / 3 / r · esc volver"
	if m.stepEdit.review.Suggested != "" {
		hint = "1 / 2 / 3 / r · esc volver"
	}
	b.WriteString(hintStyle.Render(hint))
	b.WriteString("\n")
	return modalBorder.Render(b.String())
}
