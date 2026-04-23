// Package dash — auto-loop engine (Step 6).
//
// El auto-loop observa el snapshot del Source y dispara automáticamente
// `che validate`, `che iterate` o `che execute` sobre entidades que
// estén en estados "intermedios" sin verdict resolutorio — cerrando el
// ciclo humano → IA sin intervención manual.
//
// Reglas (5):
//   1. Status=plan sin PlanVerdict                 → che validate <IssueNumber>
//   2. Status=validated + PlanVerdict=changes-req  → che iterate  <IssueNumber>
//      (post-v0.0.49: validate transiciona plan→validated; el verdict
//       "changes-requested" queda como label plan-validated:* sobre un
//       issue en che:validated.)
//   3. Status=executed sin PRVerdict               → che validate <PRNumber>
//   4. Status=executed + PRVerdict=changes-req     → che iterate  <PRNumber>
//   5. Status=validated + PlanVerdict=approve      → che execute  <IssueNumber>
//      (issue-only; sin PR previo. Cierra el gap post-validate plan:
//       un plan aprobado automáticamente pasa a ejecución.)
//
// Stop conditions por entity:
//   - verdict=approve        → done (feliz), no dispatch.
//   - verdict=needs-human    → done (requiere ojo humano), no dispatch.
//   - Locked=true            → skip este tick (no terminal; el próximo
//                              intenta de vuelta cuando se destrabe).
//   - rounds[id] >= LoopCap  → cap alcanzado, no dispatch.
//
// Concurrency: a lo sumo 1 flow issue-side + 1 PR-side simultáneos en todo
// el board. La clasificación es por Kind (KindIssue=issue-side, KindFused
// =PR-side). Los flows disparados por el humano desde el modal también
// ocupan slots — son "rounds efectivas" sobre la entity y suman al cap.
//
// Estado en memoria (no persiste a disco — ver project_tui_session_state.md):
// master switch + flags por regla + contador de rounds. Todo protegido por
// un mutex dedicado (loopMu) separado del que protege el overlay de running,
// porque los dominios son independientes y no queremos bloquear handlers
// HTTP mientras el tick evalúa.
//
// Agentes por defecto (hoy no configurable — la decisión de hacerlo
// configurable es un follow-up explícito):
//   - validate: 1x opus  — default del subcomando `che validate` (flag
//     `--validators` default = "opus", 1 validador).
//   - iterate:  1x opus  — `che iterate` no tiene `--agent`, es opus por
//     diseño del flow.
//   - execute:  1x opus  — `che execute` default de `--agent` es opus.
//     El loop dispatcha execute solo via RuleExecutePlan (Status=validated
//     + PlanVerdict=approve). Es una señal explícita del humano (validar
//     con approve equivale a "luz verde") — sin approve no hay dispatch.
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
// El contador se resetea al reiniciar (in-memory). 5 es conservador: cubre
// 2-3 rondas iterate↔validate más algún retry manual antes de cortar.
const LoopCap = 5

// LoopRule es el identificador de una de las 4 reglas loopeables. Usado
// como clave del map del state y en la URL del endpoint POST /loop/rule/...
type LoopRule string

const (
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
	// RuleValidatePR: entity en Status=executed sin PRVerdict → validate.
	RuleValidatePR LoopRule = "validate-pr"
	// RuleIteratePR: entity con PRVerdict=changes-requested → iterate.
	// Matchea en dos estados: Status=executed (validate-pr todavía no corrió)
	// y Status=validated (validate-pr ya transicionó executed→validated y
	// dejó el verdict en validated:*). Post-validate PR es el caso común:
	// el flow natural deja al fused en validated con verdict.
	RuleIteratePR LoopRule = "iterate-pr"
)

// allLoopRules es la allowlist + orden canónico de evaluación. El tick
// recorre las 5 en este orden para cada entity, primera que matchee gana.
// El orden importa: si una entity tuviera 2 reglas aplicables (no debería
// pasar — las condiciones son mutuamente exclusivas por status + verdict),
// la más "progresiva" (validate antes que iterate; execute-plan después
// de las de plan/status para mantener el flujo natural issue→PR) queda
// primera dentro de su bloque.
var allLoopRules = []LoopRule{
	RuleValidatePlan,
	RuleIteratePlan,
	RuleExecutePlan,
	RuleValidatePR,
	RuleIteratePR,
}

