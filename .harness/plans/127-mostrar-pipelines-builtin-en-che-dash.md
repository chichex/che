# Plan: Mostrar pipelines builtin (che-funnel) en che dash

Refs #127 - https://github.com/chichex/che/issues/127

## Contexto

Hoy `che dash` enumera pipelines leyendo solo `~/.che/pipelines/*.yaml` desde `loadPipelinesFromDir` en `internal/dash/pipelines.go:104`. Los builtins shippeados con el binario (hoy unicamente `che-funnel`, expuestos por `wizard.Builtins()` en `internal/wizard/embedded.go:32`) nunca llegan al endpoint. El usuario nuevo abre el dash, no ve nada y se confunde — el wizard CLI los muestra con chip `[default]` (`internal/wizard/list.go:165`), creando expectativa de paridad.

## Objetivo

Mergear los builtins en las respuestas del dash (`GET /api/pipelines` y `GET /api/pipelines/:slug`) reusando `wizard.Builtins()` / `wizard.BuiltinBySlug()`. El JSON gana un campo additive `builtin: true` para que el front pueda chipear `[default]`. La regla de overlap es on-disk-wins: si existe `~/.che/pipelines/<slug>.yaml`, gana esa copia (consistente con el copy-on-edit del wizard) y `builtin` no se setea. El dash NO escribe nada al FS.

## Approach

1. Centralizar la merge logic en un helper privado nuevo (`mergeBuiltinsAndDisk`) en `internal/dash/pipelines.go`. Toma el dir on-disk, llama a `loadPipelinesFromDir` + `wizard.Builtins()`, hace dedupe por slug priorizando on-disk, y devuelve un slice ordenado por slug. Si `wizard.Builtins()` retorna error (bug del binario), se loguea con prefijo `[dash]` y se sigue solo con on-disk — no romper el endpoint.
2. Agregar campo `Builtin bool` con tag `json:"builtin,omitempty"` a `pipelineJSON` y `pipelineDetailJSON`. `omitempty` mantiene el JSON estable para items user-created (no se renderiza `false`).
3. Refactor `handleListPipelines` para consumir el merger y setear `Builtin` por item.
4. Refactor `getPipelineDetail` para que cuando el path on-disk no exista (`os.IsNotExist`), intente `wizard.BuiltinBySlug(slug)`. Si existe, responde el detail completo con `Builtin: true`. Si no, mantiene el 404 actual. Si hay error sistemico de `wizard.Builtins()`, log + 500 (mismo patron que el load on-disk).
5. Tocar el front (`internal/dash/assets/dash.html`): en el `Row` del `Sidebar` agregar un chip `[default]` cuando `p.builtin === true`, y replicarlo en el header de `PipelineDetail`. Reusar la convencion visual de `Badge` o un span equivalente — mantener el cambio chico y additive.
6. Tests en `internal/dash/pipelines_test.go`: builtin-only sin disco, builtin+on-disk override (on-disk gana, sin `builtin:true`), detail de builtin sin override, detail de builtin overrideado, y last_run populado para builtin cuando hay runs en `~/.che/runs/<slug>/`.

## Pasos

- [ ] Agregar helper `mergeBuiltinsAndDisk(dir string) []pipelinePair` en `internal/dash/pipelines.go` que combina builtins + on-disk con on-disk-wins, ordenado por slug. Loguear y degradar si `wizard.Builtins()` falla.
- [ ] Agregar campo `Builtin bool` con tag `json:"builtin,omitempty"` a `pipelineJSON` y `pipelineDetailJSON`.
- [ ] Refactor `handleListPipelines` para usar `mergeBuiltinsAndDisk` y propagar `Builtin`.
- [ ] Refactor `getPipelineDetail` para fallback a `wizard.BuiltinBySlug` cuando el YAML on-disk no existe; setear `Builtin: true` y conservar el 404 cuando tampoco hay builtin.
- [ ] Actualizar `internal/dash/assets/dash.html`: chip `[default]` en `Sidebar.Row` y en el header de `PipelineDetail` cuando `p.builtin === true`.
- [ ] Agregar tests en `internal/dash/pipelines_test.go`: builtin-only, builtin+on-disk override, detail builtin sin override, detail builtin overrideado, last_run de builtin.

## Archivos afectados

- `internal/dash/pipelines.go` - modificar - introducir merger, agregar campo `Builtin`, refactor de los dos handlers.
- `internal/dash/pipelines_test.go` - modificar - agregar 4-5 tests cubriendo merge, override y detail.
- `internal/dash/assets/dash.html` - modificar - agregar chip `[default]` en Row y header detail.

## Riesgos

- `wizard.Builtins()` carga el embed cada vez. Aceptable para v1: el binario lo parsea en startup tambien. Si el dash hot-pathea esto, una pequena cache en proceso es trivial — fuera de scope para este PR salvo que un test marque overhead notable.
- Tests existentes de `pipelines_test.go` podrian asumir "lista vacia cuando dir vacio". Hay que revisarlos: ahora la lista contiene `che-funnel` siempre que `wizard.Builtins()` funcione. Ajustar expectativas o forzar un dir donde `wizard.Builtins()` no se llame solo si fuera posible — en general los tests deben actualizarse para reflejar la nueva semantica.
- El front actual no conoce `builtin`. El cambio es additive (`omitempty`), pero el chip nuevo debe degradar grace si el campo no llega (ej. dashboards de versiones viejas pegando contra binarios nuevos no aplica aca, pero conviene chequear que el render no rompa si `p.builtin` es undefined).

## Out of scope

- Agregar nuevos builtins ademas de `che-funnel`.
- Exponer la accion de copy-on-edit desde el dash (sigue siendo del wizard).
- Cambiar el contrato o la fuente de `last_run`.
- Cachear en memoria los builtins parseados.

## Asunciones tecnicas validadas

1. `wizard.Builtins()` y `wizard.BuiltinBySlug()` son la fuente canonica y se pueden importar desde `internal/dash/`.
2. El campo `Builtin bool` con `omitempty` es backward compatible — el front lo ignora hasta que se actualice.
3. Sort estable por slug es suficiente; no se requiere "builtins primero" como politica.
4. Los tests del dash usan dirs temporales bajo `t.TempDir()`; los nuevos tests pueden hacer lo mismo y combinarlos con los builtins reales del binario (no hace falta inyectar fakes para `wizard.Builtins()`).
5. `last_run` de un builtin se resuelve por slug en `~/.che/runs/<slug>/` igual que para user-created; no requiere logica nueva.
6. `getPipelineDetail` solo necesita fallback al builtin cuando `os.IsNotExist`; otros errores de carga siguen devolviendo 500.

---

_Plan generado por `/hs-auto` a partir de #127._
