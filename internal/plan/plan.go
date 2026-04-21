// Package plan define el contrato del "plan consolidado" que `che explore`
// escribe al body del issue y `che execute` consume. Existe para eliminar la
// duplicación del shape entre los dos flows: Render y Parse viven acá como
// una sola fuente de verdad, con tests de round-trip que detectan drift.
//
// Compat legacy: el header puede aparecer con o sin paréntesis
// ("## Plan consolidado" o "## Plan consolidado (post-exploración)"); Parse
// matchea con strings.Contains así ambos formatos siguen funcionando.
//
// Política de tolerancia: Parse devuelve error accionable SOLO cuando detecta
// más de una ocurrencia del header "## Plan consolidado" (ambigüedad
// irrecuperable). Para cualquier otro caso no-parseable (body vacío, header
// ausente, header único sin sub-secciones) devuelve (&ConsolidatedPlan{
// Summary: body}, nil) silenciosamente — el caller inspecciona el resultado
// (campos vacíos o HasConsolidatedHeader) y decide si seguir con un plan
// degradado o abortar. El parser no escribe a stdout/stderr para no acoplar
// el paquete al logger global ni ensuciar la CLI.
package plan

import (
	"fmt"
	"regexp"
	"strings"
)

// ConsolidatedPlan es el plan final post-convergencia que se escribe al body
// del issue. Superset que incluye todos los campos que emite el consolidador
// de explore. Los tags JSON se preservan exactamente porque el agente de
// consolidación emite este shape y explore hace json.Unmarshal sobre el
// output.
type ConsolidatedPlan struct {
	Summary            string   `json:"summary"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Approach           string   `json:"approach"`
	Steps              []string `json:"steps"`
	RisksToMitigate    []Risk   `json:"risks_to_mitigate"`
	OutOfScope         []string `json:"out_of_scope"`
}

// Risk es un riesgo con mitigación asociada. El mismo shape se usa tanto en
// el análisis de explore (Response.Risks) como en el plan consolidado
// (ConsolidatedPlan.RisksToMitigate).
type Risk struct {
	Risk       string `json:"risk"`
	Likelihood string `json:"likelihood"`
	Impact     string `json:"impact"`
	Mitigation string `json:"mitigation"`
}

// consolidatedHeader es el prefijo exacto que Parse busca para detectar
// ambigüedad. El header real puede tener sufijo entre paréntesis (ej.
// "(post-exploración)"), pero el prefijo no cambia.
const consolidatedHeader = "## Plan consolidado"

// Render arma el body del issue: plan consolidado arriba (listo para
// ejecución), idea original preservada abajo como referencia.
func Render(c *ConsolidatedPlan, originalBody string) string {
	var sb strings.Builder
	sb.WriteString("## Plan consolidado (post-exploración)\n\n")
	sb.WriteString("**Resumen:** " + c.Summary + "\n\n")
	sb.WriteString("**Goal:** " + c.Goal + "\n\n")

	sb.WriteString("### Criterios de aceptación\n")
	for _, crit := range c.AcceptanceCriteria {
		sb.WriteString("- [ ] " + crit + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("### Approach\n")
	sb.WriteString(c.Approach + "\n\n")

	sb.WriteString("### Pasos\n")
	for i, step := range c.Steps {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
	}
	sb.WriteString("\n")

	if len(c.RisksToMitigate) > 0 {
		sb.WriteString("### Riesgos a mitigar\n")
		for _, r := range c.RisksToMitigate {
			sb.WriteString(fmt.Sprintf("- **%s** (likelihood=%s, impact=%s) — %s\n",
				r.Risk, r.Likelihood, r.Impact, r.Mitigation))
		}
		sb.WriteString("\n")
	}

	if len(c.OutOfScope) > 0 {
		sb.WriteString("### Fuera de alcance\n")
		for _, o := range c.OutOfScope {
			sb.WriteString("- " + o + "\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("## Idea original (de `che idea`)\n\n")
	sb.WriteString(originalBody)
	if !strings.HasSuffix(originalBody, "\n") {
		sb.WriteString("\n")
	}

	return sb.String()
}

// ErrAmbiguousPlan indica que el body tiene más de una ocurrencia del header
// "## Plan consolidado". No hay forma de elegir entre ellos automáticamente;
// el caller debe abortar y pedir intervención humana. Se envuelve con %w para
// que los call sites puedan usar errors.Is.
var ErrAmbiguousPlan = fmt.Errorf("body has multiple '## Plan consolidado' headers")

// Parse extrae las secciones del body consolidado que escribe `che explore`.
//
// Contract:
//   - Si el body contiene >1 header Markdown real "## Plan consolidado"
//     (ignorando ocurrencias dentro de texto o de bloques fenced) devuelve
//     ErrAmbiguousPlan (wrapped) — el caller debe abortar con mensaje
//     accionable.
//   - En cualquier otro caso (body vacío, header ausente, header único sin
//     sub-secciones parseables, secciones vacías) devuelve
//     (&ConsolidatedPlan{Summary: body}, nil) sin loguear nada. El caller
//     decide si el resultado es procesable: un plan con Summary=body (sin
//     Goal/Steps/AC) es un issue legacy válido, pero un plan con header
//     presente y sub-secciones vacías típicamente es un plan degradado que
//     conviene re-consolidar — ese distinction la hace el caller (ver
//     HasConsolidatedHeader).
//
// Detección y extracción son consistentes: ambas usan findRealHeaders, que
// solo cuenta líneas que empiezan exactamente con el prefijo y están fuera
// de bloques fenced. Una mención del header en prosa o dentro de un code
// fence NO dispara el branch de "header presente".
func Parse(body string) (*ConsolidatedPlan, error) {
	body = strings.TrimSpace(body)

	headers := findRealHeaders(body, consolidatedHeader)

	// Ambigüedad: múltiples headers del plan consolidado. No podemos elegir,
	// el caller debe abortar.
	if len(headers) > 1 {
		return nil, ErrAmbiguousPlan
	}

	// Sin header real → fallback silencioso. El caller ve que Goal/Steps/AC
	// quedaron vacíos y actúa acorde.
	if len(headers) == 0 {
		return &ConsolidatedPlan{Summary: body}, nil
	}

	// Header presente (único). Acotamos las búsquedas de sub-secciones al
	// cuerpo del plan consolidado para que un `### Pasos` que aparezca en
	// "## Idea original" o en otra sección posterior no filtre acá.
	p := &ConsolidatedPlan{}
	section := extractSection(body, consolidatedHeader)
	if section != "" {
		// La primera línea suele ser "**Resumen:** ..."
		if idx := strings.Index(section, "**Resumen:**"); idx >= 0 {
			rest := section[idx+len("**Resumen:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Summary = strings.TrimSpace(rest[:nl])
			} else {
				p.Summary = strings.TrimSpace(rest)
			}
		}
		if idx := strings.Index(section, "**Goal:**"); idx >= 0 {
			rest := section[idx+len("**Goal:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Goal = strings.TrimSpace(rest[:nl])
			} else {
				p.Goal = strings.TrimSpace(rest)
			}
		}
	}
	if v := extractSection(section, "### Criterios de aceptación"); v != "" {
		p.AcceptanceCriteria = parseChecklist(v)
	}
	if v := extractSection(section, "### Approach"); v != "" {
		p.Approach = strings.TrimSpace(v)
	}
	if v := extractSection(section, "### Pasos"); v != "" {
		p.Steps = parseNumbered(v)
	}
	if v := extractSection(section, "### Fuera de alcance"); v != "" {
		p.OutOfScope = parseBullets(v)
	}
	if v := extractSection(section, "### Riesgos a mitigar"); v != "" {
		p.RisksToMitigate = parseRisks(v)
	}

	// Header único pero sin contenido parseable: fallback silencioso. El
	// caller distingue este caso porque HasConsolidatedHeader devuelve true
	// sobre el body pero Goal/Steps/AC quedan vacíos.
	if p.Summary == "" && p.Goal == "" && len(p.Steps) == 0 && len(p.AcceptanceCriteria) == 0 {
		return &ConsolidatedPlan{Summary: body}, nil
	}

	return p, nil
}

// HasConsolidatedHeader indica si body tiene al menos una ocurrencia real
// (fuera de prosa y de bloques fenced) del header "## Plan consolidado".
// Es la misma detección que usa Parse para decidir el branch de fallback vs
// extracción; se expone para que el caller pueda diferenciar un issue legacy
// (sin header) de un plan degradado (con header pero sub-secciones vacías).
func HasConsolidatedHeader(body string) bool {
	return len(findRealHeaders(strings.TrimSpace(body), consolidatedHeader)) > 0
}

// findRealHeaders devuelve los offsets (en bytes) del comienzo de cada línea
// del body que arranca exactamente con prefix y NO está dentro de un bloque
// fenced (``` ... ```). Esto es lo que define "header Markdown real" para
// efectos del parser: una mención del prefijo en prosa o como ejemplo en un
// code fence no cuenta. Es la única fuente de verdad para detección y para
// la búsqueda inicial en extractSection — así detección, extracción y
// ambigüedad usan exactamente el mismo criterio.
func findRealHeaders(body, prefix string) []int {
	var positions []int
	inFence := false
	offset := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		} else if !inFence && strings.HasPrefix(line, prefix) {
			positions = append(positions, offset)
		}
		offset += len(line) + 1
	}
	return positions
}

