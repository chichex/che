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
// El package se diseñó self-contained sobre interfaces (`Invoker`) y tipos
// minimales (`Pipeline`/`Step`) para que se pueda mergear antes que PR1
// (`internal/agentregistry`) y PR2 (`internal/pipeline`). Cuando esos PRs
// estén en main, un follow-up wirea `engine.Run` para consumir
// `pipeline.Pipeline` directamente y resolver agentes via `agentregistry`.
package engine

import (
	"encoding/json"
	"regexp"
	"strings"
)

// MarkerKind enumera los 3 tipos de control de flujo que un agente puede
// emitir al final de su output. PRD §3.c define la spec formal del parser.
type MarkerKind int

const (
	// MarkerNone indica que el output no terminó con un marker reconocido.
	// El motor lo trata como [next] cuando la invocación fue técnicamente
	// exitosa (PRD §3.b paso 5: "Si no hay marker → asume [next]").
	MarkerNone MarkerKind = iota

	// MarkerNext: avanzar al siguiente step. También es el default cuando
	// la invocación terminó bien y no se encontró marker.
	MarkerNext

	// MarkerGoto: saltar al step nombrado en Goto. Si el step no existe
	// en el pipeline, el motor convierte esto en MarkerStop con razón
	// "step destino desconocido" (PRD §3.c "Step destino inválido").
	MarkerGoto

	// MarkerStop: parar el pipeline. che marca la entity como blocked y
	// notifica al humano. También se aplica automáticamente cuando la
	// invocación del agente falla técnicamente (PRD §3.b paso 4).
	MarkerStop
)

// String facilita el logging y los test failures.
func (m MarkerKind) String() string {
	switch m {
	case MarkerNone:
		return "none"
	case MarkerNext:
		return "next"
	case MarkerGoto:
		return "goto"
	case MarkerStop:
		return "stop"
	}
	return "unknown"
}

// Marker es el resultado parseado del output de un agente. Goto sólo se
// llena cuando Kind == MarkerGoto.
type Marker struct {
	Kind MarkerKind
	// Goto es el nombre del step destino cuando Kind == MarkerGoto. El
	// nombre ya viene normalizado (sin espacios alrededor del ":"). Para
	// los otros Kinds queda vacío.
	Goto string
}

// markerRe es la regex canónica del PRD §3.c.
//
//   - ancla a línea completa (^...$): el marker tiene que ocupar la línea
//     entera, sin prosa antes ni después (whitespace permitido).
//   - case-sensitive: `[Next]` / `[GOTO: x]` NO matchean. Sin (?i).
//   - el step destino respeta el fragmento `[a-z_][a-z0-9_]*` — el mismo
//     que valida el name de Step en `internal/pipeline`.
//   - el `\s*` después de `goto:` permite tanto `[goto:foo]` como
//     `[goto: foo]`. Trailing whitespace después del `]` se tolera porque
//     editores agregan trailing spaces sin que el agente se entere.
var markerRe = regexp.MustCompile(`^\s*\[(next|stop|goto:\s*([a-z_][a-z0-9_]*))\]\s*$`)

// ParseMarker busca un marker en la última línea no-vacía del output.
// Devuelve (Marker{MarkerNone}, false) si no encuentra marker. Trailing
// newlines/whitespace se ignoran. Markers en líneas intermedias se
// ignoran — el parser sólo mira la última línea no vacía (PRD §3.c
// "Markers en líneas intermedias se ignoran").
//
// El segundo return distingue "no había marker" de "hay un marker pero es
// MarkerStop": MarkerStop es un marker válido y devuelve true.
func ParseMarker(output string) (Marker, bool) {
	last, ok := lastNonEmptyLine(output)
	if !ok {
		return Marker{Kind: MarkerNone}, false
	}
	m := markerRe.FindStringSubmatch(last)
	if m == nil {
		return Marker{Kind: MarkerNone}, false
	}
	// m[1] es el grupo principal (next | stop | goto:...).
	// m[2] es el step destino cuando es un goto.
	switch {
	case m[1] == "next":
		return Marker{Kind: MarkerNext}, true
	case m[1] == "stop":
		return Marker{Kind: MarkerStop}, true
	default:
		// goto: el match garantiza m[2] no vacío.
		return Marker{Kind: MarkerGoto, Goto: m[2]}, true
	}
}

// lastNonEmptyLine devuelve la última línea de s que tiene al menos un
// caracter no-whitespace. ok=false si el output entero está vacío o sólo
// tiene whitespace.
func lastNonEmptyLine(s string) (string, bool) {
	// Recorrer hacia atrás es marginalmente más rápido que split + reverse,
	// y evita una alocación de slice cuando hay millones de líneas (caso
	// extremo del stream-json del executor).
	end := len(s)
	for end > 0 {
		// encontrar el inicio de la línea actual
		start := strings.LastIndexByte(s[:end], '\n')
		// start == -1 → no hay newline antes; la línea va desde 0
		// start >= 0  → la línea va desde start+1
		var line string
		if start == -1 {
			line = s[:end]
		} else {
			line = s[start+1 : end]
		}
		if strings.TrimSpace(line) != "" {
			return line, true
		}
		if start == -1 {
			return "", false
		}
		end = start
	}
	return "", false
}

// ParseStreamMarker busca un marker en el stream-json de claude (formato
// `--output-format stream-json --verbose`). Recorre los eventos NDJSON y
// se queda con el último evento `result`. Aplica ParseMarker sobre el
// campo `result` (que claude usa para el texto final del asistente).
//
// PRD §3.c "OutputStreamJSON": "para agentes que ejecutan via streaming
// (executors con tool use), che parsea el último evento `result` del
// stream y aplica la regex al campo `text`. Si el último output no es
// texto (LLM cerró con tool_use sin response final), default [next]".
//
// Implementación: el evento `result` de claude trae el texto en el campo
// `result` (no `text` — la PRD lo simplifica para el lector). Si el
// stream no tiene ningún evento `result` con campo string, devolvemos
// (Marker{MarkerNone}, false) y el caller aplica el default [next].
//
// Líneas que no parsean como JSON se ignoran (no fallan el parser). Esto
// es importante porque los fakes de e2e a veces escriben texto plano al
// stdout — y porque claude con --verbose puede emitir headers no-JSON.
func ParseStreamMarker(stream string) (Marker, bool) {
	var lastResultText string
	var foundResult bool
	for _, line := range strings.Split(stream, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
			continue
		}
		if ev.Type == "result" {
			lastResultText = ev.Result
			foundResult = true
		}
	}
	if !foundResult || lastResultText == "" {
		return Marker{Kind: MarkerNone}, false
	}
	return ParseMarker(lastResultText)
}
