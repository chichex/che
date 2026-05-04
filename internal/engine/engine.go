package engine

import (
	"context"
	"errors"
	"fmt"
)

// MaxTransitions es el cap global de transiciones por corrida del motor
// (PRD §3.c "Cap global de transiciones"). Si el contador llega a este
// valor → [stop] automático con razón "loop cap exceeded". No es
// configurable en v1: el objetivo es proteger contra loops accidentales,
// no parametrizar.
//
// "Transición" cuenta cada decisión de step→step que toma el motor:
// arranque (entry → primer step), avance natural ([next] o default),
// salto explícito ([goto: X]). Stop NO cuenta como transición (no avanza).
const MaxTransitions = 20

// Step es la unidad mínima del pipeline desde la perspectiva del motor.
// Sólo necesitamos Name + Agents + Aggregator para invocar — el resto de
// metadata vive en el paquete `internal/pipeline` (PR2). Cuando PR2
// merguea, un follow-up adapta `engine.Run` para consumir
// `pipeline.Pipeline` directamente y este tipo desaparece.
type Step struct {
	// Name es identificador único dentro del pipeline. Los markers
	// `[goto: <name>]` lo referencian. El motor valida que el destino
	// exista y convierte un goto a step desconocido en [stop] con razón.
	Name string

	// Agents es la lista de refs a agentes (built-in o custom) que el
	// step debe correr. Si len > 1, los agentes corren en paralelo y
	// Aggregator decide cómo resolver markers en conflicto. Un agente
	// repetido equivale a N instancias paralelas (best-of-N con el
	// mismo modelo).
	Agents []string

	// Aggregator selecciona la política de resolución cuando len(Agents)>1.
	// Vacío == default (`majority`). Para 1 agente, el campo se ignora —
	// `runStep` produce el mismo outcome con cualquier preset.
	Aggregator AggregatorKind
}

// Pipeline es la secuencia ordenada de steps que el motor ejecuta. PR5d
// agrega un Entry opcional que corre ANTES del primer step y decide desde
// dónde arrancar (`[next]` → primer step, `[goto: X]` → step X, `[stop]`
// → no correr nada). Si Entry == nil, el motor arranca desde el primer
// step directamente (comportamiento PR5b).
type Pipeline struct {
	Entry *EntrySpec
	Steps []Step
}

// EntrySpec describe el entry agent del pipeline (PRD §5.a). El entry es
// un validador/router que corre ANTES de los steps y emite un marker que
// define desde qué step arrancar:
//   - `[next]` o sin marker → primer step (comportamiento default).
//   - `[goto: X]` → arrancar en el step X.
//   - `[stop]` → no correr ningún step (rebote del input).
//
// Multi-agente: la struct ya soporta varios agentes + Aggregator, pero
// PR5d (este PR) sólo invoca al primer agente — igual que PR5b hace con
// los Steps. El multi-agente + aggregator vive en PR5c/follow-up; cuando
// PR5c merguee, un cambio mínimo (reemplazar el invoke directo por
// runStep) habilita el flow multi-agente sin romper el shape.
type EntrySpec struct {
	// Agents es la lista de refs a agentes que corren como entry. PR5d
	// usa Agents[0] solamente (mirror de PR5b para Steps); multi-agente
	// llega con PR5c.
	Agents []string

	// Aggregator es la política de resolución cuando len(Agents) > 1.
	// PR5d preserva el campo para no romper el shape — el motor lo
	// IGNORA en single-agent (que es el único modo que corre hoy).
	Aggregator AggregatorKind
}

// Invoker es el contrato que el motor usa para llamar a un agente. Está
// abstraído para que el motor sea testeable sin spawnear procesos y para
// que la integración con `internal/agent` (claude CLI) viva en otro
// archivo sin acoplar el motor a la forma exacta de ese paquete.
//
// Invoke devuelve:
//   - output: el stdout completo del agente. Vacío si la invocación
//     falló antes de producir output.
//   - format: el formato del output (Text vs StreamJSON). El motor lo
//     usa para elegir entre ParseMarker y ParseStreamMarker. Se pasa
//     como parte del return — y no como parámetro de entrada — porque
//     el implementador puede decidir el formato según el agente
//     (executors → stream-json, validators → text). PR5b acepta
//     cualquiera de los dos.
//   - err: error técnico. La definición de "técnico" (PRD §3.b paso 4):
//     timeout, exit code != 0, error de red, crash del binario, etc.
//     Cualquier error aquí gatilla [stop] automático en el motor.
//     Output sin marker pero err == nil = MarkerNext (default).
type Invoker interface {
	Invoke(ctx context.Context, agent string, input string) (output string, format OutputFormat, err error)
}

