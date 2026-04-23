package iterate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/flow/stateref"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
	planpkg "github.com/chichex/che/internal/plan"
)

func TestFilterIterable(t *testing.T) {
	mk := func(n int, lbls ...string) validate.PullRequest {
		pr := validate.PullRequest{Number: n}
		for _, l := range lbls {
			pr.Labels = append(pr.Labels, struct {
				Name string `json:"name"`
			}{Name: l})
		}
		return pr
	}
	nums := func(cs []validate.Candidate) []int {
		out := make([]int, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Number)
		}
		return out
	}
	cases := []struct {
		name    string
		in      []validate.PullRequest
		wantNum []int
	}{
		{"empty", nil, nil},
		{"sin labels excluido", []validate.PullRequest{mk(1), mk(2)}, nil},
		{"changes-requested + validated incluido",
			[]validate.PullRequest{mk(1, labels.ValidatedChangesRequested, labels.CheValidated)}, []int{1}},
		{"changes-requested sin che:validated excluido",
			[]validate.PullRequest{mk(1, labels.ValidatedChangesRequested)}, nil},
		{"approve excluido (no pidió cambios)",
			[]validate.PullRequest{mk(1, labels.ValidatedApprove, labels.CheValidated)}, nil},
		{"needs-human excluido (decisión humana, no técnica)",
			[]validate.PullRequest{mk(1, labels.ValidatedNeedsHuman, labels.CheValidated)}, nil},
		{"mix",
			[]validate.PullRequest{
				mk(1, labels.ValidatedApprove, labels.CheValidated),
				mk(2, labels.ValidatedChangesRequested, labels.CheValidated),
				mk(3, labels.ValidatedNeedsHuman, labels.CheValidated),
				mk(4, labels.ValidatedChangesRequested, labels.CheValidated),
				mk(5),
			},
			[]int{2, 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nums(filterIterable(c.in))
			if len(got) != len(c.wantNum) {
				t.Fatalf("want %v, got %v", c.wantNum, got)
			}
			for i := range got {
				if got[i] != c.wantNum[i] {
					t.Errorf("index %d: want %d, got %d", i, c.wantNum[i], got[i])
				}
			}
		})
	}
}

func TestLatestValidateFindings(t *testing.T) {
	mk := func(body string) validate.PRComment {
		return validate.PRComment{Body: body, CreatedAt: time.Now()}
	}
	cases := []struct {
		name     string
		comments []validate.PRComment
		wantLen  int
	}{
		{"empty", nil, 0},
		{"plain human comments", []validate.PRComment{mk("hola"), mk("gracias")}, 0},
		{"un validator iter=1",
			[]validate.PRComment{
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\n## findings\n- foo"),
				mk("<!-- claude-cli: flow=validate iter=1 role=summary -->\nresumen"),
			},
			1},
		{"dos validators misma iter",
			[]validate.PRComment{
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\nbody1"),
				mk("<!-- claude-cli: flow=validate iter=1 agent=codex instance=1 role=validator -->\nbody2"),
				mk("<!-- claude-cli: flow=validate iter=1 role=summary -->\nresumen"),
			},
			2},
		{"múltiples iters → solo la última",
			[]validate.PRComment{
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\nviejo"),
				mk("<!-- claude-cli: flow=validate iter=2 agent=opus instance=1 role=validator -->\nnuevo1"),
				mk("<!-- claude-cli: flow=validate iter=2 agent=codex instance=1 role=validator -->\nnuevo2"),
			},
			2},
		{"otros flows ignorados",
			[]validate.PRComment{
				mk("<!-- claude-cli: flow=execute iter=99 role=pr-link -->\nlink"),
				mk("<!-- claude-cli: flow=iterate iter=1 agent=opus instance=1 role=executor -->\npasado"),
			},
			0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LatestValidateFindings(c.comments)
			if len(got) != c.wantLen {
				t.Fatalf("want %d findings, got %d: %+v", c.wantLen, len(got), got)
			}
		})
	}
}

