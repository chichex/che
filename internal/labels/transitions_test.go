package labels

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipelinelabels"
)

// TestTransitionFor_Valid verifica las 21 transiciones v2 registradas en
// `validTransitions`. Cubre los 5 flows (explore / execute / iterate plan /
// iterate PR / validate / close) con éxito y rollback.
func TestTransitionFor_Valid(t *testing.T) {
	cases := []struct {
		from, to string
		wantAdd  []string
		wantRem  []string
	}{
		{
			from:    pipelinelabels.StateIdea,
			to:      pipelinelabels.StateApplyingExplore,
			wantAdd: []string{pipelinelabels.StateApplyingExplore},
			wantRem: []string{pipelinelabels.StateIdea},
		},
		{
			from:    pipelinelabels.StateApplyingExplore,
			to:      pipelinelabels.StateExplore,
			wantAdd: []string{pipelinelabels.StateExplore},
			wantRem: []string{pipelinelabels.StateApplyingExplore},
		},
		{
			from:    pipelinelabels.StateApplyingExplore,
			to:      pipelinelabels.StateIdea,
			wantAdd: []string{pipelinelabels.StateIdea},
			wantRem: []string{pipelinelabels.StateApplyingExplore},
		},
		{
			from:    pipelinelabels.StateApplyingExplore,
			to:      pipelinelabels.StateValidatePR,
			wantAdd: []string{pipelinelabels.StateValidatePR},
			wantRem: []string{pipelinelabels.StateApplyingExplore},
		},
		{
			from:    pipelinelabels.StateValidatePR,
			to:      pipelinelabels.StateApplyingExplore,
			wantAdd: []string{pipelinelabels.StateApplyingExplore},
			wantRem: []string{pipelinelabels.StateValidatePR},
		},
		{
			from:    pipelinelabels.StateIdea,
			to:      pipelinelabels.StateApplyingExecute,
			wantAdd: []string{pipelinelabels.StateApplyingExecute},
			wantRem: []string{pipelinelabels.StateIdea},
		},
		{
			from:    pipelinelabels.StateExplore,
			to:      pipelinelabels.StateApplyingExecute,
			wantAdd: []string{pipelinelabels.StateApplyingExecute},
			wantRem: []string{pipelinelabels.StateExplore},
		},
		{
			from:    pipelinelabels.StateValidatePR,
			to:      pipelinelabels.StateApplyingExecute,
			wantAdd: []string{pipelinelabels.StateApplyingExecute},
			wantRem: []string{pipelinelabels.StateValidatePR},
		},
		{
			from:    pipelinelabels.StateApplyingExecute,
			to:      pipelinelabels.StateExecute,
			wantAdd: []string{pipelinelabels.StateExecute},
			wantRem: []string{pipelinelabels.StateApplyingExecute},
		},
		{
			from:    pipelinelabels.StateApplyingExecute,
			to:      pipelinelabels.StateIdea,
			wantAdd: []string{pipelinelabels.StateIdea},
			wantRem: []string{pipelinelabels.StateApplyingExecute},
		},
		{
			from:    pipelinelabels.StateApplyingExecute,
			to:      pipelinelabels.StateExplore,
			wantAdd: []string{pipelinelabels.StateExplore},
			wantRem: []string{pipelinelabels.StateApplyingExecute},
		},
		{
			from:    pipelinelabels.StateApplyingExecute,
			to:      pipelinelabels.StateValidatePR,
			wantAdd: []string{pipelinelabels.StateValidatePR},
			wantRem: []string{pipelinelabels.StateApplyingExecute},
		},
		{
			from:    pipelinelabels.StateExplore,
			to:      pipelinelabels.StateApplyingValidatePR,
			wantAdd: []string{pipelinelabels.StateApplyingValidatePR},
			wantRem: []string{pipelinelabels.StateExplore},
		},
		{
			from:    pipelinelabels.StateExecute,
			to:      pipelinelabels.StateApplyingValidatePR,
			wantAdd: []string{pipelinelabels.StateApplyingValidatePR},
			wantRem: []string{pipelinelabels.StateExecute},
		},
		{
			from:    pipelinelabels.StateApplyingValidatePR,
			to:      pipelinelabels.StateValidatePR,
			wantAdd: []string{pipelinelabels.StateValidatePR},
			wantRem: []string{pipelinelabels.StateApplyingValidatePR},
		},
		{
			from:    pipelinelabels.StateApplyingValidatePR,
			to:      pipelinelabels.StateExplore,
			wantAdd: []string{pipelinelabels.StateExplore},
			wantRem: []string{pipelinelabels.StateApplyingValidatePR},
		},
		{
			from:    pipelinelabels.StateApplyingValidatePR,
			to:      pipelinelabels.StateExecute,
			wantAdd: []string{pipelinelabels.StateExecute},
			wantRem: []string{pipelinelabels.StateApplyingValidatePR},
		},
		{
			from:    pipelinelabels.StateExecute,
			to:      pipelinelabels.StateApplyingClose,
			wantAdd: []string{pipelinelabels.StateApplyingClose},
			wantRem: []string{pipelinelabels.StateExecute},
		},
		{
			from:    pipelinelabels.StateValidatePR,
			to:      pipelinelabels.StateApplyingClose,
			wantAdd: []string{pipelinelabels.StateApplyingClose},
			wantRem: []string{pipelinelabels.StateValidatePR},
		},
		{
			from:    pipelinelabels.StateApplyingClose,
			to:      pipelinelabels.StateClose,
			wantAdd: []string{pipelinelabels.StateClose},
			wantRem: []string{pipelinelabels.StateApplyingClose},
		},
		{
			from:    pipelinelabels.StateApplyingClose,
			to:      pipelinelabels.StateExecute,
			wantAdd: []string{pipelinelabels.StateExecute},
			wantRem: []string{pipelinelabels.StateApplyingClose},
		},
		{
			from:    pipelinelabels.StateApplyingClose,
			to:      pipelinelabels.StateValidatePR,
			wantAdd: []string{pipelinelabels.StateValidatePR},
			wantRem: []string{pipelinelabels.StateApplyingClose},
		},
	}
	for _, c := range cases {
		t.Run(c.from+"→"+c.to, func(t *testing.T) {
			tr, err := TransitionFor(c.from, c.to)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equal(tr.Add, c.wantAdd) {
				t.Errorf("Add: got %v, want %v", tr.Add, c.wantAdd)
			}
			if !equal(tr.Remove, c.wantRem) {
				t.Errorf("Remove: got %v, want %v", tr.Remove, c.wantRem)
			}
		})
	}
}

