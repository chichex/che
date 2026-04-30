// Package markerparser implementa el parser puro de los markers de control
// de flujo que los agentes de che emiten al final de su output (PRD #50
// §3.c). Los 3 markers son:
//
//   - [next]              avanzar al siguiente step del pipeline (default
//     implícito si la invocación terminó bien y no hay marker reconocido).
//   - [goto: <step_name>] saltar al step nombrado. <step_name> debe matchear
//     el fragmento `[a-z_][a-z0-9_]*` (mismo identificador que valida el
//     name de Step en `internal/pipeline`).
//   - [stop]              parar el pipeline. che marca la entity como
//     blocked y notifica al humano.
//
// El paquete es **puro**: string in, marker out. No depende de `internal/
// engine` ni de `internal/agent` — al revés, esos paquetes lo consumen. La
// validación de que el step destino de `[goto: X]` exista en el pipeline
// vive en el motor (no acá): el parser sólo dice "qué pidió el agente"; el
// motor decide qué hacer si el destino no es alcanzable.
//
// API pública:
//
//   - Parse(text)           recorre el output completo del agente y aplica la
//     regex sobre la última línea no vacía. Devuelve (Marker, ok).
//   - ParseLastLine(line)   parsea una sola línea sin la lógica de "buscar la
//     última no-vacía". Útil cuando el caller ya tiene la línea aislada.
//   - ParseStreamJSON(s)    parser dedicado al stream-json --verbose de claude
//     (executors con tool use). Encuentra el último evento `result` y aplica
//     Parse sobre su campo `result`.
//
// Ninguno de los tres aplica un *default* `[next]` cuando no encuentra un
// marker — ese fallback es responsabilidad del caller (motor) porque depende
// de si la invocación fue técnicamente exitosa o falló. Ver PRD §3.b paso 5.
package markerparser

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Kind enumera los 3 tipos de control de flujo que un agente puede emitir.
// Una cuarta variante None se reserva para "no se encontró marker" — el
// caller decide si reemplazarla por Next (invocación exitosa) o Stop (error
// técnico).
type Kind int

const (
	// None indica que el output no terminó con un marker reconocido.
	// Caller decide el default según el outcome de la invocación.
	None Kind = iota

	// Next: avanzar al siguiente step.
	Next

	// Goto: saltar al step nombrado en Marker.Goto. La existencia del
	// step en el pipeline NO es responsabilidad del parser — el motor la
	// chequea.
	Goto

	// Stop: parar el pipeline.
	Stop
)

// String facilita el logging y los test failures.
func (k Kind) String() string {
	switch k {
	case None:
		return "none"
	case Next:
		return "next"
	case Goto:
		return "goto"
	case Stop:
		return "stop"
	}
	return "unknown"
}

// Marker es el resultado parseado del output de un agente.
type Marker struct {
	// Kind es el tipo de marker. Cuando es None significa que el parser no
	// encontró un marker válido; los demás son los 3 markers del PRD.
	Kind Kind
	// Goto es el nombre del step destino cuando Kind == Goto. Viene
	// normalizado (sin espacios alrededor del ":"). Vacío para los otros
	// Kinds.
	Goto string
}

// markerRe es la regex canónica del PRD §3.c.
//
//   - Anclada a línea completa (^...$): el marker tiene que ocupar la línea
//     entera, sin prosa antes ni después (whitespace permitido).
//   - Case-sensitive: `[Next]` / `[GOTO: x]` NO matchean. Sin (?i).
//   - Step destino respeta `[a-z_][a-z0-9_]*` — el mismo fragmento que valida
//     el name de Step en `internal/pipeline`.
//   - El `\s*` después de `goto:` permite tanto `[goto:foo]` como
//     `[goto: foo]`. Trailing whitespace después del `]` se tolera porque
//     algunos editores agregan trailing spaces sin que el agente se entere.
var markerRe = regexp.MustCompile(`^\s*\[(next|stop|goto:\s*([a-z_][a-z0-9_]*))\]\s*$`)

// Parse busca un marker en la última línea no-vacía del output. Trailing
// newlines/whitespace se ignoran. Markers en líneas intermedias se ignoran
// — sólo se mira la última línea no vacía (PRD §3.c).
//
// Retorno:
//   - Marker{Kind: None}, false si no encontró marker (output vacío,
//     output sólo whitespace, última línea no matchea la regex).
//   - (m, true) si encontró un marker válido. Para Goto, m.Goto contiene
//     el step destino sin espacios.
//
// El segundo return distingue "no había marker" de "había Stop": Stop es un
// marker válido y devuelve true.
func Parse(text string) (Marker, bool) {
	last, ok := lastNonEmptyLine(text)
	if !ok {
		return Marker{Kind: None}, false
	}
	return ParseLastLine(last)
}

// ParseLastLine aplica la regex a una sola línea, sin lógica de "buscar la
// última no-vacía". Útil cuando el caller ya tiene la línea aislada (e.g.
// el invoker streamea línea por línea y quiere reportar el marker apenas
// llega).
//
// Whitespace al inicio/fin de la línea se tolera (mismo comportamiento que
// Parse). Línea vacía o sólo whitespace → (Marker{None}, false).
func ParseLastLine(line string) (Marker, bool) {
	m := markerRe.FindStringSubmatch(line)
	if m == nil {
		return Marker{Kind: None}, false
	}
	// m[1] es el grupo principal (next | stop | goto:...).
	// m[2] es el step destino cuando es un goto.
	switch {
	case m[1] == "next":
		return Marker{Kind: Next}, true
	case m[1] == "stop":
		return Marker{Kind: Stop}, true
	default:
		// goto: el match garantiza m[2] no vacío.
		return Marker{Kind: Goto, Goto: m[2]}, true
	}
}

// lastNonEmptyLine devuelve la última línea de s que tiene al menos un
// caracter no-whitespace. ok=false si el input entero está vacío o sólo
// tiene whitespace.
//
// Recorre el string hacia atrás (no Split) para evitar la alocación del
// slice intermedio cuando hay muchas líneas — el stream-json del executor
// puede traer miles.
func lastNonEmptyLine(s string) (string, bool) {
	end := len(s)
	for end > 0 {
		start := strings.LastIndexByte(s[:end], '\n')
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

// ParseStreamJSON busca un marker en el stream-json de claude (formato
// `--output-format stream-json --verbose`, el que usan los executors con
// tool use). Recorre los eventos NDJSON y se queda con el ÚLTIMO evento
// `result`, aplicando Parse sobre su campo `result`.
//
// PRD §3.c "OutputStreamJSON":
//
//	"para agentes que ejecutan via streaming, che parsea el último evento
//	`result` del stream y aplica la regex al campo de texto. Si el último
//	output no es texto (LLM cerró con tool_use sin response final),
//	default [next] (resolver en el caller)."
//
// Notas de implementación:
//
//   - El evento `result` de claude trae el texto del asistente en el campo
//     `result` (sí, el campo se llama igual que el tipo). Mantenemos el
//     nombre para no inventar un mapping propio.
//   - Líneas que no parsean como JSON se ignoran (no fallan el parser).
//     Esto es importante porque algunos fakes de e2e o headers de
//     `claude --verbose` escriben texto plano al stdout.
//   - Si hay múltiples eventos `result` (caso raro), gana el último —
//     porque el stream cierra con su outcome final.
//   - Si no hay ningún evento `result` con texto no vacío, devolvemos
//     (Marker{None}, false) y el caller aplica el default [next].
func ParseStreamJSON(stream string) (Marker, bool) {
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
		return Marker{Kind: None}, false
	}
	return Parse(lastResultText)
}
