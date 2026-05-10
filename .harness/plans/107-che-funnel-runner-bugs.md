# Plan: cadena de bugs del che-funnel runner ({{INPUT}}, validator parser, feedback, events)

Refs #107 - https://github.com/chichex/che/issues/107

## Contexto

El run real `che-funnel/2026-05-10T20-02-07` exhibio 5 bugs encadenados en el runner del che-funnel:

1. `{{INPUT}}` no se interpola en `step.Content`/`step.Validator.Content`. El payload viaja por stdin pero el content sale literal y codex/claude tiene que adivinar.
2. El parser de verdicts (`splitVerdictBlocks` + `tryParseVerdictBlock` en `internal/runner/validator.go`) no entra a fences markdown ```yaml```; verdicts validos quedan ignorados con `no verdict block`.
3. Cuando el feedback del validator queda vacio, `tryParseVerdictBlock` setea `Feedback = "verdict: <token>"` y `mergeFeedbackIntoPayload` lo prependea al payload del rerun: ruido puro al modelo.
4. `events.jsonl` se abre con `O_TRUNC` por rerun (linea 226 de `spawn.go`): se pierde la traza de los reruns previos.
5. El prompt del validator del step `execute` en `che-funnel.yaml` exige formato `prs:` y empuja al modelo a culpar al templating en lugar de razonar sobre la salida real.

Origen tecnico ya identificado en el issue. El plan implementa los 5 fixes y agrega cobertura.

## Objetivo

Que el che-funnel corra end-to-end sobre prompts reales sin requerir cancel manual; que reruns reciban feedback util; que `events.jsonl` permita debug post-mortem; que el parser tolere verdicts en fences markdown.

## Approach

PR unico con 5 fixes acotados. Estrategia:

- Fix 1 (raiz): nueva helper `interpolateInput(content, payload string) string` invocada desde `buildSpawnArgs` (para `step.Content`) y desde `runValidator` (para `step.Validator.Content`) antes de armar args. Mantener payload por stdin como hoy. Auditar `internal/wizard/embedded/*.yaml` para confirmar que sustituir `{{INPUT}}` no rompe casos existentes.
- Fix 2: extender `splitVerdictBlocks` para que tambien extraiga el contenido de fences markdown ```yaml ... ``` (solo info-string `yaml`/`yml`/vacio aceptado) y los agregue como candidatos antes de los bloques top-level. Orden: candidatos top-level primero (compat), luego fences. `tryParseVerdictBlock` no cambia su contrato.
- Fix 3: en `tryParseVerdictBlock` sigue seteando `Feedback = "verdict: " + raw` (lo necesita el modal RP / `last_feedback`), pero agregamos un flag `Verdict.RawFeedbackOnly bool` (o helper `IsRawVerdictFallback`). `mergeFeedbackIntoPayload` consulta ese flag y NO prependea el bloque cuando el feedback efectivo es solo el raw verdict o esta vacio. Alternativa simple si preferimos no tocar el struct: heuristica en `mergeFeedbackIntoPayload` que detecte el patron `^verdict: \w+$` y trate como vacio.
- Fix 4: rotar `events.jsonl` a `events.RUN-<K>.jsonl` por loop, persistiendo `K` en el manifest del step. Mantener un symlink/archivo `events.jsonl` apuntando al activo para no romper consumidores externos. Actualizar `readPermissionDenials` para leer la rotacion activa, no toda la historia.
- Fix 5: editar el bloque `validator` del step `execute` en `internal/wizard/embedded/che-funnel.yaml`: el prompt razona sobre la salida real (stdin tras preambulo), no exige `prs:`, y emite `fail` con feedback accionable hacia el step.

Tests:

- `spawn_test.go`: caso `interpolateInput` reemplaza `{{INPUT}}` en step.Content; otro caso confirma que sin `{{INPUT}}` el content queda intacto.
- `validator_test.go`: stdout = texto markdown + fence ```yaml verdict: fail feedback: "..."``` parsea correctamente.
- `running_test.go`: `mergeFeedbackIntoPayload` con feedback `"verdict: changes_requested"` o vacio NO prependea bloque.
- e2e (nuevo o ampliado en `internal/runner` o `e2e/`): step con validator y 2 reruns; assert que los archivos `events.RUN-1.jsonl` y `events.RUN-2.jsonl` existen y contienen eventos de cada vuelta.

## Pasos

