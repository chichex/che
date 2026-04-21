package agent

import (
	"encoding/json"
	"strings"
)

// FormatOpusLine traduce una línea del stream-json de claude a un mensaje
// corto y descriptivo. Devuelve (msg, true) si hay algo que mostrar, o
// ("", false) para omitir la línea (eventos irrelevantes como tool_result o
// bloques de texto del asistente, que inundarían la TUI sin info accionable).
//
// Si la línea no parsea como JSON (caso típico de los fakes en e2e, que
// devuelven texto plano), se devuelve tal cual vino — así no rompemos los
// tests que escriben al stdout del agente sin serializar como evento.
//
// toolPrefix se antepone al detalle de tool_use: execute usa "" para mostrar
// "Edit foo.go"; iterate usa "tool: " para mostrar "tool: Edit foo.go". El
// prefijo va sólo adelante del nombre de la tool, NO del mensaje de system
// init ni del resultado final — que históricamente no se prefijan en ningún
// flow.
func FormatOpusLine(line string, toolPrefix string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return line, true
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content []struct {
				Type  string                 `json:"type"`
				Name  string                 `json:"name"`
				Input map[string]interface{} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return line, true
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return "sesión lista, arrancando…", true
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" {
				return toolPrefix + describeOpusTool(c.Name, c.Input), true
			}
		}
	case "result":
		if ev.Subtype == "success" {
			return "agente terminó OK", true
		}
		if ev.Subtype != "" {
			return "agente terminó (" + ev.Subtype + ")", true
		}
	}
	return "", false
}

// describeOpusTool arma el nombre + detalle de una tool_use event del stream
// de claude. Path para file edits/reads; comando truncado a 80 chars para
// Bash; patrón para Glob/Grep; descripción para Task; URL para WebFetch.
// Tools no reconocidas caen al nombre pelado.
func describeOpusTool(name string, input map[string]interface{}) string {
	detail := ""
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		if v, ok := input["file_path"].(string); ok {
			detail = v
		}
	case "Bash":
		if v, ok := input["command"].(string); ok {
			detail = truncate(v, 80)
		}
	case "Glob", "Grep":
		if v, ok := input["pattern"].(string); ok {
			detail = v
		}
	case "Task":
		if v, ok := input["description"].(string); ok {
			detail = v
		}
	case "WebFetch":
		if v, ok := input["url"].(string); ok {
			detail = v
		}
	}
	if detail == "" {
		return name
	}
	return name + " " + detail
}

// truncate recorta un string a max caracteres, reemplazando newlines por
// espacios primero (para no ensuciar los logs de 1-línea). Si no pasa del
// máximo, lo devuelve intacto. Si lo pasa, corta y pone "…" al final.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

