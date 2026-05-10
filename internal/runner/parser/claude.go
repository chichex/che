package parser

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
	// Campos top-level de eventos system task_started / task_progress /
	// task_notification (subagentes Task/Explore). El shape lo inferimos
	// observando un run real: description marca el step actual del
	// subagente; summary aparece en task_notification con el resultado;
	// status indica completed/failed.
	Description string `json:"description,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Status      string `json:"status,omitempty"`
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
	// tool_result (en user echo) — Content puede ser string o array de
	// bloques; lo guardamos crudo y lo desempacamos en summarizeToolResult.
	Content json.RawMessage `json:"content,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
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
		return claudeRenderSystem(ev)
	case "assistant":
		return claudeRenderMessage(ev.Message, false)
	case "user":
		// user trae el echo del tool_result. Por defecto lo silenciamos
		// (mucho ruido); solo levantamos los is_error para que el viewport
		// muestre la falla sin tener que abrir events.jsonl.
		return claudeRenderUserToolResults(ev.Message)
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

// claudeRenderSystem maneja los subtipos de eventos system. Los task_*
// vienen de subagentes (Task/Explore) y son utiles para el viewport porque
// muestran el avance del subagente sin tener que abrir events.jsonl.
func claudeRenderSystem(ev claudeEvent) []Line {
	switch ev.Subtype {
	case "init":
		return []Line{{Text: "· session iniciada"}}
	case "task_started":
		if ev.Description != "" {
			return []Line{{Text: "· task started: " + truncate(ev.Description, 100)}}
		}
		return []Line{{Text: "· task started"}}
	case "task_progress":
		if ev.Description != "" {
			return []Line{{Text: "· task: " + truncate(ev.Description, 100)}}
		}
		return []Line{{Text: "· task_progress"}}
	case "task_notification":
		status := ev.Status
		if status == "" {
			status = "done"
		}
		if ev.Summary != "" {
			return []Line{{Text: fmt.Sprintf("· task %s: %s", status, truncate(ev.Summary, 100))}}
		}
		return []Line{{Text: "· task " + status}}
	case "":
		return nil
	default:
		return []Line{{Text: "· system " + ev.Subtype}}
	}
}

// claudeRenderUserToolResults extrae solo los tool_result con is_error=true
// del echo de user. El resto se omite (el output exitoso suele ser data
// verbosa que no aporta al viewport y events.jsonl lo guarda igual).
func claudeRenderUserToolResults(raw json.RawMessage) []Line {
	if len(raw) == 0 {
		return nil
	}
	var msg claudeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	var out []Line
	for _, c := range msg.Content {
		if c.Type != "tool_result" || !c.IsError {
			continue
		}
		txt := summarizeToolResult(c.Content)
		if txt == "" {
			out = append(out, Line{Text: "· tool_result error"})
		} else {
			out = append(out, Line{Text: "· tool_result error: " + truncate(txt, 100)})
		}
	}
	return out
}

// claudeRenderMessage descompone el envelope de message.content[] en lineas
// humanas. Cada bloque text → "> ..."; cada tool_use → "· tool: name [—
// resumen]"; thinking → "· thinking" (el contenido viene encriptado en
// stream-json --verbose, solo podemos marcar que ocurrio).
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
			line := "· tool: " + name
			if summary := summarizeToolInput(name, c.Input); summary != "" {
				line += " — " + summary
			}
			out = append(out, Line{Text: line})
		case "thinking":
			// El stream emite el `thinking` con la firma encriptada (texto
			// vacio para consumers externos), asi que solo podemos marcar
			// que el modelo pensó. Mantiene paridad con el comportamiento
			// historico cuando este case caia al default.
			out = append(out, Line{Text: "· thinking"})
		case "tool_result":
			// Asistente nunca emite tool_result, pero por las dudas:
			// silenciamos (los errores se levantan desde el echo user).
		default:
			// Bloques desconocidos: marcamos sin volcar contenido.
			out = append(out, Line{Text: "· " + c.Type})
		}
	}
	return out
}

// summarizeToolInput devuelve una descripcion corta del input de un
// tool_use para enriquecer la linea del viewport. Cubre los tools mas
// frecuentes; el resto cae a "" (sin sufijo). Las claves se observaron
// directo del stream — Bash usa description+command, Read/Edit/Write usan
// file_path, Grep/Glob usan pattern, Task usa description.
func summarizeToolInput(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return ""
	}
	str := func(k string) string {
		if v, ok := in[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	switch name {
	case "Bash":
		if d := str("description"); d != "" {
			return truncate(d, 80)
		}
		if cmd := str("command"); cmd != "" {
			return "$ " + truncate(strings.ReplaceAll(cmd, "\n", " "), 80)
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if p := str("file_path"); p != "" {
			return filepath.Base(p)
		}
	case "Grep":
		if p := str("pattern"); p != "" {
			return fmt.Sprintf("%q", truncate(p, 60))
		}
	case "Glob":
		if p := str("pattern"); p != "" {
			return truncate(p, 80)
		}
	case "Task":
		if d := str("description"); d != "" {
			return truncate(d, 80)
		}
	case "WebFetch":
		if u := str("url"); u != "" {
			return truncate(u, 80)
		}
	case "WebSearch":
		if q := str("query"); q != "" {
			return truncate(q, 80)
		}
	}
	return ""
}

// summarizeToolResult extrae la primera porcion de texto de un tool_result.
// El campo Content puede venir como string crudo o como array de bloques
// (cada uno con type=text). Devuelve "" si no hay nada utilizable.
func summarizeToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	}
	var blocks []claudeContent
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(strings.ReplaceAll(b.Text, "\n", " "))
			}
		}
	}
	return ""
}

// truncate corta s a max runas y agrega "…" si efectivamente se truncó.
// Cuenta runas (no bytes) para no romper con acentos.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