// loopState es el estado del auto-loop mantenido por el Server. Zero value:
// master OFF, todas las reglas OFF, rounds vacío.
type loopState struct {
	mu      sync.Mutex
	enabled bool
	rules   map[LoopRule]bool
	// rounds trackea cuántos dispatches (auto + manual) se hicieron sobre
	// cada IssueNumber desde que arrancó el server. Se usa para el cap.
	rounds map[int]int
	// capNotified trackea qué issueNumbers ya logueamos "cap hit" a stderr
	// para no spamear cada tick.
	capNotified map[int]bool
}

// newLoopState devuelve un estado inicial zero-valued pero con los maps
// instanciados para poder escribir sin chequeos extra.
func newLoopState() *loopState {
	return &loopState{
		rules:       map[LoopRule]bool{},
		rounds:      map[int]int{},
		capNotified: map[int]bool{},
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

// snapshotRules devuelve un slice ordenado de {regla, on?} para renderear
// el popover. No expone el map interno (evitar aliasing).
func (l *loopState) snapshotRules() []loopRuleView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]loopRuleView, 0, len(allLoopRules))
	for _, r := range allLoopRules {
		out = append(out, loopRuleView{Name: r, Label: ruleLabel(r), On: l.rules[r]})
	}
	return out
}

// isEnabled devuelve el estado del master bajo lock.
func (l *loopState) isEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// setEnabled flipea el master. Devuelve el valor nuevo.
func (l *loopState) toggleEnabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = !l.enabled
	return l.enabled
}

// toggleRule flipea una regla. Devuelve el valor nuevo. Asume r válida
// (el handler valida antes de llamar).
func (l *loopState) toggleRule(r LoopRule) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rules[r] = !l.rules[r]
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

// ruleLabel es el texto humano que se muestra en el popover para una regla.
func ruleLabel(r LoopRule) string {
	switch r {
	case RuleValidatePlan:
		return "validate plan (plan sin verdict → validate)"
	case RuleIteratePlan:
		return "iterate plan (validated con changes-requested → iterate)"
	case RuleExecutePlan:
		return "execute plan (validated + approve → execute)"
	case RuleValidatePR:
		return "validate PR (executed sin verdict → validate)"
	case RuleIteratePR:
		return "iterate PR (executed con changes-requested → iterate)"
	}
	return string(r)
}

