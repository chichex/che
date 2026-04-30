// Package engine es el orquestador puro de pipelines configurables (PRD #50).
// Toma una secuencia declarativa de steps + agentes, los invoca via Invoker,
// parsea el marker que cada agente emite y aplica la transición al siguiente
// step. che no sabe qué hace cada agente — sólo coordina.
//
// PR5b (este PR) cubre el "engine core": invocación de un agente, parser de
// markers, distinción entre error técnico (→stop) y output sin marker
// (→next), validación del step destino en `[goto: X]`, y cap global de 20
// transiciones por corrida. Multi-agente + aggregator + cancelación parcial
// viven en PR5c; entry agent + `--from` en PR5d.
//
// PR5a (mergeado en este árbol) extrajo el parser de markers a un paquete
// puro `internal/markerparser`. Este archivo conserva los nombres del PR5b
// original (`MarkerKind`, `MarkerNext`, `ParseMarker`, …) como aliases /
// thin wrappers para no romper los tests del motor ni los callers internos
// — la lógica de parsing vive una sola vez, en `markerparser`.
package engine

import (
	"github.com/chichex/che/internal/markerparser"
)

// MarkerKind enumera los 3 tipos de control de flujo que un agente puede
// emitir. Es alias de markerparser.Kind — se mantiene el nombre antiguo
// para los callers ya escritos en este paquete (engine.go, engine_test.go).
type MarkerKind = markerparser.Kind

// Constantes-alias que apuntan a los valores de markerparser. Mantener los
// nombres "MarkerXxx" en engine evita renombrar el motor cuando el parser
// se extrajo a su propio paquete (PR5a). Los callers nuevos deberían usar
// directamente markerparser.Next / markerparser.Stop / etc.
const (
	MarkerNone = markerparser.None
	MarkerNext = markerparser.Next
	MarkerGoto = markerparser.Goto
	MarkerStop = markerparser.Stop
)

// Marker es el resultado parseado del output de un agente. Alias de
// markerparser.Marker; los campos (Kind, Goto) y la semántica son
// idénticos.
type Marker = markerparser.Marker

// ParseMarker delega en markerparser.Parse. Se conserva el nombre con
// prefijo "Marker" porque el motor lo usa internamente y porque hay tests
// existentes que importan el símbolo desde este paquete. PR5a movió la
// implementación al paquete puro `internal/markerparser`.
func ParseMarker(output string) (Marker, bool) {
	return markerparser.Parse(output)
}

// ParseStreamMarker delega en markerparser.ParseStreamJSON. Mismo motivo
// que ParseMarker: nombre legacy del PR5b, lógica en el paquete puro.
func ParseStreamMarker(stream string) (Marker, bool) {
	return markerparser.ParseStreamJSON(stream)
}
