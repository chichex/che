# Plan: codex CLI via stdin para evitar E2BIG en validators con prompt grande

Refs #114 - https://github.com/chichex/che/issues/114

## Contexto

El spawn del CLI `codex` arma argv como `[]string{"exec", "--json", step.Content}` en `internal/runner/spawn.go:139-143`. `step.Content` llega ya interpolado por `interpolateInput(step.Content, payload)` desde `defaultSpawnCmd` (spawn.go:94-95), o sea prompt completo + payload inline cuando el content embebe `{{INPUT}}`. Cuando el payload supera ARG_MAX del kernel (~256 KB a 1 MB en macOS) la llamada `fork/exec` falla con `argument list too long` (E2BIG) y el step muere antes de empezar.

El bug es disparado por validators del che-funnel: el prompt del validator suele tener ~200-300 lineas + `{{INPUT}}` reemplazado por el OUTPUT del step previo. Un OUTPUT de >= 256 KB (ej. respuesta de claude con varios PRs creados) basta para volar el argv. El validator del step `execute` reusa `defaultValidatorSpawnCmd` (validator.go:437-439), que delega a `defaultSpawnCmd` — el bug es el mismo. Mismo patron latente para cualquier step codex (no solo validators) con payloads grandes.

Origen real: run `che-funnel/2026-05-10T20-49-40` step-03 validator loop 1, OUTPUT de 678 KB → verdict literal `spawn error: fork/exec /opt/homebrew/bin/codex: argument list too long`. El step reintento con un OUTPUT mas chico (RUN-02 = 26 KB) y entro en argv por casualidad — iteracion accidental y costosa que no debe ocurrir.

`codex exec --help` confirma soporte de stdin: "If not provided as an argument (or if `-` is used), instructions are read from stdin. If stdin is piped and a prompt is also provided, stdin is appended as a `<stdin>` block". Es decir: omitiendo el argumento posicional, codex toma todo el prompt de stdin sin tope practico de tamano.

## Objetivo

Que steps y validators con CLI=codex y `{{INPUT}}` inline acepten payloads >= 512 KB sin E2BIG, pasando el prompt interpolado por stdin en lugar de argv. Sin regresion para claude/gemini/opencode.

## Approach

Cambio acotado en `internal/runner/spawn.go`: dividir el path de `defaultSpawnCmd` segun CLI para `codex`, manteniendo el resto idem.

- `buildSpawnArgs` para `CLI=codex` devuelve `[]string{"exec", "--json"}` (sin el content como tercer argv). Los otros CLIs no cambian.
- `defaultSpawnCmd` detecta `step.CLI == "codex"` y setea `cmd.Stdin = strings.NewReader(stepForArgs.Content)` (el content YA interpolado por `interpolateInput`). El payload pelado deja de viajar por stdin para codex — el content interpolado lo embebe via `{{INPUT}}`, asi codex recibe exactamente el mismo prompt que antes pero ahora por stdin.
- Para los otros CLIs (claude/gemini/opencode) la rama default sigue como hoy: content por argv + payload por stdin.

`defaultValidatorSpawnCmd` no se toca: reusa `defaultSpawnCmd` y el fix se propaga automaticamente al spawn del validator.

Tests:

- `spawn_test.go` cubre: (a) `buildSpawnArgs` con codex no incluye el content en args, (b) `defaultSpawnCmd` con codex setea `cmd.Stdin` con el content interpolado (no con el payload pelado), (c) claude/gemini/opencode siguen pasando content por argv (no regresion), (d) un step codex con `step.Content` interpolado a >= 512 KB arma el cmd sin error y los args quedan acotados (no hay E2BIG en build — el spawn real se prueba con un fake si hace falta).
- Nada cambia en e2e: el harness e2e existente del che-funnel sigue verde porque el contrato observable de codex (prompt + stdin) se preserva — solo cambia el canal por el que viaja el prompt.

## Pasos