// OutputFormat selecciona cómo el motor parsea el output del agente.
// Mirror de `internal/agent.OutputFormat` para no acoplar al motor a ese
// paquete (PR1/PR2 no están mergeados todavía y el motor tiene que ser
// testeable independientemente).
type OutputFormat string

const (
	// FormatText: stdout es texto plano. Aplicar ParseMarker sobre todo
	// el output buscando la última línea no vacía.
	FormatText OutputFormat = "text"

	// FormatStreamJSON: stdout es NDJSON del stream-json --verbose de
	// claude. Aplicar ParseStreamMarker que encuentra el último evento
	// `result` y le aplica la regex.
	FormatStreamJSON OutputFormat = "stream-json"
)

// StopReason explica por qué el motor terminó con [stop]. Cada constructor
// de stop produce un Reason distinto para que la UX (logs, audit comment,
// notificación al humano) pueda diferenciar.
type StopReason string

const (
	// StopReasonAgentMarker: el agente emitió `[stop]` explícito. Razón
	// más común — el agente decidió parar (gate de seguridad, blocker
	// detectado, etc.).
	StopReasonAgentMarker StopReason = "agent emitted [stop]"

	// StopReasonTechnicalError: la invocación del agente falló (timeout,
	// exit != 0, crash). PRD §3.b paso 4: "Si la invocación falla → trata
	// el outcome como [stop] automático".
	StopReasonTechnicalError StopReason = "agent technical error"

	// StopReasonUnknownStep: `[goto: foo]` apuntó a un step que no
	// existe. PRD §3.c "Step destino inválido": "error explícito + [stop]
	// con razón 'step destino desconocido'".
	StopReasonUnknownStep StopReason = "goto target step does not exist"

	// StopReasonLoopCap: el contador alcanzó MaxTransitions. PRD §3.c
	// "Cap global de transiciones".
	StopReasonLoopCap StopReason = "loop cap exceeded"

	// StopReasonEmptyPipeline: el pipeline no tiene steps. Caso defensivo
	// — el loader (PR3) ya rechaza pipelines vacíos, pero el motor lo
	// vuelve a chequear porque acepta cualquier `Pipeline` construido en
	// memoria (incluyendo el `Default()` de PR2).
	StopReasonEmptyPipeline StopReason = "pipeline has no steps"

	// StopReasonNoAgents: el step actual no tiene agentes declarados.
	// Idem StopReasonEmptyPipeline: defensa frente a pipelines mal
	// armados que escaparon al validator.
	StopReasonNoAgents StopReason = "step has no agents"

	// StopReasonEntryStop: el entry agent emitió `[stop]` (PRD §5.a) —
	// rebote explícito del input, ningún step corre. Distinto de
	// StopReasonAgentMarker para que la UX (audit log, dash) pueda
	// diferenciar "el pipeline rebotó en el entry" de "un step paró el
	// pipeline en el medio".
	StopReasonEntryStop StopReason = "entry agent emitted [stop]"

	// StopReasonEntryNoAgents: defensa para EntrySpec con Agents vacío
	// — análogo a StopReasonNoAgents pero diferenciado para que el
	// caller sepa que el config del entry está mal, no el de un step.
	StopReasonEntryNoAgents StopReason = "entry has no agents"
)

