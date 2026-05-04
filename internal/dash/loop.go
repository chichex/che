// Package dash — auto-loop engine (Step 6).
//
// El auto-loop observa el snapshot del Source y dispara automáticamente
// `che explore`, `che validate`, `che iterate` o `che execute` sobre
// entidades que estén en estados "intermedios" sin verdict resolutorio —
// cerrando el ciclo humano → IA sin intervención manual.
//
// Reglas (7), enumeradas en orden didáctico (issue-side primero, después PR-side):
//  1. Status=idea (o "")                          → che explore  <IssueNumber>
//     (idea→plan: arranca el ciclo desde un issue con `ct:plan` aplicado.)
//  2. Status=plan sin PlanVerdict                 → che validate <IssueNumber>
//  3. Status=validated + PlanVerdict=changes-req  → che iterate  <IssueNumber>
//     (post-v0.0.49: validate transiciona plan→validated; el verdict
//     "changes-requested" queda como label plan-validated:* sobre un
//     issue en che:validated.)
//  4. Status=validated + PlanVerdict=approve      → che execute  <IssueNumber>
//     (issue-only; sin PR previo. Cierra el gap post-validate plan:
//     un plan aprobado automáticamente pasa a ejecución.)
//  5. Status=plan sin PlanVerdict                 → che execute  <IssueNumber>
//     (fast-lane "plan→executed" sin pasar por validate. Mutuamente
//     excluyente con regla 2 a nivel de UI: si ambas están ON, validate
//     gana — preferimos validar antes de ejecutar cuando hay duda.)
//  6. Status=executed sin PRVerdict               → che validate <PRNumber>
//  7. Status=executed + PRVerdict=changes-req     → che iterate  <PRNumber>
//     (también matchea Status=validated cuando validate-pr ya transicionó.)
//
// Stop conditions por entity:
//   - verdict=approve        → done (feliz), no dispatch.
//   - verdict=needs-human    → done (requiere ojo humano), no dispatch.
//   - Locked=true            → skip este tick (no terminal; el próximo
//     intenta de vuelta cuando se destrabe).
//   - rounds[id] >= LoopCap  → cap alcanzado, no dispatch.
//
// Concurrency: a lo sumo 1 flow issue-side + 1 PR-side simultáneos en todo
// el board. La clasificación es por Kind (KindIssue=issue-side, KindFused
// =PR-side). Los flows disparados por el humano desde el modal también
// ocupan slots — son "rounds efectivas" sobre la entity y suman al cap.
//
// Estado en memoria (no persiste a disco — ver project_tui_session_state.md):
// flags por regla + contador de rounds. Todo protegido por un mutex
// dedicado (loopMu) separado del que protege el overlay de running, porque
// los dominios son independientes y no queremos bloquear handlers HTTP
// mientras el tick evalúa. El loop se considera "encendido" cuando hay al
// menos una regla on (no hay master switch — se borró en v0.0.77).
//
// Agentes por defecto (hoy no configurable — la decisión de hacerlo
// configurable es un follow-up explícito):
//   - explore:  1x opus  — `che explore` default de `--agent` es opus.
//   - validate: 1x opus  — default del subcomando `che validate` (flag
//     `--validators` default = "opus", 1 validador).
//   - iterate:  1x opus  — `che iterate` no tiene `--agent`, es opus por
//     diseño del flow.
//   - execute:  1x opus  — `che execute` default de `--agent` es opus.
//     El loop dispatcha execute via dos reglas: RuleExecutePlan (validated
//   - approve = luz verde explícita del validador) o RuleExecuteRaw
//     (plan sin verdict = fast-lane, opt-in para usuarios que confían en
//     el plan sin validarlo).
//
// El loop invoca los subcomandos sin flags de agent — heredan estos
// defaults. Mantener esto sincronizado si los defaults cambien en cmd/.
package dash

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// LoopCap es el número máximo de dispatches automáticos + manuales que el
// loop engine permite para una misma entidad desde que arrancó el server.
// El contador se resetea al reiniciar (in-memory). 10 cubre ~5 rondas
// iterate↔validate — suficiente para un feature que necesita varios pulidos
// antes de aprobarse, sin entrar en loops infinitos si el validador nunca
// converge. El handler manual de /action NO consulta este cap (solo gatea
// el auto-loop engine), así que el humano puede seguir disparando a mano
// desde el modal después de que el cap se alcance.
const LoopCap = 10

// LoopRule es el identificador de una de las 4 reglas loopeables. Usado
// como clave del map del state y en la URL del endpoint POST /loop/rule/...
type LoopRule string

