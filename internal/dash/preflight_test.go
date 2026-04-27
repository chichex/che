package dash

import (
	"strings"
	"testing"
)

// TestComputeGates es un table-driven sweep por las 5 funciones de gate.
// Cubre los 5 blockers que el review de abril 2026 marcó (gate↔flow drift)
// y los edge cases conocidos:
//   - explore: NO acepta che:plan (explore.go:665-677 lo rechaza).
//   - execute: respeta verdicts bloqueantes en che:plan; acepta che:validated
//     sin verdict (execute.go:743 lo permite).
//   - validate: KindFused + Status=adopt pasa (validate.go:433 hasExecutedState
//     es opcional).
//   - close: KindIssue sin PR bloquea; KindFused/KindPR pasan.
//   - body: HasConsolidatedHeader es case-sensitive y respeta code fences.
func TestComputeGates(t *testing.T) {
	cases := []struct {
		name      string
		entity    Entity
		wantAvail map[string]bool
		// wantReasonContains por flow: substring que tiene que aparecer
		// en Gates[flow].Reason. Se chequea solo si el caso lo lista.
		wantReasonContains map[string]string
	}{
		// ====================== explore ======================
		{
			name:   "issue idea — explore y execute habilitados, validate/iterate/close bloqueados",
			entity: Entity{Kind: KindIssue, Status: "idea", IssueNumber: 10},
			wantAvail: map[string]bool{
				flowExplore:  true,
				flowValidate: false,
				flowIterate:  false,
				flowExecute:  true,
				flowClose:    false, // KindIssue sin PR
			},
			wantReasonContains: map[string]string{
				flowValidate: "che:plan",
				flowIterate:  "che:validated",
				flowClose:    "sin PR",
			},
		},
		{
			name:   "issue plan — explore BLOQUEADO (re-explore no soportado por el flow real)",
			entity: Entity{Kind: KindIssue, Status: "plan", IssueNumber: 11, IssueBody: "## Plan consolidado\nx"},
			wantAvail: map[string]bool{
				flowExplore: false, // FIX blocker #1: che:plan ya no es aceptable
			},
			wantReasonContains: map[string]string{
				flowExplore: "re-explore",
			},
		},
		{
			name:   "issue executing — explore bloqueado (más allá del pipeline)",
			entity: Entity{Kind: KindIssue, Status: "executing"},
			wantAvail: map[string]bool{
				flowExplore: false,
			},
			wantReasonContains: map[string]string{
				flowExplore: "che:executing",
			},
		},
		// ====================== validate ======================
		{
			name: "issue plan + body consolidado — validate habilitado",
			entity: Entity{
				Kind: KindIssue, Status: "plan", IssueNumber: 11,
				IssueBody: "## Plan consolidado\n\n**Resumen:** algo\n",
			},
			wantAvail: map[string]bool{flowValidate: true},
		},
		{
			name: "issue plan + body SIN header consolidado — validate BLOQUEADO con razón edit-manual",
			entity: Entity{
				Kind: KindIssue, Status: "plan", IssueNumber: 146,
				IssueBody: "# Plan: hacer cosas\n\nPasos: ...\n",
			},
			wantAvail: map[string]bool{flowValidate: false},
			wantReasonContains: map[string]string{
				// FIX wording abril 2026: ya no sugiere `che explore`
				// porque explore tampoco acepta che:plan; sugiere edit
				// manual o reset de label.
				flowValidate: "editá el body",
			},
		},
		{
			name: "issue plan + header dentro de code fence — NO cuenta (HasConsolidatedHeader skipea fences)",
			entity: Entity{
				Kind: KindIssue, Status: "plan", IssueNumber: 200,
				IssueBody: "# título\n\n```\n## Plan consolidado\n```\n",
			},
			wantAvail: map[string]bool{flowValidate: false},
			wantReasonContains: map[string]string{
				flowValidate: "Plan consolidado",
			},
		},
		{
			name: "issue plan + header con casing distinto — NO cuenta (HasConsolidatedHeader case-sensitive)",
			entity: Entity{
				Kind: KindIssue, Status: "plan", IssueNumber: 201,
				IssueBody: "## Plan Consolidado (mayúscula)\n\n",
			},
			wantAvail: map[string]bool{flowValidate: false},
			wantReasonContains: map[string]string{
				flowValidate: "case-sensitive",
			},
		},
		{
			name: "issue validated + body consolidado — re-validate habilitado",
			entity: Entity{
				Kind: KindIssue, Status: "validated", IssueNumber: 12,
				IssueBody: "## Plan consolidado\n", PlanVerdict: "approve",
			},
			wantAvail: map[string]bool{flowValidate: true},
		},
		{
			name:      "fused executed — validate habilitado",
			entity:    Entity{Kind: KindFused, Status: "executed", PRNumber: 50, IssueNumber: 49},
			wantAvail: map[string]bool{flowValidate: true},
		},
		{
			name:      "fused validated — re-validate habilitado",
			entity:    Entity{Kind: KindFused, Status: "validated", PRNumber: 51, IssueNumber: 50, PRVerdict: "approve"},
			wantAvail: map[string]bool{flowValidate: true},
		},
		{
			name:   "fused adopt — solo validate (set fijo de adopt)",
			entity: Entity{Kind: KindFused, Status: "adopt", PRNumber: 60, IssueNumber: 59},
			wantAvail: map[string]bool{
				flowValidate: true,
				flowExplore:  false,
				flowExecute:  false,
				flowIterate:  false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				flowClose:   "no aplica desde adopt",
				flowIterate: "no aplica desde adopt",
			},
		},
		// ====================== iterate ======================
		{
			name:      "issue validated + changes-requested — iterate habilitado",
			entity:    Entity{Kind: KindIssue, Status: "validated", IssueNumber: 13, PlanVerdict: "changes-requested"},
			wantAvail: map[string]bool{flowIterate: true, flowExecute: false},
			wantReasonContains: map[string]string{
				flowExecute: "iterate", // execute sugiere correr iterate
			},
		},
		{
			name:      "issue validated + needs-human — iterate y execute bloqueados",
			entity:    Entity{Kind: KindIssue, Status: "validated", PlanVerdict: "needs-human"},
			wantAvail: map[string]bool{flowIterate: false, flowExecute: false},
			wantReasonContains: map[string]string{
				flowIterate: "needs-human",
				flowExecute: "needs-human",
			},
		},
		{
			name:      "issue plan + changes-requested — execute BLOQUEADO (FIX blocker #2)",
			entity:    Entity{Kind: KindIssue, Status: "plan", IssueNumber: 14, PlanVerdict: "changes-requested"},
			wantAvail: map[string]bool{flowExecute: false},
			wantReasonContains: map[string]string{
				flowExecute: "iterate",
			},
		},
		{
			name:      "issue plan + needs-human — execute BLOQUEADO",
			entity:    Entity{Kind: KindIssue, Status: "plan", PlanVerdict: "needs-human"},
			wantAvail: map[string]bool{flowExecute: false},
			wantReasonContains: map[string]string{
				flowExecute: "needs-human",
			},
		},
		{
			name:      "fused executed sin verdict — validate ok, iterate bloqueado",
			entity:    Entity{Kind: KindFused, Status: "executed", PRNumber: 70, IssueNumber: 69},
			wantAvail: map[string]bool{flowValidate: true, flowIterate: false},
			wantReasonContains: map[string]string{
				flowIterate: "verdict",
			},
		},
		{
			name:      "fused executed + changes-requested — iterate habilitado (pre-validate-PR)",
			entity:    Entity{Kind: KindFused, Status: "executed", PRNumber: 71, IssueNumber: 70, PRVerdict: "changes-requested"},
			wantAvail: map[string]bool{flowIterate: true},
		},
		{
			name:      "fused validated + approve — close habilitado, iterate bloqueado",
			entity:    Entity{Kind: KindFused, Status: "validated", PRVerdict: "approve", PRNumber: 52, IssueNumber: 51},
			wantAvail: map[string]bool{flowClose: true, flowIterate: false},
			wantReasonContains: map[string]string{
				flowIterate: "approve",
			},
		},
		// ====================== execute ======================
		{
			name:      "issue validated + approve — execute habilitado",
			entity:    Entity{Kind: KindIssue, Status: "validated", PlanVerdict: "approve"},
			wantAvail: map[string]bool{flowExecute: true},
		},
		{
			name:      "issue validated SIN verdict — execute HABILITADO (FIX blocker #3)",
			entity:    Entity{Kind: KindIssue, Status: "validated", PlanVerdict: ""},
			wantAvail: map[string]bool{flowExecute: true},
		},
		// ====================== close ======================
		{
			name:      "issue idea — close BLOQUEADO (FIX blocker #5: no hay PR)",
			entity:    Entity{Kind: KindIssue, Status: "idea"},
			wantAvail: map[string]bool{flowClose: false},
			wantReasonContains: map[string]string{
				flowClose: "sin PR",
			},
		},
		{
			name:      "issue plan — close BLOQUEADO (no hay PR)",
			entity:    Entity{Kind: KindIssue, Status: "plan", IssueBody: "## Plan consolidado\n"},
			wantAvail: map[string]bool{flowClose: false},
		},
		{
			name:      "issue closing — close bloqueado (ya en curso)",
			entity:    Entity{Kind: KindFused, Status: "closing", PRNumber: 80, IssueNumber: 79},
			wantAvail: map[string]bool{flowClose: false},
			wantReasonContains: map[string]string{
				flowClose: "closing",
			},
		},
		{
			name:      "issue closed — todos bloqueados",
			entity:    Entity{Kind: KindIssue, Status: "closed"},
			wantAvail: map[string]bool{flowClose: false},
			wantReasonContains: map[string]string{
				flowClose: "cerrado",
			},
		},
		// ====================== locked ======================
		{
			name:   "issue locked — todos bloqueados con razón locked + ref",
			entity: Entity{Kind: KindIssue, IssueNumber: 99, Status: "plan", Locked: true, IssueBody: "## Plan consolidado\n"},
			wantAvail: map[string]bool{
				flowExplore:  false,
				flowValidate: false,
				flowIterate:  false,
				flowExecute:  false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				// lockedReason ahora incluye `che unlock #99`
				flowValidate: "che unlock #99",
			},
		},
		{
			name:   "PR adopt locked — lockedReason usa ref con !",
			entity: Entity{Kind: KindPR, Status: "adopt", PRNumber: 88, Locked: true},
			wantAvail: map[string]bool{
				flowValidate: false,
			},
			wantReasonContains: map[string]string{
				flowValidate: "che unlock !88",
			},
		},
		// ====================== KindPR (adopt) ======================
		{
			name:   "PR adopt — solo validate (set fijo de adopt sin close)",
			entity: Entity{Kind: KindPR, Status: "adopt", PRNumber: 99},
			wantAvail: map[string]bool{
				flowValidate: true,
				flowExplore:  false,
				flowExecute:  false,
				flowIterate:  false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				flowExplore: "PR",
				flowExecute: "PR",
				flowIterate: "no aplica desde adopt",
				flowClose:   "no aplica desde adopt",
			},
		},
		{
			name:   "issue adopt — explore/execute habilitado, validate bloqueado (necesita plan)",
			entity: Entity{Kind: KindIssue, Status: "adopt", IssueNumber: 700},
			wantAvail: map[string]bool{
				flowExplore:  true,
				flowExecute:  true,
				flowValidate: false,
				flowIterate:  false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				flowValidate: "necesita che:plan",
				flowIterate:  "no aplica desde adopt",
				flowClose:    "no aplica desde adopt",
			},
		},
		// =========== KindPR post-adopt (state machine tras validate) ============
		// Cubre el bug de abril 2026: gateIterate rechazaba incondicionalmente
		// todo KindPR (mensaje "iterate no aplica a PRs adopt — corré validate
		// primero"), aunque el flow real soporta iterate sobre PR puro vía
		// stateref con fallback. Resultado: auto-loop dispatchaba iterate y
		// el doble check de gates lo mataba con "skip iterate"; los botones
		// del drawer aparecían disabled. La rama KindPR ahora se fusiona con
		// KindFused.
		{
			name: "KindPR validated + changes-requested → iterate ON, validate ON, close ON",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "validated",
				PRVerdict: "changes-requested"},
			wantAvail: map[string]bool{
				flowIterate:  true,
				flowValidate: true,
				flowClose:    true,
				flowExplore:  false,
				flowExecute:  false,
			},
		},
		{
			name: "KindPR validated + approve → iterate OFF (verdict no es changes-requested)",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "validated",
				PRVerdict: "approve"},
			wantAvail: map[string]bool{
				flowIterate:  false,
				flowValidate: true,
				flowClose:    true,
			},
			wantReasonContains: map[string]string{
				flowIterate: "validated:approve",
			},
		},
		{
			name: "KindPR validated sin verdict → iterate OFF con razón explícita",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "validated"},
			wantAvail: map[string]bool{
				flowIterate:  false,
				flowValidate: true,
				flowClose:    true,
			},
			wantReasonContains: map[string]string{
				flowIterate: "no tiene verdict",
			},
		},
		{
			name: "KindPR executed + changes-requested → iterate ON",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "executed",
				PRVerdict: "changes-requested"},
			wantAvail: map[string]bool{
				flowIterate:  true,
				flowValidate: true,
				flowClose:    true,
			},
		},
		{
			name:   "KindPR closed → validate y close OFF (defensivo)",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "closed"},
			wantAvail: map[string]bool{
				flowValidate: false,
				flowIterate:  false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				flowValidate: "ya está cerrado",
				flowClose:    "ya está cerrado",
			},
		},
		{
			name:   "KindPR closing → validate y close OFF (close en curso)",
			entity: Entity{Kind: KindPR, PRNumber: 26, Status: "closing"},
			wantAvail: map[string]bool{
				flowValidate: false,
				flowClose:    false,
			},
			wantReasonContains: map[string]string{
				flowValidate: "che:closing",
				flowClose:    "che:closing",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gates := computeGates(c.entity)
			for flow, wantAvail := range c.wantAvail {
				g, ok := gates[flow]
				if !ok {
					t.Errorf("gate ausente para flow=%q", flow)
					continue
				}
				if g.Available != wantAvail {
					t.Errorf("flow=%q: Available=%v, want %v (Reason=%q)", flow, g.Available, wantAvail, g.Reason)
				}
				// Contrato: si bloqueamos, Reason no puede estar vacío.
				if !g.Available && g.Reason == "" {
					t.Errorf("flow=%q: Available=false pero Reason vacío (contrato roto)", flow)
				}
			}
			for flow, wantSub := range c.wantReasonContains {
				g, ok := gates[flow]
				if !ok {
					continue
				}
				if !strings.Contains(strings.ToLower(g.Reason), strings.ToLower(wantSub)) {
					t.Errorf("flow=%q: Reason=%q no contiene %q", flow, g.Reason, wantSub)
				}
			}
		})
	}
}

