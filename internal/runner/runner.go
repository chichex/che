// Package runner orquesta `che run`: traduce el modelo declarativo de
// `internal/pipeline` al modelo de invocación de `internal/engine`,
// aplica el modo (auto-loop vs manual) y rendea el header del PRD
// §3.e ("[step: <name> · mode: <auto-loop|manual> · agents: <lista>]")
// antes de cada step.
//
// PR9a (este paquete) sólo cubre la parte de "modo de ejecución":
//
//   - Auto-loop: corre todos los agentes declarados en cada step.
//   - Manual: deja que el usuario seleccione un subset por step
//     (default: todos preseleccionados).
//
// La detección de TTY vs no-TTY vive en el caller (cmd/run.go) — el
// paquete acepta un Selector arbitrario para que los tests puedan
// inyectar uno determinístico sin spawnear bubbletea.
//
// Multi-agente real con aggregator es PR5c — PR9a sigue invocando un
// solo agente por step (el primero del subset elegido) porque el motor
// (PR5b) todavía no soporta multi-agente. Cuando PR5c land, este
// paquete sólo necesita pasar la lista completa al motor en vez de
// truncar.
//
// Decisión: el runner NO usa engine.RunPipeline directamente. Replica
// el loop chico (orden de steps + parser de marker + cap) para poder
// emitir el header del PRD §3.e ANTES de cada invocación,
// distinguiendo correctamente el step actual incluso cuando un goto
// reentra a uno ya visitado. Cuando PR5c agregue hooks por-step al
// motor, este loop se reemplaza por engine.RunPipeline + hook.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/chichex/che/internal/engine"
	"github.com/chichex/che/internal/pipeline"
)

// Mode identifica cómo che ejecutó el pipeline. Aparece en el header del
// PRD §3.e — los strings son contrato visible al usuario y a los
// scrapers de logs.
type Mode string

const (
	// ModeAuto = corre todos los agentes declarados (default cuando
	// stdin no es TTY: dash, CI, scripts).
	ModeAuto Mode = "auto-loop"

	// ModeManual = el usuario eligió un subset interactivamente. Sólo
	// posible cuando stdin es TTY (TUI); los flags de override en CLI
	// también caen acá porque el usuario eligió el subset
	// explícitamente.
	ModeManual Mode = "manual"
)

// Selector decide qué subset de agentes correr para un step dado.
//
//   - stepName: nombre del step canónico del pipeline (no path).
//   - agents:   lista canónica declarada en el .json del pipeline,
//     en el orden original (preservar el orden importa para que la UX
//     muestre los agentes en el mismo orden que el JSON).
//
// Devuelve la lista de agentes a correr (subset, en cualquier orden) y
// un eventual error. El runner valida que el subset:
//   - sea no-vacío (al menos 1 agente: si querés saltar el step,
//     editá el pipeline);
//   - cada elemento esté en `agents` (sin agentes de fantasía);
//   - sin duplicados.
//
// Cancelación: si el usuario aborta el selector (Esc/Ctrl+C en TTY),
// devolver ErrSelectionCancelled. El caller lo trata como salida limpia
// (exit 0, mensaje informativo) — distinto a un error técnico.
type Selector func(stepName string, agents []string) ([]string, error)

// ErrSelectionCancelled lo devuelve un Selector cuando el usuario
// cancela el wizard. El runner lo propaga sin tratarlo como error
// técnico para que cmd/run.go pueda mapear a exit code dedicado.
var ErrSelectionCancelled = errors.New("selección de agentes cancelada por el usuario")

// AutoSelector es el Selector default para no-TTY (dash, CI). Devuelve
// la lista completa sin pedir input. Equivalente a "modo auto-loop".
func AutoSelector(stepName string, agents []string) ([]string, error) {
	// Copia defensiva: el caller podría mutar el slice y eso no debe
	// afectar al pipeline original.
	out := make([]string, len(agents))
	copy(out, agents)
	return out, nil
}

