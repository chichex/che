# EXEC_NOTES — Issue #55 ([#50 PR6c] Drop labels v1)

## Estado: BLOQUEADO. No se ejecutó ningún cambio de código.

## Razón

El plan de #55 dice textualmente: *"Borrar el módulo viejo cuando **todos los flows migraron**."* Las dependencias declaradas no están listas:

| Dep | Issue | Estado actual | PR asociado |
|---|---|---|---|
| PR6a — `internal/labels` v2 con shim | #64 | `che:plan` (no ejecutado) | ninguno |
| PR6b — Migración flow por flow a labels v2 | #67 | `che:plan` (no ejecutado) | ninguno |

Verificado contra `gh issue view 64/67` y `gh pr list --search "PR6a OR PR6b OR labels v2"` (cero resultados).

## Por qué no se puede ejecutar parcial

- El paquete `internal/labels` v2 (con `che:state:<step>` / `applying:` / `lock:` derivados de `pipeline.Pipeline`) **no existe** en el repo. Solo está el v1 (9 estados `che:*` hardcoded en `internal/labels/labels.go`, `lock.go`, `scope.go`).
- 19 archivos del codebase importan `internal/labels` activamente:
  - Flows: `idea`, `explore`, `execute`, `validate`, `iterate`, `close`
  - `internal/flow/stateref/stateref.go`
  - `internal/dash/gh_source.go`
  - `internal/tui/model.go`
  - `internal/startup/checks.go`
  - `cmd/unlock.go`, `cmd/migrate_labels.go`
  - tests asociados
- Borrar el módulo v1 sin haber introducido v2 ni migrado los call sites = build roto y todos los flows caídos. Esto contradice la regla del repo (`feedback_pr_refactor_workflow.md`): refactors grandes en PRs secuenciales con shims temporales.

## Qué hace falta antes de retomar #55

1. **Implementar #64 (PR6a)**: nuevo paquete (p. ej. `internal/labelsv2` o `internal/labels/v2`) con generadores de:
   - `che:state:<step>` y `che:state:applying:<step>` derivados de `pipeline.Pipeline`
   - `che:lock:<ts>:<pid-host>` con TTL/heartbeat helpers
   - el v1 sigue intacto (shim coexistente).
2. **Implementar #67 (PR6b)**: migrar uno a uno los 6 flows + dash + cmds + TUI + startup checks a leer/escribir vía v2. Tests verdes en cada PR. Posiblemente sub-PRs (1 por flow) según la nota del PRD §10.
3. **Retomar #55 (este PR)**: ejecución trivial una vez completada la migración —
   - `git rm internal/labels/labels.go internal/labels/lock.go internal/labels/scope.go` y sus tests asociados (`labels_test.go`, `lock_test.go`, `scope_test.go`, `hardcoded_test.go`).
   - Verificar con `goimports`/`go build ./...` que no quedan referencias.
   - Borrar el subcomando `che migrate-labels` viejo (`cmd/migrate_labels.go`) si la migración la asume el comando nuevo `che pipeline migrate-labels` (PR8 #62) — coordinar con #62 según el orden real de merge.

## Recomendación

- Devolver #55 a `che:plan` (o `che:idea`) y bloquearlo detrás de #64 + #67.
- Cerrar este worktree sin merge. El scope real de #55 son ~5 archivos borrados + cleanup de imports y se ejecuta en minutos una vez que #64/#67 estén mergeados; no tiene sentido abrir un PR vacío hoy.
- Alternativa: si el harness exige que este PR exista para no romper la cadena, mergear este `EXEC_NOTES.md`-only PR como recordatorio explícito y reabrir #55 cuando se desbloquee (poco recomendado — ensucia git history).
