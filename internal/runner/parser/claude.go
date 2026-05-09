package parser

import (
	"encoding/json"
	"fmt"
	"strings"
)

// claudeParser convierte stream-json --verbose de claude en lineas humanas
// para el log pane. Sigue la memory "Invocacion al CLI de claude": el
// stream emite eventos JSON con type=assistant / tool_use / system / result /
// user (echo de tool_results). H5 cubre los tipos relevantes para el
// viewport — lo demas cae a un fallback que muestra el type entre llaves
// para no perder visibilidad si el shape evoluciona.
//
// Cualquier linea que NO parsee como JSON cae a stdout crudo (el log pane
// la muestra tal cual). Es el unico fallback porque hubo casos historicos
// (memoria del proyecto) donde claude emite un warning de stderr o un mensaje
// de inicializacion antes del primer system_init.
type claudeParser struct{}

// Claude devuelve un parser de stream-json de claude. Es stateless — toda la
// conversacion del state se guarda en events.jsonl (responsabilidad del
// caller, no del parser).
func Claude() Parser { return claudeParser{} }

func (claudeParser) Name() string { return "claude" }

// claudeEvent es el shape minimo que el parser entiende. Cualquier campo
// extra se ignora (RawMessage para preservar shape al re-serializar). Solo
// extraemos lo necesario para renderear lineas humanas; el resto va a
// events.jsonl crudo.
type claudeEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
}

// claudeMessage representa el envelope de message dentro del evento (los
// type=assistant y type=user lo usan). Tambien encaja el shape "tool_use"
// que viene anidado en message.content[].
type claudeMessage struct {
	Role    string            `json:"role,omitempty"`
	Content []claudeContent   `json:"content,omitempty"`
	Stop    string            `json:"stop_reason,omitempty"`
	Usage   map[string]any    `json:"usage,omitempty"`
	Extra   map[string]string `json:"-"`
}

type claudeContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (claudeParser) Parse(raw string) ([]Line, Event) {
	raw = strings.TrimRight(raw, "\r\n")
	if raw == "" {
		return nil, Event{}
	}
	// Intento JSON; si falla cae a raw (defensivo). El parser nunca panic-ea
	// por shape — los errores son lineas vacias en el viewport, no crashes.
	var ev claudeEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		// No es JSON — texto crudo. Lo mostramos como esta en el viewport
		// pero NO lo appendeamos a events.jsonl (no es un evento valido).
		return []Line{{Text: raw}}, Event{}
	}

	event := Event{Raw: raw}
	lines := claudeRender(ev)
	return lines, event
}

// claudeRender mapea el evento a lineas humanas. Devuelve nil si el evento
// no aporta nada visual (p.ej. system_init, ping). Sigue la convencion del
// doc: "> assistant text" / "· tool: <name>".
func claudeRender(ev claudeEvent) []Line {
	switch ev.Type {
	case "system":
		// system_init / system_error — emit una linea breve si hay subtype.
		if ev.Subtype == "init" {
			return []Line{{Text: "· session iniciada"}}
		}
		if ev.Subtype != "" {
			return []Line{{Text: fmt.Sprintf("· system %s", ev.Subtype)}}
		}
		return nil
	case "assistant":
		return claudeRenderMessage(ev.Message, false)
	case "user":
		// user es el echo del tool_result que claude se manda a si mismo.
		// Lo dejamos invisible al viewport por ruido — events.jsonl lo
		// guarda igual.
		return nil
	case "result":
		// result trae stop_reason / usage / cost. Una linea sumaria.
		return []Line{{Text: "· result"}}
	default:
		// Tipos desconocidos: marcamos para diagnostico sin hacer ruido.
		if ev.Type != "" {
			return []Line{{Text: fmt.Sprintf("· %s", ev.Type)}}
		}
		return nil
	}
}

// claudeRenderMessage descompone el envelope de message.content[] en lineas
// humanas. Cada bloque text → "> ..."; cada tool_use → "· tool: name".
func claudeRenderMessage(raw json.RawMessage, _ bool) []Line {
	if len(raw) == 0 {
		return nil
	}
	var msg claudeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var out []Line
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			// Multi-linea: prefijo "> " a la primera, identamos las demas
			// con dos espacios para alinear visualmente.
			lines := strings.Split(text, "\n")
			for i, l := range lines {
				if i == 0 {
					out = append(out, Line{Text: "> " + l})
				} else {
					out = append(out, Line{Text: "  " + l})
				}
			}
		case "tool_use":
			name := c.Name
			if name == "" {
				name = "(sin nombre)"
			}
			out = append(out, Line{Text: "· tool: " + name})
		case "tool_result":
			// Lo omitimos en el viewport (suele ser data verbosa).
		default:
			// Bloques desconocidos: marcamos sin volcar contenido.
			out = append(out, Line{Text: "· " + c.Type})
		}
	}
	return out
}
