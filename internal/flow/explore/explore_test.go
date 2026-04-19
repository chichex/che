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
