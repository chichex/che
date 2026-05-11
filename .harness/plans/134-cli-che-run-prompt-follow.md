# Plan: feat(cli): che run <slug> [prompt] — arrancar runs desde CLI con follow + exit code

Refs #134 - https://github.com/chichex/che/issues/134

## Contexto

PR #133 (recien mergeado) introdujo `POST /api/pipelines/:slug/runs` en el dash y `runner.StartHeadless` en `internal/runner`. Falta el subcomando CLI que conecte ambos para que un agente (Claude/Codex) pueda invocar `che run sarlanga "<prompt>"` y obtener un exit code segun el status final del run, sin abrir el dash ni la TUI.

## Objetivo

Agregar `che run <slug> [prompt]` a `cmd/`:
- Resuelve el slug (pipelines de usuario + builtins) y el input (arg posicional > stdin > error si requerido).
- Backend "auto": intenta hablar al dash via HTTP usando un `~/.che/dash.port` file; si el dash no responde, cae a `runner.StartHeadless` in-proc.
- Follow + exit code: streamea logs de los steps al stdout (prefijo `[step.name] <linea>`) y termina con exit 0 si `status=done`, exit 1 si `status=failed` u otro terminal-no-done.

## Approach

1. **Discovery del dash port via PID/port file.** El dash hoy bindea 7878 por defecto con fallback a puerto efimero — sin contrato externo. Agregar en `internal/dash/dash.go` `Serve()`: al obtener el listener, escribir `~/.che/dash.port` (TCP port en texto plano) y `defer os.Remove` antes del return. El CLI lee este archivo. Si no existe o la conexion falla, fallback a headless.

2. **`runner.HeadlessRun.LiveOutput`.** Extender la struct con un campo opcional `LiveOutput io.Writer`. `runStepHeadless` ya tiene `io.MultiWriter(stdoutFile, &stdoutBuf)` — agregar un tercer writer con prefijo `[step.name] ` si `LiveOutput != nil`. Stderr va al mismo writer con el mismo prefijo (multiplex). Sin cambios en el contrato existente (zero-value sigue funcionando como antes — el dash que no setea LiveOutput sigue escribiendo solo a archivo + buffer).

3. **`cmd/run.go` — subcomando cobra.** Define `runCmd` con:
   - Args: `cobra.RangeArgs(1, 2)` — `<slug>` requerido, `[prompt]` opcional.
   - Input resolution: arg posicional [1] gana; si falta y `stdin` no es TTY (`term.IsTerminal(int(os.Stdin.Fd())) == false`), lee `io.ReadAll(os.Stdin)`. Si no hay arg ni stdin → input vacio (legitimo solo si el pipeline lo permite).
   - Slug validation: cargar pipeline igual que el dash (helper compartido o duplicacion minima — preferir un `runner.LoadPipelineBySlug` reutilizable si no hay ya uno; ver paso 4).
   - Input gate: si `input.kind != "none"` y el input resuelto esta vacio, error con exit code != 0 y mensaje claro a stderr antes de arrancar nada.
   - Backend dispatch: `dialDash(port)` (HTTP probe rapido — `net.DialTimeout` 200ms a `127.0.0.1:<port>` leido del file) → si OK, `runViaDash(port, slug, input)`; si NO, `runViaHeadless(slug, input)`.

4. **`runViaDash`:** POST `http://127.0.0.1:<port>/api/pipelines/<slug>/runs` con `Content-Type: application/json` body `{"input": "<value>"}` (o vacio si input.kind=none). Parsear `{"run_id": "..."}` del 201. Si 400/404/409/500 → imprimir el error JSON a stderr y exit code != 0. Si 201 → abrir SSE `GET /api/pipelines/<slug>/runs/<run_id>/events`, parsear eventos:
   - `step:start` → memorizar `idx → name` en un map local.
   - `step:stdout` → imprimir `[<name>] <line>\n` a stdout (resolver name via el map; si no esta, usar `step-NN`).
   - `step:end` → si `status=failed`, imprimir `[<name>] FAILED exit=<code> error=<msg>` a stderr.
   - `run:status` con `status` terminal (`done`/`failed`/`interrupted`/`cancelled`) → cerrar SSE y exit 0 si `done`, 1 si no.
   - Heartbeats (`: heartbeat`) → ignorar.

