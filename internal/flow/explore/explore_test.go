package explore

import (
	"testing"
)

// TestCoversSamePlanQuestion_QuotedMatch: validators que citan textualmente
// la pregunta del plan entre comillas y la glosan como "decisión de
// producto" deben ser detectados como cubiertos.
func TestCoversSamePlanQuestion_QuotedMatch(t *testing.T) {
	planQs := []Question{
		{Q: "¿Cuál es el input de `che execute`? ¿Un issue-ref, un prompt arbitrario, o ambos?", Blocker: true},
	}
	finding := "'¿Cuál es el input de che execute?' es una decisión de producto del dueño del CLI, no algo que el ejecutor pueda resolver leyendo código."

	if !coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("expected finding to be detected as duplicate of plan question (quoted match)")
	}
}

// TestCoversSamePlanQuestion_QuotedShortMatch: el quoted ("¿Qué significa
// ejecutar?") es más corto que la pregunta del plan ("¿Qué significa
// exactamente 'ejecutar'?") — igual debe detectarse porque sus tokens
// significativos son subset del plan.
func TestCoversSamePlanQuestion_QuotedShortMatch(t *testing.T) {
	planQs := []Question{
		{Q: "¿Qué significa exactamente 'ejecutar'? ¿Sólo correr el agente y postear output, o escribir archivos?", Blocker: true},
	}
	finding := "'¿Qué significa ejecutar?' (comentario vs archivos vs PR) es decisión de producto — define el blast radius del subcomando."

	if !coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("expected finding to be detected as duplicate via quoted subset")
	}
}

// TestCoversSamePlanQuestion_MetaPhraseOverlap: sin comillas pero con frase
// meta + 3+ tokens significativos compartidos con el plan.
func TestCoversSamePlanQuestion_MetaPhraseOverlap(t *testing.T) {
	planQs := []Question{
		{Q: "¿Requiere el issue estar en status:plan para ejecutar, y a qué label transiciona al terminar?", Blocker: true},
	}
	finding := "Pre/post condiciones de labels (status:plan requerido, transición final) son parte del modelo de estado del embudo — decisión de producto."

	if !coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("expected finding to be detected as duplicate via meta phrase + token overlap")
	}
}

// TestCoversSamePlanQuestion_GenuineExtra: un finding legítimo sobre un tema
// distinto (permisos de auth) no debería ser marcado como cubierto aunque
// mencione algún token común.
func TestCoversSamePlanQuestion_GenuineExtra(t *testing.T) {
	planQs := []Question{
		{Q: "¿Cuál es el input de `che execute`? ¿Un issue-ref, un prompt arbitrario, o ambos?", Blocker: true},
	}
	finding := "Falta considerar scopes de GITHUB_TOKEN y manejo de secretos si execute abre PRs — requerimientos de seguridad distintos a explore."

	if coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("did not expect genuine-new finding to be marked as duplicate")
	}
}

// TestCollectValidatorQuestions_FiltersDuplicates: reproduce el caso del
// issue #5 — el validator opus#1 citó textualmente las 3 preguntas del plan.
// collectValidatorQuestions debe descartar las 3 y devolver slice vacío (no
// hay observaciones genuinamente nuevas).
func TestCollectValidatorQuestions_FiltersDuplicates(t *testing.T) {
	planQs := []Question{
		{Q: "¿Cuál es el input de `che execute`? ¿Un issue-ref como en explore, un prompt arbitrario, o ambos?", Blocker: true},
		{Q: "¿Qué significa exactamente 'ejecutar'? ¿Postear comentario, escribir archivos, abrir branch+PR, o stdout?", Blocker: true},
		{Q: "¿Requiere el issue estar en status:plan para ejecutar, y a qué label transiciona al terminar?", Blocker: true},
	}
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentOpus, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true,
						Issue: "'¿Cuál es el input de che execute?' es una decisión de producto del dueño del CLI, no algo que el ejecutor pueda resolver leyendo código."},
					{Severity: "blocker", Area: "questions", NeedsHuman: true,
						Issue: "'¿Qué significa ejecutar?' (comentario vs archivos vs PR) es decisión de producto — define el blast radius del subcomando."},
					{Severity: "blocker", Area: "questions", NeedsHuman: true,
						Issue: "Pre/post condiciones de labels (status:plan requerido, transición final) son parte del modelo de estado del embudo — decisión de producto."},
				},
			},
		},
	}

	extras := collectValidatorQuestions(results, planQs)
	if len(extras) != 0 {
		t.Fatalf("expected all 3 validator findings to be filtered as duplicates of plan questions, got %d extras: %+v",
			len(extras), extras)
	}
}