// extractSection devuelve el texto entre un header (ej. "## X") y el próximo
// header de nivel <= al del header dado. Devuelve "" si no encuentra.
//
// La búsqueda del header inicial usa findRealHeaders: solo matchea líneas
// que arrancan exactamente con el prefijo y están fuera de bloques fenced.
// Esto evita falsos positivos cuando el prefijo aparece en prosa o como
// ejemplo dentro de un code fence — y mantiene paridad con la lógica de
// detección/ambigüedad de Parse.
//
// Fenced code blocks: el extractor también trackea inFence en líneas que
// empiezan con ``` (con o sin lenguaje, ej. ```go) y NO corta la sección en
// headers que aparezcan dentro del fence — sólo headers reales en el flow
// del markdown terminan la extracción. Los edge cases que quedan fuera son
// fences mal balanceados (sin cierre) o fences indentados con tabs/espacios
// raros; en esos casos el comportamiento es best-effort.
func extractSection(body, header string) string {
	headers := findRealHeaders(body, header)
	if len(headers) == 0 {
		return ""
	}
	rest := body[headers[0]+len(header):]
	lines := strings.Split(rest, "\n")
	var out []string
	// Determinar el nivel del header (# count).
	level := 0
	for _, c := range header {
		if c == '#' {
			level++
		} else {
			break
		}
	}
	inFence := false
	for i, line := range lines {
		if i == 0 {
			out = append(out, line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		// Track fenced code blocks para no confundir líneas "###" dentro de
		// un fence con headers reales. El toggle cubre ``` y ```lang.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if !inFence && strings.HasPrefix(trimmed, "#") {
			// Contar nivel.
			n := 0
			for _, c := range trimmed {
				if c == '#' {
					n++
				} else {
					break
				}
			}
			if n > 0 && n <= level {
				break
			}
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// parseChecklist extrae items de un bloque "- [ ] foo\n- [x] bar".
var checklistRe = regexp.MustCompile(`^\s*-\s*\[.\]\s*(.+)$`)

func parseChecklist(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := checklistRe.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// parseNumbered extrae items de "1. foo\n2. bar".
var numberedRe = regexp.MustCompile(`^\s*\d+\.\s+(.+)$`)

func parseNumbered(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := numberedRe.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// parseBullets extrae items de "- foo\n- bar". Ignora checklist items.
var bulletRe = regexp.MustCompile(`^\s*-\s+(.+)$`)

func parseBullets(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			text := strings.TrimSpace(m[1])
			if !strings.HasPrefix(text, "[") {
				out = append(out, text)
			}
		}
	}
	return out
}

// parseRisks parsea el bloque "Riesgos a mitigar" que Render emite como
// "- **<risk>** (likelihood=X, impact=Y) — <mitigation>". Es best-effort: si
// una línea no matchea, se saltea. El Render siempre usa este formato exacto,
// pero issues legacy o ediciones humanas pueden romperlo; en ese caso el risk
// simplemente no vuelve al plan parseado.
var riskRe = regexp.MustCompile(`^\s*-\s+\*\*(.+?)\*\*\s+\(likelihood=(\w+),\s*impact=(\w+)\)\s+[—-]\s+(.+)$`)

func parseRisks(s string) []Risk {
	var out []Risk
	for _, line := range strings.Split(s, "\n") {
		if m := riskRe.FindStringSubmatch(line); m != nil {
			out = append(out, Risk{
				Risk:       strings.TrimSpace(m[1]),
				Likelihood: m[2],
				Impact:     m[3],
				Mitigation: strings.TrimSpace(m[4]),
			})
		}
	}
	return out
}