const (
	// RuleExploreIdea: entity issue-only en Status="" o "idea" → explore.
	// Arranca el ciclo desde un issue con `ct:plan` aplicado pero sin
	// che:* todavía (o con che:idea explícito). Issue-only (KindIssue):
	// explore no opera sobre PRs ni fused.
	RuleExploreIdea LoopRule = "explore-idea"
	// RuleValidatePlan: entity en Status=plan sin PlanVerdict → validate.
	RuleValidatePlan LoopRule = "validate-plan"
	// RuleIteratePlan: entity en Status=validated con PlanVerdict=
	// changes-requested → iterate. Post-v0.0.49 validate transiciona
	// plan→validated; el verdict vive en plan-validated:* sobre un issue
	// ya en che:validated. `che iterate` también lee el issue desde
	// che:validated, así que el match cierra el loop validate↔iterate.
	RuleIteratePlan LoopRule = "iterate-plan"
	// RuleExecutePlan: entity en Status=validated (issue-only) con
	// PlanVerdict=approve → execute. Cierra el tramo post-validate plan:
	// si el validador aprobó, automáticamente ejecutamos. Solo issue-only
	// (KindIssue) — en fused no aplica, ese lado del flow ya implica PR
	// abierto y execute no corre sobre PRs existentes.
	RuleExecutePlan LoopRule = "execute-plan"
	// RuleExecuteRaw: entity en Status=plan sin PlanVerdict → execute,
	// sin pasar por validate. Fast-lane "plan→executed" para usuarios que
	// confían en el plan sin validarlo. Si tanto RuleValidatePlan como
	// RuleExecuteRaw están ON sobre el mismo plan, validate gana
	// (preferimos validar antes de ejecutar — el matcher chequea validate
	// primero dentro del case "plan").
	RuleExecuteRaw LoopRule = "execute-raw"
	// RuleValidatePR: entity en Status=executed sin PRVerdict → validate.
	RuleValidatePR LoopRule = "validate-pr"
	// RuleIteratePR: entity con PRVerdict=changes-requested → iterate.
	// Matchea en dos estados: Status=executed (validate-pr todavía no corrió)
	// y Status=validated (validate-pr ya transicionó executed→validated y
	// dejó el verdict en validated:*). Post-validate PR es el caso común:
	// el flow natural deja al fused en validated con verdict.
	RuleIteratePR LoopRule = "iterate-pr"
)

// allLoopRules es la allowlist + orden canónico de display en el popover.
// Lectura didáctica del lifecycle: arranca por idea→plan (explore), sigue
// el bloque issue-side (plan→validated→{plan,executed} via validate/iterate/
// execute), y cierra con PR-side (executed→validated→executed via
// validate/iterate-PR). Una entity no matchea más de una regla a la vez
// porque las condiciones son mutuamente exclusivas por (status, verdict);
// la única excepción es plan-sin-verdict, donde RuleValidatePlan y
// RuleExecuteRaw compiten — el matcher prefiere validate (ver nextDispatch).
var allLoopRules = []LoopRule{
	RuleExploreIdea,
	RuleValidatePlan,
	RuleIteratePlan,
	RuleExecutePlan,
	RuleExecuteRaw,
	RuleValidatePR,
	RuleIteratePR,
}

// loopState es el estado del auto-loop mantenido por el Server. Zero value:
// todas las reglas OFF, rounds vacío. El loop dispatcha cuando hay ≥1 regla
// on; con todas off, runTick es no-op (early return).
type loopState struct {
	mu    sync.Mutex
	rules map[LoopRule]bool
	// rounds trackea cuántos dispatches (auto + manual) se hicieron sobre
	// cada IssueNumber desde que arrancó el server. Se usa para el cap.
	rounds map[int]int
	// capNotified trackea qué issueNumbers ya logueamos "cap hit" a stderr
	// para no spamear cada tick.
	capNotified map[int]bool
	// gateNotified trackea qué (id, flow, reason) ya logueamos "gate skip"
	// a stderr. Mismo patrón que capNotified pero por triple — un mismo
	// issue puede tener gate skip por flows distintos al mismo tiempo, y
	// si la razón cambia (ej: estaba locked, se destrabó pero ahora le
	// falta el body) queremos volver a loguear. La key es "id|flow|reason".
	gateNotified map[string]bool
}

// newLoopState devuelve un estado inicial zero-valued pero con los maps
// instanciados para poder escribir sin chequeos extra.
func newLoopState() *loopState {
	return &loopState{
		rules:        map[LoopRule]bool{},
		rounds:       map[int]int{},
		capNotified:  map[int]bool{},
		gateNotified: map[string]bool{},
	}
}

// isValidRule devuelve si r está en allLoopRules. Defensa primaria para el
// handler POST /loop/rule/{name} — igual patrón que allowedFlows.
func isValidRule(r LoopRule) bool {
	for _, v := range allLoopRules {
		if v == r {
			return true
		}
	}
	return false
}

// activeRuleCount devuelve cuántas reglas están ON. Bajo lock.
func (l *loopState) activeRuleCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, on := range l.rules {
		if on {
			n++
		}
	}
	return n
}

// snapshotRules devuelve un slice ordenado de {regla, label, transition,
// on?} para renderear el popover. No expone el map interno (evitar
// aliasing). Label es la acción ("validate plan"); Transition es el
// efecto visible en código-style ("plan → validated") — el template los
// renderiza juntos para que el humano vea "qué hace cada regla" sin tener
// que leer el código fuente.
func (l *loopState) snapshotRules() []loopRuleView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]loopRuleView, 0, len(allLoopRules))
	for _, r := range allLoopRules {
		out = append(out, loopRuleView{
			Name:       r,
			Label:      ruleLabel(r),
			Transition: ruleTransition(r),
			On:         l.rules[r],
		})
	}
	return out
}

// anyRuleOn devuelve si al menos una regla está ON. Bajo lock. Se usa como
// "is the loop active?" — reemplaza el viejo isEnabled() del master switch
// que se borró en v0.0.77. Si ninguna regla está on, runTick es no-op.
func (l *loopState) anyRuleOn() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, on := range l.rules {
		if on {
			return true
		}
	}
	return false
}

