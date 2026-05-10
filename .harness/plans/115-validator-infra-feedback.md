# Plan: feedback accionable cuando el validator sale con exit != 0 (infra-fail)

Refs #115 - https://github.com/chichex/che/issues/115

## Contexto

Cuando el validator de un step termina con exit code != 0, `runValidator` (en `internal/runner/validator.go:680`) setea `Verdict.Feedback = fmt.Sprintf("validator exit %d", exitCode)` y descarta el stdout/stderr capturados. Ese string pelado es lo unico que llega al rerun del step via `rerunStepWithFeedback` -> `mergeFeedbackIntoPayload` (en `internal/runner/running.go:680-756`).

El modelo del step recibe `"validator exit 1"` como feedback. No es accionable: no dice por que fallo (context overflow, crash del CLI, timeout, spawn error). El step reintenta a ciegas y, en runs como `che-funnel/2026-05-10T20-49-40` step-02, deriva en cascada de role-confusion (claude empezo a emitir `verdict: approve` como output del step en RUN-03).

El dato util ya existe: el subprocess captura el stdout completo en `stdoutCopy` (validator.go:660-661) y lo propaga al `validatorDoneMsg.RawStdout` (validator.go:694). Tambien lo persiste en `verdict.yaml` truncado a 4 KiB via `truncateForRecord` (validator.go:397-403). Pero el path de exit != 0 corta el flujo antes de mirarlo: el `switch` de validator.go:676-683 entra en el `case exitCode != 0` y nunca pasa por `parseVerdict(stdoutCopy)`. Para codex stream-json, ese stdout contiene eventos `{"type":"error","message":"Codex ran out of room..."}` que serian un feedback humano perfecto.

Ademas `internal/runner/parser/codex.go` hoy es un placeholder (`Codex().Parse(raw)` delega en `Raw().Parse(raw)`). Para extraer `error.message` de stream-json codex necesitamos una helper minima (paquete `runner` mismo, o ampliando el parser) que no rompa el contrato actual.

## Objetivo

Que el feedback prependeado al rerun cuando el validator sale con exit != 0 sea accionable: si stream-json del validator contiene un evento de error parseable, extraer su `message`; si no, sintetizar un feedback con el CLI + exit + las ultimas lineas no vacias de stderr/stdout; truncado a 4 KB y envuelto con un wrapping explicito que distingue "fallo de infra" de "trabajo defectuoso".

## Approach

PR unico, foco en `validator.go` + `running.go` + tests. Estrategia:

- **Construccion del feedback (validator.go)**: cuando `exitCode != 0` en el switch de validator.go:676-683, reemplazar el feedback hardcoded por un helper `buildInfraFailFeedback(cli, exitCode, stdout, stderr) string` que aplica el siguiente orden de fallback:
  1. Intentar extraer un evento `{"type":"error","message":"..."}` del stream-json del validator (cuando `cli == "codex"`). Si matchea, el feedback es `"validator (cli=codex) infra-failed exit=N: <error.message>"`.
  2. Si no hay evento error parseable (cualquier CLI), tomar las ultimas 10 lineas no vacias concatenadas de stderr; si stderr esta vacio, de stdout. Feedback: `"validator (cli=<X>) infra-failed exit=<N>: <ultimas lineas>"`.
  3. Si ambos stdout y stderr estan vacios, fallback ultimo a `"validator exit %d"` (mantiene compat con el comportamiento anterior).