// StepRun captura el outcome de la ejecución de un solo step durante una
// corrida. Se usa para el audit log (PR7) y para el resultado del comando
// `che pipeline simulate` (PR4) — pero ya en PR5b lo exponemos para que
// los tests del motor verifiquen la secuencia completa.
type StepRun struct {
	// Step es el nombre del step que corrió.
	Step string
	// Agent es el nombre del agente que produjo el marker resuelto en
	// modo single-agente. Cuando hay multi-agente (len(Step.Agents)>1)
	// queda vacío y el detalle vive en AgentResults.
	Agent string
	// Marker es el marker resuelto: el que emitió el agente, o el default
	// `[next]` cuando no hubo marker y la invocación fue exitosa, o
	// `[stop]` cuando hubo error técnico o goto inválido.
	Marker Marker
	// Resolved indica cómo se llegó a Marker:
	//   - "explicit"          el agente emitió el marker
	//   - "default-next"      no hubo marker pero la invocación fue OK
	//   - "technical-error"   la invocación falló
	//   - "unknown-step"      el goto apuntó a un step inexistente
	//   - "aggregator"        multi-agente, decidido por el aggregator
	Resolved string
	// Err captura el error técnico del invoker, si hubo. Sólo informativo
	// — la decisión de stop ya quedó reflejada en Marker.
	Err error
	// AgentResults trae el detalle por agente en modo multi-agente
	// (len(Step.Agents)>1). En modo single-agente queda nil — el caller
	// puede ignorarlo o leer Marker/Agent directamente.
	AgentResults []AgentResult
	// AggregatorReason explica cómo el aggregator llegó al Marker en modo
	// multi-agente. Vacío en modo single-agente.
	AggregatorReason string
}

// Run es el resultado completo de una corrida del motor.
type Run struct {
	// Entry, si no es nil, captura el outcome del entry agent (PRD §5.a).
	// Es nil cuando el pipeline no tenía Entry, o cuando el caller usó
	// `Options.EntryStep` (`--from`) para bypassear el entry. La
	// presencia de Entry no implica Stopped — el entry puede haber
	// emitido [next]/[goto: X] y el pipeline siguió corriendo.
	Entry *EntryRun

	// Steps es la secuencia de StepRun ejecutada, en orden cronológico.
	Steps []StepRun

	// Stopped indica si el motor terminó con [stop] (sea explícito,
	// técnico, unknown step o loop cap). Si es false, el pipeline
	// completó todos los steps (avance hasta el final + último step
	// emitió [next] o no emitió marker).
	Stopped bool

	// StopReason explica por qué el motor terminó. Vacío cuando Stopped
	// == false.
	StopReason StopReason

	// StopDetail trae info contextual del stop (ej. el step destino
	// desconocido en StopReasonUnknownStep). Vacío cuando no aporta.
	StopDetail string

	// Transitions es la cantidad de transiciones que tomó el motor (cap
	// en MaxTransitions). Útil para tests, audit log, y para exponer
	// "transiciones: 14/20" en la UI futura (limitación documentada en
	// PRD §9). El entry NO cuenta como transición — el cap protege
	// contra loops entre steps, no contra el entry mismo (que corre una
	// sola vez por corrida).
	Transitions int
}

// EntryRun captura el outcome del entry agent (PRD §5.a). Análogo a
// StepRun pero sin Step.Name (el entry no tiene name dentro del
// pipeline) y con un campo extra StartStep que dice qué step terminó
// arrancando el motor (vacío si emitió [stop]).
type EntryRun struct {
	// Agent es el nombre del agente que el motor invocó como entry.
	// PR5d corre Agents[0]; multi-agente vive en PR5c follow-up.
	Agent string

	// Marker es el marker resuelto del entry: el que emitió el agente, o
	// `[next]` por default (output sin marker + invocación OK), o
	// `[stop]` cuando hubo error técnico o goto inválido.
	Marker Marker

	// Resolved indica cómo se llegó a Marker — análogo a StepRun.Resolved:
	//   - "explicit"          el agente emitió el marker
	//   - "default-next"      no hubo marker pero la invocación fue OK
	//   - "technical-error"   la invocación falló
	//   - "unknown-step"      el goto del entry apuntó a un step inexistente
	//   - "no-agents"         EntrySpec con Agents vacío (defensa)
	Resolved string

	// StartStep es el nombre del step desde el que arrancó el motor
	// después del entry. Vacío si el entry resolvió a [stop] (no
	// arrancó nada). Si el entry emitió [next], es el primer step del
	// pipeline; si emitió [goto: X], es X.
	StartStep string

	// Err captura el error técnico del invoker, si hubo. Sólo informativo
	// — la decisión de stop ya quedó reflejada en Marker.
	Err error
}

// ErrInvokerNil se devuelve si el caller pasa un Invoker nil. El motor no
// tiene fallback razonable — sin Invoker, no puede invocar agentes.
var ErrInvokerNil = errors.New("engine: invoker is nil")

