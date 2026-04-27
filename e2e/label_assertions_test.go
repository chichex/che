package e2e_test

import (
	"fmt"
	"strings"

	"github.com/chichex/che/e2e/harness"
)

// findLabelDeletes devuelve todos los `gh api -X DELETE
// repos/.../issues/{issueNum}/labels/<L>` excluyendo che:locked (que es un
// recurso ortogonal a la máquina de estados, no debería contar para asserts
// de transiciones). Útil para tests que quieren verificar la secuencia de
// transiciones che:* sin que el Lock/Unlock confunda el conteo.
func findLabelDeletes(inv *harness.InvocationLog, issueNum int) []harness.Invocation {
	target := fmt.Sprintf("issues/%d/labels/", issueNum)
	var out []harness.Invocation
	for _, c := range inv.For("gh") {
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "-X DELETE") {
			continue
		}
		if !strings.Contains(joined, target) {
			continue
		}
		// Extraer el nombre del label (lo que va después de
		// /labels/) — y filtrar che:locked.
		i := strings.Index(joined, target)
		if i < 0 {
			continue
		}
		rest := joined[i+len(target):]
		if j := strings.Index(rest, " "); j >= 0 {
			rest = rest[:j]
		}
		if rest == "che:locked" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// findLabelPostsByLabel cuenta cuántos `gh api -X POST .../issues/{n}/labels
// -f labels[]=<L>` se hicieron, agrupado por <L>. Una sola call POST puede
// pasar múltiples `labels[]=` — cada uno cuenta. Excluye che:locked
// (Lock/Unlock vive aparte).
func findLabelPostsByLabel(inv *harness.InvocationLog, issueNum int) map[string]int {
	out := map[string]int{}
	target := fmt.Sprintf("issues/%d/labels", issueNum)
	for _, c := range inv.For("gh") {
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "-X POST") {
			continue
		}
		if !strings.Contains(joined, target) {
			continue
		}
		// Cada arg es una entrada — buscamos los `labels[]=<L>`.
		for _, a := range c.Args {
			const prefix = "labels[]="
			if !strings.HasPrefix(a, prefix) {
				continue
			}
			label := strings.TrimPrefix(a, prefix)
			if label == "che:locked" {
				continue
			}
			out[label]++
		}
	}
	return out
}