// TestComputeGatesAllFlowsCovered es un guard: si alguien suma un flow nuevo
// a allFlows pero olvida agregarlo a computeGates, el map no lo va a tener
// y el test cae.
func TestComputeGatesAllFlowsCovered(t *testing.T) {
	gates := computeGates(Entity{Kind: KindIssue, Status: "idea"})
	for _, f := range allFlows {
		if _, ok := gates[f]; !ok {
			t.Errorf("computeGates no devuelve gate para flow=%q (drift entre allFlows y computeGates)", f)
		}
	}
}

// TestAllowedFlowsMatchAllFlows previene drift silencioso entre las dos
// listas canónicas: `allowedFlows` (server.go — flows que el handler
// /action permite) y `allFlows` (preflight.go — flows que computeGates
// evalúa). Si alguien suma un flow nuevo a una sin la otra, el test cae.
//
// Sin este test, el drift se manifestaría como: nuevo flow en allowedFlows
// → handler lo dispatcha → flowBtnState (con el fallback restrictivo
// post-fix abril 2026) marca el botón disabled con "gate ausente". El
// usuario reportaría como bug. Con el test, el drift se atrapa en CI.
func TestAllowedFlowsMatchAllFlows(t *testing.T) {
	// allFlows ⊆ allowedFlows: cada flow gateado debe poder dispatcharse.
	for _, f := range allFlows {
		if !allowedFlows[f] {
			t.Errorf("flow %q en allFlows pero no en allowedFlows (server.go) — el handler /action lo rechazará con 400", f)
		}
	}
	// allowedFlows ⊆ allFlows: cada flow dispatcheable debe tener gate.
	for f := range allowedFlows {
		found := false
		for _, x := range allFlows {
			if x == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("flow %q en allowedFlows pero no en allFlows (preflight.go) — gateOf devolverá Available=false con razón 'gate ausente' y el botón quedará disabled silenciosamente", f)
		}
	}
}
