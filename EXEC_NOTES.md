# PR6b — Migración flow por flow a labels v2: notas de ejecución

## Scope cubierto

PR mediano siguiendo la guidance del issue: "Si el alcance se vuelve XL …
priorizar 2-3 flows mas representativos y dejar los demás como TODO".
Migrados los 3 flows que cubren toda la combinatoria interesante:

1. **`internal/flow/idea`** — el más simple: solo `Ensure`/`AddLabels` +
   `--label`. Demuestra que el reemplazo `labels.CheIdea` →
   `pipelinelabels.StateIdea` es trivial cuando no hay máquina de
   estados involucrada.
2. **`internal/flow/explore`** — el primer flow con `labels.Apply(from,
   to)` real (idea → applying:explore → explore + rollback). Prueba que
   las transiciones v2 registradas en `internal/labels` funcionan
   end-to-end (Lock + Apply + rollback + tests + e2e).
3. **`internal/flow/execute`** — el flow más complejo que migra: 3
   estados de origen aceptados (idea / explore / validate_pr), rollback
   condicional según se haya creado PR o no, gate con 5 chequeos de
   estado, fromState() para resolver ambigüedad. Cubre todos los
   patrones de uso del modelo viejo.

## Coexistencia (clave del PR)

El paquete viejo `internal/labels` se preserva intacto (no se borra ni
se modifica el set de constantes / transiciones existentes). Lo único
que se agregó es:

- `internal/pipelinelabels/v2states.go` — 9 constantes públicas
  (`StateIdea`, `StateApplyingExplore`, `StateExplore`,
  `StateApplyingExecute`, `StateExecute`, `StateApplyingValidatePR`,
  `StateValidatePR`, `StateApplyingClose`, `StateClose`) que mapean los
  9 estados viejos al modelo v2 según PRD §6.c. Más constantes de step
  (`StepIdea`, `StepExplore`, `StepExecute`, `StepValidatePR`,
  `StepClose`) que actúan de fuente de verdad para los nombres.
- `internal/labels/v2_transitions.go` — `init()` que registra las 21
  transiciones del modelo viejo en `validTransitions` con keys formadas
  por strings v2. Esto permite que `labels.Apply(ref,
  pipelinelabels.StateIdea, pipelinelabels.StateApplyingExplore)` funcione
  sin tocar el código del paquete viejo. Ambos sets de keys (viejas +
  v2) coexisten en el mismo mapa — ningún colisión porque los strings
  son distintos (`"che:idea→che:planning"` vs.
  `"che:state:idea→che:state:applying:explore"`).
- Tests para ambos: `pipelinelabels/v2states_test.go` (literales fijos
  + round-trip por `Parse`), `labels/v2_transitions_test.go` (las 21
  transiciones v2 + coexistencia con las viejas).

Los flows ya migrados (`idea`, `explore`, `execute`) usan
**exclusivamente** los strings v2 en producción. Si un flow ya migrado
corre sobre un issue/PR con labels viejos (`che:plan` etc.), `Apply`
fallaría porque GitHub no encuentra el label viejo para borrar — la
conversión runtime de labels existentes vive fuera del scope de PR6b
(necesita un subcomando `migrate-labels-v2` similar al
`migrate-labels` actual; queda para PR siguiente).

## Tests verdes en cada paquete migrado

```
internal/labels                  ok (incluye nuevos v2_transitions_test.go)
internal/pipelinelabels          ok (incluye nuevos v2states_test.go)
internal/flow/idea               ok (no test files; build verificada)
internal/flow/explore            ok (fixtures en explore_test.go updated)
internal/flow/execute            ok (fixtures en execute_test.go updated)
e2e                              ok (idea/explore/execute fixtures + asserts updated)
```

`go test ./...` 100% verde tras la migración. `go vet ./...` clean.

## Pendiente (TODO para PR6c+)

Por orden de complejidad ascendente:

1. **`internal/flow/stateref`** — exporta el set `stateLabelSet` de los
   9 estados viejos (usado por `che close` para detectar issues con
   estado che:* aplicado). La migración es trivial (mapeo directo) pero
   afecta el detector de drift entre `internal/labels` y el set de
   estados que el dash/closing usan. Recomendado abordar **junto** con
   el flow de close.
2. **`internal/flow/close`** — gate ramifica por `che:executed` /
   `che:validated`, transiciones a `che:closing` → `che:closed`,
   resolución por `closingIssuesReferences` con fallback al PR. La
   migración es mecánica pero atraviesa varias funciones (`prFrom`,
   `groupCloseable`, `closeIssue`, `closeFromPR`). Tests: actualizar
   `closing_test.go` (tiene fixtures con todos los estados viejos).
3. **`internal/flow/iterate`** — dos modos (PR + plan), cada uno con su
   propio gate + Apply. PR-mode usa `che:validated` /
   `che:executing` / `che:executed`; plan-mode usa `che:validated` /
   `che:planning` / `che:plan`. También toca verdicts
   (`plan-validated:changes-requested`) que NO migran (esos labels
   sobreviven al modelo v2). Tests: `iterate_test.go` espeja la
   matriz; cuidado con la asimetría PR/plan.
4. **`internal/flow/validate`** — el más complejo. PR-mode con
   `executed → validating → validated` + rollback, plan-mode con
   `plan → validating → validated` + rollback, además aplica labels
   verdict (`validated:approve`, `plan-validated:approve`, etc.) que
   NO se migran a v2 (los verdicts conviven con la máquina de estados
   pero no son estados). El test file tiene ~1700 líneas con muchas
   matrices.