// Options configura una corrida del motor. Todos los campos son opcionales.
type Options struct {
	// EntryStep elige el primer step del pipeline a ejecutar. Si está
	// vacío, el motor corre el Entry (si existe) y arranca según su
	// marker; o arranca desde el primer step si no hay Entry.
	//
	// PR5d (este PR): EntryStep != "" BYPASSA el entry agent. Es la
	// implementación interna del flag CLI `che run --from <step>`
	// (PRD §5.c) — el override manual del usuario se respeta sin pasar
	// por el validador del entry. Esto es deliberado: si el usuario
	// pidió "arrancar desde validate_pr", el entry no debería poder
	// rebotarlo.
	EntryStep string

	// Input es el contexto inicial que se pasa al primer step (body del
	// issue, diff del PR, prompt libre). PR5b lo pasa sin transformar al
	// invoker; los PRs siguientes (PR5c+) decidirán cómo enriquecerlo
	// con outputs previos.
	Input string

	// BeforeStep corre justo antes de invocar los agentes de un step. Si
	// devuelve error, el motor corta como StopReasonTechnicalError sin invocar
	// el step. Callers lo usan para escribir che:state:applying:<step>.
	BeforeStep func(ctx context.Context, step string) error

	// AfterStepOK corre cuando un step terminó sin [stop] ni error técnico,
	// antes de avanzar al siguiente step. Callers lo usan para cerrar
	// che:state:applying:<step> -> che:state:<step>.
	//
	// No corre cuando el step emite [stop] ni cuando hay error técnico
	// (StopReasonTechnicalError): en ambos casos `che:state:applying:<step>`
	// queda colgado a propósito, como marca del último intento para el
	// humano que retoma. Un caller futuro que asuma "AfterStepOK = step
	// terminó" se va a confundir; por eso queda explícito acá.
	AfterStepOK func(ctx context.Context, step string) error
}

