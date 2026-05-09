package runner

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// updateCancelModal maneja teclas mientras RC esta visible. up/down navega
// las 3 opciones; enter las dispara. esc cierra el modal y vuelve a R3.
//
// abort & save:    requestCancel + dejar que la goroutine SIGTERM al cmd.
//
//	La transicion a RF (con tono cancelado) la dispara
//	handleStepDone cuando llegue el stepDoneMsg.
//
// back to run:     m.CancelModal = false; el run sigue como estaba.
// exit che:        equivalente a abort & save + exitApp=true al volver
//
//	el msg done. Para H4 lo simplificamos: cancel +
//	exitApp inmediato, dejando que la goroutine cierre
//	el subprocess en background. handleStepDone con el
//	flag exitApp ya seteado va a tea.Quit en R4/RF —
//	pero como CLI fuerza el exit antes, no llegamos.
func (m RunModel) updateCancelModal(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		// stepDoneMsg puede llegar mientras el modal esta abierto si la
		// goroutine ya estaba cerrando — la procesamos igual para
		// transicionar a R4/RF aunque RC siga visible visualmente.
		if doneMsg, ok := msg.(stepDoneMsg); ok {
			m.CancelModal = false
			return m.handleStepDone(doneMsg)
		}
		// stepLineMsg puede seguir llegando del subprocess hasta que el
		// SIGTERM corte la salida. Drenamos appendeando al ring buffer
		// (el log pane queda actualizado por debajo del modal) y
		// re-issueamos el wait — sin esto el canal se llena y la
		// goroutine de tee se bloquea.
		if lineMsg, ok := msg.(stepLineMsg); ok {
			bufIdx := lineMsg.Idx - 1
			if bufIdx < 0 {
				bufIdx = 0
			}
			for len(m.LogBuffers) <= bufIdx {
				m.LogBuffers = append(m.LogBuffers, NewRingBuffer(2000))
			}
			kind := LogLineStdout
			if lineMsg.Line.Stderr {
				kind = LogLineStderr
			}
			m.LogBuffers[bufIdx].Append(kind, lineMsg.Line.Text)
			if m.runState != nil {
				return m, waitForLine(m.runState.lineCh)
			}
		}
		return m, nil
	}
	switch key.String() {
	case "esc":
		m.CancelModal = false
		return m, nil
	case "up", "k":
		if m.CancelChoice > CancelChoiceAbort {
			m.CancelChoice--
		}
		return m, nil
	case "down", "j":
		if m.CancelChoice < CancelChoiceExit {
			m.CancelChoice++
		}
		return m, nil
	case "enter", " ":
		switch m.CancelChoice {
		case CancelChoiceBack:
			m.CancelModal = false
			return m, nil
		case CancelChoiceAbort:
			requestStateCancel(m.runState)
			// Dejamos el modal abierto hasta que llegue stepDoneMsg
			// (handleStepDone lo cierra). El render mientras tanto
			// muestra "abortando..." para feedback inmediato.
			m.CancelChoice = CancelChoiceAbort
			return m, nil
		case CancelChoiceExit:
			requestStateCancel(m.runState)
			m.exitApp = true
			// tea.Quit acomoda; el program cierra el child por
			// SIGHUP cuando colapse el pty.
			return m, tea.Quit
		}
	}
	return m, nil
}

// requestStateCancel envia la senal de cancel al spawn goroutine. Es
// idempotente — un segundo enter sobre "abort" no rompe nada.
func requestStateCancel(state *runState) {
	if state == nil {
		return
	}
	select {
	case state.requestCancel <- struct{}{}:
	default:
	}
}

// viewCancelModal renderea las 3 opciones del modal con icono de
// seleccion. El estilo es minimal — bordes con lipgloss para que destaque
// sobre el log pane de R3.
func viewCancelModal(m RunModel) string {
	var b strings.Builder
	b.WriteString(errorStyle.Render("┌─ Cancelar run ────────────────────────────────┐"))
	b.WriteString("\n")
	options := []struct {
		choice CancelChoice
		label  string
	}{
		{CancelChoiceAbort, "abort & save"},
		{CancelChoiceBack, "back to run"},
		{CancelChoiceExit, "exit che"},
	}
	for _, opt := range options {
		marker := "  "
		text := opt.label
		if opt.choice == m.CancelChoice {
			marker = "> "
			text = pickerSelected.Render(text)
		}
		b.WriteString(marker + text + "\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ elegir · enter confirmar · esc cerrar modal"))
	b.WriteString("\n")
	return b.String()
}
