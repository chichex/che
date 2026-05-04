// Package dash — preflight gates.
//
// computeGates evalúa, sin hacer IO, qué flows son disparables sobre una
// Entity dada y por qué (Reason humano-legible). El resultado se enchufa en
// tres puntos del dash:
//
//  1. Template: cada botón hx-post consulta Gates[flow].Available y
//     renderiza disabled+title=Reason cuando es false. La UI le explica al
//     humano por qué algo no se puede correr en vez de dejarlo intentar y
//     fallar adentro del subcomando.
//  2. POST /action handler: doble barrera contra clicks rápidos / curl
//     manual — devuelve 409 con Reason si el gate falla.
//  3. Auto-loop runTick: corta el ciclo de re-dispatch sobre entities que
//     no van a progresar (caso típico: validate-plan sobre un issue cuyo
//     body no tiene "## Plan consolidado" — el flow falla con ExitSemantic
//     y rollback al estado anterior, así que la próxima ronda matchearía
//     la misma regla. Sin gate, el cap del loop frenaba a la 10ma corrida;
//     con gate, no dispatcha ninguna).
//
// Las reglas son un mirror simplificado de los gates que cada flow chequea
// adentro (ver internal/flow/<flow>.go). Conviene mantener este archivo
// sincronizado con esos gates: sumar un test que falle si divergen
// (preflight_test.go cubre los casos de cada flow). Si en el futuro se
// extrae un PreconditionsOK por flow desde flowpkg, este archivo se
// reduciría a delegar — ver "Sugerencias arquitectónicas" del review de
// abril 2026.
package dash

import (
	"fmt"

	"github.com/chichex/che/internal/pipeline"
	planpkg "github.com/chichex/che/internal/plan"
)

// FlowGate es el resultado de evaluar si un flow puede dispararse sobre una
// Entity. Cuando Available=false, Reason siempre tiene un texto humano
// pensado para mostrarse en un tooltip o un mensaje inline ("body sin plan
// consolidado — editá el body o reseteá la entity", no "ExitSemantic").
// Cuando Available=true, Reason puede tener un hint informativo o quedar
// vacío.
type FlowGate struct {
	Available bool
	Reason    string
}

// FlowGates mapea nombre de flow → gate. Se computa por entity en
// overlayRunning y se inyecta en Entity.Gates para que el template lo
// consulte sin necesidad de funcs adicionales (gotcha del embed: las funcs
// no se promueven, solo los fields — ver project_go_template_embed_gotcha).
type FlowGates map[string]FlowGate

// Lista canónica de flows que el dash gatea. Sincronizada con allowedFlows
// (server.go) — TestAllowedFlowsMatchAllFlows previene drift silencioso
// si alguien suma un flow sin agregarlo en los dos lugares.
const (
	flowExplore  = "explore"
	flowValidate = "validate"
	flowIterate  = "iterate"
	flowExecute  = "execute"
	flowClose    = "close"
)

// allFlows es el orden canónico para snapshotting / iteración determinística
// (tests, debug). El template referencia los flows por nombre, no por slice.
var allFlows = []string{flowExplore, flowValidate, flowIterate, flowExecute, flowClose}

// computeGates devuelve gates para los 5 flows sobre `e`. Pura: no hace IO,
// no consulta estado mutable. Los Reasons mantienen el tono del CLI (sugerir
// el comando concreto que destrabe la situación cuando aplica).
//
// Convención: Reason en español, usa `che <subcomando>` como referencia
// accionable cuando hay un comando concreto que destraba el caso. Si el
// comando sugerido sería ÉL MISMO el que rechaza el estado actual (caso de
// re-explore desde che:plan, ver explore.go:665-677), el Reason sugiere la
// alternativa manual (editar body, resetear labels) en vez de loopear al
// usuario.
//
// Caso especial "adopt" = puerta de entrada al state machine. Una entity en
// Status="adopt" no tiene labels che:* todavía: el flow real no va a poder
// gatear nada estructural (no hay verdict, no hay che:plan, no hay nada).
// Cualquier gate basado en "este flow espera che:X" daría falso negativo —
// el humano viene a usar adopt JUSTO PARA aplicar che:* por primera vez.
// Por eso devolvemos un set fijo por kind con todos los flows del set
// habilitados sin chequeos. El resto queda Available=false con razón
// "no aplica en adopt — usá <set>".
func computeGates(e Entity) FlowGates {
	if e.Column() == "adopt" {
		return adoptGates(e)
	}
	return FlowGates{
		flowExplore:  gateExplore(e),
		flowValidate: gateValidate(e),
		flowIterate:  gateIterate(e),
		flowExecute:  gateExecute(e),
		flowClose:    gateClose(e),
	}
}