// RunPipeline ejecuta el pipeline secuencialmente:
//
//  1. Resuelve el step inicial:
//     a. Si Options.EntryStep != "" → ese step (bypassa el entry agent).
//     b. Si Pipeline.Entry != nil → invoca el entry agent y aplica su
//     marker (`[next]` → primer step, `[goto: X]` → step X,
//     `[stop]` → no corre nada y devuelve StopReasonEntryStop).
//     c. Sin lo anterior → primer step de la lista.
//  2. Para cada step: pickea el primer agente declarado, invoca via
//     Invoker, parsea el marker (text o stream-json según el format que
//     reportó el invoker), y aplica la transición.
//  3. Cuenta cada transición; al alcanzar MaxTransitions, stop con
//     StopReasonLoopCap. El entry NO cuenta como transición.
//  4. Cuando un step emite [next] (o default por output sin marker), el
//     motor avanza al siguiente step en orden. Si era el último, la
//     corrida termina ok.
//
// PR5b/PR5d sólo soportan 1 agente por step y por entry (el primero de
// la lista). El motor multi-agente + aggregator + cancelación parcial
// vive en PR5c — el shape del Step y EntrySpec ya lo soportan (Agents
// []string), así que cuando se implemente PR5c sólo cambia el branch
// que decide cómo invocar.
func RunPipeline(ctx context.Context, p Pipeline, inv Invoker, opts Options) (Run, error) {
	if inv == nil {
		return Run{}, ErrInvokerNil
	}
	if len(p.Steps) == 0 {
		return Run{
			Stopped:    true,
			StopReason: StopReasonEmptyPipeline,
		}, nil
	}

	// Build name → index una sola vez para validar `[goto: X]` en O(1)
	// y para resolver EntryStep. Ambigüedad de nombres duplicados es
	// responsabilidad del loader (PR3); el motor asume nombres únicos.
	nameToIdx := make(map[string]int, len(p.Steps))
	for i, s := range p.Steps {
		nameToIdx[s.Name] = i
	}

	run := Run{}
	currentInput := opts.Input

	currentIdx := 0
	switch {
	case opts.EntryStep != "":
		// PRD §5.c override manual: el flag `che run --from <step>`
		// bypassa el entry agent y arranca desde el step pedido.
		idx, ok := nameToIdx[opts.EntryStep]
		if !ok {
			return Run{
				Stopped:    true,
				StopReason: StopReasonUnknownStep,
				StopDetail: fmt.Sprintf("entry step %q not in pipeline", opts.EntryStep),
			}, nil
		}
		currentIdx = idx
	case p.Entry != nil:
		// PRD §5.a: el entry agent decide desde dónde arrancar (o si
		// rebotar el input). runEntry encapsula la invocación + el
		// parseo del marker; devuelve el step inicial o un Run pre-armado
		// con [stop] cuando corresponde.
		entryRun, startIdx, halt := runEntry(ctx, p, inv, currentInput, nameToIdx)
		run.Entry = entryRun
		if halt != nil {
			// Entry abortó la corrida (stop explícito, error técnico,
			// goto inválido o sin agentes). El Run que devolvemos ya
			// trae StopReason poblado por runEntry; le agregamos el
			// EntryRun para audit.
			halt.Entry = entryRun
			return *halt, nil
		}
		currentIdx = startIdx
	}

	for {
		if ctx != nil && ctx.Err() != nil {
			// Cancelación externa (señal, deadline del caller). Mapeamos a
			// stop técnico — el caller sabrá distinguir vía ctx.Err() si
			// quiere, y el motor mantiene la invariante "Run.Stopped == true
			// implica StopReason no vacío".
			run.Stopped = true
			run.StopReason = StopReasonTechnicalError
			run.StopDetail = ctx.Err().Error()
			return run, nil
		}

		if run.Transitions >= MaxTransitions {
			run.Stopped = true
			run.StopReason = StopReasonLoopCap
			run.StopDetail = fmt.Sprintf("reached cap of %d transitions", MaxTransitions)
			return run, nil
		}
		run.Transitions++

		step := p.Steps[currentIdx]
		if opts.BeforeStep != nil {
			if err := opts.BeforeStep(ctx, step.Name); err != nil {
				run.Steps = append(run.Steps, StepRun{
					Step:     step.Name,
					Marker:   Marker{Kind: MarkerStop},
					Resolved: "before-step-error",
					Err:      err,
				})
				run.Stopped = true
				run.StopReason = StopReasonTechnicalError
				run.StopDetail = fmt.Sprintf("before step %q: %v", step.Name, err)
				return run, nil
			}
		}
		if len(step.Agents) == 0 {
			// Defensa: pipeline mal armado escapó al validator. Stop con
			// razón explícita para que el caller corrija el JSON.
			run.Steps = append(run.Steps, StepRun{
				Step:     step.Name,
				Marker:   Marker{Kind: MarkerStop},
				Resolved: "no-agents",
			})
			run.Stopped = true
			run.StopReason = StopReasonNoAgents
			run.StopDetail = fmt.Sprintf("step %q has no agents declared", step.Name)
			return run, nil
		}

		// runStep maneja tanto el caso single-agent (len==1) como multi-
		// agent (len>1, paralelo + aggregator + cancelación parcial). En
		// single-agent no hay aggregator visible al caller — runStep
		// resuelve idénticamente al motor pre-PR5c.
		outcome := runStep(ctx, step, inv, currentInput)

		stepRun := StepRun{Step: step.Name}

		if len(step.Agents) == 1 {
			// Single-agent: preservamos el shape histórico (Agent + Resolved
			// "explicit"/"default-next"/"technical-error") para no romper
			// callers existentes ni los tests heredados.
			stepRun.Agent = step.Agents[0]
			if len(outcome.Results) == 1 {
				ar := outcome.Results[0]
				stepRun.Err = ar.Err
				switch {
				case ar.Err != nil:
					stepRun.Resolved = "technical-error"
				case ar.Marker.Kind == MarkerNone:
					stepRun.Resolved = "default-next"
				default:
					stepRun.Resolved = "explicit"
				}
			}
		} else {
			// Multi-agent: detalle por-agente + razón del aggregator.
			stepRun.AgentResults = outcome.Results
			stepRun.AggregatorReason = outcome.AggregatorReason
			stepRun.Resolved = "aggregator"
			// Propagar el primer error técnico (si lo hubo) para que los
			// callers que sólo miran StepRun.Err vean algo útil sin tener
			// que iterar AgentResults.
			if outcome.TechnicalError != nil {
				stepRun.Err = outcome.TechnicalError
			}
		}

		marker := outcome.Marker
		if marker.Kind == MarkerNone {
			marker = Marker{Kind: MarkerNext}
		}
		stepRun.Marker = marker

		// Caso "todos los agentes errorearon técnicamente y aggregator
		// resolvió [stop]": tratamos como StopReasonTechnicalError igual
		// que en el motor single-agente. Esto preserva el contrato
		// histórico (un agente que falla → stop técnico).
		if marker.Kind == MarkerStop && outcome.TechnicalError != nil {
			run.Steps = append(run.Steps, stepRun)
			run.Stopped = true
			run.StopReason = StopReasonTechnicalError
			if len(step.Agents) == 1 {
				run.StopDetail = fmt.Sprintf("agent %q in step %q: %v", step.Agents[0], step.Name, outcome.TechnicalError)
			} else {
				run.StopDetail = fmt.Sprintf("step %q: %s; first error: %v", step.Name, outcome.AggregatorReason, outcome.TechnicalError)
			}
			return run, nil
		}

		// Cancelación externa del ctx parent (señal, deadline). Si el
		// outcome devolvió [stop] con error de ctx, mapeamos a stop
		// técnico igual que el bloque inicial del loop.
		if marker.Kind == MarkerStop && ctx != nil && ctx.Err() != nil {
			run.Steps = append(run.Steps, stepRun)
			run.Stopped = true
			run.StopReason = StopReasonTechnicalError
			run.StopDetail = ctx.Err().Error()
			return run, nil
		}

		// Validación de step destino para [goto: X] — PRD §3.c "Step
		// destino inválido": convertir a stop con razón explícita.
		if marker.Kind == MarkerGoto {
			if _, ok := nameToIdx[marker.Goto]; !ok {
				stepRun.Marker = Marker{Kind: MarkerStop}
				stepRun.Resolved = "unknown-step"
				run.Steps = append(run.Steps, stepRun)
				run.Stopped = true
				run.StopReason = StopReasonUnknownStep
				run.StopDetail = fmt.Sprintf("step %q emitted [goto: %s] but no such step exists", step.Name, marker.Goto)
				return run, nil
			}
		}

		switch marker.Kind {
		case MarkerStop:
			run.Steps = append(run.Steps, stepRun)
			run.Stopped = true
			run.StopReason = StopReasonAgentMarker
			run.StopDetail = fmt.Sprintf("step %q emitted [stop]", step.Name)
			return run, nil
		case MarkerGoto:
			if opts.AfterStepOK != nil {
				if err := opts.AfterStepOK(ctx, step.Name); err != nil {
					stepRun.Marker = Marker{Kind: MarkerStop}
					stepRun.Resolved = "after-step-error"
					stepRun.Err = err
					run.Steps = append(run.Steps, stepRun)
					run.Stopped = true
					run.StopReason = StopReasonTechnicalError
					run.StopDetail = fmt.Sprintf("after step %q: %v", step.Name, err)
					return run, nil
				}
			}
			run.Steps = append(run.Steps, stepRun)
			currentIdx = nameToIdx[marker.Goto]
		case MarkerNext, MarkerNone:
			if opts.AfterStepOK != nil {
				if err := opts.AfterStepOK(ctx, step.Name); err != nil {
					stepRun.Marker = Marker{Kind: MarkerStop}
					stepRun.Resolved = "after-step-error"
					stepRun.Err = err
					run.Steps = append(run.Steps, stepRun)
					run.Stopped = true
					run.StopReason = StopReasonTechnicalError
					run.StopDetail = fmt.Sprintf("after step %q: %v", step.Name, err)
					return run, nil
				}
			}
			run.Steps = append(run.Steps, stepRun)
			// Avance natural. Si era el último step, terminamos OK.
			if currentIdx == len(p.Steps)-1 {
				return run, nil
			}
			currentIdx++
		}
	}
}