5. **Callers fuera de los flows** — todavía importan `internal/labels`
   y usan las constantes viejas:
   - `cmd/migrate_labels.go` (input del migrador viejo `status:*` →
     `che:*`; queda como referencia histórica).
   - `cmd/unlock.go` (usa `labels.Unlock` — orthogonal, NO migra).
   - `internal/dash/gh_source.go` (usa `labels.CheClosed`,
     `labels.CtPlan`, `labels.CheLocked` — algunos migrarían en PR6c
     cuando dash hable v2; otros son orthogonal).
   - `internal/tui/model.go` (usa `labels.LockedRef`, `labels.Unlock`,
     `labels.ListLocked` — todos orthogonal, NO migran).
   - `internal/startup/checks.go` (no usa estados, OK).
   - `internal/flow/stateref/stateref.go` (mapeo del set de estados;
     ver pendiente #1).

6. **Nuevo subcomando `migrate-labels-v2`** — para repos vivos con
   issues en estados viejos. Renombrar in-place `che:idea` →
   `che:state:idea` etc. en todos los issues. Análogo a `cmd/migrate_labels.go`.
   FUERA del scope del PR6b por la guidance "migración flow por flow".

7. **Borrado de las constantes viejas** (`labels.CheIdea` etc.) y de
   las keys viejas en `validTransitions` — eso es PR6c (post-migración
   de TODOS los flows). El test `TestNoHardcodedLabelsOutsideThisPackage`
   se actualizará para forbiddear los strings v2 también, garantizando
   que ningún caller invente literales.

## Decisiones / desviaciones

### 1. `pipelinelabels.State*` como `var`, no `const`

Los 9 estados v2 son `var` porque se inicializan llamando
`StateLabel("idea")` / `ApplyingLabel("explore")` — las funciones del
paquete son la fuente de verdad de los prefijos (`PrefixState`,
`PrefixApplying`). Si en el futuro alguien renombrara los prefijos sin
actualizar las constantes, los tests de round-trip
(`TestV2States_LiteralValues` + `TestV2States_RoundTripParse`) lo
detectarían inmediatamente.

### 2. `labels/v2_transitions.go` usa `init()`

En vez de incrustar el mapa v2 dentro del literal de `validTransitions`
en `labels.go` (que requeriría tocar el archivo viejo y hacerlo
depender de `pipelinelabels`), uso un archivo separado con `init()` que
muta el mapa. Esto cumple "no borrar" + "coexistencia" con un solo
file diff, y un futuro PR6c puede borrar este archivo entero (más las
keys viejas) en un solo cambio.

### 3. Mapeo de estados viejos → v2 (PRD §6.c)

| viejo            | v2                              |
|------------------|---------------------------------|
| `che:idea`       | `che:state:idea`                |
| `che:planning`   | `che:state:applying:explore`    |
| `che:plan`       | `che:state:explore`             |
| `che:executing`  | `che:state:applying:execute`    |
| `che:executed`   | `che:state:execute`             |
| `che:validating` | `che:state:applying:validate_pr`|
| `che:validated`  | `che:state:validate_pr`         |
| `che:closing`    | `che:state:applying:close`      |
| `che:closed`     | `che:state:close`               |

Notar que el modelo v2 colapsa "validate sobre plan" y "validate sobre
PR" en un solo step (`validate_pr`). El modelo viejo tampoco
distinguía a nivel de label (ambos usaban `che:validating` /
`che:validated` — la distinción venía del tipo de entidad target). En
PR6b mantenemos el mapeo 1:1; refinar a steps separados (`validate_plan`
vs. `validate_pr`) requiere extender el pipeline declarativo
(`internal/pipeline`) con dos steps de validate, fuera del scope de la
migración.

### 4. Fixtures de e2e usan literales v2 directamente

Los archivos `e2e/testdata/{explore,execute,idea}/*.json` y los asserts
en `e2e/{explore,execute,idea}_test.go` ahora contienen literales v2
(`"che:state:idea"`, `"che:state:explore"`, etc.). Como los archivos
`_test.go` y `testdata/` están excluidos de
`TestNoHardcodedLabelsOutsideThisPackage`, esto es legal y evita
arrastrar `pipelinelabels` como dependencia del harness e2e.

### 5. Marker labels NO migran

`labels.CtPlan` (y `labels.CheLocked`) son labels ortogonales — NO son
parte de la máquina de estados, y el modelo v2 no los redefine. Siguen
viviendo en `internal/labels` y siguen referenciándose como `labels.CtPlan`
/ `labels.CheLocked` desde los flows migrados. Los verdicts
(`labels.ValidatedApprove`, `labels.PlanValidatedChangesRequested`,
etc.) son la misma situación: no son estados, no migran.

## Cómo continuar (PR6c+)

Recomendación de orden:
1. Migrar `stateref` + `close` juntos (forman una unidad lógica).
2. Migrar `iterate` (más complejo pero auto-contenido).
3. Migrar `validate` (el más complejo, pero con todos los demás
   migrados antes ya tenés el patrón resuelto).
4. Migrar callers no-flow (`dash/gh_source.go` para los estados,
   dejar TUI/unlock/migrate_labels en paz).
5. Borrar el shim `v2_transitions.go` y las 9 constantes viejas en
   `labels.go` (más sus 21 transiciones); actualizar
   `TestNoHardcodedLabelsOutsideThisPackage` para que prohíba ambos
   sets de literales.
