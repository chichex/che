package stateref

import (
	"fmt"
	"reflect"
	"testing"
)

// TestResolve_NoClosingIssues: PR sin issue linkeado → caemos al PR.
// Compat con PRs que el usuario metió a mano.
func TestResolve_NoClosingIssues(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		t.Fatalf("fetch should not be called for empty closingIssues")
		return nil, nil
	})()

	r := Resolve("140", []string{"validated:changes-requested"}, nil)
	if r.ResolvedToIssue {
		t.Fatalf("expected ResolvedToIssue=false, got %+v", r)
	}
	if r.Ref != "140" {
		t.Fatalf("expected Ref=140, got %q", r.Ref)
	}
	if !reflect.DeepEqual(r.Labels, []string{"validated:changes-requested"}) {
		t.Fatalf("expected PR labels preserved, got %+v", r.Labels)
	}
	if r.IssueNumber != 0 {
		t.Fatalf("expected IssueNumber=0, got %d", r.IssueNumber)
	}
}

// TestResolve_IssueWithState: PR con issue linkeado en che:executed →
// resolvemos al issue. Los labels devueltos son los del issue.
func TestResolve_IssueWithState(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		if n != 122 {
			t.Fatalf("fetch called with n=%d, want 122", n)
		}
		return []string{"ct:plan", "pricing-modes", "che:executed"}, nil
	})()

	r := Resolve("140", []string{"validated:changes-requested"}, []int{122})
	if !r.ResolvedToIssue {
		t.Fatalf("expected ResolvedToIssue=true, got %+v", r)
	}
	if r.Ref != "122" {
		t.Fatalf("expected Ref=122, got %q", r.Ref)
	}
	if r.IssueNumber != 122 {
		t.Fatalf("expected IssueNumber=122, got %d", r.IssueNumber)
	}
	if !r.HasLabel("che:executed") {
		t.Fatalf("expected HasLabel(che:executed)=true, labels=%+v", r.Labels)
	}
	if r.HasLabel("validated:changes-requested") {
		t.Fatalf("issue shouldn't have validated:changes-requested (that's on the PR)")
	}
}

// TestResolve_IssueFetchFails: si gh issue view falla (issue cerrado, 404,
// red) caemos al PR. Mejor dejar que el gate del caller aborte con los
// labels del PR que romper el flow entero.
func TestResolve_IssueFetchFails(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		return nil, fmt.Errorf("gh issue view %d: not found", n)
	})()

	r := Resolve("140", []string{"validated:changes-requested"}, []int{99})
	if r.ResolvedToIssue {
		t.Fatalf("expected fallback to PR when issue fetch fails, got %+v", r)
	}
	if r.Ref != "140" {
		t.Fatalf("expected Ref=140 (PR fallback), got %q", r.Ref)
	}
}

// TestResolve_IssueWithoutStateLabel: issue existe pero sin che:* de estado
// (quizá se le aplicaron labels a mano, o es un issue no manejado por che).
// Caemos al PR.
func TestResolve_IssueWithoutStateLabel(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		return []string{"bug", "priority:high"}, nil
	})()

	r := Resolve("140", []string{"che:executed"}, []int{50})
	if r.ResolvedToIssue {
		t.Fatalf("expected fallback when issue has no che:* state label, got %+v", r)
	}
	if r.Ref != "140" {
		t.Fatalf("expected Ref=140 (PR fallback), got %q", r.Ref)
	}
}

// TestResolve_MultipleClosingIssues_FirstWithState: iteramos en orden y
// tomamos el primero con che:*. Documenta el criterio para el caso edge
// "Closes #A, Closes #B".
func TestResolve_MultipleClosingIssues_FirstWithState(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		switch n {
		case 1:
			return []string{"che:plan"}, nil
		case 2:
			return []string{"che:executed"}, nil
		}
		return nil, fmt.Errorf("unexpected n=%d", n)
	})()

	r := Resolve("140", nil, []int{1, 2})
	if !r.ResolvedToIssue {
		t.Fatalf("expected ResolvedToIssue=true, got %+v", r)
	}
	if r.IssueNumber != 1 {
		t.Fatalf("expected first issue with state to win, got IssueNumber=%d", r.IssueNumber)
	}
}

