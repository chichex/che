# PR6c — Terminar migración a labels v2: notas de ejecución

## Scope cubierto

Migración completa de los flows restantes (validate / iterate / close +
stateref + dash) a los strings v2 (`che:state:*`), guards v1-rejection
wirecados en los 3 flows + `ValidateNoMixedLabels` enganchado, subcomando
`migrate-labels-v2` listo para repos vivos. Tests + fixtures e2e migrados;
`go test ./...` 100% verde, `go vet` clean, regresión `grep` solo deja los
hits intencionales (guards + stateref legacy lookup + dash closed filter).

## Decisiones de diseño

### 1. Helper `rejectV1Labels` por-flow, no centralizado

Cada uno de los 3 flows (validate, iterate, close) define su propio
`v1StateLabels` slice + helper `rejectV1Labels` con mensaje accionable
contextualizado al flow ("antes de validar", "antes de iterar", "antes
de cerrar"). Evaluamos centralizar en `internal/labels` pero cada
mensaje incluye el verbo del flow — extraer pierde la pista textual.
Patrón consistente con `gateBasic` de explore (PR6b). El helper wirea
`labels.ValidateNoMixedLabels` adentro: cualquier mezcla v1+v2 fall por
ahí antes que el loop sobre v1StateLabels, así no pisamos labels mezclados
con un v2 nuevo.

### 2. `stateref.stateLabelSet` mantiene AMBAS familias

`internal/flow/stateref/stateref.go:stateLabelSet` reconoce tanto los 9
labels v1 como los 9 v2 durante PR6c. Esto es necesario para que un PR v2
con `closingIssuesReferences` apuntando a un issue legacy v1 (típico de
repos a medio migrar) siga resolviendo al issue. Los gates de los flows
migrados rechazan los v1 con mensaje accionable, así que reconocer el
label v1 en stateref no afloja la validación — solo evita falsos negativos
en la resolución (caer al PR cuando el issue ESTÁ trackeado, solo que en
v1).

REMOVE IN PR6d: cuando ya no haya repos sin migrar, el set se reduce a
solo v2.

### 3. Dash `applyLabels`: prefix-aware con `v2StatusByLabel` map

El parser ramifica por:
1. `che:state:applying:*` o `che:state:*` (v2) → mapeo via
   `v2StatusByLabel` a uno de los 9 strings canónicos del kanban
   (idea/planning/plan/.../closed). El orden de chequeo es
   `PrefixState` (que cubre ambos casos) primero; el TrimPrefix correcto
   se elige por presencia de `PrefixApplying`.
2. `che:*` (v1 legacy) → fallback que preserva el comportamiento
   pre-PR6c (TrimPrefix → idea/planning/etc).
3. `che:locked` se intercepta arriba con prioridad (es marker).

El mapping es necesario porque los strings de columna del kanban del dash
(`idea`, `planning`, etc) no se cambian en PR6c — preflight.go, loop.go,
model.go esperan esos strings literales y ramifican por ellos. El
modelo v2 los preserva via mapping en vez de cascadear el cambio a 8
archivos del dash que están bien.

Decisión sobre `validate_pr` v2 → `validating`/`validated` v1:
preservamos las columnas legacy del kanban porque el modelo v1 colapsaba
plan-validate y PR-validate en ese mismo bucket también — semánticamente
no perdemos información.

### 4. Dash `fetchClosedIssues` consulta AMBAS familias

Antes filtraba por `che:closed` (v1) único. Ahora hace dos `gh issue
list --state closed --label X`, una por v1 y otra por v2, y deduplica
por número. Así repos a medio migrar (algunos issues cerrados con v1,
otros con v2) muestran el set completo en la columna `closed`.

REMOVE IN PR6d: dejar solo la query v2 cuando el v1 deje de existir.

### 5. `migrate-labels-v2` aplica por-issue, no a nivel repo

A diferencia de `migrate-labels` (renombra labels via `gh label edit`),
v2 hace remove + add por cada issue. Razón: los strings v1 → v2 son
totalmente distintos (`che:plan` vs `che:state:explore`), así que un
rename a nivel repo no funciona — son dos labels distintos. Además
preservamos los labels v1 en el repo (pueden referenciarse desde
issues cerrados o ramas viejas) hasta el cleanup de PR6d.

Implementación: ensure label v2 en el repo (idempotente) → DELETE v1
(tolerante a 404, idempotente) → POST v2 (idempotente en GitHub). Si
todo falla, paramos para no dejar labels parciales.

### 6. Caveat de `che:validating` documentado pero no resuelto

v1 colapsaba validate-de-plan y validate-de-PR en `che:validating`. v2
solo tiene `applying:validate_pr`. El subcomando mapea
`che:validating` → `che:state:applying:validate_pr` y emite un warning
en stdout para que el operador sepa que puede ser semánticamente
impreciso si el run en curso era plan-validate. En la práctica
`validating` es transitorio (solo aparece durante un run) así que
encontrarlo en migración bulk es raro. La alternativa "saltarlo y
dejarlo manual" pierde info; la alternativa "no migrarlo" deja el repo
con `che:validating` huérfano que ningún flow v2 entiende. El warn +
mapping aproximado es el menos malo.

## Lo que no se hizo (pendiente para PRs siguientes)

- Borrar las constantes v1 viejas en `internal/labels/labels.go` y las
  21 keys viejas en `validTransitions`. Eso es **PR6d**, después de
  mergear este. Borrar también el shim `internal/labels/v2_transitions.go`
  entero, las 4 listas `v1StateLabels` por flow, las ramas legacy de
  `applyLabels` en dash y el segundo query de `fetchClosedIssues`. La
  guía de cuáles archivos tienen `REMOVE IN PR6d:` está embebida en los
  comentarios de los archivos respectivos.
- Update del test `TestNoHardcodedLabelsOutsideThisPackage` para que
  prohíba los strings v1 nuevos (post-cleanup). Hoy permite ambos
  durante la transición.

## Tests verdes

```
internal/flow/validate    ok (incluye TestPullRequest_PRLabelNames migrado)
internal/flow/iterate     ok (incluye TestRunPRGate_StateFromIssue migrado)
internal/flow/close       ok (incluye TestCloseFromStateRes migrado)
internal/flow/stateref    ok
internal/dash             ok (los tests existentes pasan; no se agregan nuevos
                              porque applyLabels seguía funcionando con v1
                              y ahora también con v2)
cmd                       ok (TestMigrateV2_*, 8 casos cubriendo los 9
                              labels, idempotencia, dry-run, mixed,
                              short-circuit-resilient, output)
e2e                       ok (incluye 3 nuevos tests v1-rejection: validate,
                              iterate, close — paralelo al de explore)
```

`go build ./...` clean, `go vet ./...` clean, `go test ./...` 100% verde.

`grep -rn 'labels\\.\\(CheIdea\\|...\\)' internal/flow internal/dash` solo
muestra los hits intencionales (guards + stateref legacy lookup + dash
closed filter); todos comentados con `REMOVE IN PR6d` para señalizar el
cleanup.