// TestCollectValidatorQuestions_KeepsGenuine: un finding genuinamente nuevo
// (sobre permisos de auth) debe pasar a la lista de extras.
func TestCollectValidatorQuestions_KeepsGenuine(t *testing.T) {
	planQs := []Question{
		{Q: "¿Cuál es el input de `che execute`?", Blocker: true},
	}
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentGemini, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "major", Area: "security", NeedsHuman: true,
						Issue: "Falta definir scopes de GITHUB_TOKEN necesarios si execute abre PRs — requiere decisión sobre permisos de API."},
				},
			},
		},
	}

	extras := collectValidatorQuestions(results, planQs)
	if len(extras) != 1 {
		t.Fatalf("expected 1 genuine extra, got %d: %+v", len(extras), extras)
	}
	if extras[0].source != "gemini#1" {
		t.Fatalf("expected source=gemini#1, got %s", extras[0].source)
	}
}

// TestCollectPlanBlockers_FiltersNonProductKinds: preguntas blocker con
// kind=technical o kind=documented NO entran al human-request. Solo las
// kind=product (o sin kind, back-compat) se listan.
func TestCollectPlanBlockers_FiltersNonProductKinds(t *testing.T) {
	plan := &Response{
		Questions: []Question{
			{Q: "¿Política de reintentos? Si falla, ¿retry silencioso o aborto?", Blocker: true, Kind: KindProduct},
			{Q: "¿Migramos también transitionLabels? (alcance del refactor)", Blocker: true, Kind: KindTechnical},
			{Q: "¿Preservamos el callback progress() al migrar a labels.Ensure?", Blocker: true, Kind: KindTechnical},
			{Q: "¿El issue ya pide cerrar este punto? Lo dice el body.", Blocker: true, Kind: KindDocumented},
			{Q: "Pregunta legacy sin kind (compat backwards)", Blocker: true},
		},
	}

	got := collectPlanBlockers(plan)
	if len(got) != 2 {
		t.Fatalf("expected 2 blockers (1 product explícito + 1 legacy sin kind = product default), got %d: %+v", len(got), got)
	}
	for _, q := range got {
		if q.QuestionKind() != KindProduct {
			t.Fatalf("expected only kind=product blockers, got %q for %q", q.QuestionKind(), q.Q)
		}
	}
}

// TestHasHumanGaps_IgnoresNonProductFindings: un validator con verdict
// needs_human pero findings kind=technical NO debe pausar el flow.
func TestHasHumanGaps_IgnoresNonProductFindings(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentCodex, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindTechnical,
						Issue: "El ejecutor debió decidir si preservar el callback progress()."},
					{Severity: "minor", Area: "questions", NeedsHuman: true, Kind: KindDocumented,
						Issue: "La respuesta está en el body del issue."},
				},
			},
		},
	}
	if hasHumanGaps(results) {
		t.Fatalf("expected hasHumanGaps=false cuando todos los findings son technical/documented")
	}
}

// TestHasHumanGaps_TrueOnProductFinding: un solo finding kind=product con
// needs_human=true sí debe escalar.
func TestHasHumanGaps_TrueOnProductFinding(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentGemini, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿Política de retención tras N días sin respuesta?"},
					{Severity: "minor", Area: "questions", NeedsHuman: true, Kind: KindTechnical,
						Issue: "Naming del helper (no escala pero se reporta)."},
				},
			},
		},
	}
	if !hasHumanGaps(results) {
		t.Fatalf("expected hasHumanGaps=true cuando hay al menos un finding product needs_human")
	}
}

