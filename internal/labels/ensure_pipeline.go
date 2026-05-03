// Auto-creación de labels esperados por un pipeline (PRD §6.b).
//
// Computa el set completo de labels que un repo necesita para correr un
// pipeline determinado y los crea con `gh label create --force`
// (idempotente). El uso típico es como pre-vuelo de cualquier flow:
// antes de aplicar transiciones, garantizar que todos los labels
// existan en el repo evita que un POST de label suba con el color
// default (perdiendo el estilo que el operador haya configurado a mano).
//
// El opt-in `che init-labels` para CI usa esta misma lógica de una sola
// vez en repos nuevos.
package labels

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chichex/che/internal/pipeline"
	"github.com/chichex/che/internal/pipelinelabels"
)

// ExpectedForPipeline devuelve el set completo de labels que un repo
// necesita para correr `p` end-to-end:
//
//   - Estados terminales `che:state:<step>` y aplicantes
//     `che:state:applying:<step>` (vía pipelinelabels.Expected).
//   - Verdicts del validador de plan (`plan-validated:approve`/...).
//   - Verdicts del validador de PR (`validated:approve`/...).
//   - Marker `ct:plan` (origen del issue dentro del workflow).
//
// NO incluye `che:locked` ni `che:lock:*`: el primero es runtime
// orthogonal (lo aplica el flow al arrancar) y el segundo es dinámico
// (un label nuevo por run con timestamp único). Tampoco incluye los
// labels viejos v1 — esos no son runtime post-PR6c.
//
// El resultado está deduplicado y ordenado alfabéticamente para que
// `init-labels` emita output estable y los tests lo comparen
// bit-perfect.
func ExpectedForPipeline(p pipeline.Pipeline) []string {
	set := map[string]struct{}{}
	for _, l := range pipelinelabels.Expected(p) {
		set[l] = struct{}{}
	}
	for _, l := range AllValidated {
		set[l] = struct{}{}
	}
	for _, l := range AllPlanValidated {
		set[l] = struct{}{}
	}
	set[CtPlan] = struct{}{}
	set[CheLocked] = struct{}{}

	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// EnsureForPipeline crea (o re-asegura) en el repo todos los labels que
// `p` requiere para correr. Idempotente: si ya existen, los actualiza
// sin tocar el resto del repo.
//
// Si alguna creación falla (permisos, red), devuelve un error con el
// label que falló y el stderr de gh; los labels que sí se crearon
// quedan aplicados (efecto parcial). El caller suele propagar como
// ExitRetry.
//
// Mantenida en internal/labels (no en internal/pipelinelabels) porque
// combina las constantes de ambos paquetes — pipelinelabels no debe
// depender de labels (el ciclo lo prohíbe).
func EnsureForPipeline(p pipeline.Pipeline) error {
	expected := ExpectedForPipeline(p)
	for _, l := range expected {
		if err := Ensure(l); err != nil {
			return fmt.Errorf("ensure %s: %w", l, err)
		}
	}
	return nil
}

// FormatExpectedForPipeline devuelve el set de labels esperados como una
// lista bullet-list multi-línea, útil para el output de `init-labels`
// (modo dry-run o reporte post-creación).
func FormatExpectedForPipeline(p pipeline.Pipeline) string {
	expected := ExpectedForPipeline(p)
	var sb strings.Builder
	for _, l := range expected {
		sb.WriteString("- ")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	return sb.String()
}