func gatePipelineStep(e Entity, p pipeline.Pipeline, step string) FlowGate {
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	if e.StateApplying {
		return FlowGate{false, fmt.Sprintf("step %s en curso — esperá que termine", e.StateStep)}
	}
	if step == "" {
		return FlowGate{false, "step vacío"}
	}
	current := e.StateStep
	if current == "" {
		current = e.Status
	}
	for i, s := range p.Steps {
		if s.Name != current {
			continue
		}
		if i+1 >= len(p.Steps) {
			return FlowGate{false, fmt.Sprintf("%s es el último step del pipeline", current)}
		}
		next := p.Steps[i+1].Name
		if next != step {
			return FlowGate{false, fmt.Sprintf("el próximo step desde %s es %s", current, next)}
		}
		return FlowGate{true, ""}
	}
	return FlowGate{false, fmt.Sprintf("step actual %s no existe en el pipeline activo", emptyAsIdea(current))}
}

// adoptGates devuelve el set fijo por kind para entities en columna "adopt".
// Reflejan la decisión de UX (acordada con el usuario abril 2026): adopt es
// la puerta de entrada al state machine, los flows del set arrancan el
// pipeline. Todos enabled sin gates — los gates reales corren dentro del
// flow cuando aplica (validate vía stateref, etc.). El resto deshabilitado.
//
// Sets:
//   - KindIssue: explore + execute (el "plan" del usuario es explore, ver
//     judgment-call en commit message). validate NO entra: runPlan exige
//     che:plan + body con `## Plan consolidado`, ninguno presente en adopt
//     puro. El path natural es explore → validate. KindIssue en adopt es
//     un edge-case actual (combineEntities skippea issues sin che:*) pero
//     definimos el set por defensa / uso futuro.
//   - KindFused: validate (el path canónico de adopt para PRs con issue
//     untracked — el flow valida vía stateref con fallback al PR).
//   - KindPR: validate (PR huérfano sin issue — idéntico al fused-adopt
//     desde el punto de vista del flow).
//
// iterate, close, idea NO entran al set: iterate necesita verdict previo
// (no aplica desde cero), close necesita PR mergeable (decisión humana
// post-validate), idea no es flow.
func adoptGates(e Entity) FlowGates {
	if e.Locked {
		return FlowGates{
			flowExplore:  {false, lockedReason(e)},
			flowValidate: {false, lockedReason(e)},
			flowIterate:  {false, lockedReason(e)},
			flowExecute:  {false, lockedReason(e)},
			flowClose:    {false, lockedReason(e)},
		}
	}
	const notInAdopt = "no aplica desde adopt — usá explore/execute/validate para entrar al state machine"
	switch e.Kind {
	case KindIssue:
		return FlowGates{
			flowExplore:  {true, ""},
			flowExecute:  {true, ""},
			flowValidate: {false, "validate plan necesita che:plan + body con `## Plan consolidado` — corré explore primero, después validate"},
			flowIterate:  {false, notInAdopt},
			flowClose:    {false, notInAdopt},
		}
	case KindFused:
		return FlowGates{
			flowValidate: {true, ""},
			flowExplore:  {false, notInAdopt},
			flowExecute:  {false, notInAdopt},
			flowIterate:  {false, notInAdopt},
			flowClose:    {false, notInAdopt},
		}
	case KindPR:
		return FlowGates{
			flowValidate: {true, ""},
			flowExplore:  {false, "explore solo aplica a issues — este card es un PR sin issue linkeado"},
			flowExecute:  {false, "execute solo aplica a issues — este card es un PR"},
			flowIterate:  {false, notInAdopt},
			flowClose:    {false, notInAdopt},
		}
	}
	return FlowGates{
		flowExplore:  {false, "kind desconocido"},
		flowValidate: {false, "kind desconocido"},
		flowIterate:  {false, "kind desconocido"},
		flowExecute:  {false, "kind desconocido"},
		flowClose:    {false, "kind desconocido"},
	}
}