func TestDetermineIterateIter(t *testing.T) {
	mk := func(body string) validate.PRComment {
		return validate.PRComment{Body: body}
	}
	cases := []struct {
		name     string
		comments []validate.PRComment
		want     int
	}{
		{"empty → 1", nil, 1},
		{"sin iterate previos", []validate.PRComment{
			mk("<!-- claude-cli: flow=validate iter=3 agent=opus instance=1 role=validator -->\n"),
		}, 1},
		{"iter=1 previo → 2", []validate.PRComment{
			mk("<!-- claude-cli: flow=iterate iter=1 agent=opus instance=1 role=executor -->\n"),
		}, 2},
		{"iter=1 y iter=2 previos → 3", []validate.PRComment{
			mk("<!-- claude-cli: flow=iterate iter=1 agent=opus instance=1 role=executor -->\n"),
			mk("<!-- claude-cli: flow=iterate iter=2 agent=opus instance=1 role=executor -->\n"),
		}, 3},
		{"validate iters NO cuentan", []validate.PRComment{
			mk("<!-- claude-cli: flow=validate iter=5 agent=opus instance=1 role=validator -->\n"),
			mk("<!-- claude-cli: flow=iterate iter=1 agent=opus instance=1 role=executor -->\n"),
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetermineIterateIter(c.comments); got != c.want {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}
}

func TestBuildIteratePrompt(t *testing.T) {
	pr := &validate.PullRequest{
		Number:     42,
		Title:      "feat: algo",
		URL:        "https://github.com/acme/demo/pull/42",
		HeadBranch: "feat/x",
	}
	findings := []string{
		"<!-- claude-cli: flow=validate iter=2 agent=opus instance=1 role=validator -->\n## findings\n- falta test foo",
	}
	prompt := BuildIteratePrompt(pr, 1, findings)

	must := []string{
		"PR #42",
		"feat: algo",
		"feat/x",
		"Iter de iterate: 1",
		"falta test foo",
		"git push",
		"## Findings de los validadores",
		"## Workflow esperado",
	}
	for _, s := range must {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q", s)
		}
	}
}

func TestBuildIteratePrompt_MultipleFindings(t *testing.T) {
	pr := &validate.PullRequest{Number: 1, Title: "t", HeadBranch: "b"}
	findings := []string{"primer validator", "segundo validator"}
	prompt := BuildIteratePrompt(pr, 1, findings)
	if !strings.Contains(prompt, "primer validator") || !strings.Contains(prompt, "segundo validator") {
		t.Errorf("prompt no incluye los 2 findings")
	}
	if !strings.Contains(prompt, "---") {
		t.Errorf("prompt no separa los validators con '---'")
	}
}

func TestRenderIterateComment(t *testing.T) {
	body := RenderIterateComment(3, []string{"fix: test foo", "fix: handle nil"})
	must := []string{
		"<!-- claude-cli: flow=iterate iter=3 agent=opus instance=1 role=executor -->",
		"## [che · iterate · opus#1 · iter:3]",
		"fix: test foo",
		"fix: handle nil",
		"validated:changes-requested",
	}
	for _, s := range must {
		if !strings.Contains(body, s) {
			t.Errorf("comment missing %q", s)
		}
	}
}

func TestRenderIterateComment_NoCommits(t *testing.T) {
	body := RenderIterateComment(1, nil)
	if !strings.Contains(body, "no dejó commits") {
		t.Errorf("expected fallback message when no commits, got:\n%s", body)
	}
}

func TestSanitizeBranchSlug(t *testing.T) {
	cases := map[string]string{
		"main":          "main",
		"feat/x":        "feat-x",
		"user/fix.go":   "user-fix.go",
		"":              "pr",
		"weird!@#":      "weird---",
	}
	for in, want := range cases {
		if got := sanitizeBranchSlug(in); got != want {
			t.Errorf("sanitizeBranchSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractOriginalBody(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"plan consolidado + idea original separados",
			"## Plan consolidado (post-exploración)\n\n**Resumen:** x\n\n---\n\n## Idea original (de `che idea`)\n\n## Idea\nnecesito foo\n",
			"## Idea\nnecesito foo\n",
		},
		{
			"sin idea original → devuelve body completo",
			"## Plan consolidado\n\n**Resumen:** x\n",
			"## Plan consolidado\n\n**Resumen:** x\n",
		},
		{
			"idea original sin doble newline",
			"## Idea original\nfoo\nbar",
			"foo\nbar",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractOriginalBody(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseIteratedPlan(t *testing.T) {
	goodPlan := planpkg.ConsolidatedPlan{
		Summary:            "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac1"},
		Steps:              []string{"s1"},
	}
	goodJSON, _ := json.Marshal(goodPlan)

	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"plain JSON ok", string(goodJSON), false},
		{"JSON con code fence", "```json\n" + string(goodJSON) + "\n```\n", false},
		{"JSON con texto antes", "Acá va el plan:\n" + string(goodJSON), false},
		{"JSON inválido", "not a json", true},
		{"sin summary", `{"goal":"g","approach":"a","acceptance_criteria":["ac"],"steps":["s"]}`, true},
		{"sin goal", `{"summary":"s","approach":"a","acceptance_criteria":["ac"],"steps":["s"]}`, true},
		{"sin approach", `{"summary":"s","goal":"g","acceptance_criteria":["ac"],"steps":["s"]}`, true},
		{"sin acceptance_criteria", `{"summary":"s","goal":"g","approach":"a","steps":["s"]}`, true},
		{"sin steps", `{"summary":"s","goal":"g","approach":"a","acceptance_criteria":["ac"]}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseIteratedPlan(c.raw)
			if c.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPlansEqual(t *testing.T) {
	a := &planpkg.ConsolidatedPlan{
		Summary: "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s1"},
	}
	// Misma shape, campos iguales → iguales.
	b := &planpkg.ConsolidatedPlan{
		Summary: "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s1"},
	}
	if !plansEqual(a, b) {
		t.Errorf("iguales deberían ser iguales")
	}
	// Cambio de summary → distintos.
	c := &planpkg.ConsolidatedPlan{
		Summary: "x", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s1"},
	}
	if plansEqual(a, c) {
		t.Errorf("summary distinto deberían ser distintos")
	}
	// Orden de steps distinto → distintos.
	d := &planpkg.ConsolidatedPlan{
		Summary: "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s2", "s1"},
	}
	e := &planpkg.ConsolidatedPlan{
		Summary: "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s1", "s2"},
	}
	if plansEqual(d, e) {
		t.Errorf("orden distinto deberían ser distintos")
	}
}

func TestBuildPlanIteratePrompt(t *testing.T) {
	issue := &validate.Issue{
		Number: 42,
		Title:  "feat: foo",
		URL:    "https://github.com/acme/demo/issues/42",
		Body:   "body original",
	}
	currentPlan := &planpkg.ConsolidatedPlan{
		Summary: "plan actual", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac1"},
		Steps:              []string{"step uno"},
	}
	findings := []string{
		"<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\n## findings\n- falta cubrir edge case X",
	}
	prompt := BuildPlanIteratePrompt(issue, "body original", currentPlan, findings, 1)

	must := []string{
		"Issue #42",
		"feat: foo",
		"body original",
		"plan actual",
		"step uno",
		"falta cubrir edge case X",
		"Plan consolidado actual",
		"Findings de los validadores",
		"delta mínimo",
		"kind=product",
		"Iter de iterate: 1",
	}
	for _, s := range must {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q", s)
		}
	}
}

func TestBuildPlanIteratePrompt_MultipleFindings(t *testing.T) {
	issue := &validate.Issue{Number: 1, Title: "t"}
	currentPlan := &planpkg.ConsolidatedPlan{
		Summary: "s", Goal: "g", Approach: "a",
		AcceptanceCriteria: []string{"ac"},
		Steps:              []string{"s1"},
	}
	findings := []string{"primer validator", "segundo validator"}
	prompt := BuildPlanIteratePrompt(issue, "body", currentPlan, findings, 1)
	if !strings.Contains(prompt, "primer validator") || !strings.Contains(prompt, "segundo validator") {
		t.Errorf("prompt no incluye los 2 findings")
	}
	if !strings.Contains(prompt, "---") {
		t.Errorf("prompt no separa los findings con '---'")
	}
}

func TestRenderIteratePlanComment(t *testing.T) {
	body := RenderIteratePlanComment(2, 3)
	must := []string{
		"<!-- claude-cli: flow=iterate iter=2 agent=opus instance=1 role=executor -->",
		"## [che · iterate · plan · iter:2]",
		"3 validador",
		"plan-validated:changes-requested",
		"che validate",
	}
	for _, s := range must {
		if !strings.Contains(body, s) {
			t.Errorf("comment missing %q", s)
		}
	}
}

func TestFilterIterablePlanCandidates(t *testing.T) {
	mk := func(n int, title, url string) validate.Issue {
		return validate.Issue{Number: n, Title: title, URL: url}
	}
	cases := []struct {
		name    string
		in      []validate.Issue
		wantLen int
	}{
		{"empty", nil, 0},
		{"todo incluido (el filtro lo hace gh --label)", []validate.Issue{
			mk(1, "a", "u1"), mk(2, "b", "u2"),
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterIterablePlanCandidates(c.in)
			if len(got) != c.wantLen {
				t.Fatalf("want %d, got %d", c.wantLen, len(got))
			}
		})
	}

	// Asegurar projection correcta (number/title/url)
	in := []validate.Issue{mk(7, "título", "https://...")}
	got := filterIterablePlanCandidates(in)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Number != 7 || got[0].Title != "título" || got[0].URL != "https://..." {
		t.Errorf("projection incorrecta: %+v", got[0])
	}
}

// TestLabelsConstants es un smoke test: si el nombre del label
// plan-validated:changes-requested cambia en labels.go, este test rompe
// — es un canario para evitar drift entre iterate y el paquete labels.
func TestLabelsConstants(t *testing.T) {
	if labels.PlanValidatedChangesRequested != "plan-validated:changes-requested" {
		t.Errorf("unexpected label constant: %q", labels.PlanValidatedChangesRequested)
	}
}

// TestRunPRGate_StateFromIssue simula el escenario real: PR #140 sin che:*
// en sus labels, pero con issue #122 linkeado en che:executed (y ya con
// validated:changes-requested sobre el PR, aplicado por un che:validate
// previo). El gate de iterate.runPR debe resolver al issue, no caer al
// PR, y ver che:executed ahí para el path correcto. Este test no ejecuta
// runPR (requiere gh + claude + worktree) — valida la lógica de
// resolución que el gate usa directamente.
//
// Es el caso que motivó el fix: antes de él, `che iterate 140` fallaba
// con "no está en che:validated" porque leía del PR, que nunca había
// tenido el label.
func TestRunPRGate_StateFromIssue(t *testing.T) {
	defer stateref.SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		if n != 122 {
			return nil, fmt.Errorf("unexpected n=%d", n)
		}
		// Escenario post-validate exitoso: issue en che:validated.
		return []string{"ct:plan", "pricing-modes", labels.CheValidated}, nil
	})()

	pr := validate.PullRequest{
		Number:     140,
		State:      "OPEN",
		HeadBranch: "feat/pricing",
	}
	pr.Labels = append(pr.Labels, struct {
		Name string `json:"name"`
	}{Name: labels.ValidatedChangesRequested})
	pr.ClosingIssuesReferences = []struct {
		Number int `json:"number"`
	}{{Number: 122}}

	r := pr.ResolveStateRef("140")

	// El gate de runPR lee che:validated desde stateRes.HasLabel(...). Si
	// la resolución resolvió al issue con che:validated, el gate pasa.
	if !r.ResolvedToIssue {
		t.Fatalf("expected resolution to go to issue, got %+v", r)
	}
	if r.IssueNumber != 122 {
		t.Fatalf("expected IssueNumber=122, got %d", r.IssueNumber)
	}
	if !r.HasLabel(labels.CheValidated) {
		t.Fatalf("gate should see che:validated on the issue: %v", r.Labels)
	}
	if r.Ref != "122" {
		t.Fatalf("transitions should target issue ref '122', got %q", r.Ref)
	}
}

// TestRunPRGate_StateFallbackToPR: cuando el PR no tiene issue linkeado
// (ej. PR que el usuario metió a mano, no creado por che execute), el
// gate cae al PR y lee los labels de ahí. Preserva compat.
func TestRunPRGate_StateFallbackToPR(t *testing.T) {
	defer stateref.SetFetchIssueLabelsForTest(func(n int) ([]string, error) {
		t.Fatalf("fetcher should not be called when no closing issues")
		return nil, nil
	})()

	pr := validate.PullRequest{
		Number:     140,
		State:      "OPEN",
		HeadBranch: "feat/x",
	}
	// PR ajeno: usuario aplicó che:validated a mano.
	pr.Labels = append(pr.Labels,
		struct {
			Name string `json:"name"`
		}{Name: labels.CheValidated},
		struct {
			Name string `json:"name"`
		}{Name: labels.ValidatedChangesRequested},
	)

	r := pr.ResolveStateRef("140")
	if r.ResolvedToIssue {
		t.Fatalf("expected fallback to PR, got %+v", r)
	}
	if r.Ref != "140" {
		t.Fatalf("expected Ref=140, got %q", r.Ref)
	}
	if !r.HasLabel(labels.CheValidated) {
		t.Fatalf("gate should see che:validated on the PR: %v", r.Labels)
	}
}

func TestFormatOpusLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "hola", "hola", true},
		{"empty", "", "", false},
		{"init", `{"type":"system","subtype":"init"}`, "sesión lista, arrancando…", true},
		{"tool_use sin input", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}}`, "tool: Edit", true},
		{"tool_use Edit con file_path", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/foo.go"}}]}}`, "tool: Edit /tmp/foo.go", true},
		{"tool_use Bash con command", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}`, "tool: Bash git status", true},
		{"tool_use Grep con pattern", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}]}}`, "tool: Grep TODO", true},
		{"result success", `{"type":"result","subtype":"success"}`, "agente terminó OK", true},
		{"result error", `{"type":"result","subtype":"error_max_turns"}`, "agente terminó (error_max_turns)", true},
		{"non-event json", `{"type":"unknown"}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := formatOpusLine(c.in)
			if ok != c.ok {
				t.Fatalf("ok: got %v want %v (out=%q)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
