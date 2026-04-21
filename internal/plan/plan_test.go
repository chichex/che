package plan

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// roundTrip valida que Parse(Render(p)) produce un plan con los mismos
// campos "estructurados" que el original. No compara campos que Render no
// persiste (ej. originalBody queda fuera).
func roundTrip(t *testing.T, name string, p *ConsolidatedPlan) {
	t.Helper()
	rendered := Render(p, "(body original)")
	got, err := Parse(rendered)
	if err != nil {
		t.Fatalf("%s: Parse(Render(p)) err: %v", name, err)
	}
	if got.Summary != p.Summary {
		t.Errorf("%s: Summary: got %q, want %q", name, got.Summary, p.Summary)
	}
	if got.Goal != p.Goal {
		t.Errorf("%s: Goal: got %q, want %q", name, got.Goal, p.Goal)
	}
	if !reflect.DeepEqual(nonNil(got.AcceptanceCriteria), nonNil(p.AcceptanceCriteria)) {
		t.Errorf("%s: AcceptanceCriteria: got %v, want %v", name, got.AcceptanceCriteria, p.AcceptanceCriteria)
	}
	// Approach puede tener trailing whitespace tras el round-trip; comparamos
	// con TrimSpace para ser robustos a los "\n\n" que inserta Render.
	if strings.TrimSpace(got.Approach) != strings.TrimSpace(p.Approach) {
		t.Errorf("%s: Approach: got %q, want %q", name, got.Approach, p.Approach)
	}
	if !reflect.DeepEqual(nonNil(got.Steps), nonNil(p.Steps)) {
		t.Errorf("%s: Steps: got %v, want %v", name, got.Steps, p.Steps)
	}
	if !reflect.DeepEqual(nonNilRisks(got.RisksToMitigate), nonNilRisks(p.RisksToMitigate)) {
		t.Errorf("%s: RisksToMitigate: got %v, want %v", name, got.RisksToMitigate, p.RisksToMitigate)
	}
	if !reflect.DeepEqual(nonNil(got.OutOfScope), nonNil(p.OutOfScope)) {
		t.Errorf("%s: OutOfScope: got %v, want %v", name, got.OutOfScope, p.OutOfScope)
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilRisks(s []Risk) []Risk {
	if s == nil {
		return []Risk{}
	}
	return s
}

func TestRoundTrip_Minimal(t *testing.T) {
	p := &ConsolidatedPlan{
		Summary:            "una línea",
		Goal:               "hacer X",
		AcceptanceCriteria: []string{"crit único"},
		Approach:           "approach breve",
		Steps:              []string{"paso único"},
	}
	roundTrip(t, "minimal", p)
}

func TestRoundTrip_Complete(t *testing.T) {
	p := &ConsolidatedPlan{
		Summary:            "Implementar feature foo",
		Goal:               "El usuario puede hacer foo sin escribir código.",
		AcceptanceCriteria: []string{"crit 1", "crit 2", "crit 3"},
		Approach:           "Agregar handler en pkg/foo y wirearlo en main.",
		Steps:              []string{"crear foo.go", "agregar tests", "wirear en main"},
		RisksToMitigate: []Risk{
			{Risk: "race en writer", Likelihood: "low", Impact: "high", Mitigation: "mutex en struct"},
			{Risk: "panic en edge case", Likelihood: "medium", Impact: "medium", Mitigation: "validar antes"},
		},
		OutOfScope: []string{"UI changes", "refactor del paquete adyacente"},
	}
	roundTrip(t, "complete", p)
}

func TestRoundTrip_EmptySections(t *testing.T) {
	// Sin AcceptanceCriteria, sin Risks, sin OutOfScope. Summary/Goal/Steps
	// son los campos "required" por el shape del agente.
	p := &ConsolidatedPlan{
		Summary:  "resumen",
		Goal:     "goal",
		Approach: "approach",
		Steps:    []string{"paso 1"},
	}
	roundTrip(t, "empty sections", p)
}

func TestRoundTrip_MixedCheckboxes(t *testing.T) {
	// Este test arranca del markdown directamente porque Render siempre
	// emite "- [ ]", pero issues legacy pueden tener una mezcla "- [ ]" /
	// "- [x]". El parser tiene que tolerar ambos.
	body := `## Plan consolidado (post-exploración)

**Resumen:** r

**Goal:** g

### Criterios de aceptación
- [ ] crit pendiente
- [x] crit completado
- [ ] otro pendiente

### Approach
a

### Pasos
1. paso uno
`
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"crit pendiente", "crit completado", "otro pendiente"}
	if !reflect.DeepEqual(p.AcceptanceCriteria, want) {
		t.Errorf("criteria: got %v, want %v", p.AcceptanceCriteria, want)
	}
}

func TestRoundTrip_LegacyHeaderWithParens(t *testing.T) {
	// Un body con el header parentheticado (como lo emite Render). Hay que
	// parsear igual que "## Plan consolidado" solo.
	body := `## Plan consolidado (post-exploración)

**Resumen:** resumen legacy

**Goal:** goal legacy

### Criterios de aceptación
- [ ] c1

### Approach
approach legacy

### Pasos
1. p1
`
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != "resumen legacy" {
		t.Errorf("summary: %q", p.Summary)
	}
	if p.Goal != "goal legacy" {
		t.Errorf("goal: %q", p.Goal)
	}
	if len(p.Steps) != 1 || p.Steps[0] != "p1" {
		t.Errorf("steps: %v", p.Steps)
	}
}

func TestRoundTrip_FencedCodeWithInnerHeaders(t *testing.T) {
	// Approach/Pasos con bloque de código que contiene "###" dentro. El
	// extractor tiene que ignorar esas líneas como headers y no truncar la
	// sección ahí.
	p := &ConsolidatedPlan{
		Summary: "s",
		Goal:    "g",
		AcceptanceCriteria: []string{"c"},
		Approach: "Usar este snippet:\n" +
			"```go\n" +
			"// ### inner header dentro de fence\n" +
			"func foo() {}\n" +
			"```\n" +
			"Listo.",
		Steps: []string{"step con fence"},
	}
	rendered := Render(p, "original")
	got, err := Parse(rendered)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(got.Approach, "### inner header dentro de fence") {
		t.Errorf("approach truncado en fence: %q", got.Approach)
	}
	if !strings.Contains(got.Approach, "Listo.") {
		t.Errorf("approach perdió línea post-fence: %q", got.Approach)
	}
	if len(got.Steps) != 1 || got.Steps[0] != "step con fence" {
		t.Errorf("steps: %v", got.Steps)
	}
}

func TestParse_AmbiguousMultipleHeaders(t *testing.T) {
	body := `## Plan consolidado

**Resumen:** primer plan

### Pasos
1. uno

---

## Plan consolidado (post-exploración)

**Resumen:** segundo plan

### Pasos
1. dos
`
	_, err := Parse(body)
	if err == nil {
		t.Fatalf("expected ErrAmbiguousPlan, got nil")
	}
	if !errors.Is(err, ErrAmbiguousPlan) {
		t.Errorf("expected ErrAmbiguousPlan, got %v", err)
	}
}

func TestParse_NotAmbiguousWhenHeaderInIdeaBody(t *testing.T) {
	// "## Plan consolidado" aparece dentro de la idea original como texto
	// citado (no como header real de nivel 2 al comienzo de línea). El
	// parser NO debería marcar esto como ambiguo.
	body := `## Plan consolidado (post-exploración)

**Resumen:** r

**Goal:** g

### Pasos
1. paso único

---

## Idea original

El usuario dice: "quiero el siguiente '## Plan consolidado' dentro del texto porque hablo de planes consolidados en la idea original".
`
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("unexpected ErrAmbiguousPlan on in-body occurrence: %v", err)
	}
	if p.Summary != "r" {
		t.Errorf("summary: %q", p.Summary)
	}
}

