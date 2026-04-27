package labels

import (
	"fmt"
	"regexp"
)

// scopeRequiredRe detecta el mensaje de GraphQL de gh cuando faltan scopes:
// "your token has not been granted the required scopes ... requires one of
// the following scopes: ['read:org']" o "...: read:org". gh emite ambos
// formatos según la versión y el endpoint; la regex acepta brackets/comillas
// como separadores. Capturamos el listado de scopes pedidos para mostrarlos
// en la sugerencia; si no matchea, devolvemos el err original.
var scopeRequiredRe = regexp.MustCompile(`(?i)(?:requires|require) one of the following scopes:\s*([\[\]'":a-z0-9_,\s]+)`)

// WrapGhError detecta errores de scope en stderr de gh y agrega una línea
// accionable con el comando para refrescar el token. Si el stderr no
// contiene la firma de scope-required, devuelve err sin tocar.
//
// Lo expone el paquete porque cualquier shell-out a gh puede pegarse contra
// esto (no solo labels), pero por ahora solo lo usan los helpers REST de
// acá. Si en el futuro otros paquetes lo necesitan, mover a internal/gh.
func WrapGhError(err error, stderr []byte) error {
	if err == nil {
		return nil
	}
	hint := scopeHint(stderr)
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w\n%s", err, hint)
}

// scopeHint devuelve la línea accionable si stderr contiene la firma de
// scope-required, vacío en caso contrario. Aislado para testear sin armar
// errores reales.
func scopeHint(stderr []byte) string {
	m := scopeRequiredRe.FindSubmatch(stderr)
	if m == nil {
		return ""
	}
	scopes := string(m[1])
	// Normalizar: gh devuelve "read:org" o "read:org, repo" — armamos el
	// comando refresh con todo lo pedido + repo (siempre necesario).
	return fmt.Sprintf("→ ejecutá: gh auth refresh -s %s", normalizeScopes(scopes))
}

// normalizeScopes parsea la lista de scopes (separados por coma o espacio),
// asegura que `repo` esté incluido (siempre necesario), y devuelve una lista
// canónica separada por comas. Idempotente.
func normalizeScopes(s string) string {
	seen := map[string]bool{}
	var ordered []string
	for _, tok := range splitScopes(s) {
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		ordered = append(ordered, tok)
	}
	if !seen["repo"] {
		ordered = append(ordered, "repo")
	}
	out := ""
	for i, t := range ordered {
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}

func splitScopes(s string) []string {
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, r := range s {
		switch r {
		case ',', ' ', '\t', '\n', '[', ']', '\'', '"':
			flush()
		default:
			cur += string(r)
		}
	}
	flush()
	return out
}
