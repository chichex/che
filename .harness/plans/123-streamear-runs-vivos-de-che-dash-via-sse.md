# Plan: Streamear runs vivos de che dash via SSE (per-run events)

Refs #123 · https://github.com/chichex/che/issues/123

## Contexto

Spec 3 del dashboard. Cuando el usuario abre un run con `status=running` desde el dash, el panel derecho debe mostrar stdout línea por línea conforme el step lo emite, los step states pasando de pending → running → done/failed, y los validator loops iterando en tiempo real. Sin refresh manual. Builds sobre `hs-plan/121` (PR #122).

**Resolución del gap del spec**: el spec menciona "event bus in-process" + "el runner publica al bus", pero `che` (TUI/runner) y `che dash` corren en procesos separados. Un bus de canales Go directo entre runner y dash no es viable sin fusionar procesos (gran cambio de UX/arquitectura). Resolución: el "bus" vive ENTERAMENTE dentro del proceso `che dash`, alimentado por un **disk-watcher** que observa `~/.che/runs/<slug>/<runId>/manifest.yaml` y `step-NN.stdout.log`. La behavior visible para el usuario es idéntica a lo descrito en el spec.

## Objetivo

Servir `GET /api/pipelines/:slug/runs/:runId/events` como Server-Sent Events emitiendo `run:status`, `step:start`, `step:end`, `step:stdout`, `validator:loop` en vivo, con heartbeat 15s y replay completo al conectar. Wirear el frontend a EventSource para que el tab output streamee, los dots de step se actualicen, los validator loops aparezcan en vivo, y el badge "live" refleje el estado de conexión. Cuando el run termina, conmutar a modo frozen.

## Approach

Implementar el bus + watcher + handler SSE íntegramente dentro de `internal/dash/`, sin tocar `internal/runner`. El watcher polling-based (`os.Stat` cada 250ms) lee deltas en `manifest.yaml` (atomic rename via `.tmp`) y `step-NN.stdout.log` (append-only), y los traduce a eventos del bus. El bus es ref-counted: el primer subscriber para un (slug, runId) inicia el watcher; el último que se va lo detiene. El handler SSE subscribe primero, hace snapshot del disco después, drain del buffer del bus, y entra en tail loop con heartbeats cada 15s. El frontend abre `EventSource` cuando el run está `running|paused`, dedup por keys explícitos, badge "live" pulsante, auto-scroll con threshold 50px.

## Pasos

- [ ] `internal/dash/bus.go`: `Bus.Subscribe(runId) (<-chan Event, cancel)`, `Bus.Publish(Event)`. Buffer 256 por suscriptor. Slow → close + drop.
- [ ] `internal/dash/watcher.go`: watcher por (slug, runId). Polling 250ms con `os.Stat`. State: último `manifest mtime+size`, último `stdout size por step`. En cada tick lee diffs y publica al bus.
- [ ] Lifecycle integrado al bus: ref-counted. Primer subscriber inicia watcher; último que se va halt.
- [ ] `internal/dash/sse.go`: `handleEvents(runsDir, bus)`. Subscribe primero, leer snapshot del disco, drain del buffer del bus, después entrar en tail. Headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. `http.Flusher` post cada chunk. Ticker 15s para heartbeat. Cleanup en `r.Context().Done()`.
- [ ] Replay desde disco: emit `run:status=<actual>`, `step:start`/`step:end` por step del manifest con timestamps, `step:stdout` línea-por-línea del `step-NN.stdout.log` del running step (con ordinal incrementing), `validator:loop` históricos del manifest. Termina con suscripción al tail.
- [ ] Detección de fin de run en watcher: cuando manifest transitions de `running` a terminal (`done|failed|cancelled|interrupted`), emit `run:status=<terminal>` + halt watcher después de drain stdout pending. Estado `paused` mantiene watcher activo.
- [ ] Routing: extender dispatcher en `dash.go` para match `[slug, "runs", runId, "events"]` → `handleEvents`.
- [ ] `Serve()` inicializa instancia singleton del bus y la inyecta al handler.
- [ ] Frontend HTML — EventSource lifecycle:
  - useEffect en run detail: cuando `runDetail.status in ["running", "paused"]`, `new EventSource("/api/pipelines/:slug/runs/:runId/events")`. Cleanup cierra.
  - `addEventListener` para cada tipo: `run:status`, `step:start`, `step:end`, `step:stdout`, `validator:loop`.
  - Cuando llega `run:status` terminal != paused: close EventSource + refetch `/api/pipelines/:slug/runs/:runId` para reconciliar.
- [ ] Frontend HTML — dedup:
  - `Set<string>` con keys `step:<idx>:start`, `step:<idx>:end`, `validator:<idx>:<loop>` para transitions.
  - Contador `nextLineOrdinal[idx]` por step; eventos con `ordinal < nextLineOrdinal` descartados.
- [ ] Frontend HTML — tab output streaming:
  - Cada `step:stdout` agrega línea al state local del step (`stdoutLines[idx]`).
  - Auto-scroll: state `autoScrollEnabled` (init `true`). Listener `onscroll` desactiva si user scrollea arriba >50px del bottom; reactiva si vuelve al bottom. Si activo, `scrollTop = scrollHeight` después de cada nueva línea.
- [ ] Frontend HTML — columna de steps:
  - Cada `step:start` setea `step.status = "running"` y `started_at`.
  - Cada `step:end` setea `step.status`, `exit_code`, `finished_at`, `error?`.
- [ ] Frontend HTML — tab validator:
  - Cada `validator:loop` push al array `step.validator.loops`.
- [ ] Frontend HTML — badge "live":
  - 4 states: `live` (pulsing green dot), `reconnecting…` (yellow dot, mientras `onerror` con readyState=CONNECTING), `disconnected` (red dot + botón retry, después de 10s sin reconnect exitoso), hidden (run frozen).
  - Timer de 10s arranca en `onerror`, se cancela en `onopen` exitoso.
- [ ] Tests:
  - `bus_test.go`: subscribe/publish básico, multiple subs fanout, slow-sub overflow → drop, unsubscribe libera.
  - `watcher_test.go`: con `t.TempDir()`, write manifest yaml → recibe step transitions; append a stdout file → recibe líneas con ordinals; transition a terminal → emit final + halt.
  - `sse_test.go`: replay correcto desde fixture, tail con mock publisher al bus, heartbeat tras silencio, headers SSE correctos, cleanup en context cancel.
- [ ] Smoke test manual en PR body: crear fixture de run en `~/.che/runs/`, `curl -N http://127.0.0.1:7878/api/pipelines/<slug>/runs/<run-id>/events`, append a stdout via shell, verificar eventos.

## Archivos afectados

- `internal/dash/bus.go` — crear — pub/sub in-process por runId con buffer 256
- `internal/dash/bus_test.go` — crear — cobertura básica + drop on slow sub
- `internal/dash/watcher.go` — crear — disk watcher polling 250ms por (slug, runId)
- `internal/dash/watcher_test.go` — crear — manifest + stdout detection con fixture
- `internal/dash/sse.go` — crear — handler SSE con replay + tail + heartbeat
- `internal/dash/sse_test.go` — crear — replay correctness + tail + cleanup
- `internal/dash/dash.go` — modificar — agregar dispatcher route + init bus en Serve()
- `internal/dash/assets/dash.html` — modificar — EventSource + dedup + auto-scroll + badge

## Riesgos

- Polling cadence × N suscriptores: 250ms × N watchers concurrentes. CPU acumula con muchos runs vivos. Mitigación: test con 10 watchers; cadence ajustable a 500ms si hace falta.
- macOS file change quirks: evitados al usar polling explícito en lugar de fsnotify.
- Race replay vs tail: diseño explícito subscribe→snapshot→drain elimina el race. Tests cubren el caso.
- Frontend dedup imperfecto: keys explícitos por tipo (status transitions + line ordinals) garantizan idempotencia.
- Stdout rápido (1000+ líneas/seg): polling cada 250ms lee bursts de 250 líneas. Aceptado v1 sin batching.
- Watcher leak: ref-counting + cleanup en `r.Context().Done()` mitiga.
- Deviation del spec ("in-process bus" → "in-dash disk-watcher"): preserva behavior visible; documentado como asunción #1.

## Out of scope

- SSE global del rail (spec 4)
- Patch al `internal/runner` (no se modifica)
- fsnotify (polling es portable y suficiente; fsnotify es mejora futura si la cadence resulta insuficiente)
- Batching/throttling de stdout events
- Stderr endpoint (queda como follow-up)
- Comandos via SSE (read-only)
- Fusionar `che` (TUI) y `che dash` en un único proceso

## Asunciones tecnicas validadas

1. **Bus + watcher EN el proceso `che dash`, NO patch al runner.** El spec menciona "event bus in-process" + "runner publica al bus", pero `che` (TUI/runner) y `che dash` corren en procesos separados. El disk-watcher dentro del dash observa los archivos en disco (manifest atomic rename + stdout append) y traduce a eventos del bus. Behavior visible del spec preservada; arquitectura del repo no cambia.
2. **Estrategia de file watching**: polling `os.Stat` cada 250ms (no `fsnotify`). Detecta manifest changes via `mtime`+`size`; stdout appends via `size delta`. Portable, evita quirks de macOS.
3. **Lifecycle del watcher**: por (slug, runId). Ref-counted via bus — primer subscriber inicia watcher; último que se va detiene. Sin watchers ociosos.
4. **Buffer del bus**: 256 eventos por suscriptor. Overflow → close + drop el subscriber (cliente reconecta + replay).
5. **Replay correctness**: subscriber se suscribe AL bus PRIMERO, después se lee snapshot del disco, después se drain el buffer del bus. Garantiza no perder eventos publicados durante la ventana del snapshot.
6. **SSE format**: `event: <type>\ndata: {...json...}\n\n`. Headers `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. `http.Flusher` después de cada chunk.
7. **Heartbeat**: `time.Ticker` cada 15s emitiendo `: heartbeat\n\n`.
8. **Cleanup**: subscribe al `r.Context().Done()` para detectar client disconnect → unsubscribe + close. Estado terminal != paused → emit `run:status` final + close server-side.
9. **Detección de fin de run en watcher**: cuando manifest transitions de `running` a terminal, watcher emite `run:status=<terminal>` y termina su goroutine (después de drain de stdout pending). Estado `paused` mantiene el watcher activo.
10. **Frontend EventSource**: API estándar del browser. `new EventSource(URL)`. `addEventListener` per event type. Cleanup en useEffect cleanup.
11. **Dedup en frontend**: `Set<string>` con keys `step:<idx>:start`, `step:<idx>:end`, `validator:<idx>:<loop>` para transitions; para stdout, contador `nextLineOrdinal` por step.
12. **Auto-scroll**: threshold 50px del bottom. State `autoScrollEnabled` toggle por `onscroll` (off si user scrollea up >50px del bottom; on si vuelve).
13. **Badge states**: 4 — `live` (pulsing green), `reconnecting…` (yellow, durante `onerror` con readyState=CONNECTING), `disconnected` (red, después de 10s sin succeed; muestra botón "retry"), hidden (run frozen).
14. **Refetch del manifest al cerrar SSE**: cuando se cierra por terminal status, frontend hace fetch `/api/pipelines/:slug/runs/:runId` para reconciliar state.
15. **Tests**: `bus_test.go` (subscribe/publish, fanout múltiple, slow-sub drop, unsubscribe libera), `watcher_test.go` (con `t.TempDir()`, write manifest yaml → recibe events; append stdout → recibe líneas; transition a terminal → emit + halt), `sse_test.go` (replay desde fixture, tail con mock publisher, heartbeat tras silencio, headers SSE).
16. **Routing**: extender el dispatcher en `dash.go` con `[slug, "runs", runId, "events"]` → `handleEvents`.
17. **Inicialización del bus**: singleton instanciado en `Serve()`, inyectado a los handlers SSE. Tests usan un bus separado.
18. **JSON shape de payloads**:
    - `run:status`: `{status}`
    - `step:start`: `{idx, name, started_at}`
    - `step:end`: `{idx, status, exit_code, finished_at, error?}`
    - `step:stdout`: `{idx, line, ts, ordinal}` — agrego `ordinal` para dedup determinístico (extension al spec, justificado como detalle de implementación)
    - `validator:loop`: `{idx, loop, max_loops, verdict, feedback}`
    - Timestamps en `time.RFC3339`.
19. **Smoke test manual en PR body**: crear fixtures de run en `~/.che/runs/<slug>/<runId>/`, simular writes via shell, `curl -N` al endpoint, verificar eventos.

---

_Plan generado por `/hs-plan` a partir de #123._