5. **`runViaHeadless`:** `runner.StartHeadless(target, input, "")` → si error, exit 1. Setear `h.LiveOutput = os.Stdout`. Llamar `h.Execute()` blocking. Exit 0 si nil error, 1 si error.

6. **Pipeline loading reutilizable.** En `internal/runner` ya existe `loadPipelineForRun(target)` (privado) — promoverlo a `LoadPipelineByTarget` (export) o duplicar la logica de slug-resolution en `cmd/run.go`. Preferir lo primero (zero duplicacion) si no rompe el API publico. `target` puede ser `"builtin:<slug>"` o un path; agregar helper `ResolveSlug(slug string) (target string, kind string, err error)` que devuelva el shape que `StartHeadless` espera.

7. **Tests.**
   - `cmd/run_test.go`: parser de args (con/sin prompt, con stdin pipeado, error si stdin TTY y pipeline requiere input). Mock del backend via interface o seam.
   - `cmd/run_dash_test.go`: simular un dash con `httptest.NewServer` que responde 201 + SSE con secuencia conocida; verificar prefijo de logs y exit code segun terminal status.
   - `cmd/run_headless_test.go`: usar `runner.startHeadlessFromPipeline` con un pipeline fake de un step `echo` (igual que `headless_test.go`); verificar exit 0 + output con prefijo.
   - `internal/runner/headless_test.go`: nuevo caso para `LiveOutput` — verificar que el writer recibe las lineas con prefijo.
   - `internal/dash/dash_test.go`: test del port file (Serve crea el file, Shutdown lo borra). Si no existe el test file, crearlo.

## Pasos

- [ ] 1. **Promover `loadPipelineForRun`** a export (o agregar wrapper `LoadPipelineByTarget`) en `internal/runner`, mas un helper `ResolveSlug(slug) (target, inputKind, error)` que devuelva `"builtin:<slug>"` para builtins y el path para pipelines de usuario en `~/.che/pipelines/`. Si el slug no existe → error.
- [ ] 2. **Agregar `HeadlessRun.LiveOutput io.Writer`** y reescribir la firma de `runStepHeadless(step, payload, runDir, idx)` → `runStepHeadless(step, payload, runDir, idx, live io.Writer)`. Si `live != nil`, agregar un prefix-writer al MultiWriter. Sin cambios en callers existentes (el dash sigue pasando nil → comportamiento intacto).
- [ ] 3. **Extender `internal/dash/dash.go` `Serve()`** para escribir `~/.che/dash.port` con el TCP port del listener apenas se obtiene `ln`, y borrarlo en defer antes de return. Si la escritura falla, log con prefijo `[dash]` y seguir (no abortar — el dash debe arrancar igual).
- [ ] 4. **Crear `cmd/run.go`** con el subcomando cobra: args, stdin handling, slug+input validation, dispatch `dialDash` → `runViaDash` o `runViaHeadless`. Registrar con `rootCmd.AddCommand(runCmd)` en `init()`. Tests minimo de parser en `cmd/run_test.go`.
- [ ] 5. **`runViaDash`:** POST + SSE consumer. Parser de eventos: usar `bufio.Scanner` por linea + state machine pequena (event/data lines). Mapear `idx → step.name` desde `step:start` para prefijar los `step:stdout` (cuando el primer step empieza, ya tenemos el nombre).
- [ ] 6. **`runViaHeadless`:** thin wrapper sobre `StartHeadless` + `Execute()`, setando `LiveOutput=os.Stdout`. Map exit code.
- [ ] 7. **Tests:**
   - `cmd/run_test.go` — parser/args + stdin handling + slug missing.
   - `cmd/run_dash_test.go` — `httptest.Server` con SSE canned; verificar prefijo y exit.
   - `cmd/run_headless_test.go` — pipeline fake one-step (`startHeadlessFromPipeline` via test helper exportado del paquete `runner` si hace falta, o usar `runner.StartHeadless` con un builtin minimo).
   - `internal/runner/headless_test.go` — nuevo case para `LiveOutput`.
   - `internal/dash/dash_test.go` — Serve crea/borra `dash.port` file (con `t.TempDir()` override).
- [ ] 8. **Build + suite:** `go build ./...` + `go vet ./...` + `go test ./internal/dash/... ./internal/runner/... ./cmd/...`.

