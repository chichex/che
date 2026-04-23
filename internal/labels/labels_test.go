package labels

import (
	"strings"
	"testing"
)

func TestTransitionFor_Valid(t *testing.T) {
	cases := []struct {
		from, to string
		wantAdd  []string
		wantRem  []string
	}{
		{
			from:    StatusIdea,
			to:      StatusPlan,
			wantAdd: []string{StatusPlan},
			wantRem: []string{StatusIdea},
		},
		{
			from:    StatusPlan,
			to:      StatusExecuting,
			wantAdd: []string{StatusExecuting},
			wantRem: []string{StatusPlan},
		},
		{
			from:    StatusExecuting,
			to:      StatusExecuted,
			wantAdd: []string{StatusExecuted},
			wantRem: []string{StatusExecuting},
		},
		{
			from:    StatusExecuting,
			to:      StatusPlan,
			wantAdd: []string{StatusPlan},
			wantRem: []string{StatusExecuting},
		},
		{
			from:    StatusExecuted,
			to:      StatusClosed,
			wantAdd: []string{StatusClosed},
			wantRem: []string{StatusExecuted},
		},
		// ─── Máquina nueva (prefix `che:*`), 21 transiciones ────────────────
		{
			from:    CheIdea,
			to:      ChePlanning,
			wantAdd: []string{ChePlanning},
			wantRem: []string{CheIdea},
		},
		{
			from:    ChePlanning,
			to:      ChePlan,
			wantAdd: []string{ChePlan},
			wantRem: []string{ChePlanning},
		},
		{
			from:    ChePlanning,
			to:      CheIdea,
			wantAdd: []string{CheIdea},
			wantRem: []string{ChePlanning},
		},
		{
			from:    ChePlanning,
			to:      CheValidated,
			wantAdd: []string{CheValidated},
			wantRem: []string{ChePlanning},
		},
		{
			from:    CheIdea,
			to:      CheExecuting,
			wantAdd: []string{CheExecuting},
			wantRem: []string{CheIdea},
		},
		{
			from:    ChePlan,
			to:      CheExecuting,
			wantAdd: []string{CheExecuting},
			wantRem: []string{ChePlan},
		},
		{
			from:    CheValidated,
			to:      CheExecuting,
			wantAdd: []string{CheExecuting},
			wantRem: []string{CheValidated},
		},
		{
			from:    CheExecuting,
			to:      CheExecuted,
			wantAdd: []string{CheExecuted},
			wantRem: []string{CheExecuting},
		},
		{
			from:    CheExecuting,
			to:      CheIdea,
			wantAdd: []string{CheIdea},
			wantRem: []string{CheExecuting},
		},
		{
			from:    CheExecuting,
			to:      ChePlan,
			wantAdd: []string{ChePlan},
			wantRem: []string{CheExecuting},
		},
		{
			from:    CheExecuting,
			to:      CheValidated,
			wantAdd: []string{CheValidated},
			wantRem: []string{CheExecuting},
		},
		{
			from:    ChePlan,
			to:      CheValidating,
			wantAdd: []string{CheValidating},
			wantRem: []string{ChePlan},
		},
		{
			from:    CheExecuted,
			to:      CheValidating,
			wantAdd: []string{CheValidating},
			wantRem: []string{CheExecuted},
		},
		{
			from:    CheValidating,
			to:      CheValidated,
			wantAdd: []string{CheValidated},
			wantRem: []string{CheValidating},
		},
		{
			from:    CheValidating,
			to:      ChePlan,
			wantAdd: []string{ChePlan},
			wantRem: []string{CheValidating},
		},
		{
			from:    CheValidating,
			to:      CheExecuted,
			wantAdd: []string{CheExecuted},
			wantRem: []string{CheValidating},
		},
		{
			from:    CheExecuted,
			to:      CheClosing,
			wantAdd: []string{CheClosing},
			wantRem: []string{CheExecuted},
		},
		{
			from:    CheValidated,
			to:      CheClosing,
			wantAdd: []string{CheClosing},
			wantRem: []string{CheValidated},
		},
		{
			from:    CheClosing,
			to:      CheClosed,
			wantAdd: []string{CheClosed},
			wantRem: []string{CheClosing},
		},
		{
			from:    CheClosing,
			to:      CheExecuted,
			wantAdd: []string{CheExecuted},
			wantRem: []string{CheClosing},
		},
		{
			from:    CheClosing,
			to:      CheValidated,
			wantAdd: []string{CheValidated},
			wantRem: []string{CheClosing},
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
		{StatusIdea, StatusExecuting},         // no se puede saltar explore
		{StatusExecuted, StatusPlan},          // no hay vuelta atrás desde executed
		{"", StatusExecuting},                 // from vacío
		{StatusPlan, ""},                      // to vacío
		{StatusPlan, "status:ready-to-close"}, // estado no soportado todavía
		{StatusPlan, StatusClosed},            // plan no va directo a closed (execute primero)
		{StatusExecuting, StatusClosed},       // executing no va directo a closed (terminar exec primero)

		// ─── Casos inválidos en la máquina nueva (`che:*`) ───────────────
		{CheIdea, CheValidated},    // no se puede saltar planning/plan/executing/executed
		{CheClosed, CheIdea},       // closed es terminal — no hay transición de salida
		{CheClosed, ChePlan},       // closed es terminal — no hay transición de salida
		{CheValidated, CheClosed},  // hay que pasar por closing primero
		{CheExecuting, CheClosing}, // no se puede cerrar un execute en curso
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
