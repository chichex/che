// Package stateref resuelve "¿dónde viven los labels che:* de estado?" para
// flows que arrancan sobre un PR. El issue linkeado (via
// closingIssuesReferences) es el source of truth: execute.go escribe las
// transiciones che:idea/planning/plan/executing/executed sobre el issue, y
// el dashboard lee Status desde el issue. Los flows que arrancan sobre un
// PR (validate, iterate, close) antes leían/escribían sobre el PR, con lo
// cual nunca veían los labels que execute dejó en el issue — resultado:
// gates que no disparaban (validate salteaba la transición che:executed →
// che:validating) e iteraciones posteriores que fallaban ("no está en
// che:validated").
//
// Este helper centraliza la resolución:
//
//   - Si el PR tiene closingIssuesReferences: la máquina de estados vive en
//     el primer issue. Fetch labels del issue, transiciones sobre issueRef.
//   - Si no hay issue linkeado (PR ajeno, no creado por `che execute`):
//     caemos al PR. Los labels que ve el caller son los que vinieron en el
//     gh pr view; las transiciones se aplican sobre prRef. Preserva compat
//     con PRs que el usuario metió a mano.
//
// Los labels validated:* / plan-validated:* (verdicts) y che:locked (lock
// del recurso) se manejan aparte en cada flow — este paquete solo habla de
// los che:* de la máquina de estados.
package stateref

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/pipelinelabels"
)

// Resolution describe dónde aplicar las transiciones che:* para un PR.
//
// - Ref es el identificador que el caller pasa a labels.Apply / gh issue
//   edit. Cuando ResolvedToIssue=true es el número del issue como string
//   ("122"); cuando es false es el prRef crudo que recibió el helper.
// - Labels son los labels actuales en Ref — típicamente usados para gates
//   ("este issue/PR está en che:executed?").
// - ResolvedToIssue=true cuando encontramos un closing issue con
//   che:idea/planning/plan/executing/executed/validating/validated/closing/
//   closed presente. False cuando el PR no tenía issue linkeado, o todos
//   los issues linkeados fallaron al fetchear, o ninguno tenía labels de
//   estado.
// - IssueNumber es el número del issue resuelto (0 si cayó al PR). Útil
//   para mensajes de error accionables ("issue #122 no está en
//   che:executed" vs "PR #140 ...").
type Resolution struct {
	Ref             string
	Labels          []string
	ResolvedToIssue bool
	IssueNumber     int
}

// fetchIssueLabelsFn es el fetcher de labels de un issue por número. Variable
// para que los tests lo stubeen sin montar un fake gh en PATH. Default llama
// a `gh issue view <n> --json labels`.
var fetchIssueLabelsFn = fetchIssueLabelsViaGh

// FetchIssueLabels corre `gh issue view <n> --json labels` y devuelve el
// slice de nombres. Expuesto por si otro paquete lo quiere reusar — no
// esperamos que sea el caso.
func FetchIssueLabels(number int) ([]string, error) {
	return fetchIssueLabelsFn(number)
}

// Resolve devuelve la resolución de state-ref para un PR.
//
// prRef es el identificador que el caller le pasaría a `gh pr edit` (número,
// URL, owner/repo#N). prLabels son los labels que ya trae el fetch inicial
// del PR — se usan como fallback cuando no hay issue linkeado o cuando
// fetchear el issue falla. closingIssues son los números de
// pr.ClosingIssuesReferences en el orden que vino de la API.
//
// Contrato:
//  1. Si closingIssues está vacío → resolvemos al PR (compat con PRs ajenos).
//  2. Iteramos closingIssues en orden y tomamos el primero cuyos labels traen
//     al menos un label de máquina de estados che:* (idea..closed). El resto
//     de los issues los ignoramos (un PR que closes #A #B típicamente solo
//     tiene un issue "principal" manejado por che; los otros son colaterales).
//  3. Si `gh issue view` falla (issue cerrado/eliminado, red, 404) para un
//     issue dado, seguimos con el siguiente. Si todos fallan o ninguno tiene
//     labels de estado, caemos al PR. Eso significa "no sé dónde vive el
//     estado — usá lo que ya tenías" en vez de romper el flow entero.
//  4. Nunca devuelve error: best-effort. El caller usa el Resolution para
//     decidir gates/mensajes; si el estado no está donde esperaba, el gate
//     mismo abortará con mensaje accionable.
func Resolve(prRef string, prLabels []string, closingIssues []int) Resolution {
	fallback := Resolution{
		Ref:             prRef,
		Labels:          prLabels,
		ResolvedToIssue: false,
	}
	if len(closingIssues) == 0 {
		return fallback
	}
	for _, n := range closingIssues {
		if n <= 0 {
			continue
		}
		labels, err := fetchIssueLabelsFn(n)
		if err != nil {
			// issue cerrado/eliminado/404 → probar el próximo
			continue
		}
		if !hasCheStateLabel(labels) {
			// issue existente pero sin labels de máquina: típicamente es
			// un issue referenciado pero no manejado por che. Próximo.
			continue
		}
		return Resolution{
			Ref:             fmt.Sprintf("%d", n),
			Labels:          labels,
			ResolvedToIssue: true,
			IssueNumber:     n,
		}
	}
	return fallback
}

