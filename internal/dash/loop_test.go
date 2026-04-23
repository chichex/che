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
			// Fused en validated con changes-requested: el verdict aplica al
			// plan, pero execute no corre sobre PRs. El flow de iterate PR
			// (rule4) dispara solo en Status=executed + PRVerdict, así que
			// este caso no tiene regla aplicable — cae en validated-not-
			// issue-only.
			name:       "rule2: fused en validated + changes-requested → skip (no issue-only)",
			e:          Entity{Kind: KindFused, IssueNumber: 42, PRNumber: 77, Status: "validated", PlanVerdict: "changes-requested"},
			rules:      map[LoopRule]bool{RuleIteratePlan: true},
			wantFlow:   "",
			wantSubstr: "validated-not-issue-only",
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
			name:       "status no loopable (idea) → no match",
			e:          Entity{Kind: KindIssue, IssueNumber: 42, Status: "idea"},
			rules:      map[LoopRule]bool{RuleValidatePlan: true},
			wantFlow:   "",
			wantSubstr: "status-not-loopable",
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
			name:       "rule5: validated + approve pero fused → skip (execute no corre sobre fused)",
			e:          Entity{Kind: KindFused, IssueNumber: 122, PRNumber: 140, Status: "validated", PlanVerdict: "approve"},
			rules:      map[LoopRule]bool{RuleExecutePlan: true},
			wantFlow:   "",
			wantSubstr: "validated-not-issue-only",
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
func newLoopServer(t *testing.T, entities []Entity) (*Server, *fakeRunner) {
	t.Helper()
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
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 1, Status: "plan"},
		{Kind: KindIssue, IssueNumber: 2, Status: "plan"},
		{Kind: KindIssue, IssueNumber: 3, Status: "plan"},
	}
	src := &fixedSource{snap: Snapshot{NWO: "demo/che", LastOK: time.Now(), Entities: ents}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	s.loop.enabled = true
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
	s.loop.enabled = true
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
	s.loop.enabled = true
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

// TestTick_MasterOff: reglas ON pero enabled=false → no dispatcha.
func TestTick_MasterOff(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.enabled = false
	s.loop.rules[RuleValidatePlan] = true

	if n := s.runTick(); n != 0 {
		t.Errorf("tick con master OFF: got %d want 0", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner no debería ser llamado con master OFF; got %d", fr.count())
	}
}

// TestTick_AllRulesOff: master ON pero todas las reglas OFF → no dispatcha.
func TestTick_AllRulesOff(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.enabled = true
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
	s.loop.enabled = true
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
	s.loop.enabled = true
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
	s.loop.enabled = true
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
	s.loop.enabled = true
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
	// 4 reglas enlistadas.
	for _, r := range allLoopRules {
		if !strings.Contains(got, string(r)) {
			t.Errorf("popover missing rule %q", r)
		}
	}
	// Master master switch hx-post.
	if !strings.Contains(got, `hx-post="/loop/toggle"`) {
		t.Errorf("popover missing hx-post to /loop/toggle")
	}
}

// TestLoopEndpoints_Toggle flipea el master y verifica OOB del pill.
func TestLoopEndpoints_Toggle(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	if s.loop.isEnabled() {
		t.Fatalf("master debería arrancar OFF")
	}

	resp, err := http.Post(ts.URL+"/loop/toggle", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if !s.loop.isEnabled() {
		t.Errorf("master debería estar ON tras toggle")
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	// Response incluye OOB del pill + popover.
	if !strings.Contains(got, `hx-swap-oob="outerHTML"`) {
		t.Errorf("POST response missing hx-swap-oob para pill")
	}
	if !strings.Contains(got, `id="auto-loop-toggle"`) {
		t.Errorf("POST response missing id=auto-loop-toggle (pill OOB)")
	}
	// Label refleja ON.
	if !strings.Contains(got, "auto-loop ON") {
		t.Errorf("POST response label debe decir 'auto-loop ON'; got: %s", got[:min(200, len(got))])
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

// TestTopbarPillLabel_AfterToggle: tras toggle, el pill dice "ON (N/4)".
func TestTopbarPillLabel_AfterToggle(t *testing.T) {
	s := NewServer(MockSource{}, "che-cli", 15)
	s.loop.enabled = true
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
	if !strings.Contains(got, "auto-loop ON (2/5)") {
		t.Errorf("topbar debería decir 'auto-loop ON (2/5)'; got head: %s", got[:min(500, len(got))])
	}
}

// TestLoopEndpoints_Concurrency flipea el master concurrently — verifica
// que el race detector no explote. El estado final es no determinístico
// (N toggles consecutivos), pero el test garantiza safety.
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
			resp, err := http.Post(ts.URL+"/loop/toggle", "", nil)
			if err != nil {
				t.Errorf("POST: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	// No hay assert concreto sobre el estado final — el objetivo es que el
	// race detector no encuentre nada raro.
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
	s.loop.enabled = true
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
// RuleExecutePlan va entre IteratePlan y ValidatePR: mantiene el bloque
// "todas las reglas issue-side primero, todas las PR-side después" para
// que el matcher no tenga sorpresas al leer el orden.
func TestAllLoopRules_OrderStable(t *testing.T) {
	want := []LoopRule{RuleValidatePlan, RuleIteratePlan, RuleExecutePlan, RuleValidatePR, RuleIteratePR}
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
	cases := []struct {
		data loopPopoverData
		want string
	}{
		{loopPopoverData{Enabled: false}, "auto-loop OFF"},
		{loopPopoverData{Enabled: true, ActiveRules: 0}, "auto-loop ON (0/5)"},
		{loopPopoverData{Enabled: true, ActiveRules: 3}, "auto-loop ON (3/5)"},
		{loopPopoverData{Enabled: true, ActiveRules: 5}, "auto-loop ON (5/5)"},
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
	s.loop.enabled = true
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

// TestTick_ExecutePlanSkipsFused: un fused en validated+approve no es
// elegible (execute rechaza PRs; el loop tampoco debe dispatchar).
func TestTick_ExecutePlanSkipsFused(t *testing.T) {
	ents := []Entity{
		{Kind: KindFused, IssueNumber: 122, PRNumber: 140, Status: "validated", PlanVerdict: "approve"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.enabled = true
	s.loop.rules[RuleExecutePlan] = true

	if n := s.runTick(); n != 0 {
		t.Errorf("tick sobre fused: got %d want 0 (execute-plan no aplica)", n)
	}
	if fr.count() != 0 {
		t.Errorf("runner NO debería ser llamado; got %d", fr.count())
	}
}

// TestRuleLabel_ExecutePlan fija el texto del popover para la regla nueva.
func TestRuleLabel_ExecutePlan(t *testing.T) {
	got := ruleLabel(RuleExecutePlan)
	if !strings.Contains(got, "execute plan") {
		t.Errorf("ruleLabel(RuleExecutePlan): got %q; esperaba que contenga 'execute plan'", got)
	}
	if !strings.Contains(got, "approve") {
		t.Errorf("ruleLabel(RuleExecutePlan): debería mencionar el trigger 'approve'; got %q", got)
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
	s.loop.enabled = true
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
// tests existentes que usan Entity sin CreatedAt.
func TestTick_ZeroCreatedAtKeepsOriginalOrder(t *testing.T) {
	ents := []Entity{
		{Kind: KindIssue, IssueNumber: 10, Status: "plan"},
		{Kind: KindIssue, IssueNumber: 20, Status: "plan"},
	}
	s, fr := newLoopServer(t, ents)
	s.loop.enabled = true
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

// Stub — silencia el unused fmt si el archivo queda sin usarlo.
var _ = fmt.Sprintf
