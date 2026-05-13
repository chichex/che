package runner

import (
	"fmt"
	"sort"
	"strings"
)

// modelsByCLI es el whitelist hardcoded de modelos aceptados por cada CLI
// soportado por el runner. Cualquier `model:` declarado en el YAML que no
// matchee se rechaza en preflight con un error claro ANTES de spawnear.
//
// La tabla se actualiza por PR cuando salen modelos nuevos — no hay
// autodescubrimiento. Cuando un CLI saca un alias nuevo, agregar la entry
// aca y bumpear el slice en orden alfabetico para que el remedy del
// preflight liste los validos de forma estable.
//
// opencode no esta en la tabla: no soporta override de modelo desde el
// YAML (el flag --model no es estable en su CLI). ValidateModel maneja el
// case especial: si un step con cli=opencode trae model: distinto de "",
// rebota con un mensaje accionable.
var modelsByCLI = map[string][]string{
	"claude": {
		// Aliases cortos (preferidos para escribir en YAML).
		"opus",
		"sonnet",
		"haiku",
		"opusplan",
		// Nombres completos.
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
	},
	"codex": {
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex-spark",
		"gpt-5.2",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex-mini",
	},
	"gemini": {
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"gemini-2.5-flash-lite",
	},
}

// defaultModelByCLI es el modelo que el runner pasa cuando un step no
// declara `model:` en el YAML. Para opencode el default es "" (no se setea
// flag — queda el que tenga configurado el propio CLI).
var defaultModelByCLI = map[string]string{
	"claude":   "opus",
	"codex":    "gpt-5.5",
	"gemini":   "gemini-2.5-pro",
	"opencode": "",
}

// DefaultModel devuelve el modelo default para el CLI dado. Para CLIs
// desconocidos devuelve "" (el caller normalmente termina pasando este
// valor sin flag — los CLIs nuevos no soportan override desde YAML hasta
// que se agreguen al whitelist).
func DefaultModel(cli string) string {
	return defaultModelByCLI[cli]
}

// ValidateModel verifica que el modelo declarado en el YAML sea aceptado
// por el CLI. Reglas:
//
//   - model == "" siempre pasa (la rama "sin override" cae al default por
//     CLI; compat con YAMLs viejos que no declaran el campo).
//   - cli == "opencode" + model != "" siempre rechaza con un mensaje
//     accionable. El YAML no es el lugar para configurar el modelo de
//     opencode.
//   - Para claude/codex/gemini, el modelo tiene que matchear el whitelist
//     exactamente (case-sensitive). Si no, error con la lista de aceptados.
//   - CLIs desconocidos (no en modelsByCLI ni "opencode") pasan: el runner
//     no opina sobre overrides para CLIs que no soporta como ciudadano
//     primer-class. Si despues alguien quiere whitelist para un CLI nuevo,
//     se agrega aca.
func ValidateModel(cli, model string) error {
	if model == "" {
		return nil
	}
	if cli == "opencode" {
		return fmt.Errorf("opencode no soporta override de modelo desde YAML; usá la config del propio CLI")
	}
	allowed, known := modelsByCLI[cli]
	if !known {
		return nil
	}
	for _, m := range allowed {
		if m == model {
			return nil
		}
	}
	return fmt.Errorf("modelo %q no soportado para cli=%s; aceptados: %s",
		model, cli, strings.Join(sortedCopy(allowed), ", "))
}

// sortedCopy devuelve una copia ordenada del slice (no muta el original).
// La lista en modelsByCLI ya esta razonablemente ordenada por estabilidad
// (alias cortos primero, despues nombres completos / versiones), pero el
// error message lo mostramos alfabeticamente para que el usuario pueda
// scannearlo rapido.
func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
