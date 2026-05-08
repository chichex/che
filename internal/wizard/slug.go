package wizard

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Slug deriva un identificador de archivo del nombre del pipeline. Reglas:
//   - lowercase
//   - acentos y diacriticos pelados (NFKD + remove marks): "café" -> "cafe"
//   - cualquier caracter fuera de [a-z0-9] colapsa a un solo "-"
//   - trim de "-" en bordes
//
// Si tras todo eso queda vacio (input solo emoji, solo simbolos, etc.),
// devuelve "" — el caller debe tratarlo como nombre invalido y mostrar
// el hint correspondiente.
func Slug(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	// 1) NFKD descompone "é" en "e" + diacrítico, despues filtramos las
	//    marcas (Mn = nonspacing mark). Asi "café" → "cafe", "Ñ" → "N".
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	stripped, _, err := transform.String(t, name)
	if err != nil {
		// transform.String no falla con los transformers de arriba, pero
		// si pasa algo raro caemos al input crudo y dejamos que el regex
		// haga lo que pueda.
		stripped = name
	}

	stripped = strings.ToLower(stripped)

	// 2) Reemplazar todo lo que no sea [a-z0-9] por "-", colapsando runs.
	var b strings.Builder
	b.Grow(len(stripped))
	dashPending := false
	for _, r := range stripped {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dashPending && b.Len() > 0 {
				b.WriteByte('-')
			}
			dashPending = false
			b.WriteRune(r)
			continue
		}
		dashPending = true
	}

	return b.String()
}
