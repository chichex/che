package explore

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/comments"
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

// TestValidateAndNormalize_UpgradesApproveWithProductFinding: un validator que
// emite verdict=approve junto con un finding kind=product needs_human=true es
// un output inconsistente — el flow SÍ va a escalar por hasHumanGaps, así que
// tenemos que reflejarlo en el verdict mutado para que todos los consumers
// (renderers, embedded JSON, resume prompt) lo vean igual.
func TestValidateAndNormalize_UpgradesApproveWithProductFinding(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentCodex, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "approve",
				Summary: "Plan ok pero hay una decisión de producto pendiente.",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿Política de timeout del escape humano? Producto."},
				},
			},
		},
	}

	var stderr bytes.Buffer
	if err := validateAndNormalizeValidatorResults(results, &stderr); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if got := results[0].Response.Verdict; got != "needs_human" {
		t.Fatalf("expected verdict to be upgraded to needs_human, got %q", got)
	}
	if !strings.Contains(stderr.String(), "upgrading to needs_human") {
		t.Fatalf("expected warning in stderr about upgrade, got: %q", stderr.String())
	}
}

// TestValidateAndNormalize_UpgradesChangesRequestedWithProductFinding:
// mismo caso que el anterior pero arrancando desde changes_requested. La
// canonicalización dura debe cubrir cualquier verdict != needs_human.
func TestValidateAndNormalize_UpgradesChangesRequestedWithProductFinding(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentGemini, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "changes_requested",
				Summary: "Hay gaps técnicos y una decisión de producto.",
				Findings: []Finding{
					{Severity: "major", Area: "risks", Kind: KindTechnical, Issue: "Falta handling de error."},
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿Retry indefinido o timeout?"},
				},
			},
		},
	}

	var stderr bytes.Buffer
	if err := validateAndNormalizeValidatorResults(results, &stderr); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if got := results[0].Response.Verdict; got != "needs_human" {
		t.Fatalf("expected verdict to be upgraded to needs_human, got %q", got)
	}
}

// TestValidateAndNormalize_ResumeRoundTrip_ApproveWithProductFinding es el
// test estructural del fix 2c: un validator iter=1 emite approve+product
// finding; la canonicalización dura muta el struct a needs_human ANTES de
// que postValidatorComments renderice el embedded JSON. En iter=2 (resume),
// parseConversation relee ese JSON y buildValidatorResumePrompt debe
// mostrar verdict=needs_human en la sección de "findings previos", no el
// crudo approve. Sin la canonicalización dura, el resume prompt contaminaba
// al validador iter=2 con el verdict crudo.
func TestValidateAndNormalize_ResumeRoundTrip_ApproveWithProductFinding(t *testing.T) {
	// iter=1: el validator emite approve + product finding (crudo).
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentCodex, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "approve",
				Summary: "Todo ok salvo una decisión de producto pendiente.",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿Política de timeout del escape humano?"},
				},
			},
		},
	}

	// Canonicalización dura: muta el struct in-place.
	var stderr bytes.Buffer
	if err := validateAndNormalizeValidatorResults(results, &stderr); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	// Simulo el rendering del comment (lo que posteVaría postValidatorComments).
	validatorCommentBody := renderValidatorComment(results[0], 1)

	// Construyo el issue con el comment del validador + un human-request +
	// una respuesta humana. Es la condición mínima para que parseConversation
	// marque state.PreviousFindings correctamente.
	humanRequestBody := comments.Header{Flow: "explore", Iter: 1, Role: "human-request"}.Format() +
		"\n## 🧑 Necesito tu input\n\n1. ¿Política de timeout?\n"

	// Executor plan embebido (parseConversation lo necesita para que state
	// tenga ExecutorPlan no-nil; si es nil el prompt explota con "<nil>").
	executorPlan := &Response{
		Summary:   "Plan resumen",
		Questions: []Question{{Q: "¿Timeout?", Blocker: true, Kind: KindProduct}},
		Risks:     []Risk{{Risk: "x", Likelihood: "low", Impact: "low", Mitigation: "y"}},
		Paths: []Path{
			{Title: "A", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: true},
			{Title: "B", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: false},
		},
		NextStep: "paso",
	}
	var executorSB strings.Builder
	executorSB.WriteString(comments.Header{Flow: "explore", Iter: 1, Agent: "claude", Role: "executor"}.Format() + "\n")
	executorSB.WriteString("## Plan\n\n")
	appendEmbeddedJSON(&executorSB, executorPlan, "Plan en JSON")

	now := time.Now()
	issue := &Issue{
		Number: 42,
		Title:  "Test issue",
		Body:   "Body",
		Comments: []IssueComment{
			{Body: executorSB.String(), CreatedAt: now.Add(-3 * time.Hour)},
			{Body: validatorCommentBody, CreatedAt: now.Add(-2 * time.Hour)},
			{Body: humanRequestBody, CreatedAt: now.Add(-1 * time.Hour)},
			{Body: "Timeout de 10 minutos.", CreatedAt: now},
		},
	}

	// parseConversation debería recuperar PreviousFindings con el verdict
	// canonicalizado (porque el embedded JSON fue renderizado DESPUÉS de la
	// mutación dura).
	state := parseConversation(issue)
	if len(state.PreviousFindings) != 1 {
		t.Fatalf("expected 1 previous finding, got %d", len(state.PreviousFindings))
	}
	if got := state.PreviousFindings[0].Response.Verdict; got != "needs_human" {
		t.Fatalf("expected parsed verdict needs_human (canonicalized), got %q — el embedded JSON preservó el crudo", got)
	}

	// buildValidatorResumePrompt debe formatear el verdict correcto. El
	// prompt contiene las reglas ("devolvé verdict=approve") como texto
	// instruccional; lo que me interesa es la sección de previousFindings,
	// que lleva el string "codex#1 (verdict=<canonicalizado>, N findings)".
	prompt := buildValidatorResumePrompt(issue, state)
	if !strings.Contains(prompt, "codex#1 (verdict=needs_human") {
		t.Fatalf("expected resume prompt to render codex#1 with verdict=needs_human (canonicalized); got:\n%s", prompt)
	}
	if strings.Contains(prompt, "codex#1 (verdict=approve") {
		t.Fatalf("expected resume prompt to NOT render codex#1 with verdict=approve (crudo pre-canonicalización); got:\n%s", prompt)
	}
}

