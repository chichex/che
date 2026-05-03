# PR7 — Lock + heartbeat + auto-creación de labels: notas de ejecución

## Scope cubierto

Cuatro features cohesivas pero independientes (PRD §6.a, §6.b, §6.d, §8):

1. **Lock con heartbeat + TTL** (`internal/lock`): label
   `che:lock:<unix-nano>:<pid>-<host>` con TTL=5min, heartbeat=60s,
   detección de stale + race-loss best-effort. Reusa el formato y el
   parser ya definidos en `internal/pipelinelabels` (PR6a).
2. **Auto-creación de labels** (`internal/labels.ExpectedForPipeline`,
   `EnsureForPipeline`): set completo (estados, applying, verdicts, marker)
   computado desde un pipeline declarativo.
3. **Subcomando `che init-labels`** (`cmd/init_labels.go`): opt-in para
   CI / repos nuevos. Soporta `--pipeline <name>` y `--dry-run`.
4. **Audit log** (`internal/auditlog`): comment dedicado en el issue raíz
   con marker `<!-- claude-cli: skill=audit-log -->`. Idempotente: edita
   vía `gh api PATCH .../comments/<id>` en vez de crear duplicados.

Resolución del issue raíz vía `closingIssuesReferences` (PRD §6.a) ya
existía en `internal/flow/stateref` desde PR6b — reutilizada, no
duplicada.

## Decisiones de diseño

### 1. Feature flags vs. wireup duro

Las dos primitivas runtime (heartbeat lock + audit log) están detrás de
**env vars opt-in**:

- `CHE_LOCK_HEARTBEAT=1` activa el lock con heartbeat en los 5 flows
  (explore / execute / iterate-pr / iterate-plan / validate-pr /
  validate-plan / close).
- `CHE_AUDIT_LOG=1` activa la escritura del audit log en cada
  transición exitosa (y en rollbacks).

Razón: cualquier flow con un `gh api ...` nuevo en runtime rompe
inmediatamente los e2e tests existentes (el harness exige matchers
explícitos para cada llamada). Wirear duro implicaría tocar 50+ tests
con expectativas de gh y aumentar el área de superficie de PR7 más
allá de su scope. Con env-flag:

- Default off → 100% retro-compatible. Cero cambios a tests existentes.
- Activable en prod / CI cuando esté validado el formato del label y la
  carga de la API (1 PATCH por transición).
- El día que sea estable, un follow-up invierte el default (o borra
  el flag) sin tocar la lógica.

### 2. `internal/lock` no depende de `internal/labels`

`internal/labels` ya importa `internal/pipelinelabels`. Si `lock`
importara `labels` y los flows usaran `labels.AcquireHeartbeat(...)`
estaríamos a un paso del ciclo `labels → lock → labels`. La separación:

- `pipelinelabels`: fuente de verdad del FORMATO del label (LockLabelAt,
  Parse). Sin estado, sin gh.
- `lock`: adquisición/liberación + heartbeat con `gh api` propio. Hooks
  REST inyectables para tests.
- `labels`: `Lock`/`Unlock` para el binario `che:locked` (legacy mutex).
  Sin tocar.

Tests del lock hacen 100% mocking via `Options.AddLabel/DelLabel/
ListLabels/EnsureLabel/Now`. Cero shell-out a `gh`, cero `time.Sleep`.

### 3. `internal/auditlog` también es opt-in y stub-friendly

Mismo patrón: un struct `Options` con hooks para tests y `Enabled()`
gate. Marker `<!-- claude-cli: skill=audit-log -->` reutiliza la
convención del paquete `internal/comments` pero con namespace propio
(skill=audit-log vs. flow=...) para no confundirlo con un comment de
un flow.

Idempotencia por marker (no por timestamp): si dos runs paralelos
creyeran que no hay comment y crearan dos, el segundo Append los va a
ver y editar el primero — pero como no resolvemos race en el create,
queda el comment fantasma. Caso patológico (>2 humanos lanzando che
contra el mismo issue al mismo segundo); no se vio en producción.

### 4. `runguard` package centraliza el wireup

Para no duplicar 20+ líneas idénticas en cada uno de los 5 flows,
`internal/flow/runguard` expone `AcquireLock`, `ReleaseLock`,
`AuditAppend`. Cada función chequea su feature flag y devuelve no-op si
está off. Los flows cambian solo 4 líneas en cada Run().

Razón: este es el patrón que SI los features pasan a default-on, se
borra el if-flag de adentro de runguard y los flows quedan ya wireados.
Si se borra, los flows ya están conectados.

### 5. Race window de Acquire es post-check, no CAS

GitHub no expone CAS sobre labels. El protocolo es:

1. List labels → no hay lock vivo.
2. POST nuestro label.
3. Re-list → ¿hay otro lock vivo distinto al nuestro?
4. Si sí: DELETE el nuestro y devolver ErrAlreadyLocked.