// gateExplore: `che explore` espera un issue OPEN, con `ct:plan` aplicado,
// que NO haya avanzado más allá de `che:idea` (ver gateBasic en
// explore.go:658-682). En particular, `che:plan` ya está en la lista de
// "beyond" — re-explorar para refinar el plan NO es soportado por el flow.
// El path para refinar es: editar el body manualmente o resetear el label.
//
// Aceptamos Status in {"", "idea"} solamente. KindPR (adopt) no aplica —
// explore es issue-first, no opera sobre PRs.
func gateExplore(e Entity) FlowGate {
	if e.Kind == KindPR {
		return FlowGate{false, "explore solo aplica a issues — este card es un PR sin issue linkeado"}
	}
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	switch e.Status {
	case "", "idea":
		// "" = entity sin label che:* (issue legacy o KindFused sin
		// estado): explore puede arrancar el flow desde cero.
		return FlowGate{true, ""}
	case "plan":
		// Bug histórico fixeado abril 2026: gateExplore antes aceptaba
		// "plan" pensando que `che explore` soportaba re-explore para
		// refinar. NO es así — el flow real rechaza con ExitSemantic
		// (explore.go:665-677, lista beyond incluye che:plan). Sugerimos
		// el path manual.
		return FlowGate{false, fmt.Sprintf("explore no soporta re-explore desde che:plan — editá el body del issue manualmente, o `gh issue edit %d --remove-label che:plan --add-label che:idea` si querés re-empezar desde cero", e.IssueNumber)}
	default:
		return FlowGate{false, fmt.Sprintf("explore espera che:idea — este está en che:%s", e.Status)}
	}
}

// gateValidate: tres modos según Kind.
//
//   - KindIssue: che:plan + body con "## Plan consolidado" + sin lock.
//     El check de body es la diferencia clave que el dash no chequeaba antes
//     y que provocaba el ciclo del cap del auto-loop sobre issues con body
//     vacío o legacy (ver caso #146 dale-que-sale, abril 2026). También
//     aceptamos Status="validated" — re-validar un plan ya validado es UX
//     legítima (el template incluso muestra el botón "validate (re-validar
//     el plan)" en che:validated sin verdict bloqueante).
//
//   - KindFused: che:executed (path normal post-execute) O Status="adopt"
//     (validar PR adopt sin transición de máquina) O Status="validated"
//     (re-validar PR). validate.runPR (validate.go:396-410) sólo exige PR
//     OPEN + sin lock; la transición de máquina che:executed→che:validating
//     es opcional (validate.go:433-453, "if hasExecutedState").
//
//   - KindPR (adopt): solo el lock es bloqueante; el resto se chequea en el
//     flow real (validate vía stateref fallback al PR).
func gateValidate(e Entity) FlowGate {
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	switch e.Kind {
	case KindIssue:
		if e.Status != "plan" && e.Status != "validated" {
			return FlowGate{false, fmt.Sprintf("validate plan espera che:plan o che:validated (re-validar) — este está en che:%s", emptyAsIdea(e.Status))}
		}
		if !planpkg.HasConsolidatedHeader(e.IssueBody) {
			// `che explore` NO destraba el caso (post-fix abril 2026 ya
			// no acepta che:plan). Sugerimos edit manual.
			return FlowGate{false, fmt.Sprintf("el body del issue no tiene `## Plan consolidado` — editá el body y agregá el header (case-sensitive), o reseteá la entity con `gh issue edit %d --remove-label che:plan --add-label che:idea` y volvé a explorar", e.IssueNumber)}
		}
		return FlowGate{true, ""}
	case KindFused:
		// adopt: el flow valida via stateref fallback al PR (validate.go:
		// 426 ResolveStateRef + line 433 hasExecutedState opcional). Sin
		// che:executed la transición se saltea. Pasa el gate.
		if e.Status == "adopt" {
			return FlowGate{true, ""}
		}
		if e.Status != "executed" && e.Status != "validated" {
			return FlowGate{false, fmt.Sprintf("validate PR espera che:executed (path normal) o che:validated (re-validar) — este está en che:%s", emptyAsIdea(e.Status))}
		}
		return FlowGate{true, ""}
	case KindPR:
		// PR sin issue linkeado: el flow valida vía stateref con fallback
		// al PR. Status="adopt" (PR sin che:*), "validated" (re-validar
		// post-adopt) o "executed" (raro, PR con che:executed manual)
		// pasan; cerrados/en cierre rechazan defensivo (el flow real
		// fallaría de todos modos pero queremos UX clara). validating es
		// transient — si está corriendo, e.Locked se prende y sale arriba.
		switch e.Status {
		case "adopt", "validated", "executed":
			return FlowGate{true, ""}
		case "closing":
			return FlowGate{false, "ya hay un close en curso (che:closing) — esperá que termine"}
		case "closed":
			return FlowGate{false, "el PR ya está cerrado (che:closed)"}
		default:
			return FlowGate{false, fmt.Sprintf("validate PR espera Status adopt/validated/executed — este está en che:%s", emptyAsIdea(e.Status))}
		}
	}
	return FlowGate{false, "kind desconocido (defensa contra evolución del enum)"}
}