// TestCoversSamePlanQuestion_ParaphrasedWithWhy: caso real del issue chichex/cvm#20.
// El validador copia la pregunta del plan cambiando un verbo ('debe generar' →
// 'incluye') y concatena el "why" después del signo de pregunta. Las heurísticas
// viejas (contains, quoted, meta+overlap) no lo detectan, y el finding duplicado
// se filtra a "Observaciones adicionales". La similitud Jaccard sobre la primera
// oración interrogativa sí debe pescarlo.
func TestCoversSamePlanQuestion_ParaphrasedWithWhy(t *testing.T) {
	planQs := []Question{
		{Q: "¿El output de /idea es solo el issue en GitHub, o también debe generar un esqueleto de plan inicial (archivo local, comentario en el issue, o nota en memory)?", Blocker: true},
	}
	finding := "¿El output de /idea es solo el issue en GitHub, o también incluye un esqueleto de plan inicial (archivo local, comentario en el issue, o nota en memory)? El body lo deja explícitamente abierto en Notas/warnings y la respuesta define scope, integración con /pr y formato del output."

	if !coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("expected paraphrased finding (verbo cambiado + why concatenado) to be detected as duplicate via Jaccard")
	}
}

// TestCoversSamePlanQuestion_ParaphrasedReordered: segunda pregunta del mismo
// issue — el validador reordena el orden de las opciones ("interactiva o
// automática" en vez de "automática o pedir confirmación") y agrega un "why"
// extenso. Mismo tema, debe deduplicarse.
func TestCoversSamePlanQuestion_ParaphrasedReordered(t *testing.T) {
	planQs := []Question{
		{Q: "¿El skill debe inferir type/size automáticamente, o pedir confirmación interactiva al usuario antes de crear el issue?", Blocker: true},
	}
	finding := "¿La clasificación type/size debe ser interactiva (mostrar inferencia y pedir confirmación) o totalmente automática y silenciosa? Es una política UX que el repo no zanja con precedente claro (/issue no clasifica, /s sí pide selección) y determina la modalidad del skill."

	if !coversSamePlanQuestion(finding, normalizeQuestion(finding), planQs) {
		t.Fatalf("expected reordered paraphrase to be detected as duplicate via Jaccard")
	}
}

// TestCollectValidatorQuestions_Issue20Paraphrase: end-to-end del caso real
// del issue #20. Dos findings del validador que parafrasean las 2 preguntas
// del plan deben descartarse — la lista de extras queda vacía.
func TestCollectValidatorQuestions_Issue20Paraphrase(t *testing.T) {
	planQs := []Question{
		{Q: "¿El output de /idea es solo el issue en GitHub, o también debe generar un esqueleto de plan inicial (archivo local, comentario en el issue, o nota en memory)?", Blocker: true, Kind: KindProduct},
		{Q: "¿El skill debe inferir type/size automáticamente, o pedir confirmación interactiva al usuario antes de crear el issue?", Blocker: true, Kind: KindProduct},
	}
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentOpus, Instance: 1},
			Response: &ValidatorResponse{
				Verdict: "needs_human",
				Findings: []Finding{
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿El output de /idea es solo el issue en GitHub, o también incluye un esqueleto de plan inicial (archivo local, comentario en el issue, o nota en memory)? El body lo deja explícitamente abierto en Notas/warnings y la respuesta define scope, integración con /pr y formato del output."},
					{Severity: "blocker", Area: "questions", NeedsHuman: true, Kind: KindProduct,
						Issue: "¿La clasificación type/size debe ser interactiva (mostrar inferencia y pedir confirmación) o totalmente automática y silenciosa? Es una política UX que el repo no zanja con precedente claro (/issue no clasifica, /s sí pide selección) y determina la modalidad del skill."},
				},
			},
		},
	}

	extras := collectValidatorQuestions(results, planQs)
	if len(extras) != 0 {
		t.Fatalf("expected 2 paraphrased findings to be filtered as duplicates, got %d extras: %+v", len(extras), extras)
	}
}