- **Wrapping anti role-confusion (validator.go)**: el helper envuelve el feedback con un prefijo explicito tipo `"El validator no pudo evaluar tu output por una falla de infra (no por un problema del trabajo). Detalle: <X>. Reintenta el step manteniendo el output."`. El wrapping vive en `buildInfraFailFeedback`, no en `mergeFeedbackIntoPayload`, para que `Verdict.Feedback` persistido en `verdict.yaml` sea el feedback final accionable.
- **Truncamiento (validator.go)**: el output total del helper se trunca a 4 KB con sufijo `"... (truncado)"` reusando la misma constante / helper que `truncateForRecord`. El cap aplica DESPUES del wrapping para no inflar context del rerun.
- **Parser codex (parser/codex.go o runner/validator.go)**: agregar funcion `extractStreamJSONErrorMessage(stdout string) string` que itera linea por linea buscando un JSON con `{"type":"error", "message":"..."}` (acepta tambien `{"type":"turn.failed", ...}` con campo message anidado si esta presente). Devuelve `""` si nada matchea. Puede vivir en `runner/validator.go` junto a `tryExtractAgentText` (mismo patron) para no agrandar el contract del paquete `parser` con algo todavia provisional.
- **`isRawVerdictFallback` (running.go:728)**: no se toca el path de feedback vacio. El feedback de infra-fail ya viene con wrapping textual y nunca matcheara el patron `verdict: <token>`. Documentar en un comentario que el filtro defensivo sigue cubriendo solo el path 3-vias.
- **Marker explicito (running.go)**: para distinguir feedback de infra-fail de feedback "real" del validator (verdict fail con texto), agregar a `Verdict` un campo `InfraFail bool` (paralelo a `RawFeedbackOnly`). `mergeFeedbackIntoPayload` no lo necesita (el wrapping del helper ya alcanza), pero el modal RP y el manifest pueden persistirlo en `last_feedback_kind` para futuros consumidores (ej. la opcion `on_validator_infra_fail` de la asuncion 5 del spec). El flag se setea en `buildInfraFailFeedback` y lo lee el handler de R3 (handleValidatorDone).

Tests:

- `validator_test.go`:
  - `TestBuildInfraFailFeedback_CodexErrorEvent`: stdout con `{"type":"error","message":"Codex ran out of room..."}` + exit=1 -> feedback contiene el message y el wrapping.
  - `TestBuildInfraFailFeedback_FallbackTail`: stdout sin error event + stderr con 15 lineas -> feedback contiene las ultimas 10 lineas.
  - `TestBuildInfraFailFeedback_EmptyOutputs`: stdout="" + stderr="" + exit=137 -> feedback es `"validator exit 137"` (compat).
  - `TestBuildInfraFailFeedback_TruncatedTo4KB`: stdout muy largo -> feedback resultante <= 4 KB con sufijo `"(truncado)"`.
  - `TestBuildInfraFailFeedback_Wrapping`: el feedback empieza con el prefijo "El validator no pudo evaluar..." (anti role-confusion).
- `running_test.go`:
  - `TestMergeFeedbackIntoPayload_InfraFailPrepended`: con feedback wrapping de infra-fail, `mergeFeedbackIntoPayload` lo prependea al payload (no lo filtra como raw fallback).
- Cobertura existente (`TestParseVerdict_*`, `TestMergeFeedbackIntoPayload_*`, `TestRunValidator_StreamsLinesAndEmitsDone`) no debe regresar.

## Pasos

