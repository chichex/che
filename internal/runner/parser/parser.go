// Package parser convierte el stdout crudo de cada CLI en lineas humanas
// para el log pane de R3. H5 lo introduce: hasta H4 el output era opaco
// (dump al final). Cada CLI tiene su parser:
//
//   - claude.go : stream-json --verbose. Eventos JSON, cada linea un evento;
//     emite "> <texto del assistant>" / "· tool: <name>" para el viewport
//     y delega los eventos crudos a un sink opcional (events.jsonl).
//   - codex.go  : codex --json. v1 stub que cae a raw (esquema no estable).
//   - raw.go    : pass-through line-by-line para gemini / opencode (text mode).
//
// El contrato comun esta en este file: Parser.Parse(line) devuelve la(s)
// linea(s) humanas a renderear en el log pane + el evento crudo (json
// serializable) opcional para events.jsonl. Un parser puede devolver 0
// lineas humanas si el evento es un metadato sin valor visual (p.ej. el
// `system_init` de claude).
package parser

// Line es una salida humana del parser: texto + flag de stderr para que el
// renderer la pinte en rojo dimmed cuando aplica. Para parsers que no
// distinguen (raw), Stderr siempre es false — el caller separa stderr del
// stdout en el upstream del subprocess.
type Line struct {
	Text   string
	Stderr bool
}

// Event es un evento crudo (la linea original de stream-json) que el parser
// considera relevante para events.jsonl. Si Empty() es true, no se persiste.
type Event struct {
	Raw string
}

// Empty indica si el evento es vacio (no debe escribirse a events.jsonl).
func (e Event) Empty() bool { return e.Raw == "" }

// Parser es la interfaz que implementan claude/codex/raw. Parse recibe una
// linea cruda del stdout del subprocess y devuelve:
//
//   - lines: 0 o mas lineas humanas para el viewport (orden cronologico).
//   - event: el JSON crudo para events.jsonl (vacio si no aplica).
type Parser interface {
	Parse(rawLine string) (lines []Line, event Event)
	// Name devuelve el nombre del parser (debug / logging).
	Name() string
}

// ForCLI devuelve el parser indicado para el CLI dado. Si no hay match,
// devuelve el parser raw (pass-through line-by-line) — defensivo, alineado
// al doc: "Para CLIs sin stream-json (gemini text mode): mostrar el stdout
// crudo, una linea por linea".
func ForCLI(cli string) Parser {
	switch cli {
	case "claude":
		return Claude()
	case "codex":
		// v1 stub — el shape de codex --json no esta estable; cae a raw.
		// TODO H6+: parser real para codex.
		return Raw()
	default:
		return Raw()
	}
}