// HasLabel es un helper simple para consultar si el Resolution trae un label.
// Evita que los callers tengan que escribir el loop N veces.
func (r Resolution) HasLabel(name string) bool {
	for _, l := range r.Labels {
		if l == name {
			return true
		}
	}
	return false
}

// stateLabelSet es el set de labels de máquina de estados. Incluye AMBAS
// familias (v1 y v2) durante PR6c para que stateref siga reconociendo issues
// linkeados de repos no migrados (un PR creado por `che execute` v2 puede
// tener `closingIssuesReferences` apuntando a un issue legacy v1; sin esto
// validate/iterate/close PR-mode caerían al PR como si el issue no estuviera
// trackeado por che). Los gates de los flows migrados rechazan los v1 con
// mensaje accionable, así que reconocer el label v1 acá no afloja la
// validación — solo evita falsos negativos en la resolución.
//
// REMOVE IN PR6d: junto con las constantes v1 viejas, este set se reduce a
// solo v2.
var stateLabelSet = map[string]struct{}{
	// v1 (legacy — REMOVE IN PR6d)
	labels.CheIdea:       {},
	labels.ChePlanning:   {},
	labels.ChePlan:       {},
	labels.CheExecuting:  {},
	labels.CheExecuted:   {},
	labels.CheValidating: {},
	labels.CheValidated:  {},
	labels.CheClosing:    {},
	labels.CheClosed:     {},
	// v2 (modelo derivado del pipeline declarativo)
	pipelinelabels.StateIdea:               {},
	pipelinelabels.StateApplyingExplore:    {},
	pipelinelabels.StateExplore:            {},
	pipelinelabels.StateApplyingExecute:    {},
	pipelinelabels.StateExecute:            {},
	pipelinelabels.StateApplyingValidatePR: {},
	pipelinelabels.StateValidatePR:         {},
	pipelinelabels.StateApplyingClose:      {},
	pipelinelabels.StateClose:              {},
}

// hasCheStateLabel devuelve true si names contiene al menos uno de los
// labels de máquina de estados.
func hasCheStateLabel(names []string) bool {
	for _, l := range names {
		if _, ok := stateLabelSet[l]; ok {
			return true
		}
	}
	return false
}

// fetchIssueLabelsViaGh corre `gh issue view <n> --json labels` y extrae
// los nombres. Devuelve un slice vacío si el issue no tiene labels.
func fetchIssueLabelsViaGh(number int) ([]string, error) {
	cmd := exec.Command("gh", "issue", "view", fmt.Sprintf("%d", number), "--json", "labels")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue view %d: %s", number, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var wrap struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse gh issue view labels: %w", err)
	}
	names := make([]string, 0, len(wrap.Labels))
	for _, l := range wrap.Labels {
		names = append(names, l.Name)
	}
	return names, nil
}

// SetFetchIssueLabelsForTest reemplaza el fetcher para tests y devuelve un
// restore function. Expuesto en archivo no-test porque los tests de otros
// paquetes (iterate, close) también lo van a usar — no basta con
// export_test.go local.
func SetFetchIssueLabelsForTest(fn func(number int) ([]string, error)) func() {
	prev := fetchIssueLabelsFn
	fetchIssueLabelsFn = fn
	return func() { fetchIssueLabelsFn = prev }
}
