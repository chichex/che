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
// Sólo necesitamos Name + Agents para invocar — el aggregator y demás
// metadata viven en el paquete `internal/pipeline` (PR2). Cuando PR2
// merguea, un follow-up adapta `engine.Run` para consumir
// `pipeline.Pipeline` directamente y este tipo desaparece.
type Step struct {
	// Name es identificador único dentro del pipeline. Los markers
	// `[goto: <name>]` lo referencian. El motor valida que el destino
	// exista y convierte un goto a step desconocido en [stop] con razón.
	Name string

	// Agents es la lista de refs a agentes (built-in o custom) que el
	// step puede correr. PR5b sólo invoca al primero — el motor
	// multi-agente + aggregator vive en PR5c. La lista se preserva
	// completa para que la UX (CLI/dash) muestre los candidatos al
	// usuario y para que PR5c la consuma sin cambiar el shape.
	Agents []string
}

// Pipeline es la secuencia ordenada de steps que el motor ejecuta. Sin
// metadata adicional en este nivel (Entry, Version, etc.) — esos campos
// son responsabilidad del loader (PR3) y del entry-runner (PR5d). El
// motor sólo necesita la lista para resolver `[goto: X]` y aplicar el
// cap.
type Pipeline struct {
	Steps []Step
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
)

// StepRun captura el outcome de la ejecución de un solo step durante una
// corrida. Se usa para el audit log (PR7) y para el resultado del comando
// `che pipeline simulate` (PR4) — pero ya en PR5b lo exponemos para que
// los tests del motor verifiquen la secuencia completa.
type StepRun struct {
	// Step es el nombre del step que corrió.
	Step string
	// Agent es el nombre del agente que el motor invocó. PR5b corre el
	// primero de la lista; PR5c (multi-agente) extenderá esto a una lista.
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
	Resolved string
	// Err captura el error técnico del invoker, si hubo. Sólo informativo
	// — la decisión de stop ya quedó reflejada en Marker.
	Err error
}

// Run es el resultado completo de una corrida del motor.
type Run struct {
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
	// PRD §9).
	Transitions int
}

// ErrInvokerNil se devuelve si el caller pasa un Invoker nil. El motor no
// tiene fallback razonable — sin Invoker, no puede invocar agentes.
var ErrInvokerNil = errors.New("engine: invoker is nil")

// Options configura una corrida del motor. Todos los campos son opcionales.
type Options struct {
	// EntryStep elige el primer step del pipeline a ejecutar. Si está
	// vacío, el motor arranca desde el primer step de la lista. PR5d
	// agregará el entry agent y el flag `--from`; PR5b ya soporta el
	// override interno para que los tests no tengan que reconstruir el
	// pipeline para empezar desde el medio.
	EntryStep string

	// Input es el contexto inicial que se pasa al primer step (body del
	// issue, diff del PR, prompt libre). PR5b lo pasa sin transformar al
	// invoker; los PRs siguientes (PR5c+) decidirán cómo enriquecerlo
	// con outputs previos.
	Input string
}

// RunPipeline ejecuta el pipeline secuencialmente:
//
//  1. Resuelve el primer step (Options.EntryStep o steps[0]).
//  2. Para cada step: pickea el primer agente declarado, invoca via
//     Invoker, parsea el marker (text o stream-json según el format que
//     reportó el invoker), y aplica la transición.
//  3. Cuenta cada transición; al alcanzar MaxTransitions, stop con
//     StopReasonLoopCap.
//  4. Cuando un step emite [next] (o default por output sin marker), el
//     motor avanza al siguiente step en orden. Si era el último, la
//     corrida termina ok.
//
// PR5b sólo soporta 1 agente por step (el primero de Step.Agents). El
// motor multi-agente + aggregator + cancelación parcial vive en PR5c —
// el shape del Step ya lo soporta (Agents []string), así que cuando se
// implemente PR5c sólo cambia el branch que decide cómo invocar.
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

	currentIdx := 0
	if opts.EntryStep != "" {
		idx, ok := nameToIdx[opts.EntryStep]
		if !ok {
			return Run{
				Stopped:    true,
				StopReason: StopReasonUnknownStep,
				StopDetail: fmt.Sprintf("entry step %q not in pipeline", opts.EntryStep),
			}, nil
		}
		currentIdx = idx
	}

	run := Run{}
	currentInput := opts.Input

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

		// PR5b: 1 agente por step. PR5c trae multi-agente + aggregator.
		agent := step.Agents[0]

		output, format, invErr := inv.Invoke(ctx, agent, currentInput)

		stepRun := StepRun{
			Step:  step.Name,
			Agent: agent,
		}

		if invErr != nil {
			// PRD §3.b paso 4: "Si la invocación falla (timeout, error de
			// red, exit code distinto de 0 del binario, crash) → trata el
			// outcome como [stop] automático".
			stepRun.Marker = Marker{Kind: MarkerStop}
			stepRun.Resolved = "technical-error"
			stepRun.Err = invErr
			run.Steps = append(run.Steps, stepRun)
			run.Stopped = true
			run.StopReason = StopReasonTechnicalError
			run.StopDetail = fmt.Sprintf("agent %q in step %q: %v", agent, step.Name, invErr)
			return run, nil
		}

		// Parsear marker según format. Default a FormatText cuando el
		// invoker no especifica (zero value).
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
			// PRD §3.b paso 5 + §3.c: "Si no hay marker → asume [next]".
			marker = Marker{Kind: MarkerNext}
			stepRun.Resolved = "default-next"
		} else {
			stepRun.Resolved = "explicit"
		}
		stepRun.Marker = marker

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

		run.Steps = append(run.Steps, stepRun)
		// Para PR5b el "input" de cada step se mantiene como el input
		// original. Cuando PR5c+ traiga el "context base" + outputs
		// previos, este es el lugar donde se enriquece. Documentar acá
		// para que el follow-up sepa qué reemplazar.
		_ = output

		switch marker.Kind {
		case MarkerStop:
			run.Stopped = true
			run.StopReason = StopReasonAgentMarker
			run.StopDetail = fmt.Sprintf("step %q emitted [stop]", step.Name)
			return run, nil
		case MarkerGoto:
			currentIdx = nameToIdx[marker.Goto]
		case MarkerNext, MarkerNone:
			// Avance natural. Si era el último step, terminamos OK.
			if currentIdx == len(p.Steps)-1 {
				return run, nil
			}
			currentIdx++
		}
	}
}
