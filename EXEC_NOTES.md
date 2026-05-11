# Notas de ejecucion · #130

## Desviaciones respecto del plan

### AC #1 del plan vs realidad del builtin
El plan dice "POST /api/pipelines/che-funnel/runs con body vacio responde 201". Pero `internal/wizard/embedded/che-funnel.yaml` declara `input: text` en el primer step — el handler responde correctamente `400 {"error":"input requerido"}` ante body vacio. El happy-path "body vacio → 201" lo verifica el AC #1 de TestCreateRun_HappyPathNoInput, que usa un pipeline-fixture (`no-input-pipe`) creado con `input: none`. Para hacer la verificacion manual con `che-funnel` hay que pasar un body con `input`. Comportamiento es el correcto; la inconsistencia esta en el plan.

### Headless runner: parallel synchronous executor, no extraccion de runStep
El plan #1 propuso "extraer `runStepsHeadless` reusando la mecanica de `enterRunning`/`startStep`/`handleStepDone`". Esos handlers viven dentro de `tea.Cmd` con un canal `lineCh` por step y `runState` con cancel handles — extraerlos limpio era un refactor mas grande que el budget del risk #1 (~200 LOC). El path tomado:

- Nuevo archivo `internal/runner/headless.go` que escribe los mismos artefactos en disco (manifest.yaml + step-NN.{stdout,stderr,result}.{log,yaml}) reusando `loadPipelineForRun`, `wizard.IsValid`, `initRunDir` (parametrizado via `initRunDirAt`), `initManifest`, `writeManifest`, `writeStepResult`, `closeManifest` y `spawnCmdFn`.
- Step executor sincronico (`runStepHeadless`) que hace `cmd.Run()` blocking con multi-writer a archivo + buffer en memoria. Sin streaming hacia el caller (no lo necesitamos: el watcher de `runs_watcher.go` traduce los writes a SSE).
- La TUI queda 100% intacta (no se toca `spawn.go`, `running.go`, `runner.go`).

Total nuevo: `headless.go` ~260 LOC (con comentarios extensos en castellano siguiendo el estilo del repo). Bien debajo del threshold de split del plan.

### Modal: error inline + pending state
Se agrego un boton `pending` desactivado mientras la request esta en vuelo + estado `modalError` que renderea el `{"error":"..."}` del backend inline (no se cierra el modal en 400/404/409/500 — AC #8). No se cierra en 500, exactamente como pide el AC.

### Shortcut `R`: skip cuando hay input focused
El shortcut `R` no se dispara si el foco esta en un `INPUT`/`TEXTAREA`/contentEditable — evita que tipear "r" en el drawer de notes abra el modal. No estaba explicito en el AC pero es comportamiento esperado.

## Riesgos residuales (de los riesgos del plan)

1. **Lock se libera al terminar `Execute()`** — la version inicial dejaba el `runLock` activo hasta que el server reiniciaba (release solo en path de error del starter). Fixed: la interfaz `runStarter` ahora recibe `onDone func()` que el `runnerStarter` invoca via `defer` en la goroutine de `Execute()`. El handler pasa `func() { lock.release(slug) }`. Resultado: un segundo POST al mismo slug, despues de que termine el primero (cualquier status terminal), arranca sin 409. Tests: `TestCreateRun_LockReleasedOnExecuteDone` + `TestCreateRun_ConflictLockHeldByMemory` (sigue pasando: el mock `recordingStarter` ignora el callback, asi que el lock se mantiene retenido durante el test sync sin necesidad de simular el lifecycle del run).

2. **`recentRunningWindow = 60s`** — hardcoded segun el nit del validator. TODO inline en `runs_create.go` apunta a #50 para reemplazarlo por el heartbeat real cuando aterrice.

## Fix follow-up (post-feedback)

- `RunModal` ahora deriva el shape de input desde `pipeline.steps[0].input` (string `text`/`issue`/`pr`/`url`/`file`/`none`) en lugar del objeto `pipeline.input.{kind,label,placeholder}` que la API nunca expuso. Asi `che-funnel` (declara `input: text` en step 0) arranca correctamente desde el dash con un campo de texto, y los pickers mock siguen apareciendo para `issue`/`pr`. Resuelve el feedback "el RunModal espera `pipeline.input` que no esta en la API".

- `runStarter.Start` recibe `onDone func()` y el handler libera el lock al finalizar Execute() (ver punto 1).

## Verificacion local

- `go vet ./...` OK
- `go test ./...` OK (todo el repo, no solo runner/dash)
- Smoke manual del browser: NO ejecutado (este PR es draft; lo cubre el reviewer humano segun el AC final del plan).
