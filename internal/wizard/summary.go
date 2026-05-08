package wizard

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// enterSummary entra a S3. Persiste el draft con status.stage=summary
// (mientras estamos en S3, el archivo en disco indica "esta a un ctrl+s
// del ready"). Si no hay steps llegaron aca por error de invariante —
// caemos a S2 mode=create como red de seguridad.
func (m model) enterSummary() (model, tea.Cmd) {
	if len(m.pipeline.Steps) == 0 {
		return m.enterStepCreate(0)
	}

	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageSummary
	m.pipeline.Status.StepIdx = 0
	m.pipeline.Status.StepMode = ""
	m.pipeline.Status.LastSavedAt = time.Now()

	if err := Save(m.path, m.pipeline); err != nil {
		// Persistencia best-effort: si fallamos al guardar el bloque
		// summary, igual mostramos S3 (el modelo en RAM esta intacto). El
		// error queda visible junto al resumen.
		m.summaryErrs = []string{"no se pudo guardar el draft: " + err.Error()}
	} else {
		m.summaryErrs = nil
	}

	if m.summaryCursor < 0 || m.summaryCursor >= len(m.pipeline.Steps) {
		m.summaryCursor = 0
	}
	m.screen = ScreenSummary
	return m, nil
}

// updateSummary maneja teclas en S3.
//
// ↑/↓ navegan los steps; e/d/shift+↑↓/+ accionan sobre el step apuntado
// (H7). ctrl+s valida y persiste el archivo final sin status; esc vuelve
// al ultimo step en mode=edit. ctrl+c abre SC, igual que el resto del
// wizard.
func (m model) updateSummary(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return m.openCancel(ScreenSummary)
	case "esc":
		// Vuelve a S2 sobre el ultimo step en mode=edit. El modelo en RAM
		// se mantiene; enterStepEdit pisa stepEdit y persiste status.stage
		// =step para reflejar el cambio en disco.
		if len(m.pipeline.Steps) == 0 {
			return m.enterStepCreate(0)
		}
		return m.enterStepEdit(len(m.pipeline.Steps) - 1)
	case "up", "k":
		if m.summaryCursor > 0 {
			m.summaryCursor--
		}
		return m, nil
	case "down", "j":
		if m.summaryCursor < len(m.pipeline.Steps)-1 {
			m.summaryCursor++
		}
		return m, nil
	case "shift+up":
		return m.summaryReorder(-1)
	case "shift+down":
		return m.summaryReorder(+1)
	case "e":
		// Edit del step apuntado: salto a S2 mode=edit. enterStepEdit
		// persiste status.stage=step + step_idx + step_mode=edit.
		if m.summaryCursor < 0 || m.summaryCursor >= len(m.pipeline.Steps) {
			return m, nil
		}
		return m.enterStepEdit(m.summaryCursor)
	case "d":
		if len(m.pipeline.Steps) == 0 {
			return m, nil
		}
		return m.openSummaryDelete()
	case "+":
		// Append: nuevo step al final → S2 mode=create con idx == len(Steps).
		// enterStepCreate maneja persistencia del status=step.
		return m.enterStepCreate(len(m.pipeline.Steps))
	case "y":
		// H8: abre el YAML en $EDITOR y reload al volver.
		return m.summaryOpenInEditor()
	case "ctrl+s", "enter":
		return m.summarySaveAndFinish()
	}
	return m, nil
}

// summaryReorder mueve el step apuntado por summaryCursor delta posiciones
// (delta = -1 sube, +1 baja). No-op si el cursor o el destino estan fuera
// de rango. Tras el swap, el cursor sigue al step movido (UX: "el step
// que apunto sigue siendo este") y persistimos el draft con stage=summary.
func (m model) summaryReorder(delta int) (model, tea.Cmd) {
	if m.summaryCursor < 0 || m.summaryCursor >= len(m.pipeline.Steps) {
		return m, nil
	}
	target := m.summaryCursor + delta
	if target < 0 || target >= len(m.pipeline.Steps) {
		return m, nil
	}
	m.pipeline.Steps[m.summaryCursor], m.pipeline.Steps[target] =
		m.pipeline.Steps[target], m.pipeline.Steps[m.summaryCursor]
	m.summaryCursor = target
	return m.summaryPersistDraft()
}