// toggleRule flipea una regla. Devuelve el valor nuevo. Asume r válida (el
// handler valida antes de llamar). Aplica exclusión mutua entre el par
// (validate-plan, execute-raw): ambas matchean Status=plan sin verdict y no
// pueden estar on al mismo tiempo (la UI lo refleja con el separador "or";
// el matcher en nextDispatch ya prefería validate, ahora el toggle hace la
// exclusión real).
func (l *loopState) toggleRule(r LoopRule) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rules[r] = !l.rules[r]
	if l.rules[r] {
		switch r {
		case RuleValidatePlan:
			l.rules[RuleExecuteRaw] = false
		case RuleExecuteRaw:
			l.rules[RuleValidatePlan] = false
		}
	}
	return l.rules[r]
}

// incRounds incrementa el contador para id. Se llama ANTES de runAction
// para evitar el race donde el run tarda en volver y el próximo tick ve
// rounds[id] sin actualizar.
func (l *loopState) incRounds(id int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rounds[id]++
	return l.rounds[id]
}

// roundsFor devuelve el contador actual. Bajo lock.
func (l *loopState) roundsFor(id int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rounds[id]
}

// roundsSnapshot devuelve una copia del map de rounds. Bajo lock. Usado
// por overlayRunning para inyectar RunIter en las entities sin tomar
// l.mu N veces en el loop.
func (l *loopState) roundsSnapshot() map[int]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[int]int, len(l.rounds))
	for k, v := range l.rounds {
		out[k] = v
	}
	return out
}

// markCapNotified setea capNotified[id]=true si no estaba, y devuelve si
// ya estaba antes (para que el caller solo loguee la primera vez).
func (l *loopState) markCapNotified(id int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	was := l.capNotified[id]
	l.capNotified[id] = true
	return was
}

// markGateNotified marca que ya logueamos un gate-skip para el triple
// (id, flow, reason) y devuelve si ya estaba marcado. Los Reasons cambian
// con el estado (un issue puede pasar de "locked" a "body sin plan
// consolidado" a "approved"), así que la key incluye el reason — un cambio
// merece un log nuevo. La unicidad triple evita spam mientras la causa
// permanece estable.
func (l *loopState) markGateNotified(id int, flow, reason string) bool {
	key := fmt.Sprintf("%d|%s|%s", id, flow, reason)
	l.mu.Lock()
	defer l.mu.Unlock()
	was := l.gateNotified[key]
	l.gateNotified[key] = true
	return was
}

// ruleLabel es la acción que dispara la regla — el "qué hace", sin la
// transición de estados. El popover lo combina con ruleTransition para que
// se lea "validate plan · plan → validated" y el humano entienda de un
// vistazo de dónde a dónde transiciona la entity.
func ruleLabel(r LoopRule) string {
	switch r {
	case RuleExploreIdea:
		return "explore idea"
	case RuleValidatePlan:
		return "validate plan"
	case RuleIteratePlan:
		return "iterate plan"
	case RuleExecutePlan:
		return "execute plan aprobado"
	case RuleExecuteRaw:
		return "execute plan directo"
	case RuleValidatePR:
		return "validate PR"
	case RuleIteratePR:
		return "iterate PR"
	}
	return string(r)
}

// ruleSide clasifica la regla en "issue" o "pr" para agrupar el popover.
// Mismo criterio que entitySide pero a nivel de regla: las dos PR-rules
// son las únicas que operan sobre PRNumber. El resto vive sobre el issue
// (incluyendo execute, que crea el PR pero arranca desde el issue).
func ruleSide(r LoopRule) string {
	switch r {
	case RuleValidatePR, RuleIteratePR:
		return "pr"
	default:
		return "issue"
	}
}

// ruleTransition es la transición de estados que dispara la regla, en
// formato "<from> → <to>". Pensado para renderearse como chip code-style
// (monospace, color azul) al lado del label en el popover.
func ruleTransition(r LoopRule) string {
	switch r {
	case RuleExploreIdea:
		return "idea → plan"
	case RuleValidatePlan:
		return "plan → validated"
	case RuleIteratePlan:
		return "validated:changes-req → plan"
	case RuleExecutePlan:
		return "validated:approve → executed"
	case RuleExecuteRaw:
		return "plan → executed"
	case RuleValidatePR:
		return "executed → validated"
	case RuleIteratePR:
		return "validated:changes-req → executed"
	}
	return ""
}

// loopRuleView es el shape que consume el template del popover.
type loopRuleView struct {
	Name       LoopRule
	Label      string
	Transition string
	On         bool
}

// ruleFromState devuelve el estado del lifecycle desde el cual la regla
// dispara su transición — el primer término de ruleTransition antes del " →".
// El popover agrupa las reglas por este valor para que el humano lea el
// dropdown como "qué hago cuando una entity está en <state>".
//
// El estado "validated" aparece dos veces (una en issue, una en PR) porque
// son lifecycles paralelos: validated:approve / validated:changes-req del
// lado issue son distintos de validated:changes-req del lado PR. La
// agrupación final usa (side, fromState) como clave compuesta — ver
// buildLoopData.
func ruleFromState(r LoopRule) string {
	switch r {
	case RuleExploreIdea:
		return "idea"
	case RuleValidatePlan, RuleExecuteRaw:
		return "plan"
	case RuleIteratePlan, RuleExecutePlan:
		return "validated"
	case RuleValidatePR:
		return "executed"
	case RuleIteratePR:
		return "validated"
	}
	return ""
}

