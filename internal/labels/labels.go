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

// Che* labels — máquina de estados con prefix `che:*`. Reemplaza al
// modelo viejo (`status:*`, 5 estados) post-refactor. La diferencia
// clave es la introducción de 3 estados transient (planning,
// validating, closing) y un estado terminal de validate (validated):
//
//   - planning   — explore en curso (entre idea y plan).
//   - plan       — explore terminó OK; existe un plan listo para ejecutar.
//   - executing  — execute en curso (entre plan y executed); locks el issue.
//   - executed   — execute terminó OK; hay un PR abierto pendiente de validar.
//   - validating — validate en curso (sobre plan o sobre PR).
//   - validated  — validate terminó OK (los 3 verdicts: approve, changes-
//     requested, needs-human; el verdict concreto vive en los
//     labels plan-validated:* / validated:*, no acá).
//   - closing    — close en curso (entre executed/validated y closed).
//   - closed     — terminal: el issue se cerró, el PR se mergeó/cerró.
//
// Los estados transient (`*ing`) sirven como lock optimista: si dos
// instancias de che corren en paralelo sobre el mismo issue, el segundo
// ve `che:planning` y aborta. Los rollbacks viven en validTransitions:
// cualquier `*ing` puede volver al estado anterior si el flow falla.
//
// El subcomando `che migrate-labels` renombra in-place los labels viejos
// `status:*` a los nuevos `che:*` en repos que arrancaron con el modelo
// viejo (los strings literales viven solo en cmd/migrate_labels.go porque
// son entrada de migración, no uso runtime).
const (
	CheIdea       = "che:idea"
	ChePlanning   = "che:planning"
	ChePlan       = "che:plan"
	CheExecuting  = "che:executing"
	CheExecuted   = "che:executed"
	CheValidating = "che:validating"
	CheValidated  = "che:validated"
	CheClosing    = "che:closing"
	CheClosed     = "che:closed"
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

// PlanValidated labels que che validate aplica sobre un issue (plan) reflejando
// el verdict consolidado de los validadores del plan. Son mutuamente
// excluyentes: antes de aplicar uno, se quitan los otros dos. `che execute`
// los usa como gate (solo ejecuta si hay plan-validated:approve).
const (
	PlanValidatedApprove          = "plan-validated:approve"
	PlanValidatedChangesRequested = "plan-validated:changes-requested"
	PlanValidatedNeedsHuman       = "plan-validated:needs-human"
)

// AllPlanValidated lista los labels plan-validated:* — usado por validate
// para saber cuáles remover antes de aplicar el nuevo.
var AllPlanValidated = []string{
	PlanValidatedApprove,
	PlanValidatedChangesRequested,
	PlanValidatedNeedsHuman,
}

// Transition representa un cambio de estado expresado como labels a remover
// y labels a agregar. El orden no importa: `gh issue edit` aplica todo en
// una sola llamada.
type Transition struct {
	Remove []string
	Add    []string
}

// validTransitions define la máquina de estados con prefix `che:*`. 21
// transiciones que cubren los 5 flows (explore / execute / iterate plan /
// iterate PR / validate / close). Cada `*ing` (planning, executing,
// validating, closing) tiene una transición de éxito (avanza al estado
// terminal correspondiente) y una de rollback (vuelve al estado anterior
// si el flow falla). Los gates de intervención humana no viven acá: están
// en plan-validated:* (issues) y validated:* (PRs), aplicados por `che
// validate` por separado.
//
// Claves: "from→to".
var validTransitions = map[string]Transition{
	// explore arranca: idea → planning (lock).
	CheIdea + "→" + ChePlanning: {
		Remove: []string{CheIdea},
		Add:    []string{ChePlanning},
	},
	// explore termina OK / iterate plan termina OK: planning → plan.
	ChePlanning + "→" + ChePlan: {
		Remove: []string{ChePlanning},
		Add:    []string{ChePlan},
	},
	// explore rollback: planning → idea (cualquier fallo en explore).
	ChePlanning + "→" + CheIdea: {
		Remove: []string{ChePlanning},
		Add:    []string{CheIdea},
	},
	// iterate plan rollback: planning → validated (cuando se itera sobre
	// un plan ya validado y la iteración falla, volvemos al estado previo).
	ChePlanning + "→" + CheValidated: {
		Remove: []string{ChePlanning},
		Add:    []string{CheValidated},
	},
	// iterate plan start: validated → planning (lock — el humano pidió
	// iterar el plan tras un validate con changes-requested; entramos al
	// estado transient mientras opus reescribe).
	CheValidated + "→" + ChePlanning: {
		Remove: []string{CheValidated},
		Add:    []string{ChePlanning},
	},
	// execute desde idea (skipping explore — el humano pidió ejecutar
	// directo sin pasar por explore/plan).
	CheIdea + "→" + CheExecuting: {
		Remove: []string{CheIdea},
		Add:    []string{CheExecuting},
	},
	// execute desde plan (path normal post-explore).
	ChePlan + "→" + CheExecuting: {
		Remove: []string{ChePlan},
		Add:    []string{CheExecuting},
	},
	// iterate PR start: validated → executing (re-ejecutar sobre un PR
	// ya validado para aplicar el feedback).
	CheValidated + "→" + CheExecuting: {
		Remove: []string{CheValidated},
		Add:    []string{CheExecuting},
	},
	// execute / iterate PR termina OK: executing → executed.
	CheExecuting + "→" + CheExecuted: {
		Remove: []string{CheExecuting},
		Add:    []string{CheExecuted},
	},
	// execute rollback desde idea (execute que arrancó sin plan).
	CheExecuting + "→" + CheIdea: {
		Remove: []string{CheExecuting},
		Add:    []string{CheIdea},
	},
	// execute rollback desde plan (path normal).
	CheExecuting + "→" + ChePlan: {
		Remove: []string{CheExecuting},
		Add:    []string{ChePlan},
	},
	// iterate PR rollback: executing → validated.
	CheExecuting + "→" + CheValidated: {
		Remove: []string{CheExecuting},
		Add:    []string{CheValidated},
	},
	// validate plan start: plan → validating.
	ChePlan + "→" + CheValidating: {
		Remove: []string{ChePlan},
		Add:    []string{CheValidating},
	},
	// validate PR start: executed → validating.
	CheExecuted + "→" + CheValidating: {
		Remove: []string{CheExecuted},
		Add:    []string{CheValidating},
	},
	// validate termina OK: validating → validated. Aplica para los 3
	// verdicts (approve / changes-requested / needs-human) — el verdict
	// concreto vive en los labels plan-validated:* / validated:*, no en
	// la máquina de estados.
	CheValidating + "→" + CheValidated: {
		Remove: []string{CheValidating},
		Add:    []string{CheValidated},
	},
	// validate plan rollback: validating → plan.
	CheValidating + "→" + ChePlan: {
		Remove: []string{CheValidating},
		Add:    []string{ChePlan},
	},
	// validate PR rollback: validating → executed.
	CheValidating + "→" + CheExecuted: {
		Remove: []string{CheValidating},
		Add:    []string{CheExecuted},
	},
	// close PR sin validar: executed → closing (el humano decide cerrar
	// sin pasar por validate; che close warnea pero no bloquea).
	CheExecuted + "→" + CheClosing: {
		Remove: []string{CheExecuted},
		Add:    []string{CheClosing},
	},
	// close PR validado: validated → closing (path normal post-validate).
	CheValidated + "→" + CheClosing: {
		Remove: []string{CheValidated},
		Add:    []string{CheClosing},
	},
	// close termina OK: closing → closed. Terminal.
	CheClosing + "→" + CheClosed: {
		Remove: []string{CheClosing},
		Add:    []string{CheClosed},
	},
	// close rollback desde executed.
	CheClosing + "→" + CheExecuted: {
		Remove: []string{CheClosing},
		Add:    []string{CheExecuted},
	},
	// close rollback desde validated.
	CheClosing + "→" + CheValidated: {
		Remove: []string{CheClosing},
		Add:    []string{CheValidated},
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
// owner/repo#N). Asegura que todos los labels involucrados (Add y Remove)
// existan en el repo antes de aplicar el edit: `gh issue edit --remove-label X`
// falla con "not found" si X no está registrado en el repo — aunque el issue
// no lo tenga aplicado. Esto cubre el caso de issues marcados con `ct:plan` a
// mano que nunca pasaron por `che idea`, por lo que `status:idea` jamás se
// creó en el repo.
func Apply(ref, from, to string) error {
	tr, err := TransitionFor(from, to)
	if err != nil {
		return err
	}
	if err := EnsureAll(tr.Add...); err != nil {
		return err
	}
	if err := EnsureAll(tr.Remove...); err != nil {
		return err
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
