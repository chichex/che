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
// Summary: body}, nil) y loguea un warning — el caller puede seguir con un
// plan degradado en vez de abortar.
package plan

import (
	"fmt"
	"log"
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
//   - Si el body contiene >1 ocurrencia de "## Plan consolidado" devuelve
//     ErrAmbiguousPlan (wrapped) — el caller debe abortar con mensaje
//     accionable.
//   - En cualquier otro caso (body vacío, header ausente, header único sin
//     sub-secciones parseables, secciones vacías) devuelve
//     (&ConsolidatedPlan{Summary: body}, nil) y loguea un warning. El
//     ejecutor puede trabajar con eso aunque sea menos guiado — el issue
//     legacy sigue siendo procesable.
func Parse(body string) (*ConsolidatedPlan, error) {
	body = strings.TrimSpace(body)

	// Ambigüedad: múltiples headers del plan consolidado. No podemos elegir,
	// el caller debe abortar.
	if strings.Count(body, consolidatedHeader) > 1 {
		return nil, ErrAmbiguousPlan
	}

	// Sin header → fallback silencioso con warning.
	if !strings.Contains(body, consolidatedHeader) {
		if body != "" {
			log.Printf("plan.Parse: body sin header '%s', usando summary=body como fallback", consolidatedHeader)
		} else {
			log.Printf("plan.Parse: body vacío, usando summary=\"\" como fallback")
		}
		return &ConsolidatedPlan{Summary: body}, nil
	}

	// Header presente (único). Intentar extraer secciones.
	p := &ConsolidatedPlan{}
	if v := extractSection(body, consolidatedHeader); v != "" {
		// La primera línea suele ser "**Resumen:** ..."
		if idx := strings.Index(v, "**Resumen:**"); idx >= 0 {
			rest := v[idx+len("**Resumen:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Summary = strings.TrimSpace(rest[:nl])
			} else {
				p.Summary = strings.TrimSpace(rest)
			}
		}
		if idx := strings.Index(v, "**Goal:**"); idx >= 0 {
			rest := v[idx+len("**Goal:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Goal = strings.TrimSpace(rest[:nl])
			} else {
				p.Goal = strings.TrimSpace(rest)
			}
		}
	}
	if v := extractSection(body, "### Criterios de aceptación"); v != "" {
		p.AcceptanceCriteria = parseChecklist(v)
	}
	if v := extractSection(body, "### Approach"); v != "" {
		p.Approach = strings.TrimSpace(v)
	}
	if v := extractSection(body, "### Pasos"); v != "" {
		p.Steps = parseNumbered(v)
	}
	if v := extractSection(body, "### Fuera de alcance"); v != "" {
		p.OutOfScope = parseBullets(v)
	}
	if v := extractSection(body, "### Riesgos a mitigar"); v != "" {
		p.RisksToMitigate = parseRisks(v)
	}

	// Header único pero sin contenido parseable: fallback con warning.
	if p.Summary == "" && p.Goal == "" && len(p.Steps) == 0 && len(p.AcceptanceCriteria) == 0 {
		log.Printf("plan.Parse: header '%s' presente pero sin sub-secciones parseables, usando summary=body como fallback", consolidatedHeader)
		return &ConsolidatedPlan{Summary: body}, nil
	}

	return p, nil
}

// extractSection devuelve el texto entre un header (ej. "## X") y el próximo
// header de nivel <= al del header dado. Devuelve "" si no encuentra.
//
// Limitación conocida: no ignora líneas dentro de bloques ```code fenced. Si
// el contenido de una sección incluye un header como texto dentro de un
// fence (ej. ```### ejemplo``` en Approach), el extractor puede truncar la
// sección ahí. Los fixtures de round-trip cubren este caso con fenced code
// que contiene `###` y lo manejamos explícitamente cuando es detectable.
func extractSection(body, header string) string {
	idx := strings.Index(body, header)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(header):]
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