// gateIterate: post-v0.0.49 corre desde che:validated, no che:plan.
//
//   - KindIssue: che:validated + plan-validated:changes-requested + sin lock.
//     iterate.go:391-411 también rechaza che:executing/che:executed
//     concurrentes (estado anómalo); el dash no chequea raw-labels y
//     delega ese edge-case al flow.
//   - KindFused / KindPR: validated:changes-requested + sin lock. Status
//     in {executed, validated}. Ambos comparten la rama porque iterate
//     resuelve el verdict via stateref con fallback al PR cuando no hay
//     issue linkeado (validate.go:530-541 aplica che:validated al PR para
//     adopt mode, así que el snapshot lee el Status correcto del PR).
//     El flow real corre indistinto sobre fused vs PR puro.
func gateIterate(e Entity) FlowGate {
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	switch e.Kind {
	case KindIssue:
		if e.Status != "validated" {
			if e.Status == "plan" {
				return FlowGate{false, "iterate plan espera che:validated — corré validate primero"}
			}
			return FlowGate{false, fmt.Sprintf("iterate plan espera che:validated — este está en che:%s", emptyAsIdea(e.Status))}
		}
		if e.PlanVerdict == "" {
			return FlowGate{false, "el plan está en che:validated pero sin verdict — corré validate"}
		}
		if e.PlanVerdict != "changes-requested" {
			return FlowGate{false, fmt.Sprintf("el plan está en plan-validated:%s — iterate solo aplica con changes-requested", e.PlanVerdict)}
		}
		return FlowGate{true, ""}
	case KindFused, KindPR:
		if e.Status != "executed" && e.Status != "validated" {
			return FlowGate{false, fmt.Sprintf("iterate PR espera che:executed o che:validated — este está en che:%s", emptyAsIdea(e.Status))}
		}
		if e.PRVerdict == "" {
			return FlowGate{false, "el PR no tiene verdict — corré validate primero"}
		}
		if e.PRVerdict != "changes-requested" {
			return FlowGate{false, fmt.Sprintf("el PR está en validated:%s — iterate solo aplica con changes-requested", e.PRVerdict)}
		}
		return FlowGate{true, ""}
	}
	return FlowGate{false, "kind desconocido (defensa contra evolución del enum)"}
}