func TestParse_NotAmbiguousWhenHeaderInFencedCode(t *testing.T) {
	// "## Plan consolidado" aparece dentro de un code fence como ejemplo.
	// No es un header real y el parser NO debería marcarlo como ambiguo.
	body := "## Plan consolidado\n\n" +
		"**Resumen:** r\n\n" +
		"**Goal:** g\n\n" +
		"### Approach\n" +
		"Ejemplo de cómo NO escribir el body:\n\n" +
		"```markdown\n" +
		"## Plan consolidado\n" +
		"no me metan texto acá\n" +
		"```\n\n" +
		"### Pasos\n" +
		"1. paso único\n"
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("unexpected ErrAmbiguousPlan on fenced occurrence: %v", err)
	}
	if p.Summary != "r" {
		t.Errorf("summary: %q", p.Summary)
	}
}

func TestCountConsolidatedHeaders(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "single real header",
			body: "## Plan consolidado\n\nfoo\n",
			want: 1,
		},
		{
			name: "two real headers",
			body: "## Plan consolidado\n\nfoo\n\n## Plan consolidado (post-exploración)\nbar\n",
			want: 2,
		},
		{
			name: "occurrence in prose does not count",
			body: "## Plan consolidado\n\nfoo\n\nel texto ## Plan consolidado está en una línea con contenido previo no cuenta.\n",
			want: 1,
		},
		{
			name: "occurrence in fenced code does not count",
			body: "## Plan consolidado\n\nfoo\n\n```\n## Plan consolidado\n```\n",
			want: 1,
		},
		{
			name: "zero when no header",
			body: "no hay header\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countConsolidatedHeaders(c.body); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestParse_NoRealHeaderEvenWithSubSectionsAndProseMention(t *testing.T) {
	// Regresión: el body NO tiene un header real "## Plan consolidado" — la
	// cadena solo aparece en prosa o dentro de un bloque fenced. Aunque haya
	// sub-secciones tipo `### Pasos` en el body, NO debería parsearse como
	// si fuera un plan válido. La detección y la extracción usan el mismo
	// criterio (línea de header real fuera de fences), así que el resultado
	// esperado es el fallback (Summary=body, sin Goal/Steps/AC).
	cases := []struct {
		name string
		body string
	}{
		{
			name: "mención en prosa + ### Pasos posterior",
			body: "Quiero discutir el ## Plan consolidado en esta línea de prosa.\n" +
				"\n" +
				"### Pasos\n" +
				"1. paso fantasma\n",
		},
		{
			name: "mención dentro de fence + ### Pasos posterior",
			body: "Acá un ejemplo de cómo NO escribir un plan:\n" +
				"\n" +
				"```markdown\n" +
				"## Plan consolidado\n" +
				"**Resumen:** ejemplo dentro de un fence\n" +
				"```\n" +
				"\n" +
				"### Pasos\n" +
				"1. paso fantasma\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Parse(c.body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.Goal != "" {
				t.Errorf("expected empty Goal (no real header), got %q", p.Goal)
			}
			if len(p.Steps) != 0 {
				t.Errorf("expected empty Steps (no real header), got %v", p.Steps)
			}
			if len(p.AcceptanceCriteria) != 0 {
				t.Errorf("expected empty AcceptanceCriteria, got %v", p.AcceptanceCriteria)
			}
			if !strings.Contains(p.Summary, "paso fantasma") {
				t.Errorf("expected fallback Summary=body (que incluye 'paso fantasma'), got %q", p.Summary)
			}
		})
	}
}

