# PR5c — Engine multi-agente + aggregator + cancelación parcial

## Estado del PR

Cubre todo lo que el scope de #57 pide:

- **`Aggregator` interface** con 3 implementations: `majorityAgg`, `unanimousAgg`, `firstBlockerAgg` (`internal/engine/aggregator.go`).
- **`AggregatorKind`** mirror de `internal/pipeline.Aggregator` (los presets `majority`/`unanimous`/`first_blocker`). El motor no importa `internal/pipeline` para mantener self-contained.
- **`runStep`** (`internal/engine/runstep.go`) invoca a todos los agentes en paralelo via goroutines, alimenta resultados al aggregator y cancela el resto apenas el aggregator decide. Hace `context.WithCancel` sobre el ctx del motor — propaga al child process via la cadena `Invoker.Invoke → internal/agent.Run`.
- **Cancelación parcial reportada como `Cancelled`, no como error** (PRD §3.d "loguear como cancelled by aggregator"). Los `AgentResult.Cancelled=true` no votan.
- **Default aggregator = `majority`** (PRD §3.d "Default: majority").
- **Step extendido** con `Aggregator AggregatorKind` opcional. `StepRun` extendido con `AgentResults []AgentResult` + `AggregatorReason string` para audit log.
- **Backwards compat single-agent**: si `len(Step.Agents) == 1`, `StepRun` mantiene el shape histórico (Agent + Resolved "explicit"/"default-next"/"technical-error"). Cuando `>1`, queda `Resolved="aggregator"`.
- **Errores técnicos en multi-agente**: si el aggregator decide `[stop]` con al menos 1 agente que erroreó, el motor mapea a `StopReasonTechnicalError` (no `StopReasonAgentMarker`), preservando el contrato del PR5b.

## Tests

**Aggregator (aislado)** — `internal/engine/aggregator_test.go`:
- majority: stop short-circuit, 2/3 next, 2 next + 1 stop = stop, empate goto/stop = stop, empate next/goto sin stop = stop (Finalize), goto mismo destino gana 2/3, default kind, error técnico = stop.
- unanimous: todos next, divergencia early cancel, goto mismo destino, goto destinos distintos = stop.
- first_blocker: primer stop, primer goto, todos next.
- helpers: kind desconocido = nil, MarkerNone == MarkerNext en keyOf.

**runStep (paralelismo + cancelación)** — `internal/engine/runstep_test.go`:
- single agente equivalente (backwards compat).
- 3 agentes en paralelo (verifica `maxConcurrent >= 2` con atomicos).
- first_blocker cancela resto via blockingInvoker (agentes B y C bloqueados, A devuelve [stop], B y C terminan con `Cancelled=true`).
- majority stop short-circuit (B y C nunca completan, sólo A).
- unanimous divergencia early cancel.
- aggregator default = majority.
- agente repetido = N instancias (3 calls al mismo nombre).
- todos error técnico → stop con TechnicalError propagado.
- ctx ya cancelado → stop inmediato sin invocar.

**Integración con `RunPipeline`**:
- `TestRunPipeline_StepMultiAgenteIntegracion`: pipeline con un step de 3 agentes, marker resuelto por aggregator, AgentResults llenos.
- `TestRunPipeline_StepMultiAgenteErrorTecnicoMapeaATechReason`: confirma `StopReasonTechnicalError` cuando el aggregator stoppea por error.

Toda la suite del repo (`go test ./...`) verde. `go test -race ./internal/engine/...` también verde (race en `internal/startup` es pre-existente, no tocada por este PR).

## Decisiones / desviaciones

### 1. Aggregator interface vs función pura

El PRD habla del aggregator como "política" pero no fija el shape. Elegí interface con `Feed` + `Finalize` (channel-style streaming) en vez de función `(N markers) → marker` porque:

- Permite cancelación temprana natural: `Feed` devuelve `Decided=true` cuando el aggregator ya tiene info suficiente, sin necesidad de esperar a los N agentes.
- `Finalize` cubre el caso "todos terminaron sin short-circuit" (ej. unanimous con 3 agentes que coinciden en `[next]`: el aggregator no decide hasta el último).

### 2. `AgentResult` preserva `MarkerNone`

`runStep` NO normaliza `MarkerNone → MarkerNext` antes de poner el resultado en `AgentResult.Marker`. La normalización vive en el aggregator (`effectiveMarker` helper). Esto preserva el contrato single-agent del motor — los tests heredados del PR5b distinguen `Resolved="default-next"` (no había marker) de `Resolved="explicit"` (agente emitió `[next]`).

### 3. Race fix en `fakeInvoker` (test fixture)

El `fakeInvoker` heredado del PR5b no era thread-safe (map de calls sin lock). Como ahora el motor invoca múltiples agentes en goroutines paralelas, agregué un `sync.Mutex` al fixture. No es un cambio de producción — solo del test helper.

### 4. `AggregatorKind` mirror de `internal/pipeline.Aggregator`

PR2 (`internal/pipeline`) define `Aggregator` como enum string. El motor define `AggregatorKind` con los mismos valores en lugar de importar el paquete, manteniendo self-contained la dependencia. Cuando el wireup CLI (PR4/PR5d) consuma `pipeline.Step`, hará la conversión `pipeline.Aggregator → engine.AggregatorKind` (literal el mismo string).

### 5. Cancelación de agentes "in-flight"

Si un agente termina DESPUÉS de que el aggregator decidió (race entre `cancel()` y la finalización del Invoke), su resultado llega a `resultsCh` pero `runStep` lo agrega a `Results` sin alimentarlo al aggregator. Si tenía error de ctx, lo marca `Cancelled=true`; si tenía error técnico no-ctx, queda `Err` (raro: no debería pasar si el invoker respeta ctx). Esto preserva la garantía "todas las goroutines terminan antes del return" sin leakear y sin alterar la decisión.

## Pendientes (intencionalmente fuera de scope)

- **Wiring real con `internal/agent.Run`** — el motor sigue dependiendo de la `Invoker` interface. La implementación concreta que dispatcha a claude (vía `internal/agent.Run` con `KillGrace`) viene cuando se cablee el comando `che pipeline run` (PR4 / PR5d). El `runStep` ya está listo para esa Invoker — el `KillGrace` que pide el PRD §3.d (SIGTERM + 5s grace + SIGKILL) ya está implementado en `internal/agent.Run` desde antes del PRD #50, y el ctx propagado le indica cuándo matar.
- **Entry agent + flag `--from`** — PR5d.
- **Adaptación de `engine.Step` → `pipeline.Step`** — follow-up cuando el wireup CLI lo necesite. Hoy son tipos paralelos compatibles en shape.