- [ ] Auditar `internal/wizard/embedded/*.yaml` (`grep -n '{{INPUT}}'`) para listar cada step que use el placeholder y confirmar que la sustitucion no rompe casos existentes.
- [ ] Implementar Fix 1: helper `interpolateInput` + invocaciones en `buildSpawnArgs` (`internal/runner/spawn.go`) y en `runValidator` antes de `step.Validator.Content` (`internal/runner/validator.go:~403-405`). Test unit en `spawn_test.go`.
- [ ] Implementar Fix 2: extender `splitVerdictBlocks` para extraer fences ```yaml ... ```. Test unit en `validator_test.go` con verdict en fence dentro de markdown.
- [ ] Implementar Fix 3: marcador `RawFeedbackOnly` (o heuristica equivalente) consumido por `mergeFeedbackIntoPayload` (`internal/runner/running.go:692`). Test unit en `running_test.go`.
- [ ] Implementar Fix 4: rotacion de `events.jsonl` a `events.RUN-<K>.jsonl` con `K` en el manifest, abrir con `O_CREATE|O_WRONLY|O_TRUNC` por archivo (no compartido). Actualizar `readPermissionDenials` (`internal/runner/permdenied.go`) para leer la corrida activa. Mantener compat de `events.jsonl` como symlink/copia del activo si los tests externos lo asumen.
- [ ] Implementar Fix 5: reformular el prompt del validator del step `execute` en `internal/wizard/embedded/che-funnel.yaml`.
- [ ] Test e2e que dispare validator + 2 reruns y verifique persistencia de eventos en ambas vueltas.
- [ ] Build + test suite local (`go build ./...`, `go test ./internal/runner/...` y la suite del paquete tocado).

## Archivos afectados

- `internal/runner/spawn.go` - modificar - integrar `interpolateInput` en `buildSpawnArgs`; rotacion de `events.RUN-K.jsonl` (Fix 1, Fix 4).
- `internal/runner/validator.go` - modificar - llamar a `interpolateInput` antes de `step.Validator.Content`; extender `splitVerdictBlocks`/`tryParseVerdictBlock` para fences markdown y marcador de raw fallback (Fix 1, Fix 2, Fix 3).
- `internal/runner/running.go` - modificar - `mergeFeedbackIntoPayload` consulta el marcador y no prependea bloque cuando aplica (Fix 3).
- `internal/runner/permdenied.go` - modificar - leer la corrida activa (`events.RUN-<K>.jsonl`) en lugar de `events.jsonl` historico (Fix 4).
- `internal/runner/manifest.go` - modificar - persistir el contador `K` por step (Fix 4).
- `internal/wizard/embedded/che-funnel.yaml` - modificar - reformular prompt del validator del step `execute` (Fix 5).
- `internal/runner/spawn_test.go` - crear o modificar - test de `interpolateInput`.
- `internal/runner/validator_test.go` - modificar - caso de verdict en fence markdown.
- `internal/runner/running_test.go` - modificar - caso de feedback raw que no se prependea.
- `internal/runner/runner_builtin_test.go` o `e2e/` - modificar/crear - e2e con validator + 2 reruns y assert de persistencia.

## Riesgos

- Fix 1 cambia el contrato del payload-path: si algun prompt embebido espera `{{INPUT}}` literal, se rompe. Mitigacion: auditar antes de implementar y dejar el placeholder solo donde se espera sustitucion.
- Fix 4 (rotacion) puede romper consumidores externos que asumen `events.jsonl` unico. Mitigacion: mantener `events.jsonl` como copia/symlink del activo al cierre del step.
- Fix 3 si se hace via heuristica (regex `^verdict: \w+$`), puede pisarse con feedbacks legitimos cortos; preferir el flag explicito `RawFeedbackOnly`.
- La suite e2e de `che` ya existente puede tardar; correr solo el subset relevante en CI local antes de push.

## Out of scope

- Refactor del `wizard/embedded` a templating mas potente (ej. Go templates con funciones); este plan solo arregla `{{INPUT}}` por replace literal.
- Rediseno del modal RP / `last_feedback` del manifest mas alla de lo que requiere Fix 3.
- Migrar `events.jsonl` a otro formato (JSONL queda).
- Tocar otros prompts de `che-funnel.yaml` mas alla del validator del step `execute`.

## Asunciones tecnicas validadas

1. Helper `interpolateInput(content, payload)` con `strings.ReplaceAll(content, "{{INPUT}}", payload)`. Sin escape adicional.
2. `splitVerdictBlocks` extrae fences via regex `(?ms)^` `` ``` `` `(?:yaml|yml)?\n(.*?)^` `` ``` `` `\s*$` y agrega cada cuerpo como candidato. Orden de evaluacion: top-level primero, luego fences (compat).
3. `Verdict.RawFeedbackOnly bool` se setea en `tryParseVerdictBlock` cuando el feedback efectivo era vacio y se cayo al fallback `"verdict: " + raw`. `mergeFeedbackIntoPayload` consulta el flag.
4. La rotacion de eventos vive en `<runDir>/<stepN>/events.RUN-<K>.jsonl`. `K` se incrementa por rerun y se persiste en el manifest del step (`runs[].events_file` o equivalente). `events.jsonl` queda como referencia al ultimo activo (symlink o copia).
5. `readPermissionDenials` lee solo el archivo activo del rerun en curso, no toda la historia.
6. Tests unitarios usan `testdata/` solo si hace falta input largo; los casos chicos van inline en el `_test.go`.

---

_Plan generado por `/hs-auto` a partir de #107._
