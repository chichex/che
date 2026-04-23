package cmd

import (
	"testing"

	"github.com/chichex/che/internal/labels"
)

// TestMigrationPairs valida el contrato del helper `migrationPairs()`:
// los 5 pares status:* → che:* esperados, en el orden del embudo idea →
// plan → executing → executed → closed, sin duplicados de Old ni de New.
//
// Es un unit test del helper para fijar el contrato sin depender de
// `gh`. Los e2e del subcomando completo (con fakes de gh) quedan fuera
// de scope para PR1.
func TestMigrationPairs(t *testing.T) {
	pairs := migrationPairs()

	want := []pair{
		{Old: "status:idea", New: labels.CheIdea},
		{Old: "status:plan", New: labels.ChePlan},
		{Old: "status:executing", New: labels.CheExecuting},
		{Old: "status:executed", New: labels.CheExecuted},
		{Old: "status:closed", New: labels.CheClosed},
	}

	if len(pairs) != len(want) {
		t.Fatalf("len: got %d pairs, want %d", len(pairs), len(want))
	}
	for i, w := range want {
		if pairs[i] != w {
			t.Errorf("pair[%d]: got %+v, want %+v", i, pairs[i], w)
		}
	}

	// No duplicados de Old ni de New (sería un bug copy-paste obvio).
	seenOld := map[string]bool{}
	seenNew := map[string]bool{}
	for _, p := range pairs {
		if seenOld[p.Old] {
			t.Errorf("duplicate Old: %s", p.Old)
		}
		if seenNew[p.New] {
			t.Errorf("duplicate New: %s", p.New)
		}
		seenOld[p.Old] = true
		seenNew[p.New] = true
	}
}