Esto reduce la ventana de carrera pero no la elimina (dos procesos
pueden re-listar en paralelo y ambos ver el label del otro). En
escenarios reales (humano lanzando manualmente, dash auto-loop con
concurrency=1) la window es irrelevante. Documentado en docstring.

### 6. Resolución del issue raíz NO se duplicó

PR6b dejó `internal/flow/stateref/Resolve(prRef, prLabels, closingIssues)`
funcionando. Los flows que arrancan sobre PR (validate-pr, iterate-pr,
close) ya pasan por `pr.ResolveStateRef()`. Para el lock con heartbeat,
el caller le pasa `stateRes.Ref` (el issue raíz si está linkeado, el PR
si no) — alineado con donde van las transiciones. Para el audit log,
le pasa `stateRes.IssueNumber` (o `pr.Number` si no linkeado) como
target — el comment va al issue raíz cuando está disponible.

## Cobertura de tests

| Paquete | Cobertura |
|---------|-----------|
| `internal/lock` | 8 tests: fresh acquire, contended (vivo), stale evict, heartbeat refresh, release idempotente, TTL boundary, ref formats, add-failure preserva current label. Mock 100%, sin shell-out. |
| `internal/auditlog` | 5 tests: create new, edit existing, render entry (3 shapes), zero-At fills now, trailing-newlines no acumula. Mock 100%. |
| `internal/labels` | 4 tests para `ExpectedForPipeline`: golden bit-perfect sobre `pipeline.Default()`, includes-all-verdicts, alpha order, no-lock-labels. |
| `internal/flow/runguard` | 4 tests: acquire/release/audit son no-op silencioso con feature off; nil-safe. |
| `cmd/init-labels` | 2 tests: dry-run output, pipeline-not-found error. |
| `e2e/init_labels_test.go` | 3 tests: command-exists, dry-run output, real-run con `gh label create` matchers. |
| `e2e/heartbeat_lock_test.go` | 1 test: con `CHE_LOCK_HEARTBEAT=1` y un lock vivo en el issue, `che explore` aborta con exit 3 y mensaje accionable. |

## Lo que no se hizo (pendiente para PRs siguientes)

1. **Default-on**: ambos features arrancan default-off. Cuando estén
   validados en repos reales, un follow-up corto (~10 LoC) puede
   invertir los defaults o borrar las funciones `Enabled()`/
   `HeartbeatEnabled()` directamente.

2. **Audit en rollbacks de execute**: el `cleanupLocal` de
   `internal/flow/execute` es una closure compleja con 3 ramas de label
   handling (executedApplied / prCreated / default rollback). No fue
   instrumentado con `runguard.AuditAppend`; los happy-paths sí.
   Refactor a callbacks inyectables ya estaba pendiente desde PR6b
   (TODO en docstring de cleanupLocal). Cuando se haga ese refactor,
   sumar audit ahí.

3. **Comment del audit log: cap en paginación**: `gh api .../comments`
   pagina. Default per_page=30; subimos a 100 para que `Append` lo
   encuentre con un solo hit. >100 comments en un issue → el siguiente
   Append crea un comment fantasma. Caso patológico; queda como TODO
   con comentario en el código.

4. **Subcomando `che init-labels` con `--repo <path>`**: hoy el
   subcomando depende del cwd para detectar el repo root. Para CI puede
   ser útil un flag explícito. Trivial: `--repo <path>` que sustituye
   `repoRootForPipeline()`.

5. **Heartbeat con context.Context**: el goroutine del heartbeat usa
   un `chan struct{}` como stop signal. Cambiar a `context.Context` lo
   alinearía con el resto del codebase (execute, validate). Refactor
   sin impacto funcional.

6. **EnsureForPipeline NO es idempotente respecto al color/descripción
   del label**: `gh label create --force` actualiza color/descripción
   solo si los pasamos. Hoy no pasamos ninguno — el label se crea con
   default color (gris) la primera vez, y subsiguientes runs no tocan
   nada. Si alguien quiere colores específicos por estado/verdict,
   sumar `--color` y `--description` per-label en EnsureForPipeline
   (tabla en `internal/labels`).

## Compatibilidad con PR6c

`labels.V1LegacyStates()` y los guards `rejectV1Labels` siguen
existiendo y se ejecutan ANTES del lock con heartbeat. Si un repo todavía
tiene labels v1, el guard falla con mensaje accionable antes de tocar la
máquina nueva — exactamente como en PR6b/6c.

`stateref` reconoce v1+v2 (PR6c). El lock con heartbeat se aplica al
mismo `stateRef` que la transición — si stateref cae al PR (no había
issue linkeado o todos los issues fallaron al fetch), el lock va al PR.

## Checklist verde

```
go build ./...        clean
go vet ./...          clean
go test ./...         100% pass (cmd, e2e, internal/*)
```
