// Package labels centraliza las constantes de los labels que che aplica sobre
// issues de GitHub, y la máquina de transiciones entre estados. La idea es
// tener un único lugar donde definir "qué es status:plan", "cómo se pasa de
// status:plan a status:executing", etc., para que los distintos flows no
// inventen strings distintas ni violen reglas de la máquina de estados.
//
// Coexiste con los helpers duplicados de `internal/flow/idea/idea.go` y
// `internal/flow/explore/explore.go` durante la introducción de `execute`; la
// deuda de extraer esos usos acá queda para un issue futuro (ver issue #6
// "Fuera de alcance").
package labels

import (
	"fmt"
	"os/exec"
	"strings"
)

// Status labels que la máquina de estados de che maneja sobre cada issue.
// Estos son los labels "mutables": cambian a medida que el issue avanza por
// el embudo idea → explore → execute → close.
const (
	StatusIdea          = "status:idea"
	StatusPlan          = "status:plan"
	StatusExecuting     = "status:executing"
	StatusExecuted      = "status:executed"
	StatusAwaitingHuman = "status:awaiting-human"
)

// Marker labels que no cambian con el estado — identifican el origen del
// issue dentro del workflow de che.
const (
	CtPlan = "ct:plan"
)

// Validated labels que che validate aplica sobre un PR reflejando el verdict
// consolidado de los validadores. Mutan entre iteraciones: antes de aplicar
// uno, se quitan los otros dos (son mutuamente excluyentes).
const (
	ValidatedApprove          = "validated:approve"
	ValidatedChangesRequested = "validated:changes-requested"
	ValidatedNeedsHuman       = "validated:needs-human"
)

// AllValidated lista los labels validated:* — usado por validate para saber
// cuáles remover antes de aplicar el nuevo.
var AllValidated = []string{
	ValidatedApprove,
	ValidatedChangesRequested,
	ValidatedNeedsHuman,
}

// Transition representa un cambio de estado expresado como labels a remover
// y labels a agregar. El orden no importa: `gh issue edit` aplica todo en
// una sola llamada.
type Transition struct {
	Remove []string
	Add    []string
}

// validTransitions define la máquina de estados de execute. Las transiciones
// del resto de los flows (explore) no están acá todavía — cuando se extraiga
// `internal/flow/common/` esos usos deberían migrar a este paquete.
//
// Claves: "from→to". Si el state de origen puede venir con o sin
// awaiting-human, se acepta cualquiera de los dos (las transiciones quitan
// awaiting-human explícitamente cuando corresponde).
var validTransitions = map[string]Transition{
	// execute arranca: plan → executing (lock). Si había awaiting-human (por
	// alguna razón excepcional: p.ej. revisión humana del plan después de
	// consolidar) también se lo quitamos — execute asume que el plan está
	// listo para ejecutar.
	StatusPlan + "→" + StatusExecuting: {
		Remove: []string{StatusPlan, StatusAwaitingHuman},
		Add:    []string{StatusExecuting},
	},
	// execute termina OK: executing → executed + awaiting-human.
	StatusExecuting + "→" + StatusExecuted: {
		Remove: []string{StatusExecuting},
		Add:    []string{StatusExecuted, StatusAwaitingHuman},
	},
	// rollback: executing → plan (cualquier fallo post-lock).
	StatusExecuting + "→" + StatusPlan: {
		Remove: []string{StatusExecuting, StatusAwaitingHuman},
		Add:    []string{StatusPlan},
	},
}

// TransitionFor devuelve la Transition que corresponde a pasar de `from` a
// `to`. Si el par no está registrado como transición válida, error.
func TransitionFor(from, to string) (Transition, error) {
	key := from + "→" + to
	tr, ok := validTransitions[key]
	if !ok {
		return Transition{}, fmt.Errorf("invalid transition: %s → %s", from, to)
	}
	return tr, nil
}

// Apply ejecuta la transición en un issue concreto. `ref` es el identificador
// del issue en el formato que acepta `gh issue edit` (número, URL, o
// owner/repo#N). Asegura que todos los labels a agregar existan antes de
// aplicar el edit.
func Apply(ref, from, to string) error {
	tr, err := TransitionFor(from, to)
	if err != nil {
		return err
	}
	for _, lbl := range tr.Add {
		if err := Ensure(lbl); err != nil {
			return err
		}
	}
	return applyTransition(ref, tr)
}

// Ensure garantiza que un label exista en el repo antes de aplicarlo. Usa
// `gh label create --force` que es idempotente.
func Ensure(name string) error {
	cmd := exec.Command("gh", "label", "create", name, "--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ensuring label %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureAll es `Ensure` en lote, útil cuando un flow sabe de antemano todos
// los labels que va a aplicar durante su ejecución.
func EnsureAll(names ...string) error {
	for _, n := range names {
		if err := Ensure(n); err != nil {
			return err
		}
	}
	return nil
}

// applyTransition ejecuta la llamada `gh issue edit` con los flags de
// remove/add. Si una transición no tiene labels que tocar (no debería pasar
// en una transición válida, pero defendemos), devolvemos nil sin llamar a gh.
func applyTransition(ref string, tr Transition) error {
	if len(tr.Remove) == 0 && len(tr.Add) == 0 {
		return nil
	}
	args := []string{"issue", "edit", ref}
	for _, l := range tr.Remove {
		args = append(args, "--remove-label", l)
	}
	for _, l := range tr.Add {
		args = append(args, "--add-label", l)
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
