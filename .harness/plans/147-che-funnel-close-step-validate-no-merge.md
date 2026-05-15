# Plan: che-funnel close step valida sin mergear

Refs #147 - https://github.com/chichex/che/issues/147

## Contexto

El pipeline default `che-funnel` (embebido en `internal/wizard/embedded/che-funnel.yaml`) tiene 4 steps: `idea`, `explore`, `execute`, `close`. Hoy el step `close` automergea el PR con `gh pr merge --merge --delete-branch`, cierra el issue (transicionando a `che:closed`), borra branch + worktree, y postea un scoring template.

La intención es bajar la autonomía del embudo en el cierre: que el pipeline NO mergee, solo valide que el PR está listo (CI verde, sin conflictos) y deje un comment con verdict. El merge queda como decisión humana.

## Objetivo

Modificar el step `close` del pipeline default `che-funnel` para que:

1. Mantenga el fix loop existente (≤3 intentos resolviendo conflictos y esperando CI verde).
2. NO ejecute `gh pr merge`.
3. NO borre branch ni worktree.
4. NO cierre el issue asociado (el `Closes #N` en el body del PR se ejecuta solo cuando el humano mergee).
5. Aplique label `validated` al PR en lugar de `che:closed` al issue.
6. Postee siempre el comment final con verdict (passed | failed) reflejando el resultado del fix loop.

## Approach

Editar exclusivamente `internal/wizard/embedded/che-funnel.yaml`, sección del step `close` (líneas ~604-718) y su comentario de cabecera. Cambios:

- Reescribir el comentario header del step para reflejar la nueva semántica (valida-sin-mergear).
- Sacar el paso de merge (`gh pr merge --merge --delete-branch`) y el cleanup del worktree.
- Sacar la transición `che:executed → che:closing → che:closed` del issue. El issue queda como estaba al entrar al step (no se cambia de label).
- Agregar al PR el label `validated` al final del flujo feliz; crear el label con `gh label create validated --force` si no existe.
- Modificar el scoring-template comment para incluir un campo de **Validation status** (passed | failed) al inicio, y postearlo siempre — tanto si CI quedó verde como si después de 3 intentos siguió rojo/conflictivo. Mantener el resto del template (scoring 1-10, notas libres) intacto porque cumple lo mismo que ya hacía antes.
- Actualizar el bloque OUTPUT final del step para reflejar `validated: <bool>` en lugar de `merged: <bool>`.

Los otros steps (`idea`, `explore`, `execute`) no se tocan. Tampoco se toca código Go: el YAML es autocontenido y se embebe con `go:embed`.

## Pasos

- [ ] Reescribir comentario de cabecera del step `close` (líneas 604-611 actuales) describiendo el nuevo comportamiento "valida sin mergear".
- [ ] Editar el `content:` del step `close`: sacar paso 4 (merge + worktree remove) y paso 5 (transición a `che:closed`). Conservar el fix loop (paso 3 actual) tal cual.
- [ ] Agregar paso nuevo después del fix loop: `gh label create validated --force` + `gh pr edit <PR#> --add-label validated` (mutex con otros validated:* si aplica — usar `--remove-label validated:approve --remove-label validated:changes-requested --remove-label validated:needs-human` para limpieza previa).
- [ ] Modificar el scoring template: agregar campo `**Validation status:** passed|failed` al inicio del bloque, debajo del header HTML. Postearlo SIEMPRE (incluso si el fix loop no logró mergeable). Mantener scoring y notas libres.
- [ ] Actualizar el bloque OUTPUT final: cambiar `merged:` por `validated:` y `reason_if_not_merged:` por `reason_if_not_validated:`.
- [ ] Actualizar las "Reglas duras" del step para que reflejen el nuevo comportamiento (sin force-push, sin cambiar base, no mergear; si CI no pasa después de 3 intentos, postear el comment con status=failed y NO aplicar label `validated`).
- [ ] Correr `go test ./internal/wizard/...` para chequear que los embedded tests siguen pasando (solo aseguran que existe el step `close` por nombre — la edición de contenido no debería romper nada).

## Archivos afectados

- `internal/wizard/embedded/che-funnel.yaml` — modificar (reescribir step `close` y su comentario header).

## Riesgos

- **Regresión en flujos custom que extiendan `che-funnel`**: el archivo es builtin embebido; cualquier copia que el usuario haya hecho via `che pipeline copy` o similar mantiene la versión vieja. No es responsabilidad de este cambio. Asunción 10 del issue limita el scope a este YAML.
- **Loss del cleanup del worktree**: dejar el worktree `.worktrees/issue-<N>` puede acumular basura si el usuario ejecuta muchos pipelines. Es intencional según asunción 9; documentar en el comentario header que el cleanup queda manual.
- **Comportamiento ambiguo del comment "passed" cuando el step se interrumpe a mitad**: si falla por algo ajeno (CLI cae), el comment puede no postearse. Es status-quo: cualquier interrupción del pipeline ya tenía esa ambigüedad.

## Out of scope

- Pipelines custom o de scope user/project en `.che/pipelines/`.
- Cambios a otros steps (`idea`, `explore`, `execute`).
- Cambios a la lógica del runner (`internal/runner/`).
- Migración de pipelines copiados/shadowed que tengan la versión vieja del step `close`.
- Cambiar el formato del scoring (1-10 completitud / fidelidad / alineación) — se mantiene tal cual.

## Asunciones tecnicas validadas

1. El archivo a editar es `internal/wizard/embedded/che-funnel.yaml`. (Asunción 1 del issue.)
2. El step a tocar se llama `close`. (Asunción 2.)
3. La eliminación del merge implica sacar `gh pr merge --merge --delete-branch`, no borrar el step. (Asunción 3.)
4. El fix loop (≤3 intentos) se conserva. (Asunción 4.)
5. El comentario que se postea es la variante del scoring template ya existente, extendido con un campo Validation status. (Asunciones 5 y 11.)
6. El issue no se cierra desde el step; el `Closes #N` del body del PR queda intacto y se ejecutará cuando el humano mergee. (Asunción 6.)
7. El PR queda con label `validated` al terminar exitoso. (Asunción 7.)
8. Branch y worktree no se borran. (Asunciones 8 y 9.)
9. Tests del paquete `internal/wizard/...` no asertan sobre el contenido textual del step `close`, solo sobre su existencia por nombre (`embedded_test.go:54`). Esto significa que la edición de contenido es segura.

---

_Plan generado por `/hs-auto` a partir de #147 (sin labels harness)._
