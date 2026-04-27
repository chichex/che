package dash

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
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
			// PR3: labels migrados de status:* a che:*.
			if !hasLabel(i.Labels, "che:idea") {
				t.Errorf("issue #7 should have che:idea label")
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

func TestCombineEntities_AdoptsPRsWithoutClosingRefs(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	entities := combineEntities(issues, prs)

	// PR #44 no tiene closingIssuesReferences → cae a adopt (Kind=KindPR,
	// Status="adopt"). Antes se excluía silenciosamente; ahora aparece en
	// la columna opt-in "adopt".
	var pr44 *Entity
	for i := range entities {
		if entities[i].PRNumber == 44 && entities[i].Kind == KindPR {
			pr44 = &entities[i]
			break
		}
	}
	if pr44 == nil {
		t.Fatalf("PR #44 (sin closing refs) debería aparecer como KindPR/adopt; got entities=%+v", entities)
	}
	if pr44.Status != "adopt" {
		t.Errorf("PR #44 Status: got %q want adopt", pr44.Status)
	}
	if pr44.IssueNumber != 0 {
		t.Errorf("PR #44 IssueNumber: got %d want 0 (sin issue linkeado)", pr44.IssueNumber)
	}
}

// TestCombineEntities_FiltersClosedAdoptPRs: regresión abril 2026. La
// columna "adopt" mostraba PRs closed/merged que no tenían close-keyword
// (orphans) — el usuario los veía como adoptables y eso confundía. Ahora
// los closed orphans se filtran silenciosamente: si está cerrado y no hay
// nada que trackear via che, no hay nada que adoptar.
//
// También cubre el branch fused: PR closed con close-keyword a un issue
// sin che:* labels → antes caía a adopt vía Status="" fallback; ahora se
// filtra.
func TestCombineEntities_FiltersClosedAdoptPRs(t *testing.T) {
	prs := []ghPR{
		{Number: 100, Title: "open orphan", State: "OPEN"},                                                                        // visible en adopt
		{Number: 101, Title: "merged orphan", State: "MERGED"},                                                                    // filtrado
		{Number: 102, Title: "closed orphan", State: "CLOSED"},                                                                    // filtrado
		{Number: 103, Title: "open fused sin che:*", State: "OPEN", ClosingIssuesReferences: []ghCloseRef{{Number: 200}}},         // visible en adopt (fused)
		{Number: 104, Title: "merged fused sin che:*", State: "MERGED", ClosingIssuesReferences: []ghCloseRef{{Number: 201}}},     // filtrado
	}
	issues := []ghIssue{
		{Number: 200, Title: "issue sin che:*", Labels: []ghLabel{{Name: "ct:plan"}}},
		{Number: 201, Title: "issue sin che:*", Labels: []ghLabel{{Name: "ct:plan"}}},
	}
	entities := combineEntities(issues, prs)

	got := map[int]string{}
	for _, e := range entities {
		got[e.PRNumber] = e.Status
	}

	if s, ok := got[100]; !ok || s != "adopt" {
		t.Errorf("PR #100 (open orphan) debería estar en adopt, got status=%q present=%v", s, ok)
	}
	if _, ok := got[101]; ok {
		t.Errorf("PR #101 (merged orphan) NO debería aparecer — adopt filtra closed/merged")
	}
	if _, ok := got[102]; ok {
		t.Errorf("PR #102 (closed orphan) NO debería aparecer — adopt filtra closed/merged")
	}
	if s, ok := got[103]; !ok || s != "adopt" {
		t.Errorf("PR #103 (open fused sin che:*) debería estar en adopt, got status=%q present=%v", s, ok)
	}
	if _, ok := got[104]; ok {
		t.Errorf("PR #104 (merged fused sin che:*) NO debería aparecer — adopt filtra closed/merged")
	}
}

// TestCombineEntities_PRWithCheLabelDirect cubre el contrato post-stateref
// v0.0.61: si un PR tiene un label che:* directo (sin issue linkeado, o con
// issue sin che:*), `applyLabels` setea Status desde ese label y NO se pisa
// con "adopt". Antes el override forzaba Status="adopt" aunque el PR ya
// tuviera che:validated/che:executed/etc. directo.
//
// Casos cubiertos:
//   - PR sin closingIssuesReferences + che:validated directo en el PR →
//     Status="validated", NO "adopt".
//   - PR sin closingIssuesReferences sin ningún che:* en ningún lado →
//     Status="adopt" (el caso real de adopción).
//   - PR con closingIssuesReferences pero issue sin che:* y PR con
//     che:executed directo → Status="executed", NO "adopt".
//   - PR con closingIssuesReferences, issue sin che:*, PR sin che:* →
//     Status="adopt" (fallback honesto al adopt).
func TestCombineEntities_PRWithCheLabelDirect(t *testing.T) {
	prs := []ghPR{
		// PR sin issue, con che:validated directo → respeta el estado.
		{Number: 200, Title: "PR validated directo", State: "OPEN",
			Labels: []ghLabel{{Name: "che:validated"}}},
		// PR sin issue, sin che:* → adopt legítimo.
		{Number: 201, Title: "PR adopt puro", State: "OPEN"},
		// PR con issue sin che:* + che:executed directo en el PR.
		{Number: 202, Title: "PR fused executed directo", State: "OPEN",
			Labels:                  []ghLabel{{Name: "che:executed"}},
			ClosingIssuesReferences: []ghCloseRef{{Number: 300}}},
		// PR con issue sin che:* + sin che:* en el PR → adopt fused.
		{Number: 203, Title: "PR fused adopt", State: "OPEN",
			ClosingIssuesReferences: []ghCloseRef{{Number: 301}}},
	}
	issues := []ghIssue{
		{Number: 300, Title: "issue sin che:*", Labels: []ghLabel{{Name: "ct:plan"}}},
		{Number: 301, Title: "issue sin che:*", Labels: []ghLabel{{Name: "ct:plan"}}},
	}
	entities := combineEntities(issues, prs)

	got := map[int]Entity{}
	for _, e := range entities {
		got[e.PRNumber] = e
	}

	// Caso 1: PR sin issue + che:validated directo → Status="validated".
	pr200, ok := got[200]
	if !ok {
		t.Fatalf("PR #200 (che:validated directo) debería aparecer en entities")
	}
	if pr200.Status != "validated" {
		t.Errorf("PR #200 Status: got %q want %q (post-stateref el che:* directo del PR manda)", pr200.Status, "validated")
	}
	if pr200.Kind != KindPR {
		t.Errorf("PR #200 Kind: got %v want KindPR", pr200.Kind)
	}

	// Caso 2: PR sin issue, sin che:* → Status="adopt" (legítimo).
	pr201, ok := got[201]
	if !ok {
		t.Fatalf("PR #201 (sin che:*) debería aparecer en entities")
	}
	if pr201.Status != "adopt" {
		t.Errorf("PR #201 Status: got %q want %q (sin che:* en ningún lado → adopt)", pr201.Status, "adopt")
	}

	// Caso 3: fused (issue sin che:*) + che:executed directo en el PR.
	pr202, ok := got[202]
	if !ok {
		t.Fatalf("PR #202 (fused executed directo) debería aparecer en entities")
	}
	if pr202.Status != "executed" {
		t.Errorf("PR #202 Status: got %q want %q (issue sin che:* pero PR con che:executed)", pr202.Status, "executed")
	}
	if pr202.Kind != KindFused {
		t.Errorf("PR #202 Kind: got %v want KindFused (hay issue linkeado)", pr202.Kind)
	}

	// Caso 4: fused sin che:* en ningún lado → adopt.
	pr203, ok := got[203]
	if !ok {
		t.Fatalf("PR #203 (fused adopt) debería aparecer en entities")
	}
	if pr203.Status != "adopt" {
		t.Errorf("PR #203 Status: got %q want %q (issue sin che:* y PR sin che:*)", pr203.Status, "adopt")
	}
	if pr203.Kind != KindFused {
		t.Errorf("PR #203 Kind: got %v want KindFused", pr203.Kind)
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
	// PR3: columnas 1-a-1 con Status. Issue #22 (fused con PR #40) tiene
	// che:validated en el fixture → cae en "validated", no "approved".
	if pr40.Column() != "validated" {
		t.Errorf("PR #40 (status=validated) should land in 'validated' column; got %q", pr40.Column())
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
		name                       string
		checks                     []ghCheck
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
			name:   "Empty → zero",
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
		name    string
		seconds int
		wantHas string
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

// TestApplyLabels_Che cubre el parsing de labels che:* (PR3): cada sufijo
// del prefijo debe terminar en e.Status. che:locked es la excepción —
// prende e.Locked y NO toca Status.
func TestApplyLabels_Che(t *testing.T) {
	cases := []struct {
		name       string
		ls         []ghLabel
		wantStatus string
		wantLocked bool
	}{
		{
			name:       "che:idea → status idea",
			ls:         []ghLabel{{Name: "che:idea"}},
			wantStatus: "idea",
		},
		{
			name:       "che:planning → status planning",
			ls:         []ghLabel{{Name: "che:planning"}},
			wantStatus: "planning",
		},
		{
			name:       "che:closed → status closed",
			ls:         []ghLabel{{Name: "che:closed"}},
			wantStatus: "closed",
		},
		{
			name:       "che:locked → Locked=true, Status vacío",
			ls:         []ghLabel{{Name: "che:locked"}},
			wantLocked: true,
		},
		{
			name:       "che:executing + che:locked → status executing + locked",
			ls:         []ghLabel{{Name: "che:executing"}, {Name: "che:locked"}},
			wantStatus: "executing",
			wantLocked: true,
		},
		{
			name: "status:* legacy ya no impacta Status (post-PR1/PR2)",
			ls:   []ghLabel{{Name: "status:plan"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e Entity
			applyLabels(&e, tc.ls)
			if e.Status != tc.wantStatus {
				t.Errorf("Status: got %q want %q", e.Status, tc.wantStatus)
			}
			if e.Locked != tc.wantLocked {
				t.Errorf("Locked: got %v want %v", e.Locked, tc.wantLocked)
			}
		})
	}
}

// TestApplyLabels_PlanValidatedNotShadowedByValidated: el switch debe
// chequear plan-validated:* antes que validated:* — caso contrario un label
// "plan-validated:approve" matchearía el case "validated:" y dejaría
// PRVerdict="approve" / PlanVerdict="" (bug invertido). Es defensa contra
// reordenar los cases del switch sin querer.
func TestApplyLabels_PlanValidatedNotShadowedByValidated(t *testing.T) {
	var e Entity
	applyLabels(&e, []ghLabel{{Name: "plan-validated:approve"}})
	if e.PlanVerdict != "approve" {
		t.Errorf("PlanVerdict: got %q want approve", e.PlanVerdict)
	}
	if e.PRVerdict != "" {
		t.Errorf("PRVerdict debería quedar vacío con label plan-validated:approve; got %q", e.PRVerdict)
	}
}

// TestCombineEntities_SkipsIssuesWithoutCheLabel garantiza que issues-only
// sin label `che:*` no aparecen en el board. Antes caían al default "idea"
// de Column() (defensa), lo cual enmascaraba issues mal tageados como si
// fueran ideas legítimas.
//
// NOTA: adopt mode NO aplica a issue-only — la columna adopt es solo para
// PRs untracked. Un issue sin che:* sigue siendo "raro" (el humano lo ve
// via `gh issue list`), no un candidato a adoptar.
func TestCombineEntities_SkipsIssuesWithoutCheLabel(t *testing.T) {
	// issues.json incluye #100 "idea sin clasificar aún (ct:plan solo)" con
	// solo ct:plan y sin che:*. Debería estar excluido.
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
		if e.IssueNumber == 100 && e.Kind == KindIssue {
			t.Errorf("issue #100 (ct:plan sin che:*) debería estar excluido; got %+v", e)
		}
	}
}

// TestCombineEntities_ClosedIssuesIncluded simula el merge de issues open +
// closed que hace refresh(). El caller real (refresh()) hace `append(open,
// closed...)` antes de llamar a combineEntities — replicamos eso acá.
func TestCombineEntities_ClosedIssuesIncluded(t *testing.T) {
	openIssues := []ghIssue{
		{Number: 7, Title: "open idea", Labels: []ghLabel{{Name: "ct:plan"}, {Name: "che:idea"}}},
	}
	closedIssues := []ghIssue{
		{Number: 5, Title: "old done", State: "CLOSED", Labels: []ghLabel{{Name: "ct:plan"}, {Name: "che:closed"}}},
	}
	all := append([]ghIssue{}, openIssues...)
	all = append(all, closedIssues...)

	entities := combineEntities(all, nil)

	var got5 *Entity
	for i := range entities {
		if entities[i].IssueNumber == 5 {
			got5 = &entities[i]
		}
	}
	if got5 == nil {
		t.Fatalf("issue #5 closed no quedó en entities; got=%+v", entities)
	}
	if got5.Status != "closed" {
		t.Errorf("issue #5 status: got %q want closed", got5.Status)
	}
	if got5.Column() != "closed" {
		t.Errorf("issue #5 columna: got %q want closed", got5.Column())
	}
}

// ==================================================================
// Bump / adaptive polling (GhSource.Bump, Run select loop)
// ==================================================================

// newBumpableSource construye un GhSource "desconectado" de gh — sin pasar
// por NewGhSource (que valida `gh` en PATH). Setea los campos mínimos para
// que Run pueda correr con un refreshFn inyectado. Evita shells stubs y
// deja los tests ejerciendo solo la lógica del canal + ticker + minGap.
func newBumpableSource(interval time.Duration, fn func(context.Context) error) *GhSource {
	return &GhSource{
		interval:  interval,
		ClosedCap: defaultClosedCap,
		bump:      make(chan struct{}, 1),
		refreshFn: fn,
	}
}

// waitCount espera hasta que el counter atómico alcance `want` o se agote
// el timeout. Devuelve el último valor observado — los callers chequean
// igualdad exacta.
func waitCount(t *testing.T, c *atomic.Int64, want int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := c.Load(); got >= want {
			return got
		}
		if time.Now().After(deadline) {
			return c.Load()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBump_TriggersRefreshOutsideTicker: con interval=1h (ticker
// inefectivo en el test), un Bump() dispara un refresh extra sin esperar
// al tick baseline.
func TestBump_TriggersRefreshOutsideTicker(t *testing.T) {
	var count atomic.Int64
	fn := func(_ context.Context) error {
		count.Add(1)
		return nil
	}
	g := newBumpableSource(1*time.Hour, fn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	// Primer poll inmediato → count==1.
	if got := waitCount(t, &count, 1, 1*time.Second); got != 1 {
		t.Fatalf("initial poll: got count=%d want 1", got)
	}

	// Bump sin esperar minGap — debería NO disparar (el doRefresh guardea).
	g.Bump()
	time.Sleep(200 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("bump within minGap: count=%d want 1 (should be suppressed by minGap)", got)
	}

	// Esperar que pase minGap y volver a bumpear → count==2.
	time.Sleep(bumpMinGap + 100*time.Millisecond)
	g.Bump()
	if got := waitCount(t, &count, 2, 1*time.Second); got != 2 {
		t.Errorf("bump after minGap: got count=%d want 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}

// TestBump_RespectsMinGap: múltiples Bump() dentro del minGap solo
// disparan un refresh. Después del minGap el siguiente Bump sí pasa.
func TestBump_RespectsMinGap(t *testing.T) {
	var count atomic.Int64
	fn := func(_ context.Context) error {
		count.Add(1)
		return nil
	}
	g := newBumpableSource(1*time.Hour, fn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	// Primer poll → 1.
	waitCount(t, &count, 1, 1*time.Second)

	// 3 bumps en fila dentro del minGap → ningún refresh extra.
	g.Bump()
	g.Bump()
	g.Bump()
	time.Sleep(300 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Errorf("bumps within minGap: count=%d want 1", got)
	}

	// Pasado el minGap, un solo bump dispara.
	time.Sleep(bumpMinGap + 100*time.Millisecond)
	g.Bump()
	if got := waitCount(t, &count, 2, 1*time.Second); got != 2 {
		t.Errorf("bump after minGap: count=%d want 2", got)
	}

	cancel()
	<-done
}

// TestBump_CoalescesMultipleCalls: el canal tiene capacidad 1, así que N
// bumps rápidos se colapsan a lo sumo a 1 refresh extra (el primero que
// cabe en el buffer; los demás se dropean en el `default` del select de
// Bump).
func TestBump_CoalescesMultipleCalls(t *testing.T) {
	var count atomic.Int64
	// Bloqueamos el primer refresh con un canal para que los Bump() lleguen
	// mientras el loop todavía no consumió el buffer — garantiza el test
	// de coalescencia.
	release := make(chan struct{})
	releasedOnce := false
	fn := func(_ context.Context) error {
		count.Add(1)
		if !releasedOnce {
			releasedOnce = true
			<-release
		}
		return nil
	}
	g := newBumpableSource(1*time.Hour, fn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	// Esperar hasta que el primer poll empiece (está bloqueado en fn).
	waitCount(t, &count, 1, 1*time.Second)
	// Con el loop trabado en fn, 50 Bump() consecutivos → como máximo 1
	// cabe en el canal (buffer=1), el resto se dropea en el `default`.
	for i := 0; i < 50; i++ {
		g.Bump()
	}
	// Liberar el primer refresh; el loop va a consumir el bump pendiente.
	close(release)
	// El minGap entra en juego: el bump coalesced llega "justo después"
	// del primer refresh. Como count ya es 1 y el primer refresh seteó
	// lastRefresh, el doRefresh del bump va a encontrar time.Since(...)
	// < minGap y suprimirlo. Aceptamos 1 o 2 refreshes totales — el
	// contrato del test es "los 50 bumps NO generan 50 refreshes".
	time.Sleep(300 * time.Millisecond)
	if got := count.Load(); got > 2 {
		t.Errorf("coalescing failed: got count=%d, 50 bumps should collapse to ≤2 refreshes", got)
	}

	cancel()
	<-done
}
// cerrado (che:closed) + su PR mergeado (con closingIssuesReferences
// apuntándolo) se fusionen en una entidad KindFused. Antes se renderaba
// como KindIssue (single ref) porque fetchPRs solo traía --state open;
// fetchClosedPRs cerró ese hueco.
func TestCombineEntities_FusedClosedIssueAndPR(t *testing.T) {
	closedIssues := []ghIssue{
		{Number: 121, Title: "Provider Web App", State: "CLOSED",
			Labels: []ghLabel{{Name: "ct:plan"}, {Name: "che:closed"}}},
	}
	closedPRs := []ghPR{
		{Number: 130, Title: "feat: provider web app", State: "MERGED",
			ClosingIssuesReferences: []ghCloseRef{{Number: 121}}},
	}

	entities := combineEntities(closedIssues, closedPRs)

	var got *Entity
	for i := range entities {
		if entities[i].IssueNumber == 121 {
			got = &entities[i]
		}
	}
	if got == nil {
		t.Fatalf("issue #121 no quedó en entities; got=%+v", entities)
	}
	if got.Kind != KindFused {
		t.Errorf("issue #121 Kind: got %v want KindFused (debería fusionarse con !130)", got.Kind)
	}
	if got.PRNumber != 130 {
		t.Errorf("issue #121 PRNumber: got %d want 130", got.PRNumber)
	}
	if got.Column() != "closed" {
		t.Errorf("issue #121 columna: got %q want closed", got.Column())
	}
}

// TestCombineEntities_AdoptPRs cubre el caso "adopt mode" (feature v0.0.71):
// PRs que antes se descartaban silenciosamente ahora entran como Status=adopt.
// Dos escenarios:
//   - PR sin closingIssuesReferences → Kind=KindPR, sin IssueNumber.
//   - PR con closingIssuesReferences apuntando a un issue que no tiene che:*
//     → Kind=KindFused (hay issue linkeado), Status=adopt.
// Caso negativo: PR con issue che-trackeado sigue derivando su Status del
// label che:* y NO cae a adopt.
func TestCombineEntities_AdoptPRs(t *testing.T) {
	issues, err := parseIssues(readFixture(t, "adopt-issues.json"))
	if err != nil {
		t.Fatalf("parseIssues: %v", err)
	}
	prs, err := parsePRs(readFixture(t, "adopt-prs.json"))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}

	// Agregamos un PR extra (con closing ref a issue #501, que SÍ tiene
	// che:plan) para probar el caso negativo in-situ.
	prs = append(prs, ghPR{
		Number: 303, Title: "PR que cierra issue tracked",
		HeadRefName:             "feat/tracked",
		ClosingIssuesReferences: []ghCloseRef{{Number: 501}},
	})

	entities := combineEntities(issues, prs)

	// Caso 1: PR #301 sin closing refs → KindPR, Status=adopt.
	var pr301 *Entity
	for i := range entities {
		if entities[i].PRNumber == 301 {
			pr301 = &entities[i]
		}
	}
	if pr301 == nil {
		t.Fatalf("PR #301 no quedó en entities; got=%+v", entities)
	}
	if pr301.Kind != KindPR {
		t.Errorf("PR #301 Kind: got %v want KindPR (%d)", pr301.Kind, KindPR)
	}
	if pr301.Status != "adopt" {
		t.Errorf("PR #301 Status: got %q want adopt", pr301.Status)
	}
	if pr301.IssueNumber != 0 {
		t.Errorf("PR #301 IssueNumber: got %d want 0", pr301.IssueNumber)
	}
	if pr301.Column() != "adopt" {
		t.Errorf("PR #301 Column: got %q want adopt", pr301.Column())
	}

	// Caso 2: PR #302 cierra issue #500 (que solo tiene label "bug", sin
	// che:*) → Kind=KindFused, Status=adopt.
	var pr302 *Entity
	for i := range entities {
		if entities[i].PRNumber == 302 {
			pr302 = &entities[i]
		}
	}
	if pr302 == nil {
		t.Fatalf("PR #302 no quedó en entities; got=%+v", entities)
	}
	if pr302.Kind != KindFused {
		t.Errorf("PR #302 Kind: got %v want KindFused (hay issue linkeado)", pr302.Kind)
	}
	if pr302.Status != "adopt" {
		t.Errorf("PR #302 Status: got %q want adopt", pr302.Status)
	}
	if pr302.IssueNumber != 500 {
		t.Errorf("PR #302 IssueNumber: got %d want 500", pr302.IssueNumber)
	}

	// Caso negativo: PR #303 cierra #501 (che:plan tracked) → Status="plan",
	// NO adopt.
	var pr303 *Entity
	for i := range entities {
		if entities[i].PRNumber == 303 {
			pr303 = &entities[i]
		}
	}
	if pr303 == nil {
		t.Fatalf("PR #303 no quedó en entities; got=%+v", entities)
	}
	if pr303.Status == "adopt" {
		t.Errorf("PR #303 Status: got adopt, want plan (issue #501 SÍ tiene che:plan — no corresponde adoptar)")
	}
	if pr303.Status != "plan" {
		t.Errorf("PR #303 Status: got %q want plan", pr303.Status)
	}
}

