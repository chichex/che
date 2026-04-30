# PR5a — Spec formal del parser de markers + tests: notas de ejecución

## Estado del PR

Cubre el scope completo de #61:

- **Paquete puro `internal/markerparser`** — string in, marker out. Sin dependencias del engine ni del agent. Importable por cualquier paquete que necesite interpretar el output de un agente.
- **API pública decidida**:
  - `Marker` struct + `Kind` enum (`None`, `Next`, `Goto`, `Stop`).
  - `Parse(text) (Marker, bool)` — recorre el output completo, aplica la regex sobre la última línea no-vacía.
  - `ParseLastLine(line) (Marker, bool)` — versión single-line (caller ya tiene la línea aislada).
  - `ParseStreamJSON(stream) (Marker, bool)` — recorre NDJSON del `claude --output-format stream-json --verbose`, encuentra el último evento `result` y aplica `Parse` sobre `.result`.
- **Regex canónica del PRD §3.c**: `^\s*\[(next|stop|goto:\s*([a-z_][a-z0-9_]*))\]\s*$`. Case-sensitive estricta. Step destino con identificador go-like.
- **Default sin marker → caller resuelve**. El parser nunca devuelve `Next` por default — siempre devuelve `(None, false)` cuando no encontró marker. El motor (que sabe si la invocación falló o no) aplica el default `[next]` o `[stop]`.
- **Tests exhaustivos** (21 tests + 71 subtests, 92 asserts):
  - Markers válidos: `[next]`, `[stop]`, `[goto: foo]`, sin/con espacios después de `goto:`, identificadores con underscores y números.
  - Whitespace tolerance: leading, trailing, newlines, tabs, mezclas.
  - Case-sensitive: `[Next]`, `[NEXT]`, `[Stop]`, `[STOP]`, `[Goto: foo]`, `[GOTO: foo]`, `[goto: FOO]`, `[goto: Foo]` — todos rechazados.
  - Última línea no vacía: marker en línea intermedia ignorado, marker mezclado con prosa rechazado, marker con prosa antes en última línea aceptado.
  - Step destino inválido: `[goto: 123foo]`, `[goto: 1step]`, `[goto:]`, `[goto: ]`, `[goto:  ]`, `[goto: step-name]`, `[goto: step.name]`, `[goto: step name]` — todos rechazados.
  - Brackets rotos / vacíos: `[]`, `[foo]`, `[next ]`, `[next\n]`, `[next][stop]`, `[next]extra`.
  - Empty/whitespace-only string → `(None, false)`.
  - `ParseLastLine` no acepta multilínea (regex anclada con `^...$`).
  - Stream-JSON: último `result` gana, ignora líneas no-JSON, ignora JSON inválido, stream vacío, stream sin event `result`, result con texto vacío, result multilinea, result con case-mismatch.
  - `Kind.String()` para todos los valores + valor desconocido.

## Compatibilidad con PR5b (engine)

El parser ya estaba implementado dentro de `internal/engine/marker.go` (PR5b lo incluyó como anticipo porque PR5a no estaba mergeado todavía — ver el `EXEC_NOTES.md` original). PR5a hace el split limpio:

1. **Lógica del parser** vive ahora 100% en `internal/markerparser` — sólo en un lugar.
2. **`internal/engine/marker.go`** quedó como un thin shim: type aliases (`MarkerKind = markerparser.Kind`, `Marker = markerparser.Marker`, constantes `MarkerNext` etc.) + dos funciones que delegan (`ParseMarker → markerparser.Parse`, `ParseStreamMarker → markerparser.ParseStreamJSON`).
3. **`engine.go` y `engine_test.go` no tocados**: siguen importando los símbolos legacy (`MarkerNext`, `ParseMarker`, …) y todo sigue compilando + tests siguen verdes.
4. El antiguo `internal/engine/marker_test.go` se mantuvo intacto: como sólo usa la API pública, sigue funcionando contra el shim. Los tests duplicados son aceptables como red de regresión adicional; un follow-up trivial puede borrarlos cuando se quiera reducir la suite.

## Decisiones / desviaciones

### 1. `ParseLastLine` además de `Parse`

El issue pide la API `Parse` + `ParseLastLine` + `ParseStreamJSON`. La spec dice que `ParseLastLine` "parsea una sola linea, devuelve el marker + true si matcheó". La firma que elegí es `ParseLastLine(line string) (Marker, bool)` — alineada con `Parse` para que sea fácil de componer. Internamente `Parse` la llama después de extraer la última línea no-vacía.

### 2. `ParseStreamJSON` no dispara default

El issue dice: "Si el ultimo output no es texto, default Next". Decidí que el parser **no** aplica ese default — devuelve `(Marker{None}, false)` y el motor (que ya sabe si la invocación fue técnicamente exitosa) decide si lo trata como `Next` o `Stop`. Es coherente con `Parse`/`ParseLastLine` (mismo patrón) y mantiene el parser puro de toda lógica de control de flujo.

### 3. `Kind` zero-value es `None`, no `Next`

El zero-value del enum es `None`. Si un caller hace `var m Marker` el resultado es "sin marker", no "next" — esto fuerza al caller a decidir explícitamente qué default aplicar.

### 4. Step destino inválido → no matchea (vs. error explícito + stop)

El issue dice: "Step destino inválido → error explícito + `[stop]` con razón". Esa lógica vive en el motor (`engine.RunPipeline` ya valida que el step destino exista y emite `StopReasonUnknownStep`). El parser sólo dice "qué pidió el agente". Casos como `[goto: 123foo]` (regex no matchea por dígito al inicio) se reportan como `(None, false)` — el caller decide cómo escalarlo. Esto es coherente con la spec del issue: "el parser puro" (string in, marker out).

## Pendientes (intencionalmente fuera de scope)

- **Borrar `internal/engine/marker_test.go`** — los tests duplicados pasan por el shim, pero podrían borrarse como cleanup. No urgente: redundancia barata.
- **Migrar callers internos de `engine.MarkerXxx` a `markerparser.Xxx`** — el shim funciona, no hay urgencia. Cuando se haga PR4 / PR5d y se toque el motor, se aprovecha para limpiar.
- **Soporte para markers fuera del último output** — la spec del PRD dice "última línea no vacía", el parser cumple. Si en el futuro queremos parsear el último marker de cualquier línea (no sólo la última), sería un nuevo helper, no un cambio a `Parse`.