## Archivos afectados

- `cmd/run.go` — crear — subcomando `che run` con args, stdin handling, dispatch auto API/headless, follow + exit code
- `cmd/run_test.go` — crear — parser/args + stdin + slug missing
- `cmd/run_dash_test.go` — crear — `httptest.Server` con SSE canned (happy + failed)
- `cmd/run_headless_test.go` — crear — happy path con builtin fake
- `internal/runner/headless.go` — modificar — `HeadlessRun.LiveOutput io.Writer` + extender `runStepHeadless` con prefix writer
- `internal/runner/headless_test.go` — modificar — nuevo case `LiveOutput` recibe lineas prefijadas
- `internal/runner/loader.go` (o donde viva `loadPipelineForRun`) — modificar — export `LoadPipelineByTarget` + `ResolveSlug` helper
- `internal/dash/dash.go` — modificar — `Serve()` escribe `~/.che/dash.port` y lo borra al shutdown
- `internal/dash/dash_test.go` — crear o modificar — test del lifecycle del port file

## Asunciones tecnicas validadas

1. `cmd/run.go` registra con `rootCmd.AddCommand(runCmd)` igual que `dash.go`, `doctor.go`, `upgrade.go`.
2. El port file vive en `~/.che/dash.port` (mismo directorio que `~/.che/pipelines/` y `~/.che/runs/`). Formato: TCP port en texto plano sin newline.
3. `dialDash` usa `net.DialTimeout("tcp", ..., 200*time.Millisecond)` — corto para que el fallback a headless sea snappy. Si la conexion abre, el dash esta vivo; si falla, headless.
4. Detection stdin TTY: `golang.org/x/term.IsTerminal(int(os.Stdin.Fd()))`. La dep `golang.org/x/term` ya esta en el `go.mod` (usada por el wizard/TUI); si no, agregarla.
5. El SSE consumer no necesita reconectarse — si el run termina con un terminal `run:status` el server cierra el stream y el cliente exitea. Si la conexion cae antes (ej. el dash muere mid-run), el cliente termina con exit code != 0 y mensaje claro.
6. `ResolveSlug` para pipelines de usuario busca `~/.che/pipelines/<slug>.yaml` (o el shape que el dash usa para listar). Para builtins, prefija `"builtin:"`.
7. Los eventos SSE tienen el shape definido en `internal/dash/sse.go` (`step:start` payload con `name`, `step:stdout` con `idx` + `line`, `step:end` con `idx` + `status` + `exit_code` + `error`, `run:status` con `status`). El parser cubre exactamente eso.
8. Headless prefix: cuando un step escribe N lineas, el writer recibe `[name] <linea>\n` por cada una. Lineas parciales sin newline se buferean hasta el proximo flush (o el final del step).

## Riesgos

- **Mismatch entre logs SSE y headless.** El SSE replay del dash emite `step:stdout` solo para steps `running`/`done`/`failed` y los lee de `step-NN.stdout.log` — puede no incluir lineas que llegaron despues del replay si el run avanza rapido. Mitigacion: el SSE tail forwardea bus events en vivo, no solo replay; los tests deberian cubrir un run que ya empezo antes del POST (escenario realista). En headless el output es 100% fiel porque va al MultiWriter directo.
- **Port file stale.** Si el dash crashea sin defer, `~/.che/dash.port` queda con un port que ya no responde. Mitigacion: `dialDash` con timeout corto — si no conecta, cae a headless. El file se sobreescribe al proximo `che dash`.
- **Concurrencia de dos `che dash` simultaneos.** El segundo bindea otro puerto y sobreescribe el port file. Aceptable para v1 (raro en la practica); documentar en el body del PR.

## Out of scope

- Flag `--via api|headless` para forzar backend.
- Flag `--no-follow` / fire-and-forget (devolver run_id y salir).
- Cancelacion del run desde el comando con Ctrl+C (en API el run sigue corriendo; en headless el proceso muere y deja el manifest huerfano — cleanup viene de `RecoverInterruptedRuns`).
- Auth / token.
- Soporte multi-dash (port file unico).
- Reconexion del SSE si el dash se cae mid-run.

---

_Plan generado por `/hs-auto` a partir de #134 (sin labels harness)._