// Selections mapea step name → subset de agentes seleccionado para ese
// step. Steps no presentes en el mapa se interpretan como "ningún
// override": el motor corre la lista canónica del pipeline.
//
// Se popula con ResolveSelections antes de armar el engine.Pipeline.
type Selections map[string][]string

// ResolveSelections recorre los steps del pipeline en orden y aplica el
// Selector a cada uno.
//
// Reglas:
//   - Si un step no tiene agentes (caso defensivo: el validator del
//     loader ya lo bloquea), se preserva tal cual — el motor lo
//     transforma en StopReasonNoAgents.
//   - Si el Selector devuelve un subset vacío o con agentes inválidos,
//     se aborta la resolución completa con error puntual (path:
//     step name).
//   - Si el Selector devuelve ErrSelectionCancelled en cualquier step,
//     se propaga inmediatamente (no se preguntan los siguientes).
func ResolveSelections(p pipeline.Pipeline, sel Selector) (Selections, error) {
	if sel == nil {
		// Sin selector explícito → auto. Conveniente para callers que
		// sólo quieren transformar pipeline → engine sin ramas.
		sel = AutoSelector
	}
	out := make(Selections, len(p.Steps))
	for _, step := range p.Steps {
		// Pasamos una copia para que un selector mal escrito no pueda
		// mutar el pipeline original.
		canonical := append([]string(nil), step.Agents...)

		subset, err := sel(step.Name, canonical)
		if err != nil {
			return nil, err
		}
		if err := validateSubset(step.Name, canonical, subset); err != nil {
			return nil, err
		}
		out[step.Name] = subset
	}
	return out, nil
}

// validateSubset chequea que la selección del usuario sea coherente con
// los agentes declarados en el step.
func validateSubset(stepName string, canonical, subset []string) error {
	if len(canonical) == 0 {
		// Step sin agentes: aceptamos cualquier subset (incluso vacío)
		// para que el caller no tenga que ramificar. El motor frena
		// igual con StopReasonNoAgents.
		return nil
	}
	if len(subset) == 0 {
		return fmt.Errorf("step %q: subset vacío (al menos 1 agente requerido — para saltar el step, editá el pipeline)", stepName)
	}
	known := make(map[string]bool, len(canonical))
	for _, a := range canonical {
		known[a] = true
	}
	seen := make(map[string]bool, len(subset))
	for _, a := range subset {
		if !known[a] {
			return fmt.Errorf("step %q: agente %q no declarado (válidos: %s)", stepName, a, strings.Join(canonical, ", "))
		}
		if seen[a] {
			return fmt.Errorf("step %q: agente %q duplicado en la selección", stepName, a)
		}
		seen[a] = true
	}
	return nil
}

// BuildEnginePipeline traduce un pipeline declarativo al modelo del
// motor, aplicando las selecciones por step. Steps sin override en
// `sels` mantienen su lista canónica.
//
// El orden de los agentes en cada step se preserva del JSON (no del
// mapa de selección) — el motor PR5b corre el primero, así que
// preservar el orden canónico evita sorpresas para el usuario que
// editó el .json eligiendo agentes en orden de prioridad.
func BuildEnginePipeline(p pipeline.Pipeline, sels Selections) engine.Pipeline {
	steps := make([]engine.Step, 0, len(p.Steps))
	for _, s := range p.Steps {
		agents := append([]string(nil), s.Agents...)
		if sels != nil {
			if subset, ok := sels[s.Name]; ok {
				agents = filterPreservingOrder(s.Agents, subset)
			}
		}
		steps = append(steps, engine.Step{
			Name:   s.Name,
			Agents: agents,
		})
	}
	return engine.Pipeline{Steps: steps}
}

