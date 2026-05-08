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
// H6 deja casi todo en stub: ↑/↓ mueven el cursor entre steps sin accion;
// e/d/shift+↑↓/+ quedan reservados para H7. ctrl+s valida y persiste el
// archivo final sin status; esc vuelve al ultimo step en mode=edit. Sin
// ctrl+c → SC, igual que el resto del wizard.
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
	case "ctrl+s", "enter":
		return m.summarySaveAndFinish()
	}
	return m, nil
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
		b.WriteString(renderSummaryStepRow(i, s, focused))
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
	b.WriteString(hintStyle.Render("↑/↓ navegar steps · enter / ctrl+s guardar pipeline · esc volver al ultimo step"))
	b.WriteString("\n")
	return b.String()
}

// renderSummaryStepRow es la version "rica" del breadcrumb compacto de S2.
// En S2 una linea por step alcanza porque el usuario esta editando uno;
// en S3 el usuario revisa todo, asi que cada step va en bloque vertical
// con header (numero + nombre + chip estado del validator) + sub-lineas
// con CLI, kind/skill/content excerpt, input y bloque validator detallado
// si esta presente.
func renderSummaryStepRow(idx int, s Step, focused bool) string {
	var b strings.Builder

	header := fmt.Sprintf("%d. %s", idx+1, s.Name)
	if focused {
		b.WriteString(selectedItem.Render("▸ " + header))
	} else {
		b.WriteString("  " + mutedItem.Render(header))
	}
	if s.Validator != nil {
		b.WriteString("   ")
		b.WriteString(selectedOff.Render("[validator]"))
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
