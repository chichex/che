package explore

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipelinelabels"
	"github.com/chichex/che/internal/plan"
)

// validResponse devuelve una Response completa, apta para pasar validate().
// Los tests que quieran testear un fallo particular clonan y mutan el campo
// que les interesa.
func validResponse() *Response {
	return &Response{
		Summary:   "Implementar X para resolver Y.",
		Questions: nil,
		Assumptions: []Assumption{
			{What: "Reusamos internal/labels.Apply", Why: "Ya existe y respeta la máquina de estados"},
		},
		Risks: []Risk{
			{Risk: "rollback parcial si falla gh", Likelihood: "low", Impact: "medium", Mitigation: "usar --body-file atómico"},
		},
		Paths: []Path{
			{Title: "Camino A", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "S", Recommended: true},
			{Title: "Camino B", Sketch: "x", Pros: []string{"p"}, Cons: []string{"c"}, Effort: "M", Recommended: false},
		},
		NextStep: "Ejecutar con che execute",
		ConsolidatedPlan: &plan.ConsolidatedPlan{
			Summary:            "Implementar X",
			Goal:               "Que Y funcione como se pide",
			AcceptanceCriteria: []string{"Y observa Z"},
			Approach:           "Camino A: lo más directo.",
			Steps:              []string{"Paso 1", "Paso 2"},
		},
	}
}

// TestValidate_OK: una Response bien formada pasa la validación — acto de
// control positivo antes de los casos de rechazo.
func TestValidate_OK(t *testing.T) {
	if err := validate(validResponse()); err != nil {
		t.Fatalf("unexpected validate error: %v", err)
	}
}

// TestValidate_MissingConsolidatedPlan: ahora el plan consolidado es parte
// del mismo output; si falta, fallamos duro con ExitSemantic.
func TestValidate_MissingConsolidatedPlan(t *testing.T) {
	r := validResponse()
	r.ConsolidatedPlan = nil
	err := validate(r)
	if err == nil {
		t.Fatal("expected validate to reject missing consolidated_plan")
	}
	if !strings.Contains(err.Error(), "consolidated_plan") {
		t.Errorf("error should mention consolidated_plan: %v", err)
	}
}

// TestValidate_ConsolidatedPlanGoalEmpty: wrap del error de
// validateConsolidated — confirma que la validación baja al sub-struct.
func TestValidate_ConsolidatedPlanGoalEmpty(t *testing.T) {
	r := validResponse()
	r.ConsolidatedPlan.Goal = ""
	err := validate(r)
	if err == nil {
		t.Fatal("expected error when consolidated_plan.goal is empty")
	}
	if !strings.Contains(err.Error(), "goal") {
		t.Errorf("error should mention goal: %v", err)
	}
}

// TestValidate_RejectsNonProductQuestion: el ejecutor que deja una question
// con kind=technical en questions[] debe fallar la validación (es un bug del
// ejecutor, no lo filtramos silencioso).
func TestValidate_RejectsNonProductQuestion(t *testing.T) {
	r := validResponse()
	r.Questions = []Question{
		{Q: "¿Decisión técnica que debería ser assumption?", Blocker: true, Kind: KindTechnical},
	}
	if err := validate(r); err == nil {
		t.Fatalf("expected validate() to reject question with kind=technical; got nil")
	}
}

// TestValidate_AcceptsEmptyQuestions: es válido devolver questions=[] si el
// ejecutor cerró todo como assumptions.
func TestValidate_AcceptsEmptyQuestions(t *testing.T) {
	r := validResponse()
	r.Questions = nil
	if err := validate(r); err != nil {
		t.Fatalf("expected validate() to accept empty questions[], got: %v", err)
	}
}

// TestValidate_PathsRequireAtLeastTwo: un solo path = el ejecutor no exploró
// el espacio de diseño.
func TestValidate_PathsRequireAtLeastTwo(t *testing.T) {
	r := validResponse()
	r.Paths = r.Paths[:1]
	if err := validate(r); err == nil {
		t.Fatal("expected validate to reject single-path response")
	}
}