// filterPreservingOrder devuelve los elementos de `canonical` que están
// en `subset`, en el orden de canonical. Útil para que la lista del
// motor conserve la prioridad del JSON aunque el wizard haya devuelto
// el subset en otro orden (caso típico: el usuario tildó la box 3 y
// luego la 1).
func filterPreservingOrder(canonical, subset []string) []string {
	keep := make(map[string]bool, len(subset))
	for _, a := range subset {
		keep[a] = true
	}
	out := make([]string, 0, len(subset))
	for _, a := range canonical {
		if keep[a] {
			out = append(out, a)
		}
	}
	return out
}

// FormatHeader rendea el header del PRD §3.e que se imprime al inicio
// de cada step:
//
//	[step: explore · mode: auto-loop · agents: claude-opus]
//	[step: validate_pr · mode: manual · agents: code-reviewer-strict, claude-opus]
//
// Sin ANSI (los hooks de logging deciden colorearlo) y con separador
// `·` U+00B7 — mismo separador que usa el resto de la UI textual de
// che para que el output sea grep-able y consistente.
func FormatHeader(stepName string, mode Mode, agents []string) string {
	list := strings.Join(agents, ", ")
	if list == "" {
		list = "<none>"
	}
	return fmt.Sprintf("[step: %s · mode: %s · agents: %s]", stepName, mode, list)
}

// RunOptions configura una corrida de Run. Mantenemos el shape
// compacto: la mayoría de la configuración del motor (entry step,
// input inicial) se deriva del pipeline + flags del CLI.
type RunOptions struct {
	// EntryStep override del primer step. Vacío = primer step del
	// pipeline. Mismo contrato que engine.Options.EntryStep.
	EntryStep string

	// Input es el contexto inicial para el primer step (body del
	// issue, prompt libre). Pasthrough a engine.Options.
	Input string

	// Mode estampado en el header. El runner no lo deriva
	// automáticamente — el caller decide (en general, ModeManual si
	// hubo wizard, ModeAuto si fue AutoSelector / no-TTY / flag de
	// override determinístico).
	Mode Mode

	// HeaderOut es el writer donde se imprime el header del PRD §3.e
	// antes de cada step. Si es nil, no se imprime header (modo "raw"
	// para tests que sólo quieren observar el comportamiento del
	// motor sin assertar layout).
	HeaderOut io.Writer
}

