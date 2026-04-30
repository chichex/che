package engine

import (
	"context"
	"sync"
)

// stepOutcome es el resultado consolidado de la ejecución de UN step
// (potencialmente con N agentes en paralelo + aggregator).
type stepOutcome struct {
	// Marker es el marker final del step, ya resuelto por el aggregator.
	// Cuando hay errores técnicos en TODOS los agentes y el aggregator
	// resuelve [stop], TechnicalError es true para que el motor pueda
	// marcar el StopReason adecuado.
	Marker Marker

	// AggregatorReason explica cómo el aggregator llegó al Marker (texto
	// libre, sólo para audit / logs).
	AggregatorReason string

	// Results es la lista de outcomes por agente, en el orden en que
	// terminaron (no en el orden de Step.Agents). Incluye los cancelados
	// por el aggregator (Cancelled=true) para el audit log.
	Results []AgentResult

	// TechnicalError captura el primer error técnico que el aggregator usó
	// para decidir [stop]. Si el step resuelve [stop] por error técnico,
	// el motor mapea esto a StopReasonTechnicalError.
	TechnicalError error
}

// runStep invoca a todos los agentes del step en paralelo, alimenta sus
// markers al aggregator, y cancela los pendientes apenas el aggregator
// puede determinar el resultado (cancelación parcial — PRD §3.d).
//
// Garantías:
//  1. Todas las goroutines terminan antes de que runStep retorne. La
//     cancelación se hace via context.WithCancel propagado a Invoker.Invoke
//     — el invoker (vía `internal/agent.Run`) ya soporta SIGTERM + grace +
//     SIGKILL.
//  2. Los agentes cancelados se reportan con Cancelled=true en
//     stepOutcome.Results (NO como error). Esto cumple el requisito de
//     "loguear como cancelled by aggregator, no como error".
//  3. Si el aggregator nunca decide en Feed (ej. todos terminaron sin
//     short-circuit), runStep llama Finalize con los resultados acumulados.
//  4. Si el ctx parent está cancelado al entrar, runStep retorna [stop]
//     inmediatamente sin invocar agentes.
func runStep(ctx context.Context, step Step, inv Invoker, input string) stepOutcome {
	if ctx.Err() != nil {
		return stepOutcome{
			Marker:           Marker{Kind: MarkerStop},
			AggregatorReason: "context cancelled before step start: " + ctx.Err().Error(),
			TechnicalError:   ctx.Err(),
		}
	}

	agents := step.Agents
	n := len(agents)

	// stepCtx propaga la cancelación al invoker: cuando el aggregator
	// decide, llamamos cancel() y los Invoke pendientes deberían ver el
	// ctx cancelado y matar el child process. Mantiene el árbol parent →
	// step → invoker de cancelación.
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type rawResult struct {
		agent  string
		idx    int // posición en step.Agents (para audit log estable)
		output string
		format OutputFormat
		err    error
	}
	resultsCh := make(chan rawResult, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i, a := range agents {
		i, a := i, a
		go func() {
			defer wg.Done()
			out, fmt2, err := inv.Invoke(stepCtx, a, input)
			resultsCh <- rawResult{agent: a, idx: i, output: out, format: fmt2, err: err}
		}()
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	aggKind := step.Aggregator
	if aggKind == "" {
		aggKind = AggMajority
	}
	agg := NewAggregator(aggKind, n)

	var (
		decided        bool
		decidedOutcome AggregatorOutcome
		results        []AgentResult
		// firstTechErr captura el primer error técnico (NO de cancelación).
		// Sólo lo usamos para mapear a StopReasonTechnicalError cuando el
		// aggregator resuelve [stop] basado en ese error.
		firstTechErr error
	)

	// Loop principal: drainamos resultsCh; por cada resultado, parseamos
	// marker (a menos que sea cancelado), alimentamos al aggregator, y si
	// el aggregator decide cancelamos el resto y seguimos drenando hasta
	// que se cierre el channel (para no leakear goroutines).
	for r := range resultsCh {
		ar := AgentResult{Agent: r.agent}

		// Si el ctx step ya fue cancelado, marcamos como cancelled.
		// Distinguir cancelado-por-aggregator de error-real es importante:
		// el aggregator ignora cancelados (no votan), y el audit log no los
		// reporta como error.
		if r.err != nil && stepCtx.Err() != nil && decided {
			ar.Cancelled = true
			results = append(results, ar)
			continue
		}

		if r.err != nil {
			// Error técnico real (no cancelación-por-aggregator). El parent
			// ctx puede haberse cancelado externamente — en ese caso lo
			// trataremos como error técnico también (consistente con el
			// motor single-agente, donde ctx.Err() externo gatilla
			// StopReasonTechnicalError).
			ar.Err = r.err
			ar.Marker = Marker{Kind: MarkerStop}
			if firstTechErr == nil {
				firstTechErr = r.err
			}
		} else {
			// El parser devuelve (Marker{MarkerNone}, false) cuando no
			// hubo marker. PRESERVAMOS MarkerNone en AgentResult para
			// que el caller pueda distinguir "default-next" (no había
			// marker) de "explicit next" (el agente lo emitió). El
			// aggregator normaliza vía effectiveMarker — para él los
			// dos son lo mismo a efectos de voto.
			switch r.format {
			case FormatStreamJSON:
				ar.Marker, _ = ParseStreamMarker(r.output)
			default:
				ar.Marker, _ = ParseMarker(r.output)
			}
		}

		results = append(results, ar)

		if decided {
			// Aggregator ya decidió. Drenamos sin alimentar ni cancelar
			// (cancel ya fue llamado al decidir). Estos resultados llegaron
			// en flight — los reportamos pero no afectan la decisión.
			continue
		}

		outcome := agg.Feed(ar)
		if outcome.Decided {
			decided = true
			decidedOutcome = outcome
			cancel() // cortar el resto.
		}
	}

	if !decided {
		// Todos los agentes terminaron y el aggregator nunca short-circuiteó.
		// Forzar decisión sobre el set completo.
		decidedOutcome = agg.Finalize()
	}

	out := stepOutcome{
		Marker:           decidedOutcome.Marker,
		AggregatorReason: decidedOutcome.Reason,
		Results:          results,
	}

	// Si el aggregator decidió [stop] Y la causa fue al menos un error
	// técnico, propagamos el error al outcome para que el motor pueda
	// distinguir StopReasonAgentMarker de StopReasonTechnicalError.
	if decidedOutcome.Marker.Kind == MarkerStop && firstTechErr != nil {
		out.TechnicalError = firstTechErr
	}

	return out
}