// TestValidate_ExactlyOneRecommended: paths[] debe tener EXACTAMENTE un
// recommended=true — ni cero ni dos.
func TestValidate_ExactlyOneRecommended(t *testing.T) {
	// cero recommended
	r := validResponse()
	for i := range r.Paths {
		r.Paths[i].Recommended = false
	}
	if err := validate(r); err == nil {
		t.Error("expected error when no path is recommended")
	}
	// dos recommended
	r2 := validResponse()
	for i := range r2.Paths {
		r2.Paths[i].Recommended = true
	}
	if err := validate(r2); err == nil {
		t.Error("expected error when multiple paths are recommended")
	}
}

// TestValidate_RiskEnumValues: likelihood e impact fuera del enum son error.
func TestValidate_RiskEnumValues(t *testing.T) {
	r := validResponse()
	r.Risks[0].Likelihood = "bogus"
	if err := validate(r); err == nil {
		t.Error("expected error for invalid likelihood")
	}
	r2 := validResponse()
	r2.Risks[0].Impact = "xtreme"
	if err := validate(r2); err == nil {
		t.Error("expected error for invalid impact")
	}
}

// TestNormalizeKind_UnknownKindTreatedAsEmpty: un LLM que devuelve "prod" o
// "tecnico" (variante) debe quedar como "" → default "product" al aplicar
// kindOrDefault. Así no propagamos etiquetas fantasía.
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
	if got := kindOrDefault("prod"); got != KindProduct {
		t.Errorf("kindOrDefault(\"prod\") = %q, want %q", got, KindProduct)
	}
}

// TestGateBasic cubre las precondiciones: OPEN, ct:plan, NO che:plan/...
// y subsiguientes (ya avanzó en el pipeline).
func TestGateBasic(t *testing.T) {
	cases := []struct {
		name    string
		issue   Issue
		wantErr string
	}{
		{
			name: "ok",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				// Post-PR6b: el path feliz arranca con label v2.
				{Name: "ct:plan"}, {Name: pipelinelabels.StateIdea},
			}},
			wantErr: "",
		},
		{
			name:    "closed",
			issue:   Issue{Number: 1, State: "CLOSED", Labels: []Label{{Name: "ct:plan"}}},
			wantErr: "closed",
		},
		{
			name:    "missing ct:plan",
			issue:   Issue{Number: 1, State: "OPEN", Labels: []Label{{Name: pipelinelabels.StateIdea}}},
			wantErr: "ct:plan",
		},
		{
			name: "already planned",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				// Post-PR6b: el flow chequea v2 (`che:state:explore`) en lugar
				// del viejo `che:plan`.
				{Name: "ct:plan"}, {Name: "che:state:explore"},
			}},
			wantErr: "ya avanzó en el pipeline",
		},
		{
			name: "rechaza labels v1 viejos (che:idea) con ct:plan",
			// Repo no migrado a v2: el issue trae solo `che:idea` (modelo
			// viejo). Si el gate aceptara este caso, el Apply siguiente
			// dejaría labels v1+v2 mezclados — por eso rechazamos con
			// mensaje claro pidiendo `migrate-labels-v2`.
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "che:idea"},
			}},
			wantErr: "labels v1",
		},
		{
			name: "rechaza labels v1 viejos (che:plan)",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "che:plan"},
			}},
			wantErr: "labels v1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := gateBasic(&c.issue)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err %q missing %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestRenderComment_DoesNotIncludeConsolidatedPlan: el comment del ejecutor
// resume paths/risks/questions, pero NO repite el plan consolidado — ese
// vive en el body del issue. Evita duplicar info y confundir al validador.
func TestRenderComment_DoesNotIncludeConsolidatedPlan(t *testing.T) {
	r := validResponse()
	r.ConsolidatedPlan.Goal = "UN_GOAL_MUY_DISTINTO_PARA_EL_MATCHER"
	got := renderComment(r, AgentOpus, 1)

	if strings.Contains(got, r.ConsolidatedPlan.Goal) {
		t.Errorf("renderComment should NOT include consolidated plan contents; found goal text in:\n%s", got)
	}
	// Sí debe incluir el análisis del ejecutor.
	if !strings.Contains(got, r.Summary) {
		t.Errorf("renderComment should include summary:\n%s", got)
	}
	if !strings.Contains(got, r.Paths[0].Title) {
		t.Errorf("renderComment should include paths:\n%s", got)
	}
}