- [ ] Releer el switch de `runValidator` en `internal/runner/validator.go:676-683` y confirmar que el unico path que setea `"validator exit %d"` es el `case exitCode != 0` (sin otros call sites del literal).
- [ ] Implementar `buildInfraFailFeedback(cli string, exitCode int, stdout, stderr string) (feedback string, infraFail bool)` en `internal/runner/validator.go`. Orden de fallback: codex stream-json `error.message` -> ultimas 10 lineas no vacias de stderr/stdout -> `"validator exit N"`. Wrap con el prefijo anti role-confusion. Truncar a 4 KB.
- [ ] Implementar `extractStreamJSONErrorMessage(stdout string) string` en `internal/runner/validator.go` (mismo archivo que `tryExtractAgentText` por coherencia). Iterar linea por linea, JSON unmarshal sobre `{"type":"error","message":"..."}` y `{"type":"turn.failed","message":"..."}`. Devolver `""` si nada matchea.
- [ ] Reemplazar el `case exitCode != 0` del switch en `validator.go:679-680` por una llamada a `buildInfraFailFeedback`; setear `verdict.InfraFail = infraFail` si el flag bool se introduce.
- [ ] Agregar campo `InfraFail bool` a `Verdict` (en `validator.go:65-76`) con tag `yaml:"-"`. Documentar como paralelo a `RawFeedbackOnly`. Actualizar el comentario de la struct.
- [ ] Auditar `stderrBuf` en `runValidator`: hoy el `validatorDoneMsg` no propaga el stderr capturado (solo `RawStdout`). Agregar `RawStderr string` al msg + propagar desde `stderrBuf.String()` (paralelo a `stdoutCopy`). Necesario para que el helper pueda mirar stderr.
- [ ] Tests unitarios en `validator_test.go` (5 casos arriba). Reusar el patron de `TestParseVerdict_*` (inline, sin testdata).
- [ ] Test en `running_test.go`: feedback de infra-fail wrappeado NO se filtra como raw fallback y se prependea via `mergeFeedbackIntoPayload`.
- [ ] Actualizar el comentario del switch en `runValidator` (validator.go:458-462) para reflejar el nuevo comportamiento ("exit != 0 -> buildInfraFailFeedback construye feedback accionable con stdout/stderr; fallback ultimo a validator exit N").
- [ ] Documentar el comportamiento en el doc del runner. Hoy no existe `docs/pipeline-execution-flow.html` en el repo (verificar con `find . -name "pipeline-execution-flow*"`); si no esta, dejar el doc del comportamiento en el comment block del archivo + en CHANGELOG si aplica. NO crear docs nuevos sin senial del repo.
- [ ] Build + test suite local: `go build ./...` y `go test ./internal/runner/...`.

## Archivos afectados

- `internal/runner/validator.go` - modificar - agregar `buildInfraFailFeedback`, `extractStreamJSONErrorMessage`, campo `InfraFail bool` en `Verdict`, propagar `RawStderr` en `validatorDoneMsg`, reemplazar la rama `case exitCode != 0` del switch.
- `internal/runner/validator_test.go` - modificar - 5 tests unitarios nuevos para `buildInfraFailFeedback` (codex error event, fallback tail, empty outputs, truncamiento, wrapping).
- `internal/runner/running.go` - posiblemente modificar - solo si el handler de `validatorDoneMsg` necesita leer `InfraFail` para persistirlo en el manifest (`last_feedback_kind`). Si no se decide persistirlo en este PR, el archivo no se toca y el flag queda dormido en el msg para futuro.
- `internal/runner/running_test.go` - modificar - test que valida que `mergeFeedbackIntoPayload` prependea el feedback de infra-fail (no lo filtra).
- `internal/runner/parser/codex.go` - sin tocar - la helper de error event vive en `runner/validator.go` para no expandir el contracto provisional del parser.

## Riesgos

- **Prompt injection via stdout del validator**: el `error.message` o las ultimas lineas de stderr pueden contener texto manipulado por el modelo del validator (ej. instrucciones falsas tipo "ignore previous prompt"). Mitigacion: el wrapping prepende un contexto explicito que aclara que el contenido es output del validator, no instrucciones; truncar a 4 KB acota el blast radius; `mergeFeedbackIntoPayload` ya envuelve con `--- FEEDBACK del validator --- ... --- FIN FEEDBACK ---`.
- **Role-confusion (replay del bug original)**: si el feedback incluye `verdict: approve` (porque el modelo del validator lo emitio antes del crash), el modelo del step puede malinterpretarlo. Mitigacion: el wrapping anti role-confusion ("no por un problema del trabajo. Reintenta manteniendo el output") empuja al step a no pivotar.
- **Length explosion**: el stdout completo de un validator stream-json puede ser MB. Mitigacion: cap duro 4 KB DESPUES del wrapping. Reusar la constante de `truncateForRecord`.
- **CLI no-codex en el path 1**: la heuristica `cli == "codex"` para `extractStreamJSONErrorMessage` deja claude/gemini/opencode con fallback de "ultimas N lineas". Aceptable: la asuncion 2 del spec ya lo cubre. Si en el futuro queremos error.message para claude stream-json, ampliar la helper (no tocar el contrato).
- **Independencia con #114 (codex via stdin)**: este plan no depende de #114 ni lo bloquea. La spec lo aclara: los runs reales que dispararon el bug (context overflow del validator codex) se mitigaran cuando #114 reduzca el payload via stdin, pero el feedback accionable es valioso aun asi para timeouts, crashes y spawn errors.
- **Compat manifest**: agregar `RawStderr` al `validatorDoneMsg` y `InfraFail` al `Verdict` son cambios in-memory. El YAML persistido en `verdict.yaml` (`VerdictRecord`) no cambia su shape porque ambos campos usan tag `yaml:"-"`.

