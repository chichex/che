// Package labels centraliza las constantes de los labels que che aplica sobre
// issues de GitHub, y la máquina de transiciones entre estados. La idea es
// tener un único lugar donde definir los labels canónicos (validados, marker
// ct:plan, locked) y la máquina de transiciones para que los flows no
// inventen strings ni violen reglas.
//
// La máquina de estados de los flows vive en `internal/pipelinelabels` (los
// labels `che:state:*` derivados del pipeline declarativo). Este paquete
// importa las constantes v2 desde allí y arma las transiciones válidas en
// `validTransitions`. Las 21 transiciones cubren los 5 flows (explore /
// execute / iterate plan / iterate PR / validate / close) con éxito +
// rollback.
//
// El subcomando `che migrate-labels-v2` migra repos vivos del modelo viejo
// (`che:idea`/`che:plan`/...) al modelo v2 (`che:state:*`). Los strings
// literales del modelo viejo viven solo en ese subcomando porque son entrada
// de migración, no uso runtime.
package labels

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/chichex/che/internal/pipelinelabels"
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

// validTransitions define la máquina de estados v2 derivada del pipeline
// declarativo (`internal/pipelinelabels`). 21 transiciones que cubren los
// 5 flows (explore / execute / iterate plan / iterate PR / validate /
// close). Cada `applying:<step>` (transient) tiene una transición de éxito
// (avanza al estado terminal correspondiente) y una de rollback (vuelve al
// estado anterior si el flow falla). Los gates de intervención humana no
// viven acá: están en plan-validated:* (issues) y validated:* (PRs),
// aplicados por `che validate` por separado.
//
// Claves: "from→to".
//
// Mapeo (PRD §6.c):
//
//	idea       — pipelinelabels.StateIdea
//	planning   — pipelinelabels.StateApplyingExplore
//	plan       — pipelinelabels.StateExplore
//	executing  — pipelinelabels.StateApplyingExecute
//	executed   — pipelinelabels.StateExecute
//	validating — pipelinelabels.StateApplyingValidatePR
//	validated  — pipelinelabels.StateValidatePR
//	closing    — pipelinelabels.StateApplyingClose
//	closed     — pipelinelabels.StateClose
var validTransitions = map[string]Transition{
	// explore arranca: idea → applying:explore (lock).
	pipelinelabels.StateIdea + "→" + pipelinelabels.StateApplyingExplore: {
		Remove: []string{pipelinelabels.StateIdea},
		Add:    []string{pipelinelabels.StateApplyingExplore},
	},
	// explore termina OK / iterate plan termina OK: applying:explore → explore.
	pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateExplore: {
		Remove: []string{pipelinelabels.StateApplyingExplore},
		Add:    []string{pipelinelabels.StateExplore},
	},
	// explore rollback: applying:explore → idea (cualquier fallo en explore).
	pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateIdea: {
		Remove: []string{pipelinelabels.StateApplyingExplore},
		Add:    []string{pipelinelabels.StateIdea},
	},
	// iterate plan rollback: applying:explore → validate_pr (cuando se itera
	// sobre un plan ya validado y la iteración falla, volvemos al estado
	// previo).
	pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateValidatePR: {
		Remove: []string{pipelinelabels.StateApplyingExplore},
		Add:    []string{pipelinelabels.StateValidatePR},
	},
	// iterate plan start: validate_pr → applying:explore (lock — el humano
	// pidió iterar el plan tras un validate con changes-requested; entramos
	// al estado transient mientras opus reescribe).
	pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingExplore: {
		Remove: []string{pipelinelabels.StateValidatePR},
		Add:    []string{pipelinelabels.StateApplyingExplore},
	},
	// execute desde idea (skipping explore — el humano pidió ejecutar
	// directo sin pasar por explore/plan).
	pipelinelabels.StateIdea + "→" + pipelinelabels.StateApplyingExecute: {
		Remove: []string{pipelinelabels.StateIdea},
		Add:    []string{pipelinelabels.StateApplyingExecute},
	},
	// execute desde explore (path normal post-explore).
	pipelinelabels.StateExplore + "→" + pipelinelabels.StateApplyingExecute: {
		Remove: []string{pipelinelabels.StateExplore},
		Add:    []string{pipelinelabels.StateApplyingExecute},
	},
	// iterate PR start: validate_pr → applying:execute (re-ejecutar sobre un
	// PR ya validado para aplicar el feedback).
	pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingExecute: {
		Remove: []string{pipelinelabels.StateValidatePR},
		Add:    []string{pipelinelabels.StateApplyingExecute},
	},
	// execute / iterate PR termina OK: applying:execute → execute.
	pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateExecute: {
		Remove: []string{pipelinelabels.StateApplyingExecute},
		Add:    []string{pipelinelabels.StateExecute},
	},
	// execute rollback desde idea (execute que arrancó sin plan).
	pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateIdea: {
		Remove: []string{pipelinelabels.StateApplyingExecute},
		Add:    []string{pipelinelabels.StateIdea},
	},
	// execute rollback desde explore (path normal).
	pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateExplore: {
		Remove: []string{pipelinelabels.StateApplyingExecute},
		Add:    []string{pipelinelabels.StateExplore},
	},
	// iterate PR rollback: applying:execute → validate_pr.
	pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateValidatePR: {
		Remove: []string{pipelinelabels.StateApplyingExecute},
		Add:    []string{pipelinelabels.StateValidatePR},
	},
	// validate plan start: explore → applying:validate_pr.
	pipelinelabels.StateExplore + "→" + pipelinelabels.StateApplyingValidatePR: {
		Remove: []string{pipelinelabels.StateExplore},
		Add:    []string{pipelinelabels.StateApplyingValidatePR},
	},
	// validate PR start: execute → applying:validate_pr.
	pipelinelabels.StateExecute + "→" + pipelinelabels.StateApplyingValidatePR: {
		Remove: []string{pipelinelabels.StateExecute},
		Add:    []string{pipelinelabels.StateApplyingValidatePR},
	},
	// validate termina OK: applying:validate_pr → validate_pr. Aplica para
	// los 3 verdicts (approve / changes-requested / needs-human) — el verdict
	// concreto vive en los labels plan-validated:* / validated:*, no en la
	// máquina de estados.
	pipelinelabels.StateApplyingValidatePR + "→" + pipelinelabels.StateValidatePR: {
		Remove: []string{pipelinelabels.StateApplyingValidatePR},
		Add:    []string{pipelinelabels.StateValidatePR},
	},
	// validate plan rollback: applying:validate_pr → explore.
	pipelinelabels.StateApplyingValidatePR + "→" + pipelinelabels.StateExplore: {
		Remove: []string{pipelinelabels.StateApplyingValidatePR},
		Add:    []string{pipelinelabels.StateExplore},
	},
	// validate PR rollback: applying:validate_pr → execute.
	pipelinelabels.StateApplyingValidatePR + "→" + pipelinelabels.StateExecute: {
		Remove: []string{pipelinelabels.StateApplyingValidatePR},
		Add:    []string{pipelinelabels.StateExecute},
	},
	// close PR sin validar: execute → applying:close (el humano decide cerrar
	// sin pasar por validate; che close warnea pero no bloquea).
	pipelinelabels.StateExecute + "→" + pipelinelabels.StateApplyingClose: {
		Remove: []string{pipelinelabels.StateExecute},
		Add:    []string{pipelinelabels.StateApplyingClose},
	},
	// close PR validado: validate_pr → applying:close (path normal post-validate).
	pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingClose: {
		Remove: []string{pipelinelabels.StateValidatePR},
		Add:    []string{pipelinelabels.StateApplyingClose},
	},
	// close termina OK: applying:close → close. Terminal.
	pipelinelabels.StateApplyingClose + "→" + pipelinelabels.StateClose: {
		Remove: []string{pipelinelabels.StateApplyingClose},
		Add:    []string{pipelinelabels.StateClose},
	},
	// close rollback desde execute.
	pipelinelabels.StateApplyingClose + "→" + pipelinelabels.StateExecute: {
		Remove: []string{pipelinelabels.StateApplyingClose},
		Add:    []string{pipelinelabels.StateExecute},
	},
	// close rollback desde validate_pr.
	pipelinelabels.StateApplyingClose + "→" + pipelinelabels.StateValidatePR: {
		Remove: []string{pipelinelabels.StateApplyingClose},
		Add:    []string{pipelinelabels.StateValidatePR},
	},
}