func TestTransitionFor_Invalid(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		{"", pipelinelabels.StateApplyingExecute},                    // from vacío
		{pipelinelabels.StateExplore, ""},                            // to vacío
		{pipelinelabels.StateExplore, "che:state:ready-to-close"},    // estado no soportado
		{pipelinelabels.StateIdea, pipelinelabels.StateValidatePR},   // no se puede saltar pasos
		{pipelinelabels.StateClose, pipelinelabels.StateIdea},        // close es terminal — no hay transición de salida
		{pipelinelabels.StateClose, pipelinelabels.StateExplore},     // close es terminal — no hay transición de salida
		{pipelinelabels.StateValidatePR, pipelinelabels.StateClose},  // hay que pasar por applying:close primero
		{pipelinelabels.StateApplyingExecute, pipelinelabels.StateApplyingClose}, // no se puede cerrar un execute en curso
	}
	for _, c := range cases {
		t.Run(c.from+"→"+c.to, func(t *testing.T) {
			if _, err := TransitionFor(c.from, c.to); err == nil {
				t.Fatalf("expected error for %q → %q", c.from, c.to)
			}
		})
	}
}

func TestTransitionFor_ErrorMessage(t *testing.T) {
	_, err := TransitionFor("foo", "bar")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "foo") || !strings.Contains(err.Error(), "bar") {
		t.Errorf("error should mention both states: %v", err)
	}
}

// TestValidateNoMixedLabels cubre la invariante de exclusividad v1↔v2:
// ningún issue debería tener labels viejos (`che:idea`/...) y v2
// (`che:state:*`) simultáneamente. El helper detecta repos a medio migrar
// para que los guards `rejectV1Labels` de los flows puedan apuntar al
// operador a `che migrate-labels-v2`.
//
// REMOVE IN PR6d junto con `ValidateNoMixedLabels`.
func TestValidateNoMixedLabels(t *testing.T) {
	cases := []struct {
		name    string
		labels  []string
		wantErr bool
	}{
		{
			name:    "vacío",
			labels:  nil,
			wantErr: false,
		},
		{
			name:    "solo labels orthogonales",
			labels:  []string{"ct:plan", "type:feature", CheLocked, "size:m"},
			wantErr: false,
		},
		{
			name:    "v1 only",
			labels:  []string{"ct:plan", "che:idea", "type:feature"},
			wantErr: false,
		},
		{
			name:    "v2 only",
			labels:  []string{"ct:plan", pipelinelabels.StateIdea, "type:feature"},
			wantErr: false,
		},
		{
			name:    "v2 only — applying",
			labels:  []string{pipelinelabels.StateApplyingExplore},
			wantErr: false,
		},
		{
			name: "mezcla — bug típico que un repo a medio migrar exhibe",
			labels: []string{
				"ct:plan",
				"che:idea",
				pipelinelabels.StateApplyingExplore,
			},
			wantErr: true,
		},
		{
			name: "mezcla — v1 y v2 ambos terminales",
			labels: []string{
				"che:plan",
				pipelinelabels.StateExplore,
			},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateNoMixedLabels(c.labels)
			if c.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestV1LegacyStates fija el contrato del helper exportado: devuelve los 9
// labels viejos para que los guards `rejectV1Labels` de los flows puedan
// detectar repos a medio migrar.
func TestV1LegacyStates(t *testing.T) {
	got := V1LegacyStates()
	want := []string{
		"che:idea",
		"che:planning",
		"che:plan",
		"che:executing",
		"che:executed",
		"che:validating",
		"che:validated",
		"che:closing",
		"che:closed",
	}
	if !equal(got, want) {
		t.Fatalf("V1LegacyStates() = %v, want %v", got, want)
	}
	// Devolver una copia: mutar el resultado no afecta llamadas siguientes.
	got[0] = "MUTATED"
	again := V1LegacyStates()
	if again[0] != "che:idea" {
		t.Errorf("V1LegacyStates() debe devolver copia: got %v", again)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
