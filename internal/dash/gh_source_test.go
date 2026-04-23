package dash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// staticErr es un error con mensaje fijo — usado por TestErrShort sin tocar fmt.
type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }

// nowMinus devuelve time.Now() - seconds. Helper para TestHumanAgo.
func nowMinus(seconds int) time.Time {
	return time.Now().Add(-time.Duration(seconds) * time.Second)
}

// zeroTime devuelve el zero value de time.Time. Helper para TestHumanAgo.
func zeroTime() time.Time { return time.Time{} }

// readFixture lee un fixture de testdata/ — helper para los tests de parseo.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestMockSourceImplementsSource es un compile-time check, duplicado acá
// como test explícito para que el intent quede en el suite.
func TestMockSourceImplementsSource(t *testing.T) {
	var _ Source = MockSource{}
	snap := MockSource{}.Snapshot()
	if !snap.Mock {
		t.Errorf("MockSource snapshot should have Mock=true")
	}
	if snap.LastOK.IsZero() {
		t.Errorf("MockSource snapshot should have LastOK set")
	}
	if len(snap.Entities) == 0 {
		t.Errorf("MockSource snapshot should have entities")
	}
}

func TestParseIssues(t *testing.T) {
	data := readFixture(t, "issues.json")
	issues, err := parseIssues(data)
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	if len(issues) == 0 {
		t.Fatalf("parseIssues: got 0 issues, want >0")
	}
	// El fixture tiene #7 "che dash web local".
	var found bool
	for _, i := range issues {
		if i.Number == 7 && i.Title == "che dash web local" {
			found = true
			if !hasLabel(i.Labels, "ct:plan") {
				t.Errorf("issue #7 should have ct:plan label")
			}
			if !hasLabel(i.Labels, "status:idea") {
				t.Errorf("issue #7 should have status:idea label")
			}
		}
	}
	if !found {
		t.Errorf("parseIssues: issue #7 not found in fixture")
	}
}

func TestParsePRs(t *testing.T) {
	data := readFixture(t, "prs.json")
	prs, err := parsePRs(data)
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	if len(prs) == 0 {
		t.Fatalf("parsePRs: got 0 PRs, want >0")
	}
	// PR #55 tiene closing ref a #42.
	var found bool
	for _, p := range prs {
		if p.Number == 55 {
			found = true
			if len(p.ClosingIssuesReferences) != 1 || p.ClosingIssuesReferences[0].Number != 42 {
				t.Errorf("PR #55 should close #42, got %+v", p.ClosingIssuesReferences)
			}
			if p.HeadRefName != "feat/dash-fusion" {
				t.Errorf("PR #55 headRefName: got %q want feat/dash-fusion", p.HeadRefName)
			}
		}
	}
	if !found {
		t.Errorf("parsePRs: PR #55 not found in fixture")
	}
}

func TestCombineEntities_FiltersIssuesWithoutCtPlan(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// Issue #99 no tiene ct:plan → debería estar excluido.
	for _, e := range entities {
		if e.IssueNumber == 99 && e.Kind == KindIssue {
			t.Errorf("entity #99 (sin ct:plan) debería estar excluida; got %+v", e)
		}
	}
}

func TestCombineEntities_FusesPRsWithClosingRefs(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// PR #48 cierra #33 → fused, PRNumber=48, IssueNumber=33.
	var fused48 *Entity
	for i := range entities {
		if entities[i].PRNumber == 48 {
			fused48 = &entities[i]
			break
		}
	}
	if fused48 == nil {
		t.Fatalf("combineEntities: PR #48 fused entity not found")
	}
	if fused48.Kind != KindFused {
		t.Errorf("PR #48 should be KindFused, got %v", fused48.Kind)
	}
	if fused48.IssueNumber != 33 {
		t.Errorf("PR #48 should link issue #33, got %d", fused48.IssueNumber)
	}
	if fused48.IssueTitle != "refactor logger unificado" {
		t.Errorf("PR #48 issue title: got %q want 'refactor logger unificado'", fused48.IssueTitle)
	}
	if fused48.Type != "mejora" {
		t.Errorf("PR #48 should inherit type=mejora from issue; got %q", fused48.Type)
	}
	if fused48.Status != "executing" {
		t.Errorf("PR #48 should inherit status=executing from issue; got %q", fused48.Status)
	}
	if !fused48.Locked {
		t.Errorf("PR #48 should be Locked (che:locked en PR o issue)")
	}
	if fused48.Branch != "feat/logger-unif" {
		t.Errorf("PR #48 branch: got %q want feat/logger-unif", fused48.Branch)
	}
	if fused48.SHA != "3c12aa8" {
		t.Errorf("PR #48 SHA (short): got %q want 3c12aa8", fused48.SHA)
	}

	// El issue #33 NO debe aparecer como KindIssue (fue consumido).
	for _, e := range entities {
		if e.IssueNumber == 33 && e.Kind == KindIssue {
			t.Errorf("issue #33 should be consumed by PR #48, not emitted as KindIssue")
		}
	}
}