// gateExecute es issue-only y refleja exactamente el gate del flow real
// (execute.go:710-746):
//
//   - State OPEN + ct:plan: asumido por el snapshot del Source.
//   - NOT che:executing/executed/validating/closing/closed.
//   - NOT plan-validated:changes-requested/needs-human (verdicts
//     bloqueantes — execute respeta el verdict del validador).
//   - HasLabel(che:idea) OR che:plan OR che:validated.
//
// che:validated SIN plan-validated:* es un punto de entrada VÁLIDO
// (execute.go:743 + el comentario explicativo en :706-709): execute corre
// sobre el plan consolidado del body, sin requerir verdict explícito. El
// gate del dash ahora respeta esa permisividad (bug histórico de gateExecute
// pre-fix abril 2026: bloqueaba che:validated sin verdict con "corré
// validate" — sobre-restrictivo vs el flow).
func gateExecute(e Entity) FlowGate {
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	if e.Kind != KindIssue {
		return FlowGate{false, "execute solo aplica a issues — este card es un PR"}
	}
	switch e.Status {
	case "", "idea":
		return FlowGate{true, ""}
	case "plan":
		// Verdicts bloqueantes pueden coexistir con che:plan si validate
		// corrió en una versión vieja del flow (pre-v0.0.49 transicionaba
		// el verdict sobre che:plan, no che:validated). El flow rechaza —
		// reflejamos el chequeo acá.
		if e.PlanVerdict == "changes-requested" {
			return FlowGate{false, fmt.Sprintf("el plan tiene plan-validated:changes-requested — corré `che iterate %d` primero", e.IssueNumber)}
		}
		if e.PlanVerdict == "needs-human" {
			return FlowGate{false, "el plan tiene plan-validated:needs-human — resolvé a mano antes de ejecutar"}
		}
		return FlowGate{true, ""}
	case "validated":
		// post-v0.0.49: el verdict vive en plan-validated:* sobre un issue
		// en che:validated. approve y "" pasan; el flow confía en el plan
		// consolidado del body sin exigir el verdict explícito.
		if e.PlanVerdict == "changes-requested" {
			return FlowGate{false, fmt.Sprintf("el plan tiene plan-validated:changes-requested — corré `che iterate %d` primero", e.IssueNumber)}
		}
		if e.PlanVerdict == "needs-human" {
			return FlowGate{false, "el plan tiene plan-validated:needs-human — resolvé a mano antes de ejecutar"}
		}
		return FlowGate{true, ""}
	default:
		return FlowGate{false, fmt.Sprintf("execute espera che:idea, che:plan o che:validated — este está en che:%s", e.Status)}
	}
}

// gateClose: `che close` opera sobre PR (FetchPR en closing.go:413). Sin PR
// asociado el flow falla. Los gates de label (che:executed/validated) los
// chequea el flow vía stateref con fallback al PR — el dash no los duplica
// para no acoplarse a la lógica de stateref.
//
// Memoria del proyecto (feedback_close_no_gate): close no rechaza por
// VERDICT (humano decide qué cerrar). Pero sí necesita un PR para mergear.
//
//   - KindIssue (sin PR): bloquear con razón clara.
//   - KindFused / KindPR: pasar (el flow chequea estados; humano decide
//     verdict).
//   - Status "closed"/"closing": bloquear (ya cerrado / ya en curso).
//   - Locked: bloquear.
func gateClose(e Entity) FlowGate {
	if e.Locked {
		return FlowGate{false, lockedReason(e)}
	}
	if e.Status == "closed" {
		return FlowGate{false, "ya está cerrado"}
	}
	if e.Status == "closing" {
		return FlowGate{false, "ya hay un close en curso (che:closing)"}
	}
	if e.Kind == KindIssue {
		// KindIssue por definición no tiene PR (KindFused = issue+PR,
		// KindPR = PR adopt). close requiere PR para mergear/cerrar.
		return FlowGate{false, "close opera sobre PR — este card es un issue sin PR linkeado (corré explore/execute primero, o cerrá el issue desde GitHub si no querés mergear)"}
	}
	return FlowGate{true, ""}
}

// lockedReason centraliza el texto del lock para mantener consistencia entre
// gates. Incluye el ref correcto (issue # o PR !) para que `che unlock`
// reciba el número que el operador ve en el card.
func lockedReason(e Entity) string {
	ref := e.IssueNumber
	prefix := "#"
	if e.Kind == KindPR {
		ref = e.PRNumber
		prefix = "!"
	}
	return fmt.Sprintf("tiene che:locked — esperá que termine el flow en curso, o corré `che unlock %s%d`", prefix, ref)
}

// emptyAsIdea normaliza el Status vacío al label "idea" para los Reasons.
// Una entity sin che:* aparece como Status="" en el snapshot pero el humano
// la ve en la columna idea — así el mensaje queda alineado. NO aplica a
// KindPR (Status="adopt" para esos), pero los Reasons que usan emptyAsIdea
// son issue-side, así que el case adopt no entra acá.
func emptyAsIdea(s string) string {
	if s == "" {
		return "idea"
	}
	return s
}