// summaryPersistDraft escribe el pipeline a disco con stage=summary +
// LastSavedAt=now. Si tras el cambio el pipeline quedo sin steps (delete
// del unico restante), bouncea a S2 mode=create — IsValid igual rechaza
// pipelines sin steps, asi que es mas amable que dejar al usuario en S3
// con el banner de error.
func (m model) summaryPersistDraft() (model, tea.Cmd) {
	if len(m.pipeline.Steps) == 0 {
		return m.enterStepCreate(0)
	}
	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageSummary
	m.pipeline.Status.StepIdx = 0
	m.pipeline.Status.StepMode = ""
	m.pipeline.Status.LastSavedAt = time.Now()
	if err := Save(m.path, m.pipeline); err != nil {
		m.summaryErrs = []string{"no se pudo guardar el draft: " + err.Error()}
	} else {
		m.summaryErrs = nil
	}
	if m.summaryCursor < 0 {
		m.summaryCursor = 0
	}
	if m.summaryCursor >= len(m.pipeline.Steps) {
		m.summaryCursor = len(m.pipeline.Steps) - 1
	}
	m.screen = ScreenSummary
	return m, nil
}

// openSummaryDelete abre el modal de confirmacion de "borrar step". Default
// seguro = cancelar (el usuario tiene que mover + enter para confirmar) —
// pulsar enter por inercia desde S3 no debe perder el step.
func (m model) openSummaryDelete() (model, tea.Cmd) {
	m.summaryDelCursor = SummaryDeleteCancel
	m.screen = ScreenSummaryConfirmDelete
	return m, nil
}

// updateSummaryDelete maneja teclas dentro del modal de confirmacion.
func (m model) updateSummaryDelete(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "esc":
		m.screen = ScreenSummary
		return m, nil
	case "ctrl+c":
		return m.openCancel(ScreenSummaryConfirmDelete)
	case "up", "k", "left", "h":
		m.summaryDelCursor = SummaryDeleteConfirm
		return m, nil
	case "down", "j", "right", "l":
		m.summaryDelCursor = SummaryDeleteCancel
		return m, nil
	case "1":
		m.summaryDelCursor = SummaryDeleteConfirm
		return m.applySummaryDelete()
	case "2":
		m.summaryDelCursor = SummaryDeleteCancel
		return m.applySummaryDelete()
	case "enter", " ":
		return m.applySummaryDelete()
	}
	return m, nil
}

func (m model) applySummaryDelete() (model, tea.Cmd) {
	switch m.summaryDelCursor {
	case SummaryDeleteConfirm:
		return m.summaryDelete()
	case SummaryDeleteCancel:
		m.screen = ScreenSummary
		return m, nil
	}
	return m, nil
}

// summaryDelete remueve el step apuntado, reindexa y persiste. Si el
// cursor queda fuera de rango (borraste el ultimo) lo clampea a la ultima
// posicion valida. Vuelve a S3 — summaryPersistDraft ya pone screen.
func (m model) summaryDelete() (model, tea.Cmd) {
	if m.summaryCursor < 0 || m.summaryCursor >= len(m.pipeline.Steps) {
		m.screen = ScreenSummary
		return m, nil
	}
	idx := m.summaryCursor
	m.pipeline.Steps = append(m.pipeline.Steps[:idx], m.pipeline.Steps[idx+1:]...)
	return m.summaryPersistDraft()
}