// TestHasHumanGaps_BackwardsCompat_MissingKind: findings viejos sin kind
// deben tratarse como product para no romper fixtures previas.
func TestHasHumanGaps_BackwardsCompat_MissingKind(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentOpus, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true,
						Issue: "¿Cuándo cortamos el loop si no converge?"},
				},
			},
		},
	}
	if !hasHumanGaps(results) {
		t.Fatalf("expected hasHumanGaps=true para findings sin kind (default=product, compat)")
	}
}

// TestCollectValidatorQuestions_DropsNonProductFindings: un finding
// technical con needs_human=true (inconsistente) no debería aparecer en
// las "observaciones adicionales" del human-request.
func TestCollectValidatorQuestions_DropsNonProductFindings(t *testing.T) {
	planQs := []Question{
		{Q: "¿Política de rollback?", Blocker: true, Kind: KindProduct},
	}
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentCodex, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "changes_requested",
				Findings: []Finding{
					{Severity: "minor", Area: "questions", NeedsHuman: true, Kind: KindTechnical,
						Issue: "La decisión sobre naming del helper es técnica, no de producto."},
					{Severity: "major", Area: "questions", NeedsHuman: true, Kind: KindDocumented,
						Issue: "Esto ya está resuelto en el body del issue."},
				},
			},
		},
	}

	extras := collectValidatorQuestions(results, planQs)
	if len(extras) != 0 {
		t.Fatalf("expected 0 extras (technical + documented no escalan), got %d: %+v", len(extras), extras)
	}
}

// TestValidateResponse_RejectsNonProductQuestion: el ejecutor que deja una
// question con kind=technical en questions[] debe fallar la validación (es
// un bug del ejecutor, no lo filtramos silencioso).
func TestValidateResponse_RejectsNonProductQuestion(t *testing.T) {
	r := &Response{
		Summary: "Ok",
		Questions: []Question{
			{Q: "¿Decisión técnica que debería ser assumption?", Blocker: true, Kind: KindTechnical},
		},
		Risks: []Risk{{Risk: "x", Likelihood: "low", Impact: "low", Mitigation: "y"}},
		Paths: []Path{
			{Title: "A", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: true},
			{Title: "B", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: false},
		},
		NextStep: "paso",
	}
	if err := validate(r); err == nil {
		t.Fatalf("expected validate() to reject question with kind=technical; got nil")
	}
}

// TestValidateResponse_AcceptsEmptyQuestions: con el prompt nuevo es válido
// devolver questions=[] si el ejecutor cerró todo como assumptions.
func TestValidateResponse_AcceptsEmptyQuestions(t *testing.T) {
	r := &Response{
		Summary:     "Ok",
		Questions:   nil,
		Assumptions: []Assumption{{What: "Usamos labels.Ensure directo", Why: "Best practice existente"}},
		Risks:       []Risk{{Risk: "x", Likelihood: "low", Impact: "low", Mitigation: "y"}},
		Paths: []Path{
			{Title: "A", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: true},
			{Title: "B", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: false},
		},
		NextStep: "paso",
	}
	if err := validate(r); err != nil {
		t.Fatalf("expected validate() to accept empty questions[], got: %v", err)
	}
}

// TestNormalizeKind_UnknownKindTreatedAsEmpty: un LLM que devuelve "prod" o
// "tecnico" (variante) debe quedar como "" → default "product" al aplicar
// kindOrDefault. Así no propagamos etiquetas fantasía que podrían romper
// los filtros.
func TestNormalizeKind_UnknownKindTreatedAsEmpty(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"product", "product"},
		{"PRODUCT", "product"},
		{"  technical ", "technical"},
		{"documented", "documented"},
		{"prod", ""},
		{"tecnico", ""},
		{"business", ""},
	}
	for _, c := range cases {
		got := normalizeKind(c.in)
		if got != c.want {
			t.Errorf("normalizeKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// kindOrDefault cierra la compat: cualquier cosa no válida se trata
	// como product (el default conservador).
	if got := kindOrDefault("prod"); got != KindProduct {
		t.Errorf("kindOrDefault(\"prod\") = %q, want %q", got, KindProduct)
	}
}