func TestExtractSection_IgnoresHeaderInProseAndFence(t *testing.T) {
	// extractSection debe usar el mismo criterio de "header real" que
	// findRealHeaders: una mención del prefijo en prosa o dentro de un fence
	// no cuenta como sección.
	body := "Esto menciona el ### Approach pero no es un header real.\n" +
		"\n" +
		"```\n" +
		"### Approach dentro de un fence tampoco cuenta\n" +
		"con texto\n" +
		"```\n" +
		"\n" +
		"texto final fuera de cualquier sección.\n"
	got := extractSection(body, "### Approach")
	if got != "" {
		t.Errorf("expected empty (no real header), got %q", got)
	}
}

func TestParse_FallbackWhenNoHeader(t *testing.T) {
	body := "Body sin plan consolidado, solo texto libre."
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != body {
		t.Errorf("expected summary=body, got %q", p.Summary)
	}
	if p.Goal != "" || len(p.Steps) != 0 {
		t.Errorf("expected empty goal/steps in fallback")
	}
}

func TestParse_EmptyBody(t *testing.T) {
	p, err := Parse("")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != "" {
		t.Errorf("expected empty summary, got %q", p.Summary)
	}
}

func TestParse_HeaderButNoContent(t *testing.T) {
	body := "## Plan consolidado\n\n(lorem sin sub-secciones parseables)\n"
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Fallback: summary=body (el body completo), no error.
	if !strings.Contains(p.Summary, "Plan consolidado") {
		t.Errorf("expected fallback summary=body, got %q", p.Summary)
	}
}

func TestParse_FullBody(t *testing.T) {
	body := `## Plan consolidado (post-exploración)

**Resumen:** Agregar comando che execute.

**Goal:** Un desarrollador selecciona un issue y che execute lo ejecuta end-to-end.

### Criterios de aceptación
- [ ] che execute registrado como subcomando cobra
- [ ] La TUI lista solo issues con ct:plan + status:plan
- [ ] No tocar explore

### Approach
Construir execute como copia adaptada de explore.

### Pasos
1. Crear internal/flow/execute
2. Wirear cmd/execute.go
3. Agregar tests e2e

### Fuera de alcance
- Ciclo iter con scope-lock
- Workflow GHA

---

## Idea original

Lorem ipsum
`
	p, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != "Agregar comando che execute." {
		t.Errorf("summary: %q", p.Summary)
	}
	if !strings.Contains(p.Goal, "end-to-end") {
		t.Errorf("goal: %q", p.Goal)
	}
	if len(p.AcceptanceCriteria) != 3 {
		t.Errorf("criteria: got %d items: %v", len(p.AcceptanceCriteria), p.AcceptanceCriteria)
	}
	if p.AcceptanceCriteria[0] != "che execute registrado como subcomando cobra" {
		t.Errorf("criteria[0]: %q", p.AcceptanceCriteria[0])
	}
	if !strings.Contains(p.Approach, "copia adaptada") {
		t.Errorf("approach: %q", p.Approach)
	}
	if len(p.Steps) != 3 {
		t.Errorf("steps: got %d: %v", len(p.Steps), p.Steps)
	}
	if p.Steps[0] != "Crear internal/flow/execute" {
		t.Errorf("steps[0]: %q", p.Steps[0])
	}
	if len(p.OutOfScope) != 2 {
		t.Errorf("out_of_scope: got %d: %v", len(p.OutOfScope), p.OutOfScope)
	}
}