func TestCombineEntities_SkipsPRsWithoutClosingRefs(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// PR #44 no tiene closingIssuesReferences → debería estar excluido.
	for _, e := range entities {
		if e.PRNumber == 44 {
			t.Errorf("PR #44 (sin closing refs) debería estar excluido; got %+v", e)
		}
	}
}

func TestCombineEntities_PRVerdictFromLabels(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// PR #55 tiene validated:changes-requested → PRVerdict debería ser ese.
	var pr55 *Entity
	for i := range entities {
		if entities[i].PRNumber == 55 {
			pr55 = &entities[i]
			break
		}
	}
	if pr55 == nil {
		t.Fatalf("PR #55 not found")
	}
	if pr55.PRVerdict != "changes-requested" {
		t.Errorf("PR #55 verdict: got %q want changes-requested", pr55.PRVerdict)
	}
	// PR #40 → validated:approve, issue #22 executed.
	var pr40 *Entity
	for i := range entities {
		if entities[i].PRNumber == 40 {
			pr40 = &entities[i]
			break
		}
	}
	if pr40 == nil {
		t.Fatalf("PR #40 not found")
	}
	if pr40.PRVerdict != "approve" {
		t.Errorf("PR #40 verdict: got %q want approve", pr40.PRVerdict)
	}
	if pr40.Column() != "approved" {
		t.Errorf("PR #40 with approve verdict should land in 'approved' column; got %q", pr40.Column())
	}
}

func TestCombineEntities_IssueOnlyPreserved(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// Issue #7 (ct:plan, no PR) → KindIssue.
	var found bool
	for _, e := range entities {
		if e.IssueNumber == 7 && e.Kind == KindIssue {
			found = true
			if e.IssueTitle != "che dash web local" {
				t.Errorf("issue #7 title: got %q", e.IssueTitle)
			}
			if e.Type != "feature" {
				t.Errorf("issue #7 type: got %q want feature", e.Type)
			}
			if e.Status != "idea" {
				t.Errorf("issue #7 status: got %q want idea", e.Status)
			}
		}
	}
	if !found {
		t.Errorf("issue #7 should be emitted as KindIssue")
	}

	// Issue #38 → KindIssue con plan-validated:approve.
	for _, e := range entities {
		if e.IssueNumber == 38 {
			if e.PlanVerdict != "approve" {
				t.Errorf("issue #38 PlanVerdict: got %q want approve", e.PlanVerdict)
			}
		}
	}
}