## Out of scope

- Nueva clave `on_validator_infra_fail: rerun | pause | abort` en el bloque `validator` del step (asuncion 5 del spec). Queda como follow-up: requiere tocar el schema del wizard + manifest + el handler de `validatorDoneMsg` para ramificar segun la politica. Este PR solo arregla el feedback bajo el comportamiento actual (siempre rerun).
- Ampliar `parser/codex.go` a un parser stream-json completo (extraer `agent_message`, `tool_use`, etc). Hoy delega en `Raw()` y este plan no lo cambia: la helper de error event vive en `runner/validator.go` para no atar el rollout a un refactor del parser.
- Error message extraction para validators no-codex (claude, gemini, opencode). Cae al fallback de "ultimas N lineas", que la spec ya valido como minimo viable.
- Persistir `last_feedback_kind` en el manifest. El flag `Verdict.InfraFail` queda disponible para un PR futuro que lo consuma; este plan no agrega el campo al manifest schema.
- Tocar el spec #114 (codex stdin) o cualquier otro flujo: este PR es scoped a la rama exit != 0 del validator.

## Asunciones tecnicas validadas

1. `runValidator` (`internal/runner/validator.go:468-701`) ya captura stdout y stderr en `stdoutBuf` y `stderrBuf` (lineas 616, 644-647). Solo falta propagar `stderrBuf.String()` al msg + consumirlo en `buildInfraFailFeedback`.
2. El switch que decide el feedback final esta en `validator.go:676-683`. El path `case exitCode != 0` (linea 679-680) es el unico que setea `"validator exit %d"` y se reemplaza por una llamada a `buildInfraFailFeedback`.
3. `Verdict.RawFeedbackOnly` (validator.go:75) y `LastFeedbackRawOnly` (running.go:679) ya existen como precedente para flags `yaml:"-"` que no se persisten. `Verdict.InfraFail` sigue el mismo patron.
4. `mergeFeedbackIntoPayload` (running.go:746) y `isRawVerdictFallback` (running.go:728) NO necesitan cambios: el feedback wrappeado de infra-fail (`"El validator no pudo evaluar..."`) nunca matchea el patron `^verdict: <token>$`.
5. Stream-json codex emite `{"type":"error","message":"..."}` (asuncion 1 del spec, observada en `step-02.validator.01.stdout.log` del run real). `json.Unmarshal` sobre el shape `struct { Type string; Message string }` alcanza para extraer el message.
6. Cap de 4 KB es consistente con `truncateForRecord` (validator.go:397-403) que ya se aplica al `RawStdout` persistido en `verdict.yaml`. Reusar la constante mantiene una sola fuente de truth.
7. El doc `docs/pipeline-execution-flow.html` mencionado en el spec no existe en el repo (verificable con `find . -name pipeline-execution-flow*`); la documentacion del comportamiento se mantiene en los comments del archivo + CHANGELOG si aplica. No crear docs nuevos.

---

_Plan generado manualmente con harness profile a partir de #115._