// loopFromGroup agrupa reglas por estado origen del lifecycle. Exclusive=true
// indica que las reglas del grupo son mutuamente excluyentes (sólo plan ON
// hoy: validate-plan y execute-raw). El template renderea un separador "or"
// entre las reglas cuando Exclusive=true.
type loopFromGroup struct {
	State     string
	Rules     []loopRuleView
	Exclusive bool
}

// ================== Matcher puro ==================
//
// nextDispatch decide qué flow (si alguno) disparar sobre `e` dada la
// configuración de reglas y el contador de rounds. Función pura: no toca
// estado mutable, no hace IO. Testeable con tabla.
//
// Devuelve:
//   - flow: "validate" | "iterate" | "" (vacío = no dispatchar).
//   - targetRef: número a pasar al subcomando (IssueNumber o PRNumber
//     según regla + Kind — encapsula resolveTargetRef).
//   - reason: string de diagnóstico (útil para logs / tests). Siempre
//     no-vacío, describe por qué se devolvió lo que se devolvió.

// nextDispatch evalúa una entity contra la configuración del loop y
// devuelve el dispatch a ejecutar, si alguno. El caller debe además
// consultar si el slot (issue-side o PR-side) está libre — eso NO es
// puro y vive en el tick.
//
// Reglas de corte (en orden de chequeo):
//  1. Locked=true → skip (motivo: "locked").
//  2. Cap hit (rounds >= LoopCap) → stop (motivo: "cap-reached").
//  3. Verdict terminal (approve / needs-human) → stop.
//  4. Match de reglas ON → dispatch.
//
// Devuelve flow="" si nada dispatcha; reason siempre describe por qué.
func nextDispatch(e Entity, rules map[LoopRule]bool, rounds int) (flow string, targetRef int, reason string) {
	if e.Locked {
		return "", 0, "locked"
	}
	if rounds >= LoopCap {
		return "", 0, "cap-reached"
	}

	switch e.Status {
	case "", "idea":
		// Status="" = entity sin che:* (issue legacy o ct:plan recién
		// aplicado pero el watcher de labels todavía no transicionó a
		// che:idea). gateExplore acepta los dos como puntos de entrada
		// válidos para arrancar el ciclo. Issue-only: KindFused/KindPR
		// con Status="" son edge-cases raros que no escalan a explore.
		if e.Kind != KindIssue {
			return "", 0, "status-not-loopable"
		}
		if rules[RuleExploreIdea] {
			return "explore", e.IssueNumber, "rule:explore-idea"
		}
		return "", 0, "no-rule-match"
	case "plan":
		// Verdict terminal → stop.
		if e.PlanVerdict == "approve" {
			return "", 0, "plan-approved"
		}
		if e.PlanVerdict == "needs-human" {
			return "", 0, "plan-needs-human"
		}
		// Sin verdict: dos reglas compiten — validate (canal normal) y
		// execute-raw (fast-lane, opt-in). validate gana si ambas están
		// ON: el orden refleja la preferencia "validar antes de
		// ejecutar". Status=plan + PlanVerdict=changes-requested ya no
		// existe en la práctica post-v0.0.49 (validate transiciona
		// plan→validated antes de setear el verdict — ver
		// project_validation_model.md); el matching de iterate-plan vive
		// en el case "validated".
		if e.PlanVerdict == "" && rules[RuleValidatePlan] {
			return "validate", e.IssueNumber, "rule:validate-plan"
		}
		if e.PlanVerdict == "" && rules[RuleExecuteRaw] {
			return "execute", e.IssueNumber, "rule:execute-raw"
		}
		return "", 0, "no-rule-match"
	case "validated":
		// Fused/KindPR en "validated" = post validate-pr: el flow
		// transicionó executed→validated (fused con che:executed previo)
		// o aplicó che:validated directo al PR (adopt mode, KindPR puro o
		// fused sin che:executed — ver validate.go:530-541, commit
		// 955313e). En ambos casos el verdict vive en PRVerdict
		// (validated:*). Para issue-only, el verdict vive en PlanVerdict.
		// El case se bifurca por Kind porque el flow natural es distinto.
		if e.Kind == KindFused || e.Kind == KindPR {
			if e.PRVerdict == "needs-human" {
				return "", 0, "pr-needs-human"
			}
			if e.PRVerdict == "approve" {
				return "", 0, "pr-approved"
			}
			// changes-requested → iterate (rule4). Cierra el loop
			// validate↔iterate del lado PR: análogo al fix iterate-plan
			// para el lado issue (v0.0.67). Cubre KindPR adoptado +
			// re-validado con changes-requested (bug abril 2026: el
			// case sólo branchaba KindFused y un PR huérfano caía al
			// branch issue-only que mira PlanVerdict).
			if e.PRVerdict == "changes-requested" && rules[RuleIteratePR] && e.PRNumber > 0 {
				return "iterate", e.PRNumber, "rule:iterate-pr"
			}
			return "", 0, "no-rule-match"
		}
		// Issue-only:
		// PlanVerdict=needs-human → stop (humano debe resolver).
		if e.PlanVerdict == "needs-human" {
			return "", 0, "plan-needs-human"
		}
		// changes-requested → iterate (rule2). Cierra el loop
		// validate↔iterate post-v0.0.49: validate dejó el issue en
		// che:validated con plan-validated:changes-requested; iterate corre
		// desde che:validated.
		if e.PlanVerdict == "changes-requested" && rules[RuleIteratePlan] {
			return "iterate", e.IssueNumber, "rule:iterate-plan"
		}
		// approve explícito → execute (rule5). No disparamos sin verdict:
		// un issue en che:validated SIN label plan-validated:* es un estado
		// raro (snapshot stale, o humano aplicó che:validated a mano).
		// Preferimos no ejecutar speculativo.
		if e.PlanVerdict == "approve" && rules[RuleExecutePlan] {
			return "execute", e.IssueNumber, "rule:execute-plan"
		}
		return "", 0, "no-rule-match"
	case "executed":
		if e.PRVerdict == "approve" {
			return "", 0, "pr-approved"
		}
		if e.PRVerdict == "needs-human" {
			return "", 0, "pr-needs-human"
		}
		// Para executed necesitamos PRNumber (fused). Si viniera sin PR
		// por algún motivo raro, no dispatchamos — el che validate/iterate
		// del lado PR asume un número de PR válido.
		if e.PRNumber == 0 {
			return "", 0, "executed-without-pr"
		}
		if e.PRVerdict == "" && rules[RuleValidatePR] {
			return "validate", e.PRNumber, "rule:validate-pr"
		}
		if e.PRVerdict == "changes-requested" && rules[RuleIteratePR] {
			return "iterate", e.PRNumber, "rule:iterate-pr"
		}
		return "", 0, "no-rule-match"
	}
	return "", 0, "status-not-loopable"
}

