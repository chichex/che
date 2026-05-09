package parser

// codexParser es el placeholder para codex --json (TODO H6+ implementar el
// shape real). v1 cae a raw porque el esquema de codex no esta cristalizado
// al momento de H5; para no bloquear el rollout, mantenemos pass-through
// line-by-line (mismo comportamiento que gemini text mode).
//
// Cuando lleguemos al parser real, basta con reemplazar el body de Parse
// por la logica especifica — la firma queda intacta.
func Codex() Parser { return codexParser{} }

type codexParser struct{}

func (codexParser) Name() string { return "codex" }

func (codexParser) Parse(raw string) ([]Line, Event) {
	// Por ahora delegamos en raw: el log pane muestra la linea cruda y no
	// se appendea nada a events.jsonl.
	return Raw().Parse(raw)
}
