package parser

// rawParser es el pass-through line-by-line. Usado por gemini / opencode
// (text mode) y como fallback defensivo cuando un parser especifico no
// reconoce la linea — por ejemplo, claude antes del primer `system_init`
// o un opencode que en el futuro emita JSON parcial.
type rawParser struct{}

// Raw construye un parser pass-through. La linea cruda va al log pane tal
// cual y no se appendea a events.jsonl (el doc deja events.jsonl como un
// concepto de stream-json — texto crudo no aplica).
func Raw() Parser { return rawParser{} }

func (rawParser) Name() string { return "raw" }

func (rawParser) Parse(line string) ([]Line, Event) {
	if line == "" {
		return nil, Event{}
	}
	return []Line{{Text: line}}, Event{}
}