func usesPipeline(e Entity, steps []string) bool {
	// FALLBACK v1/v2: entities without che:state:<step> stay on the legacy
	// matcher. An empty pipeline snapshot also falls back so migrated entities do
	// not get stranded if pipeline resolution is temporarily unavailable.
	return e.StateStep != "" && len(steps) > 0
}

const (
	dynamicRunIssuePrefix = "run#:"
	dynamicRunPRPrefix    = "run!:"
)

type dynamicRunFlow struct {
	Step string
	PR   bool
}

func encodeDynamicRunFlow(run dynamicRunFlow) string {
	if run.PR {
		return dynamicRunPRPrefix + run.Step
	}
	return dynamicRunIssuePrefix + run.Step
}

func decodeDynamicRunFlow(flow string) (dynamicRunFlow, bool) {
	switch {
	case len(flow) > len(dynamicRunIssuePrefix) && flow[:len(dynamicRunIssuePrefix)] == dynamicRunIssuePrefix:
		return dynamicRunFlow{Step: flow[len(dynamicRunIssuePrefix):]}, true
	case len(flow) > len(dynamicRunPRPrefix) && flow[:len(dynamicRunPRPrefix)] == dynamicRunPRPrefix:
		return dynamicRunFlow{Step: flow[len(dynamicRunPRPrefix):], PR: true}, true
	default:
		return dynamicRunFlow{}, false
	}
}

func nextPipelineDispatch(e Entity, steps []string, rounds int) (flow string, targetRef int, reason string) {
	if e.StateStep == "" {
		return "", 0, "no-pipeline-state"
	}
	if len(steps) == 0 {
		return "", 0, "no-pipeline-steps"
	}
	if e.Locked {
		return "", 0, "locked"
	}
	if e.StateApplying {
		return "", 0, "applying"
	}
	if rounds >= LoopCap {
		return "", 0, "cap-reached"
	}
	for i, step := range steps {
		if step != e.StateStep {
			continue
		}
		if i+1 >= len(steps) {
			return "", 0, "pipeline-complete"
		}
		// Dynamic verdict handling lives in the pipeline agents/markers: the dash
		// only resumes from the current StateStep and lets `che run --auto` decide
		// advance, goto markers and skips. Legacy verdict rules are fallback only.
		run := dynamicRunFlow{Step: e.StateStep, PR: e.Kind == KindPR || (e.Kind == KindFused && e.PRNumber > 0)}
		targetRef := e.EntityKey()
		if run.PR {
			targetRef = e.PRNumber
		}
		return encodeDynamicRunFlow(run), targetRef, "step:" + e.StateStep
	}
	return "", 0, "step-not-in-pipeline"
}

// entitySide clasifica la entity en issue-side o PR-side para el slot de
// concurrencia. KindFused y KindPR ocupan el slot PR-side: ambos dispatchan
// sobre un PR cuando viven en el motor dinámico post-adopt.
func entitySide(e Entity) string {
	if e.Kind == KindFused || e.Kind == KindPR {
		return "pr"
	}
	return "issue"
}

// entityRef devuelve la representación corta de la entity para logs:
// "#123" para KindIssue/KindFused (referencia el issue) y "!45" para
// KindPR adopt (referencia el PR). Evita el log "#0" cuando KindPR
// (IssueNumber=0) cae en algún path de dispatch.
func entityRef(e Entity) string {
	if e.Kind == KindPR {
		return fmt.Sprintf("!%d", e.PRNumber)
	}
	if e.Kind == KindFused && e.PRNumber > 0 {
		return fmt.Sprintf("#%d→!%d", e.IssueNumber, e.PRNumber)
	}
	return fmt.Sprintf("#%d", e.IssueNumber)
}