// Run resuelve las selecciones, construye la engine.Pipeline filtrada e
// imprime el header al entrar a cada step antes de invocar al agente.
//
// Replica el loop del motor (engine.RunPipeline) pero con un hook
// explícito por-step para el header. La política de markers, default
// `[next]`, error técnico → stop, goto inválido → stop, cap de
// transiciones, todo se mantiene idéntico (los símbolos vienen del
// paquete engine para no duplicar lógica).
//
// Si Selector devuelve ErrSelectionCancelled, Run devuelve un
// engine.Run vacío + el error (sin invocar al motor). El caller mapea
// a exit code dedicado.
func Run(ctx context.Context, p pipeline.Pipeline, inv engine.Invoker, sel Selector, opts RunOptions) (engine.Run, error) {
	if inv == nil {
		return engine.Run{}, engine.ErrInvokerNil
	}

	sels, err := ResolveSelections(p, sel)
	if err != nil {
		return engine.Run{}, err
	}

	enginePipe := BuildEnginePipeline(p, sels)

	if len(enginePipe.Steps) == 0 {
		return engine.Run{
			Stopped:    true,
			StopReason: engine.StopReasonEmptyPipeline,
		}, nil
	}

	nameToIdx := make(map[string]int, len(enginePipe.Steps))
	for i, s := range enginePipe.Steps {
		nameToIdx[s.Name] = i
	}

	currentIdx := 0
	if opts.EntryStep != "" {
		idx, ok := nameToIdx[opts.EntryStep]
		if !ok {
			return engine.Run{
				Stopped:    true,
				StopReason: engine.StopReasonUnknownStep,
				StopDetail: fmt.Sprintf("entry step %q not in pipeline", opts.EntryStep),
			}, nil
		}
		currentIdx = idx
	}

	run := engine.Run{}
	currentInput := opts.Input

	for {
		if ctx != nil && ctx.Err() != nil {
			run.Stopped = true
			run.StopReason = engine.StopReasonTechnicalError
			run.StopDetail = ctx.Err().Error()
			return run, nil
		}

		if run.Transitions >= engine.MaxTransitions {
			run.Stopped = true
			run.StopReason = engine.StopReasonLoopCap
			run.StopDetail = fmt.Sprintf("reached cap of %d transitions", engine.MaxTransitions)
			return run, nil
		}
		run.Transitions++

		step := enginePipe.Steps[currentIdx]

		// Header del PRD §3.e: se imprime ANTES de invocar (incluso
		// si el step no tiene agentes — ayuda a entender qué step
		// disparó el StopReasonNoAgents).
		if opts.HeaderOut != nil {
			fmt.Fprintln(opts.HeaderOut, FormatHeader(step.Name, opts.Mode, step.Agents))
		}

		if len(step.Agents) == 0 {
			run.Steps = append(run.Steps, engine.StepRun{
				Step:     step.Name,
				Marker:   engine.Marker{Kind: engine.MarkerStop},
				Resolved: "no-agents",
			})
			run.Stopped = true
			run.StopReason = engine.StopReasonNoAgents
			run.StopDetail = fmt.Sprintf("step %q has no agents declared", step.Name)
			return run, nil
		}

		// PR5b: 1 agente por step. PR5c trae multi-agente + aggregator.
		agent := step.Agents[0]

		output, format, invErr := inv.Invoke(ctx, agent, currentInput)

		stepRun := engine.StepRun{
			Step:  step.Name,
			Agent: agent,
		}

		if invErr != nil {
			stepRun.Marker = engine.Marker{Kind: engine.MarkerStop}
			stepRun.Resolved = "technical-error"
			stepRun.Err = invErr
			run.Steps = append(run.Steps, stepRun)
			run.Stopped = true
			run.StopReason = engine.StopReasonTechnicalError
			run.StopDetail = fmt.Sprintf("agent %q in step %q: %v", agent, step.Name, invErr)
			return run, nil
		}

		var (
			marker engine.Marker
			found  bool
		)
		switch format {
		case engine.FormatStreamJSON:
			marker, found = engine.ParseStreamMarker(output)
		default:
			marker, found = engine.ParseMarker(output)
		}

		if !found {
			marker = engine.Marker{Kind: engine.MarkerNext}
			stepRun.Resolved = "default-next"
		} else {
			stepRun.Resolved = "explicit"
		}
		stepRun.Marker = marker

		if marker.Kind == engine.MarkerGoto {
			if _, ok := nameToIdx[marker.Goto]; !ok {
				stepRun.Marker = engine.Marker{Kind: engine.MarkerStop}
				stepRun.Resolved = "unknown-step"
				run.Steps = append(run.Steps, stepRun)
				run.Stopped = true
				run.StopReason = engine.StopReasonUnknownStep
				run.StopDetail = fmt.Sprintf("step %q emitted [goto: %s] but no such step exists", step.Name, marker.Goto)
				return run, nil
			}
		}

		run.Steps = append(run.Steps, stepRun)
		_ = output

		switch marker.Kind {
		case engine.MarkerStop:
			run.Stopped = true
			run.StopReason = engine.StopReasonAgentMarker
			run.StopDetail = fmt.Sprintf("step %q emitted [stop]", step.Name)
			return run, nil
		case engine.MarkerGoto:
			currentIdx = nameToIdx[marker.Goto]
		case engine.MarkerNext, engine.MarkerNone:
			if currentIdx == len(enginePipe.Steps)-1 {
				return run, nil
			}
			currentIdx++
		}
	}
}
