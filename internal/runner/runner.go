// Package runner orquesta la parte "modo de ejecución" de `che run`:
// resuelve qué subset de agentes correr en cada step según el Selector
// inyectado (auto-loop = todos; manual = subset interactivo) y expone
// las constantes de Mode que el caller (cmd/run.go) usa para el banner
// del PRD §3.e.
//
// PR9a (este paquete) sólo cubre la selección — la traducción a
// engine.Pipeline y el loop de invocación viven en cmd/run.go (vía
// engine.RunPipeline). El hook per-step para emitir el header
// "[step: <name> · mode: <auto-loop|manual> · agents: <lista>]" antes
// de cada invocación llega con PR5c, cuando el motor exponga un
// callback. Mientras tanto el banner de cmd/run.go imprime mode una
// sola vez al inicio.
//
// La detección de TTY vs no-TTY vive en el caller (cmd/run.go) — el
// paquete acepta un Selector arbitrario para que los tests puedan
// inyectar uno determinístico sin spawnear bubbletea.
package runner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/chichex/che/internal/pipeline"
)

// Mode identifica cómo che ejecutó el pipeline. Se imprime en el
// banner inicial — los strings son contrato visible al usuario y a los
// scrapers de logs.
type Mode string

const (
	// ModeAuto = corre todos los agentes declarados (default cuando
	// stdin no es TTY: dash, CI, scripts).
	ModeAuto Mode = "auto-loop"

	// ModeManual = el usuario eligió un subset interactivamente. Sólo
	// posible cuando stdin es TTY (TUI); los flags de override en CLI
	// también caen acá porque el usuario eligió el subset
	// explícitamente.
	ModeManual Mode = "manual"
)

// Selector decide qué subset de agentes correr para un step dado.
//
//   - stepName: nombre del step canónico del pipeline (no path).
//   - agents:   lista canónica declarada en el .json del pipeline,
//     en el orden original (preservar el orden importa para que la UX
//     muestre los agentes en el mismo orden que el JSON).
//
// Devuelve la lista de agentes a correr (subset, en cualquier orden) y
// un eventual error. El runner valida que el subset:
//   - sea no-vacío (al menos 1 agente: si querés saltar el step,
//     editá el pipeline);
//   - cada elemento esté en `agents` (sin agentes de fantasía);
//   - sin duplicados.
//
// Cancelación: si el usuario aborta el selector (Esc/Ctrl+C en TTY),
// devolver ErrSelectionCancelled. El caller lo trata como salida limpia
// (exit 0, mensaje informativo) — distinto a un error técnico.
type Selector func(stepName string, agents []string) ([]string, error)

// ErrSelectionCancelled lo devuelve un Selector cuando el usuario
// cancela el wizard. El caller lo propaga sin tratarlo como error
// técnico para que cmd/run.go pueda mapear a exit code dedicado.
var ErrSelectionCancelled = errors.New("selección de agentes cancelada por el usuario")

// AutoSelector es el Selector default para no-TTY (dash, CI). Devuelve
// la lista completa sin pedir input. Equivalente a "modo auto-loop".
func AutoSelector(stepName string, agents []string) ([]string, error) {
	// Copia defensiva: el caller podría mutar el slice y eso no debe
	// afectar al pipeline original.
	out := make([]string, len(agents))
	copy(out, agents)
	return out, nil
}

// Selections mapea step name → subset de agentes seleccionado para ese
// step. Steps no presentes en el mapa se interpretan como "ningún
// override": el motor corre la lista canónica del pipeline.
//
// Se popula con ResolveSelections antes de armar el engine.Pipeline.
type Selections map[string][]string

// ResolveSelections recorre los steps del pipeline en orden y aplica el
// Selector a cada uno.
//
// Reglas:
//   - Si un step no tiene agentes (caso defensivo: el validator del
//     loader ya lo bloquea), se preserva tal cual — el motor lo
//     transforma en StopReasonNoAgents.
//   - Si el Selector devuelve un subset vacío o con agentes inválidos,
//     se aborta la resolución completa con error puntual (path:
//     step name).
//   - Si el Selector devuelve ErrSelectionCancelled en cualquier step,
//     se propaga inmediatamente (no se preguntan los siguientes).
func ResolveSelections(p pipeline.Pipeline, sel Selector) (Selections, error) {
	if sel == nil {
		// Sin selector explícito → auto. Conveniente para callers que
		// sólo quieren transformar pipeline → engine sin ramas.
		sel = AutoSelector
	}
	out := make(Selections, len(p.Steps))
	for _, step := range p.Steps {
		// Pasamos una copia para que un selector mal escrito no pueda
		// mutar el pipeline original.
		canonical := append([]string(nil), step.Agents...)

		subset, err := sel(step.Name, canonical)
		if err != nil {
			return nil, err
		}
		if err := validateSubset(step.Name, canonical, subset); err != nil {
			return nil, err
		}
		out[step.Name] = subset
	}
	return out, nil
}

// validateSubset chequea que la selección del usuario sea coherente con
// los agentes declarados en el step.
func validateSubset(stepName string, canonical, subset []string) error {
	if len(canonical) == 0 {
		// Step sin agentes: aceptamos cualquier subset (incluso vacío)
		// para que el caller no tenga que ramificar. El motor frena
		// igual con StopReasonNoAgents.
		return nil
	}
	if len(subset) == 0 {
		return fmt.Errorf("step %q: subset vacío (al menos 1 agente requerido — para saltar el step, editá el pipeline)", stepName)
	}
	known := make(map[string]bool, len(canonical))
	for _, a := range canonical {
		known[a] = true
	}
	seen := make(map[string]bool, len(subset))
	for _, a := range subset {
		if !known[a] {
			return fmt.Errorf("step %q: agente %q no declarado (válidos: %s)", stepName, a, strings.Join(canonical, ", "))
		}
		if seen[a] {
			return fmt.Errorf("step %q: agente %q duplicado en la selección", stepName, a)
		}
		seen[a] = true
	}
	return nil
}
