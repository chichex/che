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

// LabelStyle es color (hex de 6 chars sin `#`) + descripción para un
// label dado. Sirve de tabla compartida entre EnsureForPipeline y el
// subcomando `che init-labels`.
type LabelStyle struct {
	Color       string
	Description string
}

// pipelineLabelStyles mapea labels conocidos a su estilo recomendado.
// La motivación: `init-labels` debe poder dejar el repo con colores
// significativos (no todo gris default) sin que el operador edite cada
// label a mano. Si el operador tiene preferencias, puede pisar los
// colores via `gh label edit` después — `--force` solo actualiza si
// pasamos los flags, así que un re-ensure no rompe overrides manuales
// si esta tabla cambia (cambia los nuevos labels al estilo nuevo, deja
// los pre-existentes con su color manual mientras no esté en la tabla).
//
// La granularidad es por *prefix* en algunos casos (todos los
// che:state:applying:* tienen el mismo color, todos los *:approve, etc.)
// — esa lógica vive en styleFor() abajo.
var pipelineLabelStyles = map[string]LabelStyle{
	// Estados terminales (no applying).
	"che:state:idea":     {Color: "cccccc", Description: "Idea sin explorar"},
	"che:state:explore":  {Color: "1d76db", Description: "Plan explorado, listo para ejecución"},
	"che:state:execute":  {Color: "1d76db", Description: "Implementación en progreso"},
	"che:state:close":    {Color: "0e8a16", Description: "Listo para cerrar"},
	// Marker.
	CtPlan:    {Color: "5319e7", Description: "ct: plan — issue dentro del workflow che"},
	CheLocked: {Color: "d93f0b", Description: "Lock binario legacy — runtime, no editar"},
}

// styleFor devuelve el LabelStyle a aplicar para un label dado.
// Maneja los casos en que el match es por prefix (applying:*,
// validate_*, plan-validated:*, validated:*).
func styleFor(label string) LabelStyle {
	if s, ok := pipelineLabelStyles[label]; ok {
		return s
	}
	switch {
	case strings.HasPrefix(label, "che:state:applying:"):
		return LabelStyle{Color: "fbca04", Description: "Aplicando paso del pipeline (no editar)"}
	case label == "che:state:validate_issue" || label == "che:state:validate_pr":
		return LabelStyle{Color: "1d76db", Description: "Esperando validación humana / agente"}
	case strings.HasSuffix(label, ":approve"):
		return LabelStyle{Color: "0e8a16", Description: "Validador aprobó"}
	case strings.HasSuffix(label, ":changes-requested"):
		return LabelStyle{Color: "f6a51e", Description: "Validador pidió cambios"}
	case strings.HasSuffix(label, ":needs-human"):
		return LabelStyle{Color: "b60205", Description: "Validador escaló a humano"}
	}
	return LabelStyle{} // sin color → respeta el del repo
}

// EnsureForPipeline crea (o re-asegura) en el repo todos los labels que
// `p` requiere para correr. Idempotente: si ya existen, los actualiza
// sin tocar el resto del repo.
//
// Aplica colores y descripciones según `styleFor`: estados, applying,
// verdicts y markers tienen colores pensados para que el dashboard del
// repo sea legible a primera vista. Labels sin entry en la tabla se
// crean sin --color/--description, manteniendo backward-compat
// (preserva colores manuales).
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
		style := styleFor(l)
		if err := EnsureWithStyle(l, style.Color, style.Description); err != nil {
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
