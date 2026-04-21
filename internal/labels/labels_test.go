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
