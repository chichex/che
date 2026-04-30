package runner

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg arma una tea.KeyMsg con la representación de tecla `s` —
// alcanza para los strings que mira el handler ("space", "enter",
// "esc", "up", "down", "a", "n").
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newSelectorModel(agents []string) selectorModel {
	checked := make([]bool, len(agents))
	for i := range checked {
		checked[i] = true
	}
	return selectorModel{
		stepName: "test_step",
		agents:   agents,
		checked:  checked,
		cursor:   0,
	}
}

// TestSelectorModel_DefaultTodosTildados: el wizard arranca con
// todos los agentes preseleccionados (PRD §3.e).
func TestSelectorModel_DefaultTodosTildados(t *testing.T) {
	m := newSelectorModel([]string{"a", "b", "c"})
	for i, c := range m.checked {
		if !c {
			t.Errorf("agent %d no preseleccionado", i)
		}
	}
}

// TestSelectorModel_SpaceToggleaActual: espacio togglea sólo el
// agente bajo el cursor.
func TestSelectorModel_SpaceToggleaActual(t *testing.T) {
	m := newSelectorModel([]string{"a", "b", "c"})
	m.cursor = 1
	updated, _ := m.Update(keyMsg(" "))
	mNew := updated.(selectorModel)
	if mNew.checked[1] {
		t.Error("space no destildó el agente actual")
	}
	if !mNew.checked[0] || !mNew.checked[2] {
		t.Error("space afectó otros agentes")
	}
}

// TestSelectorModel_DownAvanzaCursor: down/j avanza el cursor sin
// pasarse del último.
func TestSelectorModel_DownAvanzaCursor(t *testing.T) {
	m := newSelectorModel([]string{"a", "b"})
	updated, _ := m.Update(keyMsg("down"))
	mNew := updated.(selectorModel)
	if mNew.cursor != 1 {
		t.Errorf("cursor=%d want 1", mNew.cursor)
	}
	updated2, _ := mNew.Update(keyMsg("down"))
	mNew2 := updated2.(selectorModel)
	if mNew2.cursor != 1 {
		t.Errorf("cursor desbordó: %d", mNew2.cursor)
	}
}

// TestSelectorModel_UpRetrocedeCursor: up/k retrocede sin pasarse de
// 0.
func TestSelectorModel_UpRetrocedeCursor(t *testing.T) {
	m := newSelectorModel([]string{"a", "b"})
	m.cursor = 1
	updated, _ := m.Update(keyMsg("up"))
	mNew := updated.(selectorModel)
	if mNew.cursor != 0 {
		t.Errorf("cursor=%d want 0", mNew.cursor)
	}
	updated2, _ := mNew.Update(keyMsg("up"))
	mNew2 := updated2.(selectorModel)
	if mNew2.cursor != 0 {
		t.Errorf("cursor pasó por debajo: %d", mNew2.cursor)
	}
}

// TestSelectorModel_AMarcaTodos: tecla 'a' tilda todos.
func TestSelectorModel_AMarcaTodos(t *testing.T) {
	m := newSelectorModel([]string{"a", "b", "c"})
	for i := range m.checked {
		m.checked[i] = false
	}
	updated, _ := m.Update(keyMsg("a"))
	mNew := updated.(selectorModel)
	for i, c := range mNew.checked {
		if !c {
			t.Errorf("agent %d no fue tildado por 'a'", i)
		}
	}
}

// TestSelectorModel_NDesmarcaTodos: tecla 'n' destilda todos.
func TestSelectorModel_NDesmarcaTodos(t *testing.T) {
	m := newSelectorModel([]string{"a", "b", "c"})
	updated, _ := m.Update(keyMsg("n"))
	mNew := updated.(selectorModel)
	for i, c := range mNew.checked {
		if c {
			t.Errorf("agent %d no fue destildado por 'n'", i)
		}
	}
}

// TestSelectorModel_EnterRechazaSiCero: enter con 0 marcados no
// confirma (rechazo silencioso para forzar al usuario a marcar al
// menos uno o cancelar).
func TestSelectorModel_EnterRechazaSiCero(t *testing.T) {
	m := newSelectorModel([]string{"a"})
	m.checked[0] = false
	updated, cmd := m.Update(keyMsg("enter"))
	mNew := updated.(selectorModel)
	if mNew.done {
		t.Error("enter confirmó con 0 marcados")
	}
	if cmd != nil {
		t.Error("enter con 0 marcados emitió comando")
	}
}

// TestSelectorModel_EnterConfirmaSiHayMarcados: enter con ≥1
// marcado dispara done + tea.Quit.
func TestSelectorModel_EnterConfirmaSiHayMarcados(t *testing.T) {
	m := newSelectorModel([]string{"a", "b"})
	updated, cmd := m.Update(keyMsg("enter"))
	mNew := updated.(selectorModel)
	if !mNew.done {
		t.Error("enter no marcó done")
	}
	if cmd == nil {
		t.Error("enter no emitió tea.Quit")
	}
}

// TestSelectorModel_EscCancela: esc setea cancelled + tea.Quit.
func TestSelectorModel_EscCancela(t *testing.T) {
	m := newSelectorModel([]string{"a"})
	updated, cmd := m.Update(keyMsg("esc"))
	mNew := updated.(selectorModel)
	if !mNew.cancelled {
		t.Error("esc no marcó cancelled")
	}
	if cmd == nil {
		t.Error("esc no emitió tea.Quit")
	}
}

// TestSelectorModel_CtrlCCancela: ctrl+c también cancela (escape
// hatch standard de TUIs).
func TestSelectorModel_CtrlCCancela(t *testing.T) {
	m := newSelectorModel([]string{"a"})
	updated, _ := m.Update(keyMsg("ctrl+c"))
	mNew := updated.(selectorModel)
	if !mNew.cancelled {
		t.Error("ctrl+c no marcó cancelled")
	}
}

// TestSelectorModel_ViewIncluyeStepYContador: el view rendea el
// nombre del step y el contador de marcados.
func TestSelectorModel_ViewIncluyeStepYContador(t *testing.T) {
	m := newSelectorModel([]string{"a", "b", "c"})
	m.checked[2] = false
	v := m.View()
	if !strings.Contains(v, `"test_step"`) {
		t.Errorf("view sin nombre del step: %q", v)
	}
	if !strings.Contains(v, "marcados: 2/3") {
		t.Errorf("view sin contador: %q", v)
	}
}
