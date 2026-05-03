// REMOVE IN PR6c — este archivo entero (shim de coexistencia v1↔v2)
// se elimina cuando todos los flows estén migrados al modelo v2 y se
// borren las constantes viejas + 21 keys viejas en validTransitions.
//
// v2_transitions.go: shim de coexistencia entre el modelo viejo (9
// estados `che:idea`…`che:closed`) y el modelo v2 derivado del pipeline
// declarativo (`internal/pipelinelabels`).
//
// PR6b migra los flows uno por uno de las constantes viejas a las
// constantes v2 de `pipelinelabels`. Para que `labels.Apply(ref, from, to)`
// siga funcionando con argumentos que ahora son strings v2 (p.ej.
// `pipelinelabels.StateIdea` en lugar de `labels.CheIdea`), registramos
// acá las mismas 21 transiciones en el mapa `validTransitions` con keys
// formadas por los strings v2.
//
// Coexistencia: durante la migración hay flows que aún pasan strings
// viejos (`labels.CheIdea`) y flows ya migrados que pasan strings v2
// (`pipelinelabels.StateIdea`). Ambos sets de keys conviven en
// `validTransitions` sin pisarse — son strings distintos. Cuando todos
// los flows estén migrados (PR6c) se podrán borrar las keys viejas y la
// importación de pipelinelabels se vuelve la única fuente de verdad.
//
// Importante: NO hay conversión automática entre modelos. Si un flow
// migrado a v2 corre sobre un issue que aún tiene labels viejos
// (porque nunca pasó por `migrate-labels-v2`), `Apply` va a fallar
// con "label not found" — el caller debe haber visto el estado v2
// previamente. La migración de labels existentes en repos vivos vive
// fuera de este paquete (subcomando dedicado, fuera del scope de PR6b).
package labels

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chichex/che/internal/pipelinelabels"
)

// ValidateNoMixedLabels reporta error si el set `labels` contiene
// simultáneamente labels del modelo viejo (`che:idea`/`che:plan`/...) y del
// modelo v2 (`che:state:*`/`che:state:applying:*`). Durante PR6b/PR6c los
// flows migrados solo deberían operar sobre issues con labels v2; mezclar
// con v1 es síntoma de un repo que no corrió `migrate-labels-v2` (futuro
// feature) o de un flow no migrado pisando el estado.
//
// Ignora labels que no son `che:*` o que sí son `che:*` pero no pertenecen
// a ninguna de las dos máquinas (p.ej. `che:locked`, `ct:plan`, `type:*`).
//
// Returns nil si todos los labels che:state:* son v2-only o todos los
// che:* son v1-only (o si no hay ninguno de los dos). El error lista los
// labels que mezclan, ordenados, para que el mensaje sea estable.
//
// REMOVE IN PR6c — junto con el shim, este helper desaparece: post-PR6c
// el modelo viejo ya no existe y la mezcla es imposible.
func ValidateNoMixedLabels(labels []string) error {
	v1Set := map[string]bool{
		CheIdea:       true,
		ChePlanning:   true,
		ChePlan:       true,
		CheExecuting:  true,
		CheExecuted:   true,
		CheValidating: true,
		CheValidated:  true,
		CheClosing:    true,
		CheClosed:     true,
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

// init registra las 21 transiciones v2 en el mismo mapa que las viejas.
// Usamos init() en lugar de un literal de mapa para que la lectura del
// mapeo viejo↔nuevo quede explícita acá sin repetir las keys viejas.
//
// Espejo exacto de `validTransitions` (ver labels.go) usando los strings
// del modelo v2:
//
//	che:idea       ↔ pipelinelabels.StateIdea
//	che:planning   ↔ pipelinelabels.StateApplyingExplore
//	che:plan       ↔ pipelinelabels.StateExplore
//	che:executing  ↔ pipelinelabels.StateApplyingExecute
//	che:executed   ↔ pipelinelabels.StateExecute
//	che:validating ↔ pipelinelabels.StateApplyingValidatePR
//	che:validated  ↔ pipelinelabels.StateValidatePR
//	che:closing    ↔ pipelinelabels.StateApplyingClose
//	che:closed     ↔ pipelinelabels.StateClose
func init() {
	v2 := map[string]Transition{
		// explore arranca: idea → applying:explore (lock).
		pipelinelabels.StateIdea + "→" + pipelinelabels.StateApplyingExplore: {
			Remove: []string{pipelinelabels.StateIdea},
			Add:    []string{pipelinelabels.StateApplyingExplore},
		},
		// explore termina OK: applying:explore → explore.
		pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateExplore: {
			Remove: []string{pipelinelabels.StateApplyingExplore},
			Add:    []string{pipelinelabels.StateExplore},
		},
		// explore rollback: applying:explore → idea.
		pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateIdea: {
			Remove: []string{pipelinelabels.StateApplyingExplore},
			Add:    []string{pipelinelabels.StateIdea},
		},
		// iterate plan rollback: applying:explore → validate_pr.
		pipelinelabels.StateApplyingExplore + "→" + pipelinelabels.StateValidatePR: {
			Remove: []string{pipelinelabels.StateApplyingExplore},
			Add:    []string{pipelinelabels.StateValidatePR},
		},
		// iterate plan start: validate_pr → applying:explore.
		pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingExplore: {
			Remove: []string{pipelinelabels.StateValidatePR},
			Add:    []string{pipelinelabels.StateApplyingExplore},
		},
		// execute desde idea (skip explore).
		pipelinelabels.StateIdea + "→" + pipelinelabels.StateApplyingExecute: {
			Remove: []string{pipelinelabels.StateIdea},
			Add:    []string{pipelinelabels.StateApplyingExecute},
		},
		// execute desde explore (path normal post-explore).
		pipelinelabels.StateExplore + "→" + pipelinelabels.StateApplyingExecute: {
			Remove: []string{pipelinelabels.StateExplore},
			Add:    []string{pipelinelabels.StateApplyingExecute},
		},
		// iterate PR start: validate_pr → applying:execute.
		pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingExecute: {
			Remove: []string{pipelinelabels.StateValidatePR},
			Add:    []string{pipelinelabels.StateApplyingExecute},
		},
		// execute / iterate PR termina OK: applying:execute → execute.
		pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateExecute: {
			Remove: []string{pipelinelabels.StateApplyingExecute},
			Add:    []string{pipelinelabels.StateExecute},
		},
		// execute rollback desde idea.
		pipelinelabels.StateApplyingExecute + "→" + pipelinelabels.StateIdea: {
			Remove: []string{pipelinelabels.StateApplyingExecute},
			Add:    []string{pipelinelabels.StateIdea},
		},
		// execute rollback desde explore.
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
		// validate termina OK: applying:validate_pr → validate_pr.
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
		// close PR sin validar: execute → applying:close.
		pipelinelabels.StateExecute + "→" + pipelinelabels.StateApplyingClose: {
			Remove: []string{pipelinelabels.StateExecute},
			Add:    []string{pipelinelabels.StateApplyingClose},
		},
		// close PR validado: validate_pr → applying:close.
		pipelinelabels.StateValidatePR + "→" + pipelinelabels.StateApplyingClose: {
			Remove: []string{pipelinelabels.StateValidatePR},
			Add:    []string{pipelinelabels.StateApplyingClose},
		},
		// close termina OK: applying:close → close.
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
	for k, v := range v2 {
		validTransitions[k] = v
	}
}
