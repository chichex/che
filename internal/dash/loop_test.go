package dash

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ================== Matcher puro ==================

// TestNextDispatch_RuleTable es la tabla de decisión del matcher. Cada
// caso fija el contrato de un tramo de la máquina de 4 reglas — si rompe,
// alguien cambió la semántica y conviene discutirla antes de mergear.
func TestNextDispatch_RuleTable(t *testing.T) {
	tests := []struct {
		name      string
		e         Entity
		rules     map[LoopRule]bool
		rounds    int
		wantFlow  string
		wantRef   int
		wantSubstr string // substring que debe aparecer en reason
	}{
		{
			name:       "rule1: plan sin verdict + rule ON → validate",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			wantFlow:   "validate",
			wantRef:    42,
			wantSubstr: "validate-plan",
		},
		{
			name:       "rule1 OFF: plan sin verdict no dispatcha",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			rules:      map[LoopRule]bool{},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			// Post-v0.0.49: validate transiciona plan→validated antes de
			// setear el verdict. El estado real post-validate con
			// changes-requested es Status=validated, no Status=plan.
			name:       "rule2: validated + changes-requested + rule ON → iterate",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "validated", PlanVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePlan: true},
			wantFlow:   "iterate",
			wantRef:    42,
			wantSubstr: "iterate-plan",
		},
		{
			name:       "rule2 OFF: validated + changes-requested sin regla → no-rule-match",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "validated", PlanVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			// Fused en validated con PlanVerdict=changes-requested y
			// PRVerdict vacío: iterate-plan es issue-only (PlanVerdict). El
			// case fused mira PRVerdict; sin verdict no dispara nada.
			name:       "rule2: fused en validated con PlanVerdict (no PRVerdict) → no-rule-match",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PlanVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePlan: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			// Post-fix del iterate-pr: validate-pr transiciona executed→
			// validated y deja el verdict. iterate-pr debe matchear en ese
			// estado también.
			name:       "rule4: fused validated + PRVerdict=changes-requested + rule ON → iterate PR",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "iterate",
			wantRef:    77,
			wantSubstr: "iterate-pr",
		},
		{
			name:       "rule4 OFF: fused validated + changes-requested sin regla → no-rule-match",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			name:       "fused validated + PRVerdict=approve → pr-approved (stop)",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PRVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "pr-approved",
		},
		{
			name:       "fused validated + PRVerdict=needs-human → pr-needs-human",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PRVerdict: "needs-human"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "pr-needs-human",
		},
		{
			// Defensivo: fused sin PRNumber (raro, pero posible en snapshots
			// corruptos). No dispatchamos — iterate-pr necesita PR number.
			name:       "fused validated + changes-requested pero PR=0 → no-rule-match",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 0, Status: "validated", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			// KindPR (PR huérfano) post-adopt + validate dejó che:validated +
			// PRVerdict=changes-requested. Antes del fix abril 2026 el case
			// validated solo branchaba KindFused y este caso caía al branch
			// issue-only que mira PlanVerdict (siempre vacío para PR puro)
			// → no-rule-match. Ahora la rama PR cubre KindFused y KindPR.
			name:       "rule4: KindPR validated + PRVerdict=changes-requested + rule ON → iterate PR",
			e:          Entity{Kind: KindPR, PRNumber: 88, Status: "validated", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "iterate",
			wantRef:    88,
			wantSubstr: "iterate-pr",
		},
		{
			name:       "KindPR validated + PRVerdict=approve → pr-approved (stop)",
			e:          Entity{Kind: KindPR, PRNumber: 88, Status: "validated", PRVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "pr-approved",
		},
		{
			name:       "KindPR validated + PRVerdict=needs-human → pr-needs-human",
			e:          Entity{Kind: KindPR, PRNumber: 88, Status: "validated", PRVerdict: "needs-human"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "pr-needs-human",
		},
		{
			name:       "KindPR validated + changes-requested sin regla → no-rule-match",
			e:          Entity{Kind: KindPR, PRNumber: 88, Status: "validated", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			name:       "plan + approve → stop (no dispatch)",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true, RuleIteratePlan: true},
			wantFlow:   "",
			wantSubstr: "plan-approved",
		},
		{
			name:       "plan + needs-human → stop",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan", PlanVerdict: "needs-human"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			wantFlow:   "",
			wantSubstr: "plan-needs-human",
		},
		{
			name:       "rule3: executed sin verdict + rule ON → validate PR",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "executed"},
			rules:      map[LoopRule]bool{RuleValidatePR: true},
			wantFlow:   "validate",
			wantRef:    77,
			wantSubstr: "validate-pr",
		},
		{
			name:       "rule4: executed + changes-requested + rule ON → iterate PR",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "executed", PRVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePR: true},
			wantFlow:   "iterate",
			wantRef:    77,
			wantSubstr: "iterate-pr",
		},
		{
			name:       "executed + approve → stop",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "executed", PRVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleValidatePR: true, RuleIteratePR: true},
			wantFlow:   "",
			wantSubstr: "pr-approved",
		},
		{
			name:       "locked → skip (sin importar estado ni reglas)",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan", Locked: true},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			wantFlow:   "",
			wantSubstr: "locked",
		},
		{
			name:       "cap hit → stop",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			rounds:     LoopCap,
			wantFlow:   "",
			wantSubstr: "cap-reached",
		},
		{
			// idea es loopable post-RuleExploreIdea (v0.0.76+): si la regla
			// explore-idea está OFF y otras reglas ON, el matcher devuelve
			// "no-rule-match" (no "status-not-loopable" — ese reason quedó
			// para statuses no cubiertos como "planning", "closing", "closed").
			name:       "idea con explore-idea OFF → no-rule-match",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "idea"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			name:       "rule0: idea + explore-idea ON → explore",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "idea"},
			rules:      map[LoopRule]bool{RuleExploreIdea: true},
			wantFlow:   "explore",
			wantRef:    42,
			wantSubstr: "explore-idea",
		},
		{
			// Status="" = entity sin che:* (issue legacy o ct:plan recién
			// aplicado pero el watcher de labels no transicionó aún). El
			// gate y el matcher tratan "" e "idea" igual — ambos arrancan
			// el ciclo desde cero.
			name:       "rule0: status vacío + explore-idea ON → explore",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: ""},
			rules:      map[LoopRule]bool{RuleExploreIdea: true},
			wantFlow:   "explore",
			wantRef:    42,
			wantSubstr: "explore-idea",
		},
		{
			// explore es issue-first: KindFused/KindPR no escalan a explore
			// aunque tengan Status vacío (edge raro: PR adopt sin labels en
			// el issue linkeado). Cae en status-not-loopable.
			name:       "rule0: fused con status vacío + explore-idea ON → no dispatch",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: ""},
			rules:      map[LoopRule]bool{RuleExploreIdea: true},
			wantFlow:   "",
			wantSubstr: "status-not-loopable",
		},
		{
			// Status no cubierto por ningún case (ej: "planning", "closing",
			// "closed"). Sigue devolviendo "status-not-loopable" — ese reason
			// es para estados transient o terminales sin regla aplicable.
			name:       "status closed (terminal) → status-not-loopable",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "closed"},
			rules:      map[LoopRule]bool{RuleValidatePR: true},
			wantFlow:   "",
			wantSubstr: "status-not-loopable",
		},
		{
			// Fast-lane: plan sin verdict + execute-raw ON (sin validate-plan)
			// → execute directo, salteando validate.
			name:       "rule5: plan sin verdict + execute-raw ON → execute directo",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			rules:      map[LoopRule]bool{RuleExecuteRaw: true},
			wantFlow:   "execute",
			wantRef:    42,
			wantSubstr: "execute-raw",
		},
		{
			// Ambas ON: validate gana (preferimos validar antes de ejecutar
			// cuando el humano dejó las dos reglas activas).
			name:       "rule5: plan sin verdict + ambas ON → validate gana",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true, RuleExecuteRaw: true},
			wantFlow:   "validate",
			wantRef:    42,
			wantSubstr: "validate-plan",
		},
		{
			// Execute-raw NO matchea sobre plan-validated:approve (ese es
			// territorio de execute-plan). Mutual exclusion por (status,
			// verdict) — execute-raw exige verdict vacío.
			name:       "rule5 OFF: plan + approve no matchea execute-raw (es plan-approved → stop)",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "plan", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleExecuteRaw: true},
			wantFlow:   "",
			wantSubstr: "plan-approved",
		},
		{
			name:       "executed sin PR → no dispatch (defensivo)",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 0, Status: "executed"},
			rules:      map[LoopRule]bool{RuleValidatePR: true},
			wantFlow:   "",
			wantSubstr: "executed-without-pr",
		},
		{
			name:       "rule5: validated issue-only + approve + rule ON → execute",
			e:          Entity{Kind: KindIssue, IssueNumber: 122, Status: "validated", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "execute",
			wantRef:    122,
			wantSubstr: "execute-plan",
		},
		{
			name:       "rule5 OFF: validated + approve sin regla → no-rule-match",
			e:          Entity{Kind: KindIssue, IssueNumber: 122, Status: "validated", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			name:       "rule5 exige approve explícito: sin verdict → no dispatch",
			e:          Entity{Kind: KindIssue, IssueNumber: 122, Status: "validated"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			// Solo RuleExecutePlan ON: changes-requested no matchea (execute
			// pide approve); iterate-plan OFF, así que no hay dispatch.
			name:       "rule5: validated + changes-requested + solo execute-plan ON → no match",
			e:          Entity{Kind: KindIssue, IssueNumber: 122, Status: "validated", PlanVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
		{
			name:       "rule5: validated + needs-human → stop (humano)",
			e:          Entity{Kind: KindIssue, IssueNumber: 122, Status: "validated", PlanVerdict: "needs-human"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "",
			wantSubstr: "plan-needs-human",
		},
		{
			// Execute-plan no corre sobre fused: el case fused mira PRVerdict,
			// no PlanVerdict. Con PRVerdict vacío cae en no-rule-match
			// (ya no devuelve "validated-not-issue-only" — el case por Kind
			// se bifurca antes del early return viejo).
			name:       "rule5: fused validated + PlanVerdict=approve (sin PRVerdict) → no-rule-match",
			e:          Entity{Kind: KindFused, IssueNumber: 122, PRNumber: 140, Status: "validated", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "",
			wantSubstr: "no-rule-match",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flow, ref, reason := nextDispatch(tc.e, tc.rules, tc.rounds)
			if flow != tc.wantFlow {
				t.Errorf("flow: got %q want %q (reason=%s)", flow, tc.wantFlow, reason)
			}
			if tc.wantFlow != "" && ref != tc.wantRef {
				t.Errorf("ref: got %d want %d (reason=%s)", ref, tc.wantRef, reason)
			}
			if !strings.Contains(reason, tc.wantSubstr) {
				t.Errorf("reason %q doesn't contain %q", reason, tc.wantSubstr)
			}
		})
	}
}

// ================== Tick & concurrency ==================

// newLoopServer arma un Server con fixedSource mutable + fakeRunner. El
// tick se dispara explícito con runTick() — no hace falta la goroutine.
//
// Pre-populate del IssueBody para entities en che:plan: el preflight gate
// (preflight.go) requiere un header `## Plan consolidado` para que validate-
// plan dispatche. La mayoría de los tests del tick no se preocupan por el
// body (testean matcher + concurrency + cap, no parsing de plan), así que
// completamos el body cuando falta. Tests que explícitamente quieran
// validar el comportamiento del gate "body sin plan consolidado" deben
// pre-setear IssueBody="" pasándolo después de armar el slice (o usar
// fixedSource directo sin pasar por newLoopServer).
func newLoopServer(t *testing.T, entities []Entity) (*Server, *fakeRunner) {
	t.Helper()
	for i := range entities {
		if entities[i].Kind == KindIssue && entities[i].Status == "plan" && entities[i].IssueBody == "" {
			entities[i].IssueBody = "## Plan consolidado\n\n**Resumen:** test fixture\n"
		}
	}
	src := &fixedSource{snap: Snapshot{
		NWO:      "demo/che",
		LastOK:   time.Now(),
		Entities: entities,
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/fakerepo"
	return s, fr
}

// TestTick_SingleIssueSideDispatch: 3 entities issue-side, rule1 ON → solo
// 1 dispatch en el tick (concurrency 1+1). Segundo tick con la primera
// todavía corriendo → skip; al liberar + cambiar de estado → dispatch #2.
//
// El cambio de estado es clave: el matcher es stateless — si #1 sigue en
// "plan" sin verdict después de clearRunning, el próximo tick la vuelve a
// dispatchar (es el comportamiento correcto, el subproceso real no
// terminó su efecto aún). Simulamos el efecto: mutamos snap para marcar
// #1 como approve (verdict terminal, sale del loop) y así #2 gana.
func TestTick_ConcurrencyIssueSide(t *testing.T) {
	// IssueBody con header "## Plan consolidado" para que el preflight gate
	// de validate-plan deje pasar — testeamos concurrency, no parsing.
	planBody := "## Plan consolidado\n\n**Resumen:** test\n"
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 1, Status: "plan", IssueBody: planBody},
		{Kind: KindIssue, IssueNumber: 2, Status: "plan", IssueBody: planBody},
		{Kind: KindIssue, IssueNumber: 3, Status: "plan", IssueBody: planBody},
	}
	src := &fixedSource{snap: Snapshot{NWO: "demo/che", LastOK: time.Now(), Entities: ents}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick #1 dispatches: got %d want 1", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls tras tick #1: got %d want 1", fr.count())
	}
	// El primer dispatchado es #1 (primero del slice).
	first := fr.last()
	if first.EntityKey != 1 {
		t.Errorf("tick #1 first dispatch key: got %d want 1", first.EntityKey)
	}

	// Segundo tick: slot todavía ocupado (clearRunning nunca llamado).
	if n := s.runTick(); n != 0 {
		t.Errorf("tick #2 debería ser 0 (slot ocupado); got %d", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner no debería haber sido llamado de nuevo; got %d", fr.count())
	}

	// Simular "el validate terminó y emitió verdict approve sobre #1".
	// El poller real update-ea el snapshot con PlanVerdict="approve".
	s.clearRunning(1)
	src.snap.Entities[0].PlanVerdict = "approve"
	if n := s.runTick(); n != 1 {
		t.Fatalf("tick #3 dispatches: got %d want 1", n)
	}
	if fr.count() != 2 {
		t.Errorf("runner calls tras tick #3: got %d want 2", fr.count())
	}
	if got := fr.last().EntityKey; got != 2 {
		t.Errorf("tick #3 key: got %d want 2 (#1 approved → stop; #2 primero disponible)", got)
	}
}

// TestTick_IssueAndPRSimultaneous: 1 issue-side + 1 PR-side libres → los 2
// dispatchan en el mismo tick.
func TestTick_IssueAndPRSimultaneous(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 1, Status: "plan"},
		{Kind: KindFused, IssueNumber: 2, PRNumber: 22, Status: "executed"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true
	s.loop.rules[RuleValidatePR] = true

	if n := s.runTick(); n != 2 {
		t.Fatalf("tick dispatches: got %d want 2 (issue + PR slots independientes)", n)
	}
	if fr.count() != 2 {
		t.Errorf("runner calls: got %d want 2", fr.count())
	}
	// Verificar que uno fue con IssueNumber=1 (issue-side, target=issue)
	// y el otro con PRNumber=22 (PR-side, target=PR).
	seen := map[int]int{} // key → target
	fr.mu.Lock()
	for _, c := range fr.calls {
		seen[c.EntityKey] = c.TargetRef
	}
	fr.mu.Unlock()
	if seen[1] != 1 {
		t.Errorf("issue-side call: got target=%d want 1", seen[1])
	}
	if seen[2] != 22 {
		t.Errorf("PR-side call: got target=%d want 22", seen[2])
	}
}

// TestTick_CapFive: mismo entity pasa por 5 rondas; el 6to tick no dispatcha.
func TestTick_CapFive(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true

	for i := 1; i <= LoopCap; i++ {
		if n := s.runTick(); n != 1 {
			t.Fatalf("tick #%d dispatches: got %d want 1", i, n)
		}
		// Libera el slot para que el próximo tick pueda dispatchar.
		s.clearRunning(42)
	}
	if fr.count() != LoopCap {
		t.Fatalf("runner calls tras %d ticks: got %d want %d", LoopCap, fr.count(), LoopCap)
	}
	// Tick 6: cap alcanzado.
	if n := s.runTick(); n != 0 {
		t.Errorf("tick #%d (post-cap) dispatches: got %d want 0", LoopCap+1, n)
	}
	if fr.count() != LoopCap {
		t.Errorf("runner calls tras cap: got %d want %d", fr.count(), LoopCap)
	}
}

// TestTick_AllRulesOff: todas las reglas OFF → no dispatcha. Reemplaza
// al viejo TestTick_MasterOff: sin master switch (v0.0.77), "el loop está
// apagado" significa exactamente "ninguna regla activa".
func TestTick_AllRulesOff(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	// Ninguna regla activada.

	if n := s.runTick(); n != 0 {
		t.Errorf("tick sin reglas: got %d want 0", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner no debería ser llamado sin reglas; got %d", fr.count())
	}
}

// TestTick_ManualBlocksLoop: flow manual + loop no se pisan — markRunning
// desde handler POST /action bloquea el slot; tick ve slot ocupado y skippea.
func TestTick_ManualBlocksLoop(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
		{Kind: KindIssue, IssueNumber: 50, Status: "plan"}, // también issue-side
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true

	// Simular flow manual disparado antes del tick.
	if _, ok := s.markRunning(42, "execute"); !ok {
		t.Fatalf("markRunning manual falló")
	}

	// Tick: #42 tiene slot ocupado (manual); #50 no, pero es issue-side
	// también y el slot issue-side está tomado por #42. Total dispatches: 0.
	if n := s.runTick(); n != 0 {
		t.Errorf("tick con manual en vuelo: got %d want 0", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner no debería ser llamado; got %d", fr.count())
	}

	// Liberar el manual → tick ahora dispatcha #42 (sin verdict).
	s.clearRunning(42)
	if n := s.runTick(); n != 1 {
		t.Errorf("tick tras liberar manual: got %d want 1", n)
	}
}

// TestTick_LockedSkipped: entity locked no dispatcha pero tampoco rompe el
// tick — debe seguir evaluando las siguientes.
func TestTick_LockedSkipped(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan", Locked: true},
		{Kind: KindIssue, IssueNumber: 50, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1 (#42 locked se skipea, #50 dispatcha)", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls: got %d want 1", fr.count())
	}
	if got := fr.last().EntityKey; got != 50 {
		t.Errorf("dispatch entity: got %d want 50", got)
	}
}

// TestTick_ManualDispatchCountsForCap: flows manuales también incrementan
// el counter del cap (son rounds efectivas). 5 manuales + loop ON → tick no
// dispatcha (cap alcanzado).
func TestTick_ManualDispatchCountsForCap(t *testing.T) {
	ts, s, fr := newActionServer(t)

	// Disparar 5 manuales sobre #42 (libera entre cada uno).
	for i := 0; i < LoopCap; i++ {
		resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
		if err != nil {
			t.Fatalf("POST #%d: %v", i, err)
		}
		resp.Body.Close()
		s.clearRunning(42)
	}
	if fr.count() != LoopCap {
		t.Fatalf("calls tras 5 manuales: got %d want %d", fr.count(), LoopCap)
	}
	// Ahora el counter debería estar en cap.
	if r := s.loop.roundsFor(42); r != LoopCap {
		t.Errorf("rounds[42]: got %d want %d", r, LoopCap)
	}
	// Encender el loop — tick no debería dispatchar por cap.
	s.loop.rules[RuleValidatePlan] = true
	if n := s.runTick(); n != 0 {
		t.Errorf("tick post-cap: got %d want 0", n)
	}
}

// TestTick_AutoFlagSetForAutoRunning: un dispatch via el tick marca el
// autoRunning; un dispatch manual NO.
func TestTick_AutoFlagOnlyForTick(t *testing.T) {
	ts, s, fr := newActionServer(t)
	_ = fr
	// Manual primero.
	resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if s.isAutoRunning(42) {
		t.Errorf("manual dispatch no debe setear autoRunning[42]")
	}
	s.clearRunning(42)

	// Tick — reglas ON para disparar validate sobre el issue plan #42.
	s.loop.rules[RuleValidatePlan] = true
	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if !s.isAutoRunning(42) {
		t.Errorf("tick dispatch debe setear autoRunning[42]")
	}
	// clearRunning limpia el flag.
	s.clearRunning(42)
	if s.isAutoRunning(42) {
		t.Errorf("clearRunning debe limpiar autoRunning[42]")
	}
}

// ================== Endpoints ==================

// TestLoopEndpoints_GET devuelve HTML del popover.
func TestLoopEndpoints_GET(t *testing.T) {
	srv := newTestServer(t, "che-cli")
	resp, err := http.Get(srv.URL + "/loop")
	if err != nil {
		t.Fatalf("GET /loop: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	// Todas las reglas enlistadas.
	for _, r := range allLoopRules {
		if !strings.Contains(got, string(r)) {
			t.Errorf("popover missing rule %q", r)
		}
	}
	// Header counter "N / M activas" — el denominador es dinámico
	// (TotalRules = len(allLoopRules)).
	wantDenom := fmt.Sprintf("/ %d activas", len(allLoopRules))
	if !strings.Contains(got, wantDenom) {
		t.Errorf("popover missing dynamic denominator %q", wantDenom)
	}
	// Bulk endpoints (reemplazo del master switch).
	if !strings.Contains(got, `hx-post="/loop/bulk/on"`) {
		t.Errorf("popover missing hx-post a /loop/bulk/on")
	}
	if !strings.Contains(got, `hx-post="/loop/bulk/off"`) {
		t.Errorf("popover missing hx-post a /loop/bulk/off")
	}
	// Separador "or" del par excluyente (validate-plan ↔ execute-raw).
	if !strings.Contains(got, "loop-or-sep") {
		t.Errorf("popover missing separador exclusivo loop-or-sep")
	}
	// Sub-headers from-state.
	if !strings.Contains(got, "loop-from-state") {
		t.Errorf("popover missing from-state headers")
	}
}

// TestLoopEndpoints_ExclusiveValidateExecuteRaw verifica que prender una de
// las dos reglas excluyentes (validate-plan ↔ execute-raw) apaga la otra
// automáticamente. Si las dos pudieran estar on, el matcher prefiere
// validate, pero la UI ya muestra "or (no both)" — el backend mantiene la
// invariante.
func TestLoopEndpoints_ExclusiveValidateExecuteRaw(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Prender validate-plan.
	resp, err := http.Post(ts.URL+"/loop/rule/validate-plan", "", nil)
	if err != nil {
		t.Fatalf("POST validate-plan: %v", err)
	}
	resp.Body.Close()
	s.loop.mu.Lock()
	vOn := s.loop.rules[RuleValidatePlan]
	eOn := s.loop.rules[RuleExecuteRaw]
	s.loop.mu.Unlock()
	if !vOn || eOn {
		t.Fatalf("tras prender validate-plan: validate=%v execute-raw=%v want true/false", vOn, eOn)
	}

	// Prender execute-raw → debe apagar validate-plan.
	resp, err = http.Post(ts.URL+"/loop/rule/execute-raw", "", nil)
	if err != nil {
		t.Fatalf("POST execute-raw: %v", err)
	}
	resp.Body.Close()
	s.loop.mu.Lock()
	vOn = s.loop.rules[RuleValidatePlan]
	eOn = s.loop.rules[RuleExecuteRaw]
	s.loop.mu.Unlock()
	if vOn || !eOn {
		t.Fatalf("tras prender execute-raw: validate=%v execute-raw=%v want false/true", vOn, eOn)
	}

	// Volver a prender validate-plan → debe apagar execute-raw.
	resp, err = http.Post(ts.URL+"/loop/rule/validate-plan", "", nil)
	if err != nil {
		t.Fatalf("POST validate-plan #2: %v", err)
	}
	resp.Body.Close()
	s.loop.mu.Lock()
	vOn = s.loop.rules[RuleValidatePlan]
	eOn = s.loop.rules[RuleExecuteRaw]
	s.loop.mu.Unlock()
	if !vOn || eOn {
		t.Errorf("tras re-prender validate-plan: validate=%v execute-raw=%v want true/false", vOn, eOn)
	}
}

// TestLoopState_ToggleRuleExclusiveDoesNotTouchOthers verifica que la
// exclusión sólo aplica al par (validate-plan, execute-raw). Otras reglas
// que también podrían parecer excluyentes (iterate-plan + execute-plan
// sobre validated; ambas matchean validated pero con verdicts distintos)
// NO se apagan entre sí — el verdict las distingue en el matcher.
func TestLoopState_ToggleRuleExclusiveDoesNotTouchOthers(t *testing.T) {
	l := newLoopState()
	l.toggleRule(RuleValidatePlan)
	l.toggleRule(RuleIteratePlan)
	l.toggleRule(RuleExecutePlan)
	l.toggleRule(RuleValidatePR)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.rules[RuleValidatePlan] || !l.rules[RuleIteratePlan] ||
		!l.rules[RuleExecutePlan] || !l.rules[RuleValidatePR] {
		t.Errorf("reglas no excluyentes apagadas indebidamente: %+v", l.rules)
	}
}

// TestLoopEndpoints_BulkOn prende todas las reglas. Para el par excluyente
// validate-plan/execute-raw, validate-plan gana (matchea el matcher).
func TestLoopEndpoints_BulkOn(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/loop/bulk/on", "", nil)
	if err != nil {
		t.Fatalf("POST /loop/bulk/on: %v", err)
	}
	resp.Body.Close()

	s.loop.mu.Lock()
	defer s.loop.mu.Unlock()
	for _, r := range allLoopRules {
		if r == RuleExecuteRaw {
			if s.loop.rules[r] {
				t.Errorf("execute-raw NO debe quedar ON tras bulk/on (validate-plan gana por exclusión)")
			}
			continue
		}
		if !s.loop.rules[r] {
			t.Errorf("regla %q debería estar ON tras bulk/on", r)
		}
	}
}

// TestLoopEndpoints_BulkOff apaga todas las reglas.
func TestLoopEndpoints_BulkOff(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	for _, r := range allLoopRules {
		s.loop.rules[r] = true
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/loop/bulk/off", "", nil)
	if err != nil {
		t.Fatalf("POST /loop/bulk/off: %v", err)
	}
	resp.Body.Close()

	s.loop.mu.Lock()
	defer s.loop.mu.Unlock()
	for r, on := range s.loop.rules {
		if on {
			t.Errorf("regla %q debería estar OFF tras bulk/off", r)
		}
	}
}

// TestLoopEndpoints_BulkInvalidMode rechaza modes fuera de on/off.
func TestLoopEndpoints_BulkInvalidMode(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/loop/bulk/half", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

// TestLoopEndpoints_Rule flipea una regla válida.
func TestLoopEndpoints_Rule(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/loop/rule/validate-plan", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	s.loop.mu.Lock()
	on := s.loop.rules[RuleValidatePlan]
	s.loop.mu.Unlock()
	if !on {
		t.Errorf("regla validate-plan debería estar ON tras POST")
	}
}

// TestLoopEndpoints_InvalidRule rechaza name fuera de la allowlist.
func TestLoopEndpoints_InvalidRule(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/loop/rule/exec-everything", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	// Ningún estado se tocó.
	s.loop.mu.Lock()
	nrules := len(s.loop.rules)
	s.loop.mu.Unlock()
	if nrules != 0 {
		t.Errorf("rules map debería quedar vacío tras POST inválido; got %d", nrules)
	}
}

// TestTopbarPillLabel_RenderInitial: el dashboard renderiza el pill con el
// label correcto según el estado del loop. Arranca OFF.
func TestTopbarPillLabel_RenderInitial(t *testing.T) {
	srv := newTestServer(t, "che-cli")
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	if !strings.Contains(got, "auto-loop OFF") {
		t.Errorf("topbar debería decir 'auto-loop OFF' al arrancar; got head: %s", got[:min(500, len(got))])
	}
	// Wrapper del popover presente.
	if !strings.Contains(got, `id="loop-popover"`) {
		t.Errorf("topbar missing id=\"loop-popover\"")
	}
}

// TestTopbarPillLabel_AfterRulesOn: con N reglas ON el pill dice
// "auto-loop ON (N/M)". Sin master switch (v0.0.77), basta con prender
// reglas para que el loop quede ON.
func TestTopbarPillLabel_AfterRulesOn(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	s.loop.rules[RuleValidatePlan] = true
	s.loop.rules[RuleValidatePR] = true
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	want := fmt.Sprintf("auto-loop ON (2/%d)", len(allLoopRules))
	if !strings.Contains(got, want) {
		t.Errorf("topbar debería decir %q; got head: %s", want, got[:min(500, len(got))])
	}
}

// TestLoopEndpoints_Concurrency flipea reglas concurrently — verifica que
// el race detector no encuentre nada. Reemplaza al viejo test del master
// switch (borrado en v0.0.77).
func TestLoopEndpoints_Concurrency(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	var wg sync.WaitGroup
	const N = 20
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/loop/rule/validate-plan", "", nil)
			if err != nil {
				t.Errorf("POST: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

// TestRunTick_GateBlocksValidateWithoutBody: un issue en che:plan SIN body
// con "## Plan consolidado" no recibe dispatch del auto-loop, y el motivo
// se loguea a stderr. Es el caso real del PR de gates UI: el auto-loop
// pegaba 10 corridas sobre #146 dale-que-sale antes de que el cap cortara,
// gastando claude-API. Con el gate, el tick filtra antes de spawnear.
func TestRunTick_GateBlocksValidateWithoutBody(t *testing.T) {
	// Construimos el slice manualmente (sin pasar por newLoopServer) para
	// evitar el auto-fill de IssueBody — el test EXIGE body vacío.
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 146, Status: "plan", IssueTitle: "sin body", IssueBody: ""},
	}
	src := &fixedSource{snap: Snapshot{NWO: "demo/che", LastOK: time.Now(), Entities: ents}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 0 {
		t.Fatalf("gate skip: dispatches got %d want 0", n)
	}
	if fr.count() != 0 {
		t.Errorf("gate skip: runner llamado %d veces, no debería ser llamado nunca", fr.count())
	}
	// El contador de rounds NO se incrementa cuando el gate corta — sería
	// gastar el cap por algo que ni siquiera intentamos. Si el body cambia
	// (humano arregla el issue), las 10 rounds deben quedar disponibles.
	if got := s.loop.roundsFor(146); got != 0 {
		t.Errorf("gate skip: rounds got %d want 0 (gate no debe consumir cap)", got)
	}
	// 5 ticks más → sigue sin dispatchar y sigue sin acumular rounds.
	for i := 0; i < 5; i++ {
		s.runTick()
	}
	if fr.count() != 0 {
		t.Errorf("gate skip persistente: runner llamado %d veces tras 6 ticks", fr.count())
	}
	if got := s.loop.roundsFor(146); got != 0 {
		t.Errorf("gate skip persistente: rounds got %d want 0", got)
	}
}

// TestAction_GateRejects409: POST /action/validate sobre issue#146 (body
// vacío) → 409 con el Reason humano. Doble barrera del UI: el botón viene
// disabled, pero un cliente manual (curl) recibe el mismo "no" con motivo.
func TestAction_GateRejects409(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 146, Status: "plan", IssueTitle: "sin body", IssueBody: ""},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/validate/146", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (gate fail)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Plan consolidado") {
		t.Errorf("body del 409 debe contener motivo del gate; got: %q", string(body))
	}
	if fr.count() != 0 {
		t.Errorf("gate 409: runner llamado %d veces, no debería spawnear", fr.count())
	}
}

// TestRunTick_AutoChipInDrawer: un dispatch via el tick aparece como chip
// "auto" en el HTML del drawer; un manual no.
func TestRunTick_AutoChipInDrawer(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan", IssueTitle: "t"},
	}
	s, _ := newLoopServer(t, ents)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Disparar via tick (el mock Source sigue devolviendo la entity sin
	// RunningFlow, pero el overlay local la marca).
	s.loop.rules[RuleValidatePlan] = true
	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}

	// GET /drawer/42 debería mostrar el chip auto.
	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	// Buscar el chip "auto" con title distintivo.
	if !strings.Contains(got, "disparado por el auto-loop engine") {
		t.Errorf("drawer debería contener chip auto; head: %s", got[:min(800, len(got))])
	}
}

// Comprobación redundante del orden de allLoopRules (fija el contrato).
// El orden refleja la lectura didáctica del lifecycle: explore arranca el
// ciclo, después issue-side (validate/iterate/execute en sus dos
// variantes), y al final PR-side (validate/iterate). Cambiar este orden
// reordena el popover — chequear UX antes de tocar.
func TestAllLoopRules_OrderStable(t *testing.T) {
	want := []LoopRule{
		RuleExploreIdea,
		RuleValidatePlan,
		RuleIteratePlan,
		RuleExecutePlan,
		RuleExecuteRaw,
		RuleValidatePR,
		RuleIteratePR,
	}
	if len(allLoopRules) != len(want) {
		t.Fatalf("allLoopRules len: got %d want %d", len(allLoopRules), len(want))
	}
	for i, r := range allLoopRules {
		if r != want[i] {
			t.Errorf("allLoopRules[%d]: got %q want %q", i, r, want[i])
		}
	}
}

// Mini-assert del pillLabel para fijar el contrato del texto — tests del
// frontend (dash.js) + del server.go lo dan por asumido. El denominador
// es len(allLoopRules) — si se agregan reglas, este test debe cambiar
// junto con la constante.
func TestPillLabel(t *testing.T) {
	const total = 7
	cases := []struct {
		data loopPopoverData
		want string
	}{
		{loopPopoverData{ActiveRules: 0, TotalRules: total}, "auto-loop OFF"},
		{loopPopoverData{ActiveRules: 3, TotalRules: total}, "auto-loop ON (3/7)"},
		{loopPopoverData{ActiveRules: 7, TotalRules: total}, "auto-loop ON (7/7)"},
	}
	for _, tc := range cases {
		got := pillLabel(tc.data)
		if got != tc.want {
			t.Errorf("pillLabel(%+v): got %q want %q", tc.data, got, tc.want)
		}
	}
}

// TestTick_ExecutePlanDispatches: issue-only en validated+approve + rule ON
// → el tick dispatcha execute sobre IssueNumber. Cubre el happy path del
// nuevo gap que la regla cierra: post-validate-approve, el loop sigue
// solo hasta execute sin esperar click humano.
func TestTick_ExecutePlanDispatches(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 122, Status: "validated", PlanVerdict: "approve", IssueTitle: "approved"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleExecutePlan] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls: got %d want 1", fr.count())
	}
	got := fr.last()
	if got.Flow != "execute" {
		t.Errorf("flow: got %q want 'execute'", got.Flow)
	}
	if got.TargetRef != 122 || got.EntityKey != 122 {
		t.Errorf("ref mapping: got target=%d key=%d want target=122 key=122", got.TargetRef, got.EntityKey)
	}
}

// TestTick_ExploreIdeaDispatches: issue en che:idea + rule ON → el tick
// dispatcha explore sobre IssueNumber. Cubre el happy path de la regla
// que arranca el ciclo desde el origen (idea → plan).
func TestTick_ExploreIdeaDispatches(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 7, Status: "idea", IssueTitle: "fresh idea"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleExploreIdea] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls: got %d want 1", fr.count())
	}
	got := fr.last()
	if got.Flow != "explore" {
		t.Errorf("flow: got %q want 'explore'", got.Flow)
	}
	if got.TargetRef != 7 || got.EntityKey != 7 {
		t.Errorf("ref mapping: got target=%d key=%d want target=7 key=7", got.TargetRef, got.EntityKey)
	}
}

// TestTick_ExploreIdeaSkipsFused: un fused con Status vacío/idea no es
// elegible para explore (issue-first). gateExplore lo bloquea por Kind.
func TestTick_ExploreIdeaSkipsFused(t *testing.T) {
	ents := []Entity{
		{Kind: KindFused, IssueNumber: 7, PRNumber: 11, Status: "idea"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleExploreIdea] = true

	if n := s.runTick(); n != 0 {
		t.Errorf("tick sobre fused: got %d want 0 (explore es issue-first)", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner NO debería ser llamado; got %d", fr.count())
	}
}

// TestTick_ExecuteRawDispatches: issue en che:plan sin verdict + rule ON
// → execute directo sobre IssueNumber, salteando validate (fast-lane).
func TestTick_ExecuteRawDispatches(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 99, Status: "plan", IssueTitle: "trust the plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleExecuteRaw] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls: got %d want 1", fr.count())
	}
	got := fr.last()
	if got.Flow != "execute" {
		t.Errorf("flow: got %q want 'execute'", got.Flow)
	}
	if got.TargetRef != 99 {
		t.Errorf("target: got %d want 99", got.TargetRef)
	}
}

// TestTick_ValidatePlanWinsOverExecuteRaw: con ambas reglas ON sobre el
// mismo plan-sin-verdict, validate gana. Es el contrato del orden dentro
// del case "plan" en nextDispatch — preferimos validar antes de ejecutar
// cuando el humano dejó las dos prendidas.
func TestTick_ValidatePlanWinsOverExecuteRaw(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 99, Status: "plan", IssueTitle: "plan a refinar"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true
	s.loop.rules[RuleExecuteRaw] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if got := fr.last().Flow; got != "validate" {
		t.Errorf("flow: got %q want 'validate' (validate gana cuando ambas ON)", got)
	}
}

// TestTick_ExecutePlanSkipsFused: un fused en validated+approve no es
// elegible (execute rechaza PRs; el loop tampoco debe dispatchar).
func TestTick_ExecutePlanSkipsFused(t *testing.T) {
	ents := []Entity{
		{Kind: KindFused, IssueNumber: 122, PRNumber: 140, Status: "validated", PlanVerdict: "approve"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleExecutePlan] = true

	if n := s.runTick(); n != 0 {
		t.Errorf("tick sobre fused: got %d want 0 (execute-plan no aplica)", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner NO debería ser llamado; got %d", fr.count())
	}
}

// TestRuleLabelAndTransition fija el contrato del split label/transition
// para todas las reglas. Label = acción ("validate plan"), Transition =
// efecto ("plan → validated") — el popover los renderiza juntos.
func TestRuleLabelAndTransition(t *testing.T) {
	cases := []struct {
		rule           LoopRule
		wantLabelSub   string // substring que debe aparecer en Label
		wantTransition string // exacto
	}{
		{RuleExploreIdea, "explore idea", "idea → plan"},
		{RuleValidatePlan, "validate plan", "plan → validated"},
		{RuleIteratePlan, "iterate plan", "validated:changes-req → plan"},
		{RuleExecutePlan, "execute plan aprobado", "validated:approve → executed"},
		{RuleExecuteRaw, "execute plan directo", "plan → executed"},
		{RuleValidatePR, "validate PR", "executed → validated"},
		{RuleIteratePR, "iterate PR", "validated:changes-req → executed"},
	}
	for _, tc := range cases {
		t.Run(string(tc.rule), func(t *testing.T) {
			gotLabel := ruleLabel(tc.rule)
			if !strings.Contains(gotLabel, tc.wantLabelSub) {
				t.Errorf("ruleLabel(%q): got %q; esperaba que contenga %q", tc.rule, gotLabel, tc.wantLabelSub)
			}
			gotTrans := ruleTransition(tc.rule)
			if gotTrans != tc.wantTransition {
				t.Errorf("ruleTransition(%q): got %q want %q", tc.rule, gotTrans, tc.wantTransition)
			}
		})
	}
}

// TestTick_PrefersOldestFirst: con varias entidades elegibles del mismo side
// y un único slot, el tick debe dispatchar la más vieja primero. Sin este
// orden, el default de `gh list` (most-recent-first) hacía que un item con
// días esperando quedara detrás del recién creado.
func TestTick_PrefersOldestFirst(t *testing.T) {
	now := time.Now()
	ents := []Entity{
		// Orden en el slice: más nuevo primero (simula gh list default).
		{Kind: KindIssue, IssueNumber: 3, Status: "plan", CreatedAt: now.Add(-1 * time.Hour)},
		{Kind: KindIssue, IssueNumber: 2, Status: "plan", CreatedAt: now.Add(-24 * time.Hour)},
		{Kind: KindIssue, IssueNumber: 1, Status: "plan", CreatedAt: now.Add(-72 * time.Hour)},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick dispatches: got %d want 1", n)
	}
	if got := fr.last().EntityKey; got != 1 {
		t.Errorf("oldest-first: primer dispatch debe ser #1 (72h); got #%d", got)
	}

	// Liberar y resolver #1 con approve (sale del loop). Próximo tick debe
	// tomar #2 (24h), no #3.
	s.clearRunning(1)
	s.source.(*fixedSource).snap.Entities[2].PlanVerdict = "approve"
	if n := s.runTick(); n != 1 {
		t.Fatalf("tick #2 dispatches: got %d want 1", n)
	}
	if got := fr.last().EntityKey; got != 2 {
		t.Errorf("oldest-first tras #1 done: segundo dispatch debe ser #2 (24h); got #%d", got)
	}
}

// TestTick_ZeroCreatedAtKeepsOriginalOrder: entidades con CreatedAt zero
// (fixtures viejos, mocks sin poblar) empatan y caen en orden del slice por
// ser sort stable. Fija el contrato: no cambiamos el orden observable de
// tests existentes que usan Entity sin CreatedAt. IssueBody con header
// consolidado para que el preflight gate de validate-plan deje pasar — sin
// eso, el tick skipea ambas entities con razón "body sin plan consolidado"
// (caso real que motivó el feature, ver project_dash_preflight_gates).
func TestTick_ZeroCreatedAtKeepsOriginalOrder(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 10, Status: "plan", IssueBody: "## Plan consolidado\nx"},
		{Kind: KindIssue, IssueNumber: 20, Status: "plan", IssueBody: "## Plan consolidado\nx"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 1 {
		t.Fatalf("tick: got %d want 1", n)
	}
	if got := fr.last().EntityKey; got != 10 {
		t.Errorf("stable sort con empates zero: got #%d want #10 (orden original)", got)
	}
}

// TestLaterOf fija la semántica del helper max-date con zero como "sin dato".
func TestLaterOf(t *testing.T) {
	now := time.Now()
	older := now.Add(-24 * time.Hour)
	var zero time.Time

	if got := laterOf(now, older); !got.Equal(now) {
		t.Errorf("max(now, older): got %v want %v", got, now)
	}
	if got := laterOf(older, now); !got.Equal(now) {
		t.Errorf("max(older, now): got %v want %v", got, now)
	}
	if got := laterOf(zero, now); !got.Equal(now) {
		t.Errorf("max(zero, now): got %v want %v (zero debe tratarse como 'sin dato')", got, now)
	}
	if got := laterOf(now, zero); !got.Equal(now) {
		t.Errorf("max(now, zero): got %v want %v", got, now)
	}
	if got := laterOf(zero, zero); !got.IsZero() {
		t.Errorf("max(zero, zero): got %v want zero", got)
	}
}

// TestEntitySide fija la clasificación por Kind.
func TestEntitySide(t *testing.T) {
	if got := entitySide(Entity{Kind: KindIssue}); got != "issue" {
		t.Errorf("KindIssue: got %q want 'issue'", got)
	}
	if got := entitySide(Entity{Kind: KindFused}); got != "pr" {
		t.Errorf("KindFused: got %q want 'pr'", got)
	}
}

// TestRuleSide fija qué reglas viven en cada bloque del popover.
// El criterio es "qué entidad recibe el dispatch del subcomando":
// validate-pr/iterate-pr operan sobre PRNumber; el resto (incluyendo
// execute, que arranca desde el issue aunque cree un PR) operan sobre
// IssueNumber. Si esto cambia, la sección visual del popover y el slot
// de concurrencia deben moverse en sincronía.
func TestRuleSide(t *testing.T) {
	cases := []struct {
		rule LoopRule
		want string
	}{
		{RuleExploreIdea, "issue"},
		{RuleValidatePlan, "issue"},
		{RuleIteratePlan, "issue"},
		{RuleExecutePlan, "issue"},
		{RuleExecuteRaw, "issue"},
		{RuleValidatePR, "pr"},
		{RuleIteratePR, "pr"},
	}
	for _, tc := range cases {
		if got := ruleSide(tc.rule); got != tc.want {
			t.Errorf("ruleSide(%q): got %q want %q", tc.rule, got, tc.want)
		}
	}
}

// TestBuildLoopData_GroupsRules: el popover agrupa las reglas por
// (side, fromState). El template renderea cada grupo como un mini-stepper
// del lifecycle. Fija el contrato: si alguien agrega una regla nueva, debe
// declarar ruleSide + ruleFromState para que aparezca en algún grupo.
func TestBuildLoopData_GroupsRules(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	data := s.buildLoopData()

	// TotalRules = len(allLoopRules) — el header del popover lo usa.
	if data.TotalRules != len(allLoopRules) {
		t.Errorf("TotalRules: got %d want %d", data.TotalRules, len(allLoopRules))
	}

	// La suma de reglas en todos los grupos debe igualar el total.
	count := 0
	for _, g := range data.IssueGroups {
		count += len(g.Rules)
	}
	for _, g := range data.PRGroups {
		count += len(g.Rules)
	}
	if count != len(allLoopRules) {
		t.Errorf("suma de reglas en grupos: got %d want %d (alguna regla quedó fuera de un grupo — agregar al ruleSide o ruleFromState)", count, len(allLoopRules))
	}

	// Issue side: 3 grupos en orden idea, plan, validated.
	wantIssueOrder := []string{"idea", "plan", "validated"}
	if len(data.IssueGroups) != len(wantIssueOrder) {
		t.Fatalf("IssueGroups len: got %d want %d", len(data.IssueGroups), len(wantIssueOrder))
	}
	for i, st := range wantIssueOrder {
		if data.IssueGroups[i].State != st {
			t.Errorf("IssueGroups[%d].State: got %q want %q", i, data.IssueGroups[i].State, st)
		}
	}

	// PR side: 2 grupos en orden executed, validated.
	wantPROrder := []string{"executed", "validated"}
	if len(data.PRGroups) != len(wantPROrder) {
		t.Fatalf("PRGroups len: got %d want %d", len(data.PRGroups), len(wantPROrder))
	}
	for i, st := range wantPROrder {
		if data.PRGroups[i].State != st {
			t.Errorf("PRGroups[%d].State: got %q want %q", i, data.PRGroups[i].State, st)
		}
	}

	// El grupo (issue, plan) debe estar marcado Exclusive (validate-plan ↔
	// execute-raw). Ningún otro grupo es exclusive hoy.
	for _, g := range data.IssueGroups {
		wantExcl := g.State == "plan"
		if g.Exclusive != wantExcl {
			t.Errorf("IssueGroups[state=%s].Exclusive: got %v want %v", g.State, g.Exclusive, wantExcl)
		}
	}
	for _, g := range data.PRGroups {
		if g.Exclusive {
			t.Errorf("PRGroups[state=%s].Exclusive: got true; no PR group debería ser exclusive hoy", g.State)
		}
	}
}

// TestRuleFromState fija el mapping regla → estado origen del lifecycle.
// El popover agrupa por este valor, así que cambiarlo reorganiza secciones.
func TestRuleFromState(t *testing.T) {
	cases := []struct {
		rule LoopRule
		want string
	}{
		{RuleExploreIdea, "idea"},
		{RuleValidatePlan, "plan"},
		{RuleExecuteRaw, "plan"},
		{RuleIteratePlan, "validated"},
		{RuleExecutePlan, "validated"},
		{RuleValidatePR, "executed"},
		{RuleIteratePR, "validated"},
	}
	for _, tc := range cases {
		if got := ruleFromState(tc.rule); got != tc.want {
			t.Errorf("ruleFromState(%q): got %q want %q", tc.rule, got, tc.want)
		}
	}
}

// Stub — silencia el unused fmt si el archivo queda sin usarlo.
var _ = fmt.Sprintf