// v1LegacyStates lista los 9 labels del modelo viejo (`che:idea`/`che:plan`/...).
// Existe SOLO para que `ValidateNoMixedLabels` y los guards `rejectV1Labels`
// de los flows puedan detectar repos a medio migrar y pedir al operador que
// corra `che migrate-labels-v2`. Los valores son strings literales (no
// constantes exportadas) porque post-PR6c el modelo v1 ya no es runtime —
// son entrada de detección, no API.
//
// REMOVE IN PR6d: cuando ya no haya repos sin migrar, el guard se vuelve
// dead code y este slice puede eliminarse.
var v1LegacyStates = []string{
	"che:idea",
	"che:planning",
	"che:plan",
	"che:executing",
	"che:executed",
	"che:validating",
	"che:validated",
	"che:closing",
	"che:closed",
}

// V1LegacyStates devuelve los 9 labels del modelo viejo. Los guards
// `rejectV1Labels` de los flows lo consumen para detectar repos a medio
// migrar. Devuelve una copia para evitar que el caller lo mute.
//
// REMOVE IN PR6d junto con v1LegacyStates.
func V1LegacyStates() []string {
	out := make([]string, len(v1LegacyStates))
	copy(out, v1LegacyStates)
	return out
}

// ValidateNoMixedLabels reporta error si el set `labels` contiene
// simultáneamente labels del modelo viejo (`che:idea`/`che:plan`/...) y del
// modelo v2 (`che:state:*`/`che:state:applying:*`). Los flows v2 no saben
// hacer migración in-place; mezclar es síntoma de un repo que no corrió
// `che migrate-labels-v2`.
//
// Ignora labels que no son `che:*` o que sí son `che:*` pero no pertenecen
// a ninguna de las dos máquinas (p.ej. `che:locked`, `ct:plan`, `type:*`).
//
// Returns nil si todos los che:* del set son v2-only o todos son v1-only
// (o si no hay ninguno). El error lista los labels que mezclan, ordenados,
// para que el mensaje sea estable.
//
// REMOVE IN PR6d: cuando v1 deje de existir, la mezcla es imposible y este
// helper se elimina junto con `v1LegacyStates`.
func ValidateNoMixedLabels(labels []string) error {
	v1Set := map[string]bool{}
	for _, l := range v1LegacyStates {
		v1Set[l] = true
	}
	var v1Found, v2Found []string
	for _, l := range labels {
		if v1Set[l] {
			v1Found = append(v1Found, l)
			continue
		}
		// Cualquier prefix `che:state:` o `che:state:applying:` es v2.
		if strings.HasPrefix(l, pipelinelabels.PrefixState) {
			v2Found = append(v2Found, l)
		}
	}
	if len(v1Found) > 0 && len(v2Found) > 0 {
		sort.Strings(v1Found)
		sort.Strings(v2Found)
		return fmt.Errorf("labels v1 (%s) y v2 (%s) presentes simultáneamente — corré `che migrate-labels-v2` o limpiá a mano",
			strings.Join(v1Found, ","), strings.Join(v2Found, ","))
	}
	return nil
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
// mano que nunca pasaron por `che idea`, por lo que `che:state:idea` jamás se
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

// applyTransition ejecuta la transición vía REST (POST/DELETE labels) en
// vez de `gh issue edit`. Razón: `gh issue edit` y `gh pr edit` con --add-
// label/--remove-label disparan GraphQL internamente, que requiere scope
// `read:org` para PRs en repos de orgs — scope que `gh auth login` default
// no entrega. La REST API `/repos/{owner}/{repo}/issues/{n}/labels` solo
// requiere `repo` y funciona uniformemente para issues y PRs (un PR es un
// issue en REST).
func applyTransition(ref string, tr Transition) error {
	if len(tr.Remove) == 0 && len(tr.Add) == 0 {
		return nil
	}
	number, err := refNumber(ref)
	if err != nil {
		return fmt.Errorf("apply transition %s: %w", ref, err)
	}
	for _, l := range tr.Remove {
		if err := removeLabelREST(number, l); err != nil {
			return err
		}
	}
	if len(tr.Add) > 0 {
		if err := addLabelsREST(number, tr.Add...); err != nil {
			return err
		}
	}
	return nil
}

// AddLabels aplica labels al issue/PR identificado por number vía REST. No
// asume nada del estado previo: el endpoint POST es idempotente (re-aplicar
// un label existente es un no-op en GitHub). Caller-side: hacer Ensure antes
// si el label puede no existir en el repo.
func AddLabels(number int, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	return addLabelsREST(number, names...)
}

// RemoveLabel saca un label del issue/PR identificado por number vía REST.
// Tolera 404 (label no aplicado al issue, o label inexistente en el repo) —
// mismo comportamiento que Unlock, alineado con que el caller suele usar
// esto en defers / rollbacks idempotentes.
func RemoveLabel(number int, name string) error {
	return removeLabelREST(number, name)
}

func addLabelsREST(number int, names ...string) error {
	args := []string{"api", "-X", "POST", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels", number)}
	for _, n := range names {
		args = append(args, "-f", "labels[]="+n)
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return WrapGhError(fmt.Errorf("gh api POST labels: %s", strings.TrimSpace(string(out))), out)
	}
	return nil
}

func removeLabelREST(number int, name string) error {
	cmd := exec.Command("gh", "api",
		"-X", "DELETE",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels/%s", number, name),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	combined := string(out)
	if strings.Contains(combined, "Label does not exist") || strings.Contains(combined, "HTTP 404") {
		return nil
	}
	return WrapGhError(fmt.Errorf("gh api DELETE labels/%s: %s", name, strings.TrimSpace(combined)), out)
}
