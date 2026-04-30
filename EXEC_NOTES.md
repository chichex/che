# PR5b — Engine core: notas de ejecución

## Estado del PR

Cubre todo lo que el scope de #53 pide:

- **Invocación del agente** vía `engine.Invoker` (interface). 1 agente por step para PR5b — multi-agente + aggregator quedan para PR5c, pero `Step.Agents []string` ya tiene el shape multi.
- **Parser de markers** (`engine.ParseMarker` + `engine.ParseStreamMarker`). Regex case-sensitive del PRD §3.c, sólo última línea no vacía, soporte text + stream-json.
- **Distinción error técnico → stop**. `Invoker.Invoke` que devuelve `error != nil` mapea a `StopReasonTechnicalError`.
- **Default sin marker → next**. Output exitoso sin marker reconocido se trata como `[next]`.
- **Validación step destino**. `[goto: foo]` con `foo` no en pipeline → `StopReasonUnknownStep`.
- **Cap global de 20 transiciones**. `MaxTransitions = 20`, no configurable. Stop con `StopReasonLoopCap` al alcanzarlo.

Tests: 33 (16 engine + 17 marker). Toda la suite del repo (`go test ./...`) verde.

## Decisiones / desviaciones

### 1. Parser incluido en este PR (era PR5a)

El issue lista PR5a (spec formal del parser de markers) como dependencia, pero PR5a no estaba mergeado al momento de ejecutar #53. Como el scope explícito de PR5b también dice "parser" y el parser no es trivial de separar del engine sin duplicar tipos, lo incluí en `internal/engine/marker.go`. Si más adelante PR5a se mergea con un paquete separado (ej. `internal/markerparse`), un follow-up trivial reemplaza las llamadas internas y elimina `marker.go`.

### 2. Tipos `Pipeline`/`Step` definidos localmente (no `internal/pipeline`)

PR2 (`internal/pipeline`: types + Default) tampoco está en main todavía. Para que este PR sea self-contained y testeable sin esperar PR2, definí versiones minimales de `Pipeline` y `Step` dentro del paquete `engine`. Cuando PR2 merguea, un follow-up wirea `engine.RunPipeline` para consumir `pipeline.Pipeline` directamente — el shape es compatible (`Step{Name, Agents}` matchea el subset que el motor necesita).

### 3. `Invoker` como interface, no acoplado a `internal/agent` ni a PR1

PR1 (`internal/agentregistry`) no está en main. El motor define una `Invoker` interface chica (`Invoke(ctx, agent, input) (output, format, err)`) para no acoplarse al CLI de claude ni al registry. La implementación concreta que resuelve agente → binario via `agentregistry` y dispatcha al `internal/agent.Run` actual queda para un follow-up (probablemente cuando se haga el wireup CLI en PR4 o como parte del entry runner de PR5d).

### 4. `Options.EntryStep` ya soportado

El scope formal de PR5b no incluye el flag `--from` (eso va en PR5d), pero el motor expone `Options.EntryStep` para que los tests del motor puedan empezar desde el medio del pipeline sin reconstruir todo. PR5d sólo tiene que cablear ese campo desde el CLI — sin cambios al motor.

## Pendientes (intencionalmente fuera de scope)

- **Multi-agente + aggregator** — PR5c. El motor actual invoca `step.Agents[0]`.
- **Entry agent + flag `--from`** — PR5d. El motor soporta `EntryStep` interno; falta el CLI y la corrida del entry agent antes del primer step.
- **Cancelación parcial (SIGTERM + grace + SIGKILL)** — PR5c, junto con multi-agente.
- **Wiring real con `internal/agent.Run`** — follow-up cuando PR1 + PR2 estén en main. Por ahora el motor funciona con cualquier `Invoker`, lo cual es suficiente para el wireup del comando `che pipeline simulate` (PR4) que usa un dry-run invoker.