// loopRuleView es el shape que consume el template del popover.
type loopRuleView struct {
	Name  LoopRule
	Label string
	On    bool
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
//   1. Locked=true → skip (motivo: "locked").
//   2. Cap hit (rounds >= LoopCap) → stop (motivo: "cap-reached").
//   3. Verdict terminal (approve / needs-human) → stop.
//   4. Match de reglas ON → dispatch.
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
	case "plan":
		// Verdict terminal → stop.
		if e.PlanVerdict == "approve" {
			return "", 0, "plan-approved"
		}
		if e.PlanVerdict == "needs-human" {
			return "", 0, "plan-needs-human"
		}
		// Sin verdict → rule1 (validate). Nota: Status=plan +
		// PlanVerdict=changes-requested ya no existe en la práctica —
		// validate transiciona plan→validated antes de setear el verdict
		// (ver project_validation_model.md). El matching de iterate-plan
		// vive en el case "validated".
		if e.PlanVerdict == "" && rules[RuleValidatePlan] {
			return "validate", e.IssueNumber, "rule:validate-plan"
		}
		return "", 0, "no-rule-match"
	case "validated":
		// Fused en "validated" = post validate-pr: el flow transicionó
		// executed→validated y dejó el verdict en PRVerdict (validated:*).
		// Para issue-only, el verdict vive en PlanVerdict. El case se
		// bifurca por Kind porque el flow natural es distinto.
		if e.Kind == KindFused {
			if e.PRVerdict == "needs-human" {
				return "", 0, "pr-needs-human"
			}
			if e.PRVerdict == "approve" {
				return "", 0, "pr-approved"
			}
			// changes-requested → iterate (rule4). Cierra el loop
			// validate↔iterate del lado PR: análogo al fix iterate-plan
			// para el lado issue (v0.0.67).
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

// entitySide clasifica la entity en issue-side o PR-side para el slot de
// concurrencia. Por Kind: KindIssue=issue, KindFused=pr. Simple y
// determinístico.
func entitySide(e Entity) string {
	if e.Kind == KindFused {
		return "pr"
	}
	return "issue"
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
	if !s.loop.isEnabled() {
		return 0
	}
	// Snapshot de reglas bajo lock. Si todas están OFF, no hace falta
	// recorrer entidades.
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
		running := e.RunningFlow != "" || localRunning[e.IssueNumber] != ""
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
		rounds := s.loop.roundsFor(e.IssueNumber)
		flow, targetRef, reason := nextDispatch(e, rules, rounds)
		if flow == "" {
			// Cap hit: log una vez a stderr.
			if reason == "cap-reached" {
				if !s.loop.markCapNotified(e.IssueNumber) {
					fmt.Fprintf(os.Stderr, "dash auto-loop: cap %d reached for #%d — stop\n", LoopCap, e.IssueNumber)
				}
			}
			continue
		}
		// Reservar slot local. markRunning chequea el snapshot de nuevo
		// (doble barrera contra races con handler manual que acaba de
		// disparar y todavía no está en el overlay).
		if _, ok := s.markRunning(e.IssueNumber, flow); !ok {
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
		s.loop.incRounds(e.IssueNumber)
		s.markAutoRunning(e.IssueNumber, true)
		if err := s.runAction(flow, targetRef, e.IssueNumber, s.repoPath); err != nil {
			fmt.Fprintf(os.Stderr, "dash auto-loop: spawn %s #%d failed: %v (reason=%s)\n", flow, e.IssueNumber, err, reason)
			s.clearRunning(e.IssueNumber)
			s.markAutoRunning(e.IssueNumber, false)
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

// loopPopoverData es el contexto del template del popover.
type loopPopoverData struct {
	Enabled     bool
	Rules       []loopRuleView
	ActiveRules int
	ActiveLoops int // len del running map (manual+auto), para el label del pill
	AutoLoops   int // solo los auto
}

// buildLoopData arma el contexto del popover + del pill label. Separado de
// buildData para no acoplar dos endpoints distintos.
func (s *Server) buildLoopData() loopPopoverData {
	rules := s.loop.snapshotRules()
	active := 0
	for _, r := range rules {
		if r.On {
			active++
		}
	}
	s.mu.Lock()
	running := len(s.running)
	auto := len(s.autoRunning)
	s.mu.Unlock()
	return loopPopoverData{
		Enabled:     s.loop.isEnabled(),
		Rules:       rules,
		ActiveRules: active,
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

// handleLoopToggle flipea el master y devuelve popover + pill label (OOB).
func (s *Server) handleLoopToggle(w http.ResponseWriter, _ *http.Request) {
	s.loop.toggleEnabled()
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
// reglas nuevas, el label se ajusta solo. Ejemplos:
//   - master OFF                  → "auto-loop OFF"
//   - master ON, 0 rules          → "auto-loop ON (0/5)"
//   - master ON, 3 rules, 2 auto  → "auto-loop ON (3/5) · ⟳ 2"
func pillLabel(data loopPopoverData) string {
	if !data.Enabled {
		return "auto-loop OFF"
	}
	label := fmt.Sprintf("auto-loop ON (%d/%d)", data.ActiveRules, len(allLoopRules))
	return label
}

// pillLabelHasSpinner indica si mostrar el chip magenta de "⟳ N" junto al
// label. True cuando el master está ON y hay al menos un auto-dispatch en
// curso. Se expone al template para el conditional del span .count.
func pillLabelHasSpinner(data loopPopoverData) bool {
	return data.Enabled && data.AutoLoops > 0
}

