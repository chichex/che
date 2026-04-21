package agent

// OutputFormat selecciona el modo de salida de los CLIs. Los flows que sólo
// necesitan el JSON final (explore, validate) usan OutputText; los que
// benefician de ver progreso por tool_use (execute, iterate) usan
// OutputStreamJSON para que cada evento llegue en tiempo real.
type OutputFormat string

const (
	// OutputText es el default: el CLI devuelve una sola respuesta al final.
	// En claude se traduce a `--output-format text`; para codex/gemini el
	// default ya es texto.
	OutputText OutputFormat = "text"

	// OutputStreamJSON emite eventos NDJSON por stdout en vivo. Sólo claude
	// lo soporta (requiere `--verbose`). Para codex/gemini no cambia nada
	// respecto a OutputText — los parsers ignoran el flag.
	OutputStreamJSON OutputFormat = "stream-json"
)

// InvokeArgs devuelve los args de línea de comando para correr al agente en
// modo no-interactivo con el prompt dado y el formato pedido. Cada CLI tiene
// su propia sintaxis:
//   - claude  -p <prompt> --output-format text
//             -p <prompt> --output-format stream-json --verbose
//   - codex   exec --full-auto <prompt>   (full-auto evita prompts de
//     confirmación de sandbox que colgarían el proceso sin TTY; --output-
//     format no aplica)
//   - gemini  -p <prompt>                 (text es el default)
func (a Agent) InvokeArgs(prompt string, format OutputFormat) []string {
	switch a {
	case AgentOpus:
		if format == OutputStreamJSON {
			return []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
		}
		return []string{"-p", prompt, "--output-format", "text"}
	case AgentCodex:
		return []string{"exec", "--full-auto", prompt}
	case AgentGemini:
		return []string{"-p", prompt}
	}
	return nil
}
