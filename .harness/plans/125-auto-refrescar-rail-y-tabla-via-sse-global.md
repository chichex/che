# Plan: Auto-refrescar rail y tabla de runs de che dash via SSE global

Refs #125 · https://github.com/chichex/che/issues/125

## Contexto

Spec 4 del dashboard. Cierra el set de "che dash read-only" con auto-refresh: el rail y la tabla de runs se actualizan sin reload cuando suceden cambios en disco (pipelines nuevos/eliminados/flip ready↔draft, runs que arrancan o terminan). Builds sobre `hs-plan/123` (PR #124, spec 3). Sigue la decisión arquitectural de spec 3: bus + watchers viven en `internal/dash/`, no patch al runner.

## Objetivo

Servir `GET /api/events` como SSE global emitiendo `pipeline:changed`, `run:started`, `run:finished` + heartbeat 15s, sin replay (frontend ya hace fetch one-shot al cargar). Materializar el campo `last_run` opcional en `/api/pipelines` (spec 1 lo había marcado como opcional). Wirear el frontend para abrir EventSource global al mount, dedupar eventos, hacer insert/update/remove en rail y tabla, y mostrar badge "live" en el header del dash.

## Approach

Agregar dos watchers nuevos en `internal/dash/`:
- `internal/dash/pipelines_watcher.go`: observa `~/.che/pipelines/` con polling 250ms. Mantiene snapshot del listing (slug → mtime + status). Detecta create / status flip / delete y emite eventos al bus global.
- `internal/dash/runs_watcher.go` (global): observa `~/.che/runs/` recursivamente con polling 250ms. Mantiene snapshot por (slug, runId) del manifest status. Detecta nuevos manifests (`run:started`) y transitions a terminal (`run:finished`).

Reusar el `Bus` de spec 3 con una segunda categoría de suscripción: además de `Subscribe(runId)`, agregar `SubscribeGlobal()` que recibe todos los eventos globales (pipeline + run). El bus expone ambas APIs sobre la misma infraestructura.

Handler nuevo: `internal/dash/sse_global.go` con `handleGlobalEvents(bus)`. Headers SSE iguales a spec 3. Sin snapshot inicial — solo tail desde la suscripción.

Endpoint `/api/pipelines` (spec 1): extender la struct JSON para incluir `last_run` opcional. Resolver el último run de cada slug leyendo `~/.che/runs/<slug>/*/manifest.yaml` ordenado por started_at desc, tomar el primero.

Frontend: nuevo `useEffect` en App que abre `EventSource("/api/events")` al mount; handlers per tipo que llaman a setters de state. Badge "live" en el header (separado del badge per-run de spec 3). Cuando un evento llega para un run o pipeline que no es visible, igual se procesa silenciosamente (e.g., last_run del rail).

## Pasos

- [ ] Extender `internal/dash/bus.go` con `Bus.SubscribeGlobal() (<-chan Event, cancel)`. Eventos publicados sin filtro de runId van al canal global. Mantener buffer 256.
- [ ] Crear `internal/dash/pipelines_watcher.go`: snapshot del listing `*.yaml` con `mtime + status` por slug. Tick 250ms con `os.ReadDir` + `os.Stat` + parse mínimo del status. Emit `pipeline:changed` con `{slug, status, deleted?}` por cada diff detectado.
- [ ] Crear `internal/dash/runs_watcher.go`: snapshot por (slug, runId) del manifest `status`. Tick 250ms recorriendo `~/.che/runs/*/*/manifest.yaml`. Emit `run:started` cuando aparece nuevo con `status=running`, `run:finished` cuando un manifest existente transitions a terminal.
- [ ] Watchers globales arrancan al boot del `Serve()` (siempre activos, no ref-counted como los per-run de spec 3). Cleanup al shutdown.
- [ ] Crear `internal/dash/sse_global.go`: `handleGlobalEvents(bus)`. Subscribe via `bus.SubscribeGlobal()`. Headers SSE. Heartbeat 15s. Cleanup en `r.Context().Done()`. Sin snapshot inicial.
- [ ] Routing: registrar `mux.HandleFunc("/api/events", handleGlobalEvents(bus))` en `Serve()`.
- [ ] Modificar `internal/dash/pipelines.go`: agregar campo `LastRun *LastRunSummary` con `omitempty` al struct de listing JSON. Función helper `lookupLastRun(slug)` que lee el run más reciente.
- [ ] Frontend HTML — EventSource global:
  - useEffect en App al mount: `new EventSource("/api/events")`. Cleanup al unmount.
  - `addEventListener("pipeline:changed", ...)`: si `deleted=true` quitar del state; si slug nuevo insertar con fade-in 500ms; si status flip, mover bucket.
  - `addEventListener("run:started", ...)`: si slug coincide con `selectedSlug`, insertar al tope de `runs` con fade-in 500ms. Si no, update silencioso del `last_run` del pipeline en el rail.
  - `addEventListener("run:finished", ...)`: dedup por `(run_id, status)`. Si run en `runs` visible, update inline status/duración. Update `last_run` del slug en el rail.
- [ ] Frontend HTML — pipeline seleccionado deleted:
  - Si `pipeline:changed` con `deleted=true` y `slug == selectedSlug`: limpiar `selectedSlug` y mostrar empty state literal `este pipeline fue eliminado — seleccioná otro del rail`.
- [ ] Frontend HTML — badge global "live":
  - State `globalConnState` (`live` | `reconnecting` | `disconnected` | `hidden`). Pulsing dot en header del dash. Timer 10s para `disconnected` después de `onerror`.
  - Botón retry crea nuevo EventSource.
- [ ] Frontend HTML — reconnect refetch:
  - En `onopen` después de un `onerror`: refetch `GET /api/pipelines`; si hay detail abierto, refetch `GET /api/pipelines/:slug/runs`.
- [ ] Frontend HTML — render `last_run` en el rail:
  - Cada item del rail con `last_run` presente muestra mini chip de status (mismo componente de chip ya usado en tabla).
- [ ] Frontend HTML — fade-in animations:
  - CSS class `dash-fade-in` con `@keyframes` 500ms ease-out, opacity 0→1.
- [ ] Tests:
  - `pipelines_watcher_test.go`: crear archivos en `t.TempDir()` (status ready/draft), tick, verify eventos.
  - `runs_watcher_test.go`: crear manifests en `t.TempDir()`, tick, verify `run:started`/`run:finished`.
  - `sse_global_test.go`: subscribe via mock bus, publish global event, verify SSE chunk emitido. Heartbeat tras silencio.
  - `bus_test.go` extender: `SubscribeGlobal` recibe eventos globales, no recibe events filtrados por runId (y vice versa).
  - `pipelines_test.go` extender: `last_run` se serializa correctamente cuando hay manifests, omitido cuando no hay.
- [ ] Smoke manual en PR body: `curl -N http://127.0.0.1:7878/api/events` mientras `touch ~/.che/pipelines/test.yaml` o `mkdir ~/.che/runs/x/y && echo "..." > ~/.che/runs/x/y/manifest.yaml`.

## Archivos afectados

- `internal/dash/bus.go` — modificar — agregar `SubscribeGlobal()` + canal global
- `internal/dash/bus_test.go` — modificar — coverage del subscriptor global
- `internal/dash/pipelines_watcher.go` — crear — watcher de `~/.che/pipelines/`
- `internal/dash/pipelines_watcher_test.go` — crear
- `internal/dash/runs_watcher.go` — crear — watcher global de `~/.che/runs/`
- `internal/dash/runs_watcher_test.go` — crear
- `internal/dash/sse_global.go` — crear — handler `/api/events`
- `internal/dash/sse_global_test.go` — crear
- `internal/dash/dash.go` — modificar — arrancar watchers globales en `Serve()` + registrar `/api/events`
- `internal/dash/pipelines.go` — modificar — agregar `last_run` al listing
- `internal/dash/pipelines_test.go` — modificar — verify `last_run` en JSON
- `internal/dash/assets/dash.html` — modificar — EventSource global + handlers + badge + fade-in + chip last_run en rail + empty state pipeline deleted

## Riesgos

- Polling 250ms en dos directorios (`~/.che/pipelines/` y `~/.che/runs/*`): si hay 50 pipelines × 100 runs = 5000 archivos a `stat` por tick. CPU acumula. Mitigación: en `runs_watcher` solo revisar manifests no-terminales (cache de status terminal salta el stat). En `pipelines_watcher` es de 1 dir, manejable.
- Detección de "delete" en pipelines watcher: si entre dos ticks aparece y desaparece, no se emite evento (race muy improbable; aceptado v1).
- `last_run` requiere leer un manifest por slug en cada listing call. Si hay 50 pipelines, 50 lecturas de YAML. Cache simple in-memory con TTL 1s en `internal/dash/pipelines.go` para mitigar.
- Coexistencia con watcher per-run de spec 3: dos watchers pueden tocar el mismo archivo simultáneamente. `os.Stat` es read-only, no hay race.
- Frontend dedup de `run:finished`: si llega del per-run y del global casi simultáneo, se duplica. Set por `(run_id, terminal_status)` lo resuelve.
- Reconnect storm: si la red se cae y vuelve, `EventSource` reconecta + refetch one-shot. Si hay 10 tabs, 10 refetches. Aceptado v1.

## Out of scope

- Patch al `internal/runner`
- fsnotify (sigue siendo polling, consistente con spec 3)
- Comandos via SSE (read-only)
- Eventos para cambios al `content` interno de un pipeline (steps, validator). Solo metadata.
- `pipeline:running` (estado intermedio) — usamos solo `started` y `finished`.
- Auth / rate limiting (sigue siendo 127.0.0.1 only)

## Asunciones tecnicas validadas

1. **Bus extendido**: `Bus` de spec 3 gana método `SubscribeGlobal() (<-chan Event, cancel)`. Eventos publicados sin `runId` van al canal global; eventos con `runId` siguen yendo a los suscriptores per-run. Buffer 256 por suscriptor (mismo cap que spec 3).
2. **Watchers globales no son ref-counted**: arrancan al boot del `Serve()` y corren siempre. Cleanup al shutdown del server.
3. **Polling 250ms** en ambos watchers (consistente con spec 3).
4. **Pipelines watcher**: `os.ReadDir(pipelinesDir)` + `os.Stat` + parse mínimo del `status` block. Snapshot in-memory `map[slug]{mtime, status}`. Diff por tick.
5. **Runs watcher global**: recorrido `~/.che/runs/<slug>/<runId>/manifest.yaml`. Snapshot in-memory `map[(slug, runId)]status`. Optimización: si status ya es terminal, no re-stat (skipea hasta que el archivo cambie de mtime, sería nuevo manifest aplastado).
6. **`last_run` en `/api/pipelines`**: helper `lookupLastRun(slug)` que lee el manifest más reciente por `started_at`. Cache in-memory con TTL 1s para evitar recargar en cada listing.
7. **JSON shape `last_run`**: `{id, status, started_at}` (id completo, no slice — el frontend lo cortea). `omitempty` cuando no hay runs.
8. **SSE format global**: idéntico a spec 3 — `event: <type>\ndata: {...json...}\n\n`. Headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. Flusher post cada chunk.
9. **Heartbeat**: ticker 15s con `: heartbeat\n\n`.
10. **Cleanup**: subscribe al `r.Context().Done()` para client disconnect → unsubscribe + close.
11. **Sin snapshot inicial en `/api/events`**: el spec lo dice explícitamente. El frontend hace fetch one-shot al cargar, así que el SSE solo cubre cambios desde la suscripción.
12. **Frontend EventSource lifecycle**: un `useEffect` en el componente App al mount, sin deps. Cleanup cierra. Distinto al EventSource per-run de spec 3 (que vive en el componente run detail).
13. **Frontend handlers**: `addEventListener("pipeline:changed" | "run:started" | "run:finished", ...)`. Cada handler actualiza el state via setters de React.
14. **Frontend dedup**: para `run:finished` mantener `Set<string>` con keys `${run_id}:${terminal_status}`. Para `pipeline:changed` no hace falta dedup (los cambios son idempotentes en el state — re-aplicar mismo `{slug, status}` no rompe nada).
15. **Reconnect refetch**: handler `onopen` chequea un flag `wasReconnect` (seteado por el primer `onerror`); si es true, refetch `/api/pipelines` y, si hay detail, `/api/pipelines/:slug/runs`. Limpiar el flag.
16. **Badge global**: state `globalConnState` ∈ `{live, reconnecting, disconnected, hidden}`. Timer de 10s para pasar de `reconnecting` a `disconnected` con botón retry (mismo patrón que badge per-run de spec 3).
17. **Fade-in animation**: CSS `@keyframes dash-fade-in { from { opacity: 0; transform: translateY(-4px); } to { opacity: 1; transform: none; } }` aplicado a row/item nuevo via clase `dash-fade-in`, 500ms ease-out. Se quita la clase tras `animationend`.
18. **Routing**: nuevo handler `mux.HandleFunc("/api/events", handleGlobalEvents(bus))` registrado en `Serve()`. NO va al dispatcher de `/api/pipelines/` — es ruta plana.
19. **Tests**:
    - `pipelines_watcher_test.go`: con `t.TempDir()`, write YAML → recibe `pipeline:changed`. Edit status → recibe flip. Remove file → recibe delete.
    - `runs_watcher_test.go`: con `t.TempDir()`, write manifest running → recibe `run:started`. Mutate a terminal → recibe `run:finished`.
    - `sse_global_test.go`: mock bus, publish global, verify SSE chunk. Heartbeat tras silencio. Cleanup en context cancel.
    - `bus_test.go`: `SubscribeGlobal` recibe eventos globales pero no los runId-filtered, y vice versa.
    - `pipelines_test.go`: `last_run` correcto cuando hay manifest, omitido cuando no.
20. **Smoke manual en PR body**: `curl -N http://127.0.0.1:7878/api/events` mientras `touch ~/.che/pipelines/foo.yaml` o crear/editar manifests en `~/.che/runs/`.

---

_Plan generado por `/hs-plan` a partir de #125._