// viewSummaryDelete renderiza el modal de confirmacion. Muestra el numero
// + nombre del step apuntado para que el usuario sepa exactamente que va
// a borrar.
func (m model) viewSummaryDelete() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Borrar step"))
	b.WriteString("\n\n")
	if m.summaryCursor >= 0 && m.summaryCursor < len(m.pipeline.Steps) {
		s := m.pipeline.Steps[m.summaryCursor]
		b.WriteString(fmt.Sprintf("¿Borrar el step %d (%q)?", m.summaryCursor+1, s.Name))
		b.WriteString("\n\n")
	}

	options := []struct {
		choice SummaryDeleteChoice
		digit  string
		label  string
		hint   string
	}{
		{SummaryDeleteConfirm, "1", "borrar", "remueve el step y reindexa"},
		{SummaryDeleteCancel, "2", "cancelar", "volver a S3 sin tocar"},
	}
	for _, o := range options {
		line := "  " + o.digit + ". " + o.label + "  " + dimStyle.Render(o.hint)
		if m.summaryDelCursor == o.choice {
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

// stepHasBrokenPrevDep reporta si el step en idx tiene una dependencia de
// previous_output que no se puede satisfacer. Hoy esto solo pasa cuando
// el step quedo en idx 0 (no hay step previo). Tras delete/reorder un step
// que antes vivia en idx>=1 puede caer a 0 y romper la cadena.
func stepHasBrokenPrevDep(steps []Step, idx int) bool {
	if idx < 0 || idx >= len(steps) {
		return false
	}
	return steps[idx].Input == InputPreviousOutput && idx == 0
}

// summarySaveAndFinish corre IsValid + persiste el archivo final sin
// bloque status + transiciona a S4. Si IsValid falla, queda en S3 con
// los errores listados.
func (m model) summarySaveAndFinish() (model, tea.Cmd) {
	if err := IsValid(m.pipeline); err != nil {
		m.summaryErrs = validationLines(err)
		return m, nil
	}

	// Strippear el bloque status para que el archivo final sea "ready".
	// omitempty en Status (model.go) ya garantiza que Marshal no emita el
	// bloque cuando el puntero es nil.
	finalized := m.pipeline
	finalized.Status = nil

	if err := Save(m.path, finalized); err != nil {
		m.summaryErrs = []string{"no se pudo guardar: " + err.Error()}
		return m, nil
	}

	// El modelo en RAM tambien refleja "ready" — si el usuario abre SC
	// despues no queremos volver a meter status.
	m.pipeline.Status = nil
	m.summaryErrs = nil
	m.screen = ScreenSaved
	return m, nil
}

// viewSummary renderiza S3.
//
// Layout: title + breadcrumb "Pipeline: <name>" + descripcion + lista de
// steps con metadata rica (CLI, kind|skill:<name>, input, chip de
// validator), errores de IsValid (si aplica), hint de teclas.
func (m model) viewSummary() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Create pipeline · paso 3/3 · resumen"))
	b.WriteString("\n\n")

	b.WriteString(labelStyle.Render("Pipeline: "))
	b.WriteString(mutedItem.Render(m.pipeline.Name))
	b.WriteString("\n")
	if m.pipeline.Description != "" {
		b.WriteString(dimStyle.Render(m.pipeline.Description))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString(labelStyle.Render("Steps:"))
	b.WriteString("\n\n")
	for i, s := range m.pipeline.Steps {
		focused := i == m.summaryCursor
		broken := stepHasBrokenPrevDep(m.pipeline.Steps, i)
		b.WriteString(renderSummaryStepRow(i, s, focused, broken))
		b.WriteString("\n")
	}

	if len(m.summaryErrs) > 0 {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("✗ no se puede guardar:"))
		b.WriteString("\n")
		for _, line := range m.summaryErrs {
			b.WriteString(errorStyle.Render("  - " + line))
			b.WriteString("\n")
		}
	}

	if m.path != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("draft: " + m.path))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · e editar · d borrar · shift+↑↓ reordenar · + agregar · y abrir en $EDITOR"))
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("enter / ctrl+s guardar pipeline · esc volver al ultimo step"))
	b.WriteString("\n")
	return b.String()
}