// runEntry invoca al entry agent del pipeline y resuelve el step inicial
// (PRD §5.a). Devuelve:
//
//   - entryRun: el outcome del entry, no-nil siempre que se haya intentado
//     correr (incluso si terminó en error técnico). El caller lo guarda
//     en Run.Entry para audit.
//   - startIdx: índice del step desde el que arrancar el motor cuando
//     halt == nil. Sólo válido en ese caso.
//   - halt: Run pre-armado con Stopped=true cuando el entry decidió no
//     correr ningún step (stop explícito, error técnico, goto inválido,
//     sin agentes). nil cuando el motor debe seguir.
//
// Multi-agente: PR5d invoca solamente Agents[0] (mirror del comportamiento
// PR5b para Steps). El campo EntrySpec.Aggregator se preserva en el
// shape para que el follow-up post-PR5c (que tiene Aggregator + runStep
// reusables) sólo cambie el branch que decide cómo invocar — sin tocar
// el contrato de runEntry.
func runEntry(ctx context.Context, p Pipeline, inv Invoker, input string, nameToIdx map[string]int) (*EntryRun, int, *Run) {
	entry := p.Entry
	er := &EntryRun{}

	if len(entry.Agents) == 0 {
		// Defensa: EntrySpec sin agentes (config inválido escapó al
		// validator). Halt con razón explícita para que el caller
		// corrija el JSON, igual que para steps sin agentes.
		er.Marker = Marker{Kind: MarkerStop}
		er.Resolved = "no-agents"
		return er, 0, &Run{
			Stopped:    true,
			StopReason: StopReasonEntryNoAgents,
			StopDetail: "entry has no agents declared",
		}
	}

	// PR5d: 1 agente por entry. PR5c follow-up traerá multi-agente +
	// aggregator (mirror del cambio que va a hacer en runStep).
	agent := entry.Agents[0]
	er.Agent = agent

	if ctx != nil && ctx.Err() != nil {
		// Cancelación temprana antes de invocar — propagar como stop
		// técnico, igual que el loop principal hace. El formato del
		// StopDetail ("context cancelled before <…> start: …") matchea
		// el de runStep para que callers que log/parsean el detail vean
		// shape consistente entre entry y steps.
		er.Marker = Marker{Kind: MarkerStop}
		er.Resolved = "technical-error"
		er.Err = ctx.Err()
		return er, 0, &Run{
			Stopped:    true,
			StopReason: StopReasonTechnicalError,
			StopDetail: "context cancelled before entry start: " + ctx.Err().Error(),
		}
	}

	output, format, invErr := inv.Invoke(ctx, agent, input)

	if invErr != nil {
		// PRD §3.b paso 4: error técnico → stop automático. Para el
		// entry esto significa "no podemos validar el input → no
		// arranquemos".
		er.Marker = Marker{Kind: MarkerStop}
		er.Resolved = "technical-error"
		er.Err = invErr
		return er, 0, &Run{
			Stopped:    true,
			StopReason: StopReasonTechnicalError,
			StopDetail: fmt.Sprintf("entry agent %q: %v", agent, invErr),
		}
	}

	// Parsear marker según format reportado por el invoker. Default a
	// FormatText cuando el invoker no especifica (zero value).
	var (
		marker Marker
		found  bool
	)
	switch format {
	case FormatStreamJSON:
		marker, found = ParseStreamMarker(output)
	default:
		marker, found = ParseMarker(output)
	}
	if !found {
		// PRD §3.b paso 5 + §5.a: sin marker → asume [next] = arrancar
		// desde el primer step. Mismo default que en steps.
		marker = Marker{Kind: MarkerNext}
		er.Resolved = "default-next"
	} else {
		er.Resolved = "explicit"
	}
	er.Marker = marker

	switch marker.Kind {
	case MarkerStop:
		// Rebote del input — el entry decidió que el pipeline no debe
		// correr. Reason DIFERENTE de StopReasonAgentMarker para que la
		// UX pueda diferenciar "rebotó en el entry" de "un step paró".
		return er, 0, &Run{
			Stopped:    true,
			StopReason: StopReasonEntryStop,
			StopDetail: fmt.Sprintf("entry agent %q emitted [stop]", agent),
		}
	case MarkerGoto:
		idx, ok := nameToIdx[marker.Goto]
		if !ok {
			// PRD §3.c "step destino inválido" — convertir a stop con
			// razón explícita. Igual que en steps; mantener consistencia
			// hace que el caller no tenga que distinguir entry vs step
			// para este modo de falla.
			er.Marker = Marker{Kind: MarkerStop}
			er.Resolved = "unknown-step"
			return er, 0, &Run{
				Stopped:    true,
				StopReason: StopReasonUnknownStep,
				StopDetail: fmt.Sprintf("entry emitted [goto: %s] but no such step exists", marker.Goto),
			}
		}
		er.StartStep = marker.Goto
		return er, idx, nil
	default:
		// MarkerNext o MarkerNone normalizado a Next: arrancar desde el
		// primer step (comportamiento default sin entry).
		er.StartStep = p.Steps[0].Name
		return er, 0, nil
	}
}
