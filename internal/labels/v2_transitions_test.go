package labels

import (
	"testing"

	"github.com/chichex/che/internal/pipelinelabels"
)

// TestTransitionFor_V2 verifica que las 21 transiciones del modelo v2
// (registradas por v2_transitions.go) sean equivalentes al espejo de las
// viejas — mismo Add/Remove pero con strings v2. Esto garantiza la
// coexistencia: un flow ya migrado puede llamar `Apply(ref, v2From, v2To)`
// y obtener exactamente los labels v2 esperados.
func TestTransitionFor_V2(t *testing.T) {
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

// TestTransitionFor_V2_Coexistence garantiza que las viejas y las v2 no
// se pisan: ambas siguen funcionando después de que init() registra v2.
func TestTransitionFor_V2_Coexistence(t *testing.T) {
	// Vieja (debe seguir funcionando).
	if _, err := TransitionFor(CheIdea, ChePlanning); err != nil {
		t.Errorf("vieja CheIdea → ChePlanning rota tras init v2: %v", err)
	}
	// V2 (registrada por init).
	if _, err := TransitionFor(pipelinelabels.StateIdea, pipelinelabels.StateApplyingExplore); err != nil {
		t.Errorf("v2 StateIdea → StateApplyingExplore no registrada: %v", err)
	}
}