// ================== Tick ==================

// runTick ejecuta una iteración del loop: lee snapshot, evalúa reglas y
// dispatcha hasta 1 flow issue-side + 1 PR-side.
//
// Llamado por runLoop (goroutine) y directamente desde tests. Devuelve
// cuántos dispatches hizo (útil para tests).
//
// Conservador frente a snapshots stale: si algún handler acaba de
// disparar un flow manual que todavía no se reflejó en el snapshot
// (rotura entre Source.Snapshot() y overlay), el chequeo de
// markRunning fallará y el tick saltará esa entity sin incrementar
// rounds ni dispatchar — seguro.
func (s *Server) runTick() int {
	// Snapshot de reglas bajo lock. Si todas están OFF, no hace falta
	// recorrer entidades — el "loop está apagado" cuando no hay reglas on
	// (no hay master switch desde v0.0.77).
	rules := map[LoopRule]bool{}
	s.loop.mu.Lock()
	for r, on := range s.loop.rules {
		rules[r] = on
	}
	s.loop.mu.Unlock()
	anyOn := false
	for _, on := range rules {
		if on {
			anyOn = true
			break
		}
	}
	if !anyOn {
		return 0
	}
	activePipeline := s.activePipeline()
	steps := make([]string, 0, len(activePipeline.Steps))
	for _, step := range activePipeline.Steps {
		steps = append(steps, step.Name)
	}

	snap := s.source.Snapshot()
	// Priorizar más viejo → más nuevo. Con concurrency 1 issue-side + 1
	// PR-side, sin este orden el que llegó último gana el slot en cada
	// tick (gh list default es most-recent-first) y un item que lleva
	// días esperando queda rezagado. Para fused usamos max(issue, PR)
	// como CreatedAt (ver combineEntities) — un PR recién iterado baja
	// en la cola. Stable para que empates (p.ej. todos zero en tests
	// existentes) respeten el orden original del snapshot.
	ents := make([]Entity, len(snap.Entities))
	copy(ents, snap.Entities)
	sort.SliceStable(ents, func(i, j int) bool {
		return ents[i].CreatedAt.Before(ents[j].CreatedAt)
	})
	// Clasificar slots ya ocupados: si hay flows en curso (overlay local
	// o snapshot), reservamos el slot para que no dispatchemos arriba.
	// Reutilizamos la lógica de overlayRunning: buildData hace eso en
	// cada request, acá replicamos en-linea.
	issueSlotBusy := false
	prSlotBusy := false
	s.mu.Lock()
	// Snapshot runs en curso (overlay local).
	localRunning := make(map[int]string, len(s.running))
	for k, v := range s.running {
		localRunning[k] = v
	}
	s.mu.Unlock()
	for _, e := range ents {
		running := e.RunningFlow != "" || localRunning[e.EntityKey()] != ""
		if !running {
			continue
		}
		if entitySide(e) == "pr" {
			prSlotBusy = true
		} else {
			issueSlotBusy = true
		}
	}

	dispatches := 0
	for _, e := range ents {
		// Si ambos slots ocupados, salir del loop (nada más por hacer).
		if issueSlotBusy && prSlotBusy {
			break
		}
		side := entitySide(e)
		if side == "issue" && issueSlotBusy {
			continue
		}
		if side == "pr" && prSlotBusy {
			continue
		}
		// EntityKey es la clave canónica del overlay/cap/gate maps.
		// IssueNumber colisiona en KindPR (todos con IssueNumber=0). Las
		// reglas issue-side y PR-side actuales no matchean KindPR, así
		// que esto es defensa para cuando se sumen reglas adopt-side.
		key := e.EntityKey()
		rounds := s.loop.roundsFor(key)
		if e.StateStep != "" && len(steps) == 0 {
			if !s.loop.markGateNotified(key, "pipeline", "no-pipeline-steps:"+e.StateStep) {
				fmt.Fprintf(os.Stderr, "dash auto-loop: dynamic fallback for %s — no-pipeline-steps (step=%s)\n", entityRef(e), e.StateStep)
			}
		}
		var flow string
		var targetRef int
		var reason string
		usedPipeline := usesPipeline(e, steps)
		if usedPipeline {
			flow, targetRef, reason = nextPipelineDispatch(e, steps, rounds)
		} else {
			flow, targetRef, reason = nextDispatch(e, rules, rounds)
		}
		if usedPipeline && flow == "" {
			switch reason {
			case "step-not-in-pipeline", "no-pipeline-steps":
				if !s.loop.markGateNotified(key, "pipeline", reason+":"+e.StateStep) {
					fmt.Fprintf(os.Stderr, "dash auto-loop: dynamic fallback for %s — %s (step=%s)\n", entityRef(e), reason, e.StateStep)
				}
				flow, targetRef, reason = nextDispatch(e, rules, rounds)
			case "pipeline-complete":
				if !s.loop.markGateNotified(key, "pipeline", reason+":"+e.StateStep) {
					fmt.Fprintf(os.Stderr, "dash auto-loop: pipeline complete for %s at step %s — stop\n", entityRef(e), e.StateStep)
				}
			}
		}
		if flow == "" {
			// Cap hit: log una vez a stderr.
			if reason == "cap-reached" {
				if !s.loop.markCapNotified(key) {
					fmt.Fprintf(os.Stderr, "dash auto-loop: cap %d reached for %s — stop\n", LoopCap, entityRef(e))
				}
			}
			continue
		}
		// Doble barrera con preflight gates: nextDispatch ya filtra por
		// status + verdict (la lógica que vivía exclusivamente en el
		// matcher), pero las gates agregan chequeos que el matcher no
		// ve — el más importante es "body con `## Plan consolidado`"
		// para validate-plan. Sin esto, validate-plan se dispatchaba 10
		// veces (cap) sobre un issue con body legacy y rollback sucesivo,
		// gastando ~10 corridas de claude antes de cortar (caso real
		// #146 dale-que-sale, abril 2026). El gate corta el ciclo a 0
		// dispatches y loguea una sola vez por (id, flow, reason).
		//
		// Las gates se computan a partir de la entity actual del snapshot
		// (Gates ya viene poblado por overlayRunning, pero acá usamos el
		// snapshot crudo del Source — overlayRunning no corre en el tick).
		// computeGates es pura, sin IO; el costo es despreciable.
		gate := computeDispatchGates(e, activePipeline, flow)[flow]
		if !gate.Available {
			if !s.loop.markGateNotified(key, flow, gate.Reason) {
				fmt.Fprintf(os.Stderr, "dash auto-loop: skip %s %s — %s\n", flow, entityRef(e), gate.Reason)
			}
			continue
		}
		// Reservar slot local. markRunning chequea el snapshot de nuevo
		// (doble barrera contra races con handler manual que acaba de
		// disparar y todavía no está en el overlay).
		if _, ok := s.markRunning(key, flow); !ok {
			// Alguien (handler manual) se nos adelantó entre el
			// chequeo inicial de issueSlotBusy/prSlotBusy y acá. No
			// dispatch, no incrementar rounds. Marcar el slot ocupado
			// para no reintentar en el mismo tick sobre otra entity
			// del mismo side.
			if side == "pr" {
				prSlotBusy = true
			} else {
				issueSlotBusy = true
			}
			continue
		}
		// Incrementar rounds ANTES de runAction — evita race donde el
		// run tarda en volver y el próximo tick ve rounds sin
		// actualizar. Si runAction falla, liberamos markRunning pero
		// dejamos rounds incrementado: es una ronda "intentada" que
		// cuenta igual, para no entrar en loop infinito si el spawn
		// falla sistemáticamente.
		s.loop.incRounds(key)
		s.markAutoRunning(key, true)
		if err := s.runAction(flow, targetRef, key, s.repoPath); err != nil {
			fmt.Fprintf(os.Stderr, "dash auto-loop: spawn %s %s failed: %v (reason=%s)\n", flow, entityRef(e), err, reason)
			s.clearRunning(key)
			s.markAutoRunning(key, false)
			continue
		}
		dispatches++
		if side == "pr" {
			prSlotBusy = true
		} else {
			issueSlotBusy = true
		}
	}
	return dispatches
}

