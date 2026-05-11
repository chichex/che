# Plan: Exponer runs históricos de che dash (tabla + run detail read-only)

Refs #121 · https://github.com/chichex/che/issues/121

## Contexto

Spec 2 del dashboard. La tabla de runs del pipeline detail y el nuevo run detail screen consumen 3 endpoints HTTP nuevos que leen `~/.che/runs/<slug>/<run-id>/manifest.yaml` + archivos `step-NN.stdout.log`. Read-only. Builds sobre `hs-plan/119` (PR #120).

## Objetivo

Exponer 3 endpoints HTTP para (a) listar runs históricos de un pipeline, (b) leer el manifest completo de un run, y (c) servir el stdout crudo por step. Wirear la tabla de runs y el run detail del HTML embebido a esos endpoints. El server sigue sin escribir nada ni shellear a `gh`.

## Approach

Agregar `internal/dash/runs.go` con tres handlers (`handleListRuns`, `handleGetRun`, `handleGetStepStdout`), parametrizados por `runsDir`. Reusar el parsing del manifest existente (`internal/runner.Manifest`) y, si está disponible, los helpers `wizard.RunHistoryFor` para listing. Refactor de la ruta `/api/pipelines/` en `internal/dash/dash.go` a un dispatcher interno que parsea path segments y delega a la lógica correspondiente (la `http.ServeMux` no soporta routing con múltiples vars). Frontend: reemplazar `RUNS_INIT[slug]` por fetch real en pipeline detail; agregar la run detail screen con state local + lazy fetch del stdout cuando se selecciona un step; tabs `output`/`definition`/`validator?` con visibilidad/disabled según las asunciones del spec.

## Pasos

- [ ] Crear `internal/dash/runs.go` con `handleListRuns(runsDir string) http.HandlerFunc`, `handleGetRun(runsDir string) http.HandlerFunc`, `handleGetStepStdout(runsDir string) http.HandlerFunc`.
- [ ] Listing reusa `wizard.RunHistoryFor(home, slug)` si está disponible; si no, walker propio que parsea `~/.che/runs/<slug>/<runId>/manifest.yaml` con `yaml.Unmarshal` a `runner.Manifest`. Orden desc por `started_at`. Corrupt skippeado + log a stderr con prefijo `[dash]`.
- [ ] Detail parsea `manifest.yaml` con la struct `runner.Manifest`. Si no existe loader público en `internal/runner`, agregar uno mínimo en `internal/dash/runs.go` (sin modificar `internal/runner`). Corrupt → 500 + JSON `{"error":"manifest corrupt"}`.
- [ ] Stdout endpoint sirve `~/.che/runs/<slug>/<runId>/step-NN.stdout.log` (NN = `fmt.Sprintf("%02d", idx+1)`) con `http.ServeFile` y Content-Type `text/plain; charset=utf-8`. 404 + JSON `{"error":"stdout not found"}` si no existe.
- [ ] Refactor del routing en `internal/dash/dash.go`: el handler de `/api/pipelines/` se vuelve dispatcher por segments.
  - `[slug]` → pipeline detail (spec 1, sin cambios)
  - `[slug, "runs"]` → list runs
  - `[slug, "runs", runId]` → get run
  - `[slug, "runs", runId, "steps", idx, "stdout"]` → get stdout
  - Cualquier otra cosa → 404.
- [ ] Mover la lógica del actual `handleGetPipeline` (spec 1) a una función helper invocable desde el dispatcher.
- [ ] `Serve()` resuelve `runsDir = filepath.Join(home, ".che", "runs")` (al lado del existente `pipelinesDir`).
- [ ] Frontend HTML — tabla de runs del pipeline detail:
  - Reemplazar lookup en `RUNS_INIT[slug]` por `useEffect` que fetch `/api/pipelines/:slug/runs` cuando cambia el slug seleccionado.
  - Render row: id (slice 0-6 chars), status chip (reusar el componente existente), started_at relativo (tooltip absoluto al hover), duración compacta o "—" si está corriendo.
  - Click handler en row → setea `selectedRunId` y abre run detail.
- [ ] Frontend HTML — run detail screen (state local, sin URL routing):
  - Cuando `selectedRunId` cambia, fetch `/api/pipelines/:slug/runs/:runId` → setea `runDetail`.
  - Columna izquierda: steps del manifest (dot status + nombre + duración).
  - Selección inicial: primer step (idx 0).
  - Tab default: `output`.
  - Tabs `output` / `definition` / `validator?`. `validator` aparece solo si `step.validator` no nil. `output` y `validator` deshabilitadas si `step.status == "pending"`. `definition` siempre disponible.
  - Tab `output`: lazy fetch a `/api/pipelines/:slug/runs/:runId/steps/:idx/stdout` cuando se selecciona el step. Empty → copy literal `sin stdout registrado`. Si `step.error` no vacío, renderizarlo arriba del stdout.
  - Tab `definition`: render del step desde el manifest. Si `content` vacío → copy literal `definición no persistida — ver YAML del pipeline`.
  - Tab `validator`: render de `loops_run/max_loops`, `final_verdict`, `last_feedback` en `<pre class="mono whitespace-pre-wrap">`.
  - AbortController por effect para evitar race al cambiar selección rápido.
- [ ] Helpers JS inline (sin deps): `formatDuration(ms)` → "12.4s" / "1m 23s" / "1h 12m"; `formatRelative(date)` → "hace 5 min" / "hace 1 h" / fecha absoluta si > 7 días.
- [ ] Empty state `sin runs registrados — corré el pipeline desde la TUI` aparece solo cuando el fetch devuelve `[]`.
- [ ] Botones Run / Cancel / Resume siguen disabled (sin cambios respecto a spec 1).
- [ ] Tests `internal/dash/runs_test.go`: listing (empty dir → `[]`, una run, múltiples ordenadas desc, una corrupta skippeada con log); detail (200 con todos los campos, 404 si run no existe, 500 si corrupt); stdout (200 con bytes, 404 si falta). Validator omitido si nil.
- [ ] Routing tests: tabla `{path, expectedStatus, expectedBodySubstring}` en `dash_test.go` o `runs_test.go` para validar el dispatcher.
- [ ] Smoke test manual en el PR body: `go build && ./che dash --no-open --port 17878` + curls a los 3 endpoints con un fixture plantado en `~/.che/runs/`.

## Archivos afectados

- `internal/dash/runs.go` — crear — 3 handlers + helpers de listing/detail/stdout
- `internal/dash/runs_test.go` — crear — coverage de listing/detail/stdout + routing dispatcher
- `internal/dash/dash.go` — modificar — refactor del routing a dispatcher + resolver `runsDir`
- `internal/dash/pipelines.go` — modificar — el handler actual de `/api/pipelines/:slug` pasa a ser invocado desde el dispatcher (función helper sin cambios de comportamiento)
- `internal/dash/assets/dash.html` — modificar — fetch real para tabla de runs + run detail screen + tabs + helpers JS

## Riesgos

- Refactor del routing: si rompo el matching del dispatcher, `/api/pipelines/:slug` (spec 1) deja de andar. Mitigación: tabla de tests del dispatcher antes de tocar el frontend, incluyendo todos los paths de spec 1 + spec 2.
- Manifest yaml puede tener campos que no espero o tags `yaml:"..."` que no documenté. Mitigación: usar exactamente la struct `runner.Manifest` ya definida, no redefinir; tests con fixture real.
- `step-NN.stdout.log` puede ser muy grande (no hay cap del runner). `http.ServeFile` hace streaming nativo, así que la memoria del server queda OK — el riesgo se traslada al browser. Aceptado.
- Lazy fetch del stdout flapeando si el usuario cambia rápido entre steps: AbortController por effect.
- Naming del archivo de stdout (`step-NN`, 1-indexed, 2-pad): si el runner cambió el formato sin actualizar la doc, los tests fallarán inmediatamente y resolvemos.
- Helper `wizard.RunHistoryFor` puede no estar exportado o no devolver todos los campos necesarios. Fallback: walker propio + parse YAML inline.

## Out of scope

- SSE live para runs vivos (spec 3) — los runs `running` se sirven como snapshot.
- SSE global del rail (spec 4).
- Lanzar / cancelar / resume (write API, eventual spec futuro).
- Stderr endpoint (queda como follow-up si hace falta).
- Routing real con URLs del browser (`history.pushState`).
- Paginación / truncado del stdout.
- Cambios al package `internal/runner` o `internal/wizard` salvo agregar accessor mínimo si no hay loader público.

## Asunciones tecnicas validadas

1. Stack: Go puro, sin deps nuevas. Reuse de `encoding/json` (stdlib), `internal/runner` (struct `Manifest`), `internal/wizard` (helper `RunHistoryFor` o similar) y `gopkg.in/yaml.v3` (ya usado por wizard/runner).
2. Ubicación: `internal/dash/runs.go` + `internal/dash/runs_test.go`. Sin sub-paquete nuevo.
3. Listing: preferimos `wizard.RunHistoryFor(home, slug)` si está exportado y devuelve la shape adecuada. Si no, walker propio que parsea `~/.che/runs/<slug>/<runId>/manifest.yaml` con `yaml.Unmarshal` a `runner.Manifest`.
4. Detail: parsear `manifest.yaml` con la struct `runner.Manifest` ya existente. Si no hay loader público (`runner.LoadManifest`), agregar uno mínimo en `internal/dash/runs.go` sin modificar `internal/runner`.
5. Routing: refactor del handler `/api/pipelines/` a un **dispatcher manual por segments**. Path post-prefix se splittea por `/`; el largo + nombre del segmento determina qué handler corre. No usar libs de routing — stdlib only.
6. JSON shape de listing: `[{id, status, started_at, finished_at?, input_kind, input_value}]`. snake_case (consistente con spec 1).
7. JSON shape de detail: `{id, slug, status, started_at, finished_at?, input_kind, input_value, steps: [...]}`. Step: `{idx, name, cli, kind, status, exit_code, started_at?, finished_at?, error?, validator?}`. Validator: `{cli, loops_run, max_loops, on_max_loops, final_verdict, last_feedback}`. Fields con `omitempty` cuando aplique.
8. Timestamps: `time.RFC3339` para emit. Parsing en el frontend con `new Date()`.
9. Stdout: servir con `http.ServeFile`. 404 + JSON `{"error":"stdout not found"}` si no existe. Content-Type forzado a `text/plain; charset=utf-8`.
10. Inyección de dependencias: handlers se construyen con factory `func(runsDir string) http.HandlerFunc`. Tests pasan `t.TempDir()`.
11. Detección de manifest corrupto en listing: try parse; on error → `log.Printf("[dash] manifest corrupt: %s: %v", path, err)` + skip. Los demás runs siguen siendo serializados.
12. Stdout file naming: `step-NN.stdout.log` con `NN = fmt.Sprintf("%02d", idx+1)` (1-indexed, 2 dígitos).
13. Frontend: dos nuevos `useEffect` en el componente App. (a) `selectedSlug` → fetch a `/api/pipelines/:slug/runs` y setea `runs`. (b) `selectedRunId` → fetch a `/api/pipelines/:slug/runs/:runId` y setea `runDetail`. Stdout es lazy en el tab `output` con su propio `useEffect`.
14. AbortController: instanciar uno por effect; cancelar al cleanup. Evita race entre selección anterior y nueva.
15. Helpers JS: `formatDuration(ms)` y `formatRelative(date)` implementados inline (~30 LOC cada uno). Sin libs externas (moment/dayjs/etc).
16. Render del feedback del validator: `<pre class="mono whitespace-pre-wrap">` con el contenido crudo. Sin highlighting.
17. Tests: `httptest.NewRecorder` + `t.TempDir()`. Plantar fixtures en disco simulando la estructura `~/.che/runs/<slug>/<runId>/manifest.yaml` + `step-01.stdout.log`. Cleanup automático del temp dir.
18. Routing tests: tabla `{path, expectedStatus, expectedBodySubstring}` para validar el dispatcher cubriendo paths de spec 1 + spec 2.
19. Smoke test manual incluido en el PR body: `go build && ./che dash --no-open --port 17878` + 3 curls.

---

_Plan generado por `/hs-plan` a partir de #121._