// TestResolve_MultipleClosingIssues_SkipFailures: si el primer issue
// fallea el fetch o no tiene che:*, probamos el siguiente.
func TestResolve_MultipleClosingIssues_SkipFailures(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		switch n {
		case 1:
			return nil, fmt.Errorf("404")
		case 2:
			return []string{"bug"}, nil // no che:*
		case 3:
			return []string{"che:validated"}, nil
		}
		return nil, fmt.Errorf("unexpected n=%d", n)
	})()

	r := Resolve("140", nil, []int{1, 2, 3})
	if !r.ResolvedToIssue {
		t.Fatalf("expected ResolvedToIssue=true, got %+v", r)
	}
	if r.IssueNumber != 3 {
		t.Fatalf("expected to skip 1 (fail) and 2 (no state), got IssueNumber=%d", r.IssueNumber)
	}
}

// TestResolve_AllIssuesFailOrNoState: todos los closing issues fallan o no
// tienen che:* → caemos al PR.
func TestResolve_AllIssuesFailOrNoState(t *testing.T) {
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		switch n {
		case 1:
			return nil, fmt.Errorf("404")
		case 2:
			return []string{"bug"}, nil
		}
		return nil, fmt.Errorf("unexpected n=%d", n)
	})()

	prLabels := []string{"che:executed", "validated:changes-requested"}
	r := Resolve("140", prLabels, []int{1, 2})
	if r.ResolvedToIssue {
		t.Fatalf("expected fallback, got %+v", r)
	}
	if !reflect.DeepEqual(r.Labels, prLabels) {
		t.Fatalf("expected PR labels preserved, got %+v", r.Labels)
	}
}

// TestResolve_ZeroClosingIssueNumber: números inválidos (0) se skippean sin
// llamar al fetcher — defense en profundidad si la API devuelve algo raro.
func TestResolve_ZeroClosingIssueNumber(t *testing.T) {
	called := 0
	defer SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		called++
		if n == 0 {
			t.Fatalf("fetcher should skip n=0")
		}
		return []string{"che:validated"}, nil
	})()

	r := Resolve("140", nil, []int{0, 5})
	if called != 1 {
		t.Fatalf("expected fetcher called once (for 5), got %d", called)
	}
	if r.IssueNumber != 5 {
		t.Fatalf("expected IssueNumber=5, got %d", r.IssueNumber)
	}
}

// TestHasCheStateLabel cubre la membership check. Importante: che:locked
// NO es un label de estado (es el lock del recurso) — no debe disparar la
// resolución al issue.
func TestHasCheStateLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"empty", nil, false},
		{"che:idea", []string{"che:idea"}, true},
		{"che:planning", []string{"che:planning"}, true},
		{"che:plan", []string{"che:plan"}, true},
		{"che:executing", []string{"che:executing"}, true},
		{"che:executed", []string{"che:executed"}, true},
		{"che:validating", []string{"che:validating"}, true},
		{"che:validated", []string{"che:validated"}, true},
		{"che:closing", []string{"che:closing"}, true},
		{"che:closed", []string{"che:closed"}, true},
		{"che:locked alone NO counts", []string{"che:locked"}, false},
		{"only bug labels", []string{"bug", "priority:high"}, false},
		{"mixed with che:locked first", []string{"che:locked", "che:executed"}, true},
		{"validated:* alone NO counts", []string{"validated:approve"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasCheStateLabel(c.labels); got != c.want {
				t.Fatalf("hasCheStateLabel(%v) = %v, want %v", c.labels, got, c.want)
			}
		})
	}
}

// TestResolution_HasLabel protege el helper del struct.
func TestResolution_HasLabel(t *testing.T) {
	r := Resolution{Labels: []string{"che:executed", "ct:plan"}}
	if !r.HasLabel("che:executed") {
		t.Errorf("expected HasLabel(che:executed)=true")
	}
	if !r.HasLabel("ct:plan") {
		t.Errorf("expected HasLabel(ct:plan)=true")
	}
	if r.HasLabel("che:validated") {
		t.Errorf("expected HasLabel(che:validated)=false")
	}
	empty := Resolution{}
	if empty.HasLabel("che:executed") {
		t.Errorf("empty resolution shouldn't have labels")
	}
}