// TestCountChecks cubre la lógica de conteo de status/conclusion con múltiples
// shapes (CheckRun + StatusContext) y estados intermedios.
func TestCountChecks(t *testing.T) {
	cases := []struct {
		name                  string
		checks                []ghCheck
		wantOK, wantPend, wantFail int
	}{
		{
			name: "all SUCCESS CheckRuns",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "SUCCESS", Status: "COMPLETED"},
				{TypeName: "CheckRun", Conclusion: "SUCCESS", Status: "COMPLETED"},
			},
			wantOK: 2,
		},
		{
			name: "CheckRun IN_PROGRESS → pending (aunque conclusion vacío)",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "", Status: "IN_PROGRESS"},
			},
			wantPend: 1,
		},
		{
			name: "CheckRun QUEUED → pending",
			checks: []ghCheck{
				{TypeName: "CheckRun", Status: "QUEUED"},
			},
			wantPend: 1,
		},
		{
			name: "CheckRun FAILURE → fail",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "FAILURE", Status: "COMPLETED"},
			},
			wantFail: 1,
		},
		{
			name: "StatusContext SUCCESS via state",
			checks: []ghCheck{
				{TypeName: "StatusContext", State: "SUCCESS"},
			},
			wantOK: 1,
		},
		{
			name: "StatusContext PENDING via state",
			checks: []ghCheck{
				{TypeName: "StatusContext", State: "PENDING"},
			},
			wantPend: 1,
		},
		{
			name: "StatusContext ERROR → fail",
			checks: []ghCheck{
				{TypeName: "StatusContext", State: "ERROR"},
			},
			wantFail: 1,
		},
		{
			name: "Mixed: CheckRun FAILURE + StatusContext ERROR + SUCCESS + pending",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "SUCCESS", Status: "COMPLETED"},
				{TypeName: "CheckRun", Conclusion: "FAILURE", Status: "COMPLETED"},
				{TypeName: "StatusContext", State: "ERROR"},
				{TypeName: "CheckRun", Status: "IN_PROGRESS"},
			},
			wantOK: 1, wantPend: 1, wantFail: 2,
		},
		{
			name: "Desconocido → pending (defensa)",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "WHATEVER", Status: "COMPLETED"},
			},
			wantPend: 1,
		},
		{
			name: "Empty → zero",
			checks: nil,
		},
		{
			name: "TIMED_OUT y CANCELLED → fail",
			checks: []ghCheck{
				{TypeName: "CheckRun", Conclusion: "TIMED_OUT", Status: "COMPLETED"},
				{TypeName: "CheckRun", Conclusion: "CANCELLED", Status: "COMPLETED"},
			},
			wantFail: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, pend, fail := countChecks(tc.checks)
			if ok != tc.wantOK || pend != tc.wantPend || fail != tc.wantFail {
				t.Errorf("counts: got (ok=%d,pend=%d,fail=%d) want (ok=%d,pend=%d,fail=%d)",
					ok, pend, fail, tc.wantOK, tc.wantPend, tc.wantFail)
			}
		})
	}
}

// TestCombineEntities_ChecksFromFixture: PR #55 tiene 8 SUCCESS + 1 QUEUED.
func TestCombineEntities_ChecksFromFixture(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)
	for _, e := range entities {
		if e.PRNumber == 55 {
			if e.ChecksOK != 8 || e.ChecksPending != 1 || e.ChecksFail != 0 {
				t.Errorf("PR #55 checks: got (ok=%d,pend=%d,fail=%d) want (8,1,0)",
					e.ChecksOK, e.ChecksPending, e.ChecksFail)
			}
		}
	}
}

func TestHumanAgo(t *testing.T) {
	// Solo chequeamos el prefijo / forma; los valores exactos dependen del
	// tiempo de corrida.
	cases := []struct {
		name     string
		seconds  int
		wantHas  string
	}{
		{"3s", 3, "hace 3s"},
		{"59s", 59, "hace 59s"},
		{"61s → 1m", 61, "hace 1m"},
		{"4m", 240, "hace 4m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanAgo(nowMinus(tc.seconds))
			if got != tc.wantHas {
				t.Errorf("humanAgo: got %q want %q", got, tc.wantHas)
			}
		})
	}
	if got := humanAgo(zeroTime()); got != "nunca" {
		t.Errorf("humanAgo(zero): got %q want 'nunca'", got)
	}
}

// TestErrShort chequea el truncado a 40 chars.
func TestErrShort(t *testing.T) {
	if got := errShort(nil); got != "" {
		t.Errorf("errShort(nil): got %q want ''", got)
	}
	short := &staticErr{msg: "corto"}
	if got := errShort(short); got != "corto" {
		t.Errorf("errShort(short): got %q want 'corto'", got)
	}
	long := &staticErr{msg: "este es un mensaje de error bastante largo que excede el limite de cuarenta chars"}
	got := errShort(long)
	if len(got) != 40 {
		t.Errorf("errShort(long) len: got %d want 40 (got=%q)", len(got), got)
	}
}