// renderSummaryStepRow es la version "rica" del breadcrumb compacto de S2.
// En S2 una linea por step alcanza porque el usuario esta editando uno;
// en S3 el usuario revisa todo, asi que cada step va en bloque vertical
// con header (numero + nombre + chip estado del validator) + sub-lineas
// con CLI, kind/skill/content excerpt, input y bloque validator detallado
// si esta presente.
//
// broken=true marca el step en rojo y agrega un warning ⚠ explicando que
// la dependencia previous_output esta rota — H7 lo usa cuando un step que
// usaba previous_output queda en idx 0 tras un delete/reorder.
func renderSummaryStepRow(idx int, s Step, focused, broken bool) string {
	var b strings.Builder

	header := fmt.Sprintf("%d. %s", idx+1, s.Name)
	switch {
	case broken && focused:
		b.WriteString(errorStyle.Render("▸ " + header))
	case broken:
		b.WriteString("  " + errorStyle.Render(header))
	case focused:
		b.WriteString(selectedItem.Render("▸ " + header))
	default:
		b.WriteString("  " + mutedItem.Render(header))
	}
	if s.Validator != nil {
		b.WriteString("   ")
		b.WriteString(selectedOff.Render("[validator]"))
	}
	if broken {
		b.WriteString("   ")
		b.WriteString(errorStyle.Render("⚠ input previous_output sin step previo"))
	}
	b.WriteString("\n")

	indent := "     "
	b.WriteString(indent + dimStyle.Render("cli:    "))
	b.WriteString(s.CLI)
	b.WriteString("\n")

	switch s.Kind {
	case KindSkill:
		b.WriteString(indent + dimStyle.Render("skill:  "))
		b.WriteString(s.Content)
		b.WriteString("\n")
	default:
		b.WriteString(indent + dimStyle.Render("prompt: "))
		b.WriteString(excerpt(s.Content, 80))
		b.WriteString("\n")
	}

	b.WriteString(indent + dimStyle.Render("input:  "))
	b.WriteString(s.Input)
	b.WriteString("\n")

	if s.Validator != nil {
		b.WriteString(indent + dimStyle.Render("validator:"))
		b.WriteString("\n")
		valIndent := indent + "  "
		b.WriteString(valIndent + dimStyle.Render("cli:        "))
		b.WriteString(s.Validator.CLI)
		b.WriteString("\n")
		switch s.Validator.Kind {
		case KindSkill:
			b.WriteString(valIndent + dimStyle.Render("skill:      "))
			b.WriteString(s.Validator.Content)
		default:
			b.WriteString(valIndent + dimStyle.Render("prompt:     "))
			b.WriteString(excerpt(s.Validator.Content, 80))
		}
		b.WriteString("\n")
		maxLoops := s.MaxLoops
		if maxLoops == 0 {
			maxLoops = 1
		}
		onMax := s.OnMaxLoops
		if onMax == "" {
			onMax = OnMaxLoopsFail
		}
		b.WriteString(valIndent + dimStyle.Render("max_loops:  "))
		b.WriteString(fmt.Sprintf("%d", maxLoops))
		b.WriteString("\n")
		b.WriteString(valIndent + dimStyle.Render("on_max:     "))
		b.WriteString(onMax)
		b.WriteString("\n")
	}
	return b.String()
}

// excerpt corta s a max chars conservando una sola linea (newlines pasan
// a "↵") para que el resumen siempre ocupe espacio predecible. Si la
// version truncada queda mas corta que el original agregamos "…".
func excerpt(s string, max int) string {
	flat := strings.ReplaceAll(s, "\n", " ↵ ")
	if len([]rune(flat)) <= max {
		return flat
	}
	r := []rune(flat)
	return string(r[:max]) + "…"
}