// runLoop es la goroutine del tick. Corre hasta que ctx se cancele.
// Llamada desde Run() después de construir el Server.
func (s *Server) runLoop(done <-chan struct{}) {
	interval := time.Duration(s.pollInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.runTick()
		}
	}
}

// ================== Auto vs Manual flag ==================
//
// Decidimos un map paralelo `autoRunning map[int]bool` (en vez de cambiar
// `running map[int]string` a struct) por simplicidad: conserva el tipo
// existente, no churnea los tests del step 4, y el map adicional es
// barato. El lock que lo protege es el mismo `s.mu` del overlay — tienen
// el mismo ciclo de vida (seteado al dispatch, limpiado al clearRunning
// via markAutoRunning).

// markAutoRunning setea o limpia la bandera de "disparado por el loop"
// para id. Bajo s.mu porque comparte dominio con s.running.
func (s *Server) markAutoRunning(id int, auto bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoRunning == nil {
		s.autoRunning = map[int]bool{}
	}
	if auto {
		s.autoRunning[id] = true
	} else {
		delete(s.autoRunning, id)
	}
}

// isAutoRunning devuelve si id fue disparado por el loop. Para uso del
// template / UI (chip "auto").
func (s *Server) isAutoRunning(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoRunning[id]
}

// ================== HTTP handlers ==================

// loopPopoverData es el contexto del template del popover. IssueGroups y
// PRGroups agrupan las reglas por (side, fromState) para que el template
// renderee el dropdown como un mini-stepper del lifecycle: "from idea" →
// "from plan" → "from validated" en el side issue, y "from executed" →
// "from validated" en el side PR. TotalRules expone len(allLoopRules) sin
// requerir que el template lo derive (más ergonómico para el header
// "N/M reglas activas").
type loopPopoverData struct {
	IssueGroups []loopFromGroup
	PRGroups    []loopFromGroup
	ActiveRules int
	TotalRules  int
	ActiveLoops int // len del running map (manual+auto), para el label del pill
	AutoLoops   int // solo los auto
}