func TestExtractSection_StopsAtNextSameLevelHeader(t *testing.T) {
	body := `## A
foo
bar

## B
quux
`
	got := extractSection(body, "## A")
	if strings.Contains(got, "quux") {
		t.Errorf("section A should not include B: %q", got)
	}
	if !strings.Contains(got, "foo") || !strings.Contains(got, "bar") {
		t.Errorf("section A missing content: %q", got)
	}
}

func TestExtractSection_IncludesDeeperHeaders(t *testing.T) {
	body := `## Plan consolidado
**Resumen:** r

### Criterios
- [ ] crit
`
	got := extractSection(body, "## Plan consolidado")
	if !strings.Contains(got, "### Criterios") {
		t.Errorf("should include ### children: %q", got)
	}
}

func TestParse_RealIssueFixture(t *testing.T) {
	// Fixture: body del issue #8 (cerrado) tomado del repo. Valida que el
	// parser no rompe sobre un body real post-consolidación, incluyendo
	// secciones "Decisiones humanas" y "Riesgos a mitigar" que Render no
	// emite pero issues legacy pueden tener.
	path := filepath.Join("testdata", "issue-8.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p, err := Parse(string(data))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if p.Summary == "" {
		t.Error("summary empty — fixture parsing probably failed silently")
	}
	if p.Goal == "" {
		t.Error("goal empty")
	}
	if len(p.Steps) == 0 {
		t.Error("steps empty")
	}
	if len(p.AcceptanceCriteria) == 0 {
		t.Error("acceptance criteria empty")
	}
}

// TestJSONRoundTrip valida que los tags JSON preservan los nombres de campo
// que el agente de consolidación emite. Si alguien cambia un tag por
// accidente, este test se rompe antes de que se rompa el unmarshal real en
// explore.callConsolidation.
func TestJSONRoundTrip(t *testing.T) {
	raw := `{
  "summary": "s",
  "goal": "g",
  "acceptance_criteria": ["c1", "c2"],
  "approach": "a",
  "steps": ["p1", "p2"],
  "risks_to_mitigate": [
    {"risk": "r1", "likelihood": "low", "impact": "high", "mitigation": "m1"}
  ],
  "out_of_scope": ["oos1"]
}`
	var p ConsolidatedPlan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Summary != "s" {
		t.Errorf("summary: %q", p.Summary)
	}
	if p.Goal != "g" {
		t.Errorf("goal: %q", p.Goal)
	}
	if !reflect.DeepEqual(p.AcceptanceCriteria, []string{"c1", "c2"}) {
		t.Errorf("criteria: %v", p.AcceptanceCriteria)
	}
	if p.Approach != "a" {
		t.Errorf("approach: %q", p.Approach)
	}
	if !reflect.DeepEqual(p.Steps, []string{"p1", "p2"}) {
		t.Errorf("steps: %v", p.Steps)
	}
	if len(p.RisksToMitigate) != 1 || p.RisksToMitigate[0].Risk != "r1" {
		t.Errorf("risks: %v", p.RisksToMitigate)
	}
	if p.RisksToMitigate[0].Likelihood != "low" || p.RisksToMitigate[0].Impact != "high" || p.RisksToMitigate[0].Mitigation != "m1" {
		t.Errorf("risk fields: %+v", p.RisksToMitigate[0])
	}
	if !reflect.DeepEqual(p.OutOfScope, []string{"oos1"}) {
		t.Errorf("oos: %v", p.OutOfScope)
	}
	// Re-marshal y comparar que los tags siguen siendo los mismos.
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{`"summary"`, `"goal"`, `"acceptance_criteria"`, `"approach"`, `"steps"`, `"risks_to_mitigate"`, `"out_of_scope"`} {
		if !strings.Contains(string(out), k) {
			t.Errorf("marshaled JSON missing tag %s: %s", k, string(out))
		}
	}
}