// TestRenderComment_IterInHeader: el iter se propaga al header estructurado.
// Protege el wiring aunque acá siempre sea 1 en el flow actual — deja listo
// el contrato para iteraciones futuras.
func TestRenderComment_IterInHeader(t *testing.T) {
	got := renderComment(validResponse(), AgentOpus, 3)
	want := "iter=3"
	if !strings.Contains(got, want) {
		t.Errorf("expected %q in header:\n%s", want, got)
	}
}

// TestParseResponse_StripsCodeFences: los agentes a veces envuelven el JSON
// en ```json ... ```. parseResponse tolera esa envoltura.
func TestParseResponse_StripsCodeFences(t *testing.T) {
	raw := "```json\n" + minimalResponseJSON() + "\n```"
	r, err := parseResponse(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Summary != "s" {
		t.Errorf("unexpected summary: %q", r.Summary)
	}
}

// TestParseResponse_SurroundingText: si el agente concatena texto antes/
// después del JSON, parseResponse busca el primer { y el último }.
func TestParseResponse_SurroundingText(t *testing.T) {
	raw := "blah blah\n" + minimalResponseJSON() + "\nfin"
	_, err := parseResponse(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func minimalResponseJSON() string {
	return `{"summary":"s","questions":[],"risks":[{"risk":"r","likelihood":"low","impact":"low","mitigation":"m"}],"paths":[{"title":"A","sketch":"x","pros":["p"],"cons":["c"],"effort":"S","recommended":true},{"title":"B","sketch":"x","pros":["p"],"cons":["c"],"effort":"M","recommended":false}],"next_step":"paso","consolidated_plan":{"summary":"s","goal":"g","acceptance_criteria":["c"],"approach":"a","steps":["p1"]}}`
}

// TestFilterCandidates cubre los dos buckets que la TUI muestra como "ideas
// sin explorar": (a) issues creados por che idea (ct:plan) que todavía no
// pasaron por explore, y (b) issues "crudos" sin ningún label ct:* que
// explore va a reclassificar antes de explorar. che:locked excluye siempre.
func TestFilterCandidates(t *testing.T) {
	// Post-PR6b: filterCandidates chequea labels v2 (`che:state:*` /
	// `che:state:applying:*`) en lugar de los viejos `che:plan` / etc.
	in := []Issue{
		{Number: 1, Title: "idea clasica", Labels: []Label{{Name: "ct:plan"}, {Name: "che:state:idea"}}},
		{Number: 2, Title: "ya explorada", Labels: []Label{{Name: "ct:plan"}, {Name: "che:state:explore"}}},
		{Number: 3, Title: "ejecutandose", Labels: []Label{{Name: "ct:plan"}, {Name: "che:state:applying:execute"}}},
		{Number: 4, Title: "ejecutada", Labels: []Label{{Name: "ct:plan"}, {Name: "che:state:execute"}}},
		{Number: 5, Title: "locked con ct:plan", Labels: []Label{{Name: "ct:plan"}, {Name: "che:state:idea"}, {Name: "che:locked"}}},
		{Number: 6, Title: "raw sin labels", Labels: nil},
		{Number: 7, Title: "raw con type y size", Labels: []Label{{Name: "type:feature"}, {Name: "size:m"}}},
		{Number: 8, Title: "raw con che preexistente", Labels: []Label{{Name: "che:state:explore"}}},
		{Number: 9, Title: "raw locked", Labels: []Label{{Name: "che:locked"}}},
	}
	got := filterCandidates(in)
	// Orden esperado: primero ideas de che (Raw=false), después los crudos
	// (Raw=true). La TUI depende de este orden para separar las dos
	// secciones con un solo índice de cursor.
	want := []Candidate{
		{Number: 1, Title: "idea clasica", Raw: false},
		{Number: 6, Title: "raw sin labels", Raw: true},
		{Number: 7, Title: "raw con type y size", Raw: true},
		{Number: 8, Title: "raw con che preexistente", Raw: true},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want=%d; got=%+v", len(got), len(want), got)
	}
	for i, c := range want {
		if got[i] != c {
			t.Errorf("[%d] got=%+v want=%+v", i, got[i], c)
		}
	}
}
