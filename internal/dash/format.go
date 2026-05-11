// Package dash — format.go: formateo de stdout crudo a lineas humanas.
//
// Los archivos step-NN.stdout.log persisten el output crudo del CLI (para
// claude: stream-json una linea por evento). El TUI tiene su propio
// renderer; el dash usa este helper para convertir esas lineas a algo
// legible antes de emitirlas por SSE o servirlas al frontend.
//
// Reutilizamos internal/runner/parser para mantener un unico mapeo
// stream-json → humano. CLIs sin parser (gemini text mode, etc) caen al
// parser raw — pass-through linea a linea, sin perdida.
package dash

import (
	"strings"

	"github.com/chichex/che/internal/runner/parser"
)

// formatRawLine convierte una linea cruda del stdout.log en 0..N lineas
// humanas segun el parser del CLI. Devuelve nil para eventos que el parser
// considera sin valor visual (system_init claude, ping, etc) — el caller
// los descarta (no emite SSE, no incrementa ordinal).
//
// Si el parser emite varias lineas para un mismo evento (caso tipico:
// assistant.message.content con texto multi-linea), se devuelven todas;
// el caller decide si las une con \n en un solo evento SSE o las emite
// por separado.
func formatRawLine(cli, raw string) []string {
	p := parser.ForCLI(cli)
	lines, _ := p.Parse(raw)
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// formatRawLineJoined es el helper que usa el watcher / SSE replay: une
// las lineas humanas con \n y devuelve "" si el parser no aporto nada.
// El caller emite un solo step:stdout por linea cruda — asi el frontend
// no tiene que cambiar su modelo de eventos.
func formatRawLineJoined(cli, raw string) string {
	parts := formatRawLine(cli, raw)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}