// issueFromOrder y prFromOrder fijan el orden de las secciones por estado
// origen — refleja la lectura del lifecycle (idea → plan → validated en
// issue side; executed → validated en PR side). Cambiar este orden mueve
// las secciones del popover.
var (
	issueFromOrder = []string{"idea", "plan", "validated"}
	prFromOrder    = []string{"executed", "validated"}
)

// buildLoopData arma el contexto del popover + del pill label. Separado de
// buildData para no acoplar dos endpoints distintos. Agrupa las reglas por
// (side, fromState) preservando el orden didáctico de allLoopRules dentro
// de cada grupo.
func (s *Server) buildLoopData() loopPopoverData {
	rules := s.loop.snapshotRules()
	active := 0
	// buckets indexados por (side, fromState). Mantenemos un map para
	// agregación + dos slices ordenados al final con issueFromOrder /
	// prFromOrder, así no dependemos del orden de iteración del map.
	type key struct{ side, from string }
	buckets := map[key][]loopRuleView{}
	for _, r := range rules {
		if r.On {
			active++
		}
		k := key{ruleSide(r.Name), ruleFromState(r.Name)}
		buckets[k] = append(buckets[k], r)
	}
	build := func(side string, order []string) []loopFromGroup {
		out := make([]loopFromGroup, 0, len(order))
		for _, st := range order {
			rs := buckets[key{side, st}]
			if len(rs) == 0 {
				continue
			}
			// Único par excluyente hoy: (validate-plan, execute-raw) sobre
			// from=plan en issue side. Si el grupo tiene >1 regla y son
			// las dos del par, marcamos Exclusive — el template renderea
			// el separador "or" entre ellas.
			exclusive := false
			if side == "issue" && st == "plan" && len(rs) == 2 {
				exclusive = true
			}
			out = append(out, loopFromGroup{State: st, Rules: rs, Exclusive: exclusive})
		}
		return out
	}
	s.mu.Lock()
	running := len(s.running)
	auto := len(s.autoRunning)
	s.mu.Unlock()
	return loopPopoverData{
		IssueGroups: build("issue", issueFromOrder),
		PRGroups:    build("pr", prFromOrder),
		ActiveRules: active,
		TotalRules:  len(allLoopRules),
		ActiveLoops: running,
		AutoLoops:   auto,
	}
}

// handleLoopGet renderea el popover (solo el partial — el pill label se
// setea en el render del topbar). Usado cuando el usuario clickea el pill
// y htmx hace GET /loop con hx-target="#loop-popover".
func (s *Server) handleLoopGet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "loop-popover", s.buildLoopData()); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// setAllRules prende o apaga todas las reglas de una. Bajo lock. Cuando
// enable=true, respeta la exclusión validate-plan ↔ execute-raw: prende
// validate-plan (gana en el matcher) y deja execute-raw OFF.
func (l *loopState) setAllRules(enable bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range allLoopRules {
		l.rules[r] = enable
	}
	if enable {
		l.rules[RuleExecuteRaw] = false
	}
}

// handleLoopBulk acepta /on y /off. Prende o apaga todas las reglas de una
// (alternativa más explícita que el viejo master switch — ver header del
// archivo). Para /on, validate-plan gana sobre execute-raw (exclusión).
func (s *Server) handleLoopBulk(w http.ResponseWriter, r *http.Request) {
	mode := r.PathValue("mode")
	switch mode {
	case "on":
		s.loop.setAllRules(true)
	case "off":
		s.loop.setAllRules(false)
	default:
		http.Error(w, "invalid bulk mode", http.StatusBadRequest)
		return
	}
	s.writeLoopResponse(w)
}

// handleLoopRule flipea una regla. Valida el name contra allLoopRules
// antes de mutar — igual patrón que allowedFlows.
func (s *Server) handleLoopRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rule := LoopRule(name)
	if !isValidRule(rule) {
		http.Error(w, "invalid rule", http.StatusBadRequest)
		return
	}
	s.loop.toggleRule(rule)
	s.writeLoopResponse(w)
}

// writeLoopResponse escribe el popover + el pill label con hx-swap-oob.
// Template dedicado "loop-response" que concatena ambos.
func (s *Server) writeLoopResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "loop-response", s.buildLoopData()); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
}

// pillLabel devuelve el texto que va en el pill del topbar, fuera del
// popover. Se expone como template func para que el partial "auto-loop-
// toggle" lo use. El denominador es len(allLoopRules) — si se agregan
// reglas nuevas, el label se ajusta solo. Sin master switch (v0.0.77+) el
// loop se considera "ON" cuando hay ≥1 regla activa. Ejemplos:
//   - 0 rules ON                  → "auto-loop OFF"
//   - 3 rules ON, 2 auto en curso → "auto-loop ON (3/7) · ⟳ 2"
func pillLabel(data loopPopoverData) string {
	if data.ActiveRules == 0 {
		return "auto-loop OFF"
	}
	return fmt.Sprintf("auto-loop ON (%d/%d)", data.ActiveRules, data.TotalRules)
}

// pillLabelHasSpinner indica si mostrar el chip magenta de "⟳ N" junto al
// label. True cuando hay al menos una regla on y al menos un auto-dispatch
// en curso. Se expone al template para el conditional del span .count.
func pillLabelHasSpinner(data loopPopoverData) bool {
	return data.ActiveRules > 0 && data.AutoLoops > 0
}
