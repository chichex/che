package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/flow/execute"
)

// TestExecuteValidators_StateMachine verifica que desde screenExecuteAgent,
// presionar Enter lleve a screenExecuteValidators con counts limpios (default
// "none"), y que las teclas space/enter produzcan la lista de validadores
// esperada. Usamos Update() con tea.KeyMsg sintéticos — mismo patrón que
// bubbletea recomienda para testear state machines sin arrancar el runtime.
func TestExecuteValidators_StateMachine(t *testing.T) {
	m := newExecuteAtAgentScreen(t, "42")

	// Enter en agent → validators.
	m = updateKey(t, m, "enter")
	if m.screen != screenExecuteValidators {
		t.Fatalf("expected screenExecuteValidators after enter, got %v", m.screen)
	}
	if m.executeChosenAgent != execute.AgentOpus {
		t.Fatalf("expected chosen agent opus (idx 0), got %q", m.executeChosenAgent)
	}
	if got := executeValidatorsFromCounts(m.executeValidatorCount); len(got) != 0 {
		t.Fatalf("expected empty validators by default (none), got %+v", got)
	}

	// Cycle opus: space → count[opus]=1.
	m = updateKey(t, m, " ")
	if n := m.executeValidatorCount[execute.AgentOpus]; n != 1 {
		t.Fatalf("expected opus count 1 after 1 space, got %d", n)
	}

	// Down al cursor de codex (idx 1) y space → count[codex]=1.
	m = updateKey(t, m, "down")
	m = updateKey(t, m, " ")
	if n := m.executeValidatorCount[execute.AgentCodex]; n != 1 {
		t.Fatalf("expected codex count 1, got %d", n)
	}

	// La lista final debe contener opus#1 y codex#1 en el orden canónico.
	got := executeValidatorsFromCounts(m.executeValidatorCount)
	want := []execute.Validator{
		{Agent: execute.AgentOpus, Instance: 1},
		{Agent: execute.AgentCodex, Instance: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("validators len mismatch: got %+v, want %+v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("validator[%d]: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestExecuteValidators_EscReturnsToAgent: Esc vuelve a screenExecuteAgent
// y preserva los counts (permite corregir el agent sin perder la selección).
func TestExecuteValidators_EscReturnsToAgent(t *testing.T) {
	m := newExecuteAtAgentScreen(t, "7")
	m = updateKey(t, m, "enter") // → validators
	m = updateKey(t, m, " ")     // opus=1

	m = updateKey(t, m, "esc")
	if m.screen != screenExecuteAgent {
		t.Fatalf("expected screenExecuteAgent after esc, got %v", m.screen)
	}
	if n := m.executeValidatorCount[execute.AgentOpus]; n != 1 {
		t.Fatalf("counts should be preserved across esc, got opus=%d", n)
	}
}

// TestExecuteValidators_NewIssueResetsCounts: elegir una issue distinta desde
// screenExecuteSelect arranca el flow con counts vacíos — cada flow empieza
// con el default "none" sin arrastrar lo del run anterior.
func TestExecuteValidators_NewIssueResetsCounts(t *testing.T) {
	m := New("test", context.Background())
	m.executeCandidates = []execute.Candidate{{Number: 7, Title: "first"}, {Number: 42, Title: "second"}}
	m.screen = screenExecuteSelect
	m.executeValidatorCount = map[execute.Agent]int{execute.AgentOpus: 2}

	m = updateKey(t, m, "enter") // elige #7
	if m.executeValidatorCount != nil {
		t.Fatalf("expected counts cleared on new issue select, got %+v", m.executeValidatorCount)
	}
}

// TestExecuteValidators_EnterWithNoneSendsEmptySpec: default "none" — si el
// usuario confirma sin marcar nada, la lista de validadores es vacía.
// Equivalente a --validators none en CLI.
func TestExecuteValidators_EnterWithNoneSendsEmptySpec(t *testing.T) {
	m := newExecuteAtAgentScreen(t, "42")
	m = updateKey(t, m, "enter") // → validators
	if v := executeValidatorsFromCounts(m.executeValidatorCount); len(v) != 0 {
		t.Fatalf("default must be none (empty), got %+v", v)
	}
}

// TestExecuteValidatorsFromCounts_Ordering: la lista respeta el orden
// canónico de ValidAgents (opus, codex, gemini), con Instance 1..N por agente.
func TestExecuteValidatorsFromCounts_Ordering(t *testing.T) {
	counts := map[execute.Agent]int{
		execute.AgentGemini: 1,
		execute.AgentCodex:  2,
	}
	got := executeValidatorsFromCounts(counts)
	want := []execute.Validator{
		{Agent: execute.AgentCodex, Instance: 1},
		{Agent: execute.AgentCodex, Instance: 2},
		{Agent: execute.AgentGemini, Instance: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("pos %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// newExecuteAtAgentScreen arma un Model posicionado en screenExecuteAgent
// con una issue ya elegida — punto de entrada común para los tests que
// quieren ejercitar la transición agent → validators.
func newExecuteAtAgentScreen(t *testing.T, ref string) Model {
	t.Helper()
	m := New("test", context.Background())
	m.screen = screenExecuteAgent
	m.executeChosenRef = ref
	m.executeAgentIdx = 0
	return m
}

// updateKey feedea un tea.KeyMsg sintético al Update() del modelo y devuelve
// el nuevo Model. Los comandos tea.Cmd se descartan — en estos tests solo
// nos interesan las transiciones de estado.
func updateKey(t *testing.T, m Model, key string) Model {
	t.Helper()
	var km tea.KeyMsg
	switch key {
	case "enter":
		km = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		km = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		km = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		km = tea.KeyMsg{Type: tea.KeyDown}
	case " ":
		km = tea.KeyMsg{Type: tea.KeySpace}
	default:
		km = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, _ := m.Update(km)
	return next.(Model)
}