- [ ] Modificar `buildSpawnArgs` en `internal/runner/spawn.go`: case `"codex"` devuelve `[]string{"exec", "--json"}`.
- [ ] Modificar `defaultSpawnCmd` en `internal/runner/spawn.go`: si `step.CLI == "codex"`, setear `cmd.Stdin = strings.NewReader(stepForArgs.Content)`; el resto de los CLIs queda con `cmd.Stdin = strings.NewReader(payload)` como hoy.
- [ ] Actualizar comentario doc de `defaultSpawnCmd` para explicitar que codex viaja por stdin (no por argv) y la razon (E2BIG, #114).
- [ ] Agregar test unit en `spawn_test.go`: `TestBuildSpawnArgs_CodexExcludesContent` valida que `buildSpawnArgs` para codex devuelve exactamente `["exec", "--json"]`.
- [ ] Agregar test unit en `spawn_test.go`: `TestDefaultSpawnCmd_CodexStdinHasContent` valida que `cmd.Stdin` (cast a `*strings.Reader`) contiene el content interpolado y NO el payload pelado.
- [ ] Agregar test unit en `spawn_test.go`: `TestBuildSpawnArgs_NonCodexCLIs_NoRegression` valida que claude/gemini/opencode mantienen el content en argv.
- [ ] Agregar test unit en `spawn_test.go`: `TestDefaultSpawnCmd_CodexLargePayload` arma un payload sintetico de >= 512 KB con `{{INPUT}}` en el content, llama a `defaultSpawnCmd`, y verifica que (a) no hay error de build, (b) `len(cmd.Args)` es chico (no embebe el payload), (c) el stdin reader contiene el payload completo.
- [ ] Build + suite local del paquete: `go build ./...` y `go test ./internal/runner/...`.

## Archivos afectados

- `internal/runner/spawn.go` - modificar - rama `codex` en `buildSpawnArgs` (sin content en argv) y en `defaultSpawnCmd` (stdin = content interpolado). Actualizar comentarios doc.
- `internal/runner/spawn_test.go` - modificar - agregar 4 tests nuevos descritos arriba.
- `internal/runner/validator.go` - sin cambios - `defaultValidatorSpawnCmd` reusa `defaultSpawnCmd`, hereda el fix automaticamente.

## Riesgos

- El argumento posicional ausente cambia el shape de invocacion de codex: si alguna version vieja del CLI no soporta lectura desde stdin sin `-` explicito, el spawn cuelga esperando EOF. Mitigacion: el caller cierra el stdin reader cuando se agota (`strings.NewReader` ya emite EOF al terminar). Si igualmente hay duda, podemos usar `[]string{"exec", "--json", "-"}` como variante explicita (`-` es marker estandar de stdin) — definir antes del merge segun `codex exec --help` de la version del usuario.
- El payload pelado deja de viajar por stdin para codex. Si algun prompt codex NO usa `{{INPUT}}` (poco probable hoy en `internal/wizard/embedded/`), el content interpolado sigue siendo identico al content original y el payload se pierde. Auditar `internal/wizard/embedded/*.yaml` para confirmar que todos los steps/validators codex usan `{{INPUT}}` antes de implementar — si alguno no lo usa, hay que concatenar `content + "\n" + payload` en stdin como fallback.
- Solape con #115 (feedback accionable en validator exit != 0): #115 trata el caso donde el validator MUERE por infra-fail (incluido E2BIG) y como reportarlo al modelo. Este plan #114 elimina la fuente del fallo para codex — quedan otros casos de infra-fail (claude/gemini/opencode con argv gigante, o cualquier otro spawn error) que #115 sigue cubriendo. NO hay overlap de codigo: este PR toca solo `spawn.go`; #115 va a tocar `validator.go` parseo de exit codes. Ambos se pueden mergear en cualquier orden.
- Tests de spawn con `cmd.Stdin` necesitan castear `io.Reader` a `*strings.Reader` para inspeccionar el contenido. Es feo pero alcanza para el contrato — alternativa: setear `cmd.Stdin` via helper testeable y assert sobre el helper. Optar por la opcion mas simple que no requiera tocar mas codigo de produccion.

## Out of scope

- Aplicar el mismo patron a claude (`spawn.go:133-138`): el issue lo deja como follow-up explicito. Claude tiene context window 1M y stream-json amortigua el riesgo, pero el bug latente esta — va en otra spec aparte.
- Cambios en gemini/opencode: ningun CLI mas se toca en esta spec.
- Refactor de `interpolateInput` o del routing por CLI mas alla del case codex.
- Cambios en `cmd/fake/main.go` o en los matchers del e2e — el fake stub sigue leyendo argv + stdin como antes; solo cambia QUE recibe cada canal, no como los lee.
- E2E nuevo con payload sintetico de 512 KB: queda opcional como follow-up si quisieramos cubrir el path completo runStep + spawn. El test unit cubre el contrato relevante (no E2BIG en build, stdin con el content correcto).

## Asunciones tecnicas validadas

1. `codex exec` (version del usuario en `/opt/homebrew/bin/codex`) lee el prompt completo desde stdin cuando se omite el argumento posicional o se pasa `-`. Validado contra `codex exec --help` segun la spec; el plan asume omitir el arg posicional, con `-` como variante explicita si fuera necesario.
2. Todos los steps/validators codex en `internal/wizard/embedded/*.yaml` usan `{{INPUT}}` en su content, asi que tras `interpolateInput` el payload queda embebido y NO hace falta mantenerlo en stdin tambien para codex. (Auditoria antes de implementar: `grep -E 'cli: codex|validator:' internal/wizard/embedded/*.yaml -A 5`.)
3. `defaultValidatorSpawnCmd` (validator.go:437-439) reusa `defaultSpawnCmd` sin reimplementar nada; el fix se hereda automaticamente.
4. Los CLIs distintos de codex (claude/gemini/opencode) no se tocan en esta spec — sus argv y su stdin quedan idem.
5. Tests unitarios cubren el contrato sin necesidad de spawn real: assert sobre `cmd.Args` y `cmd.Stdin` (cast a `*strings.Reader`).
6. El fake `cmd/fake/main.go` no necesita cambios: sigue siendo polimorfico sobre argv + stdin como antes.

---

_Plan generado manualmente con harness profile a partir de #114._
