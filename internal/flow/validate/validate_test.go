package validate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/labels"
	planpkg "github.com/chichex/che/internal/plan"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"number", "7", false},
		{"large number", "1234", false},
		{"owner/repo#N", "acme/demo#7", false},
		{"github pull URL", "https://github.com/acme/demo/pull/7", false},
		{"github issues URL", "https://github.com/acme/demo/issues/42", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"plain text", "foo", true},
		{"non-github URL", "https://gitlab.com/acme/demo/pull/7", true},
		{"github URL without pull or issues", "https://github.com/acme/demo", true},
		{"# without repo", "#7", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseRef(c.in)
			if c.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", c.in)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
		})
	}
}

func TestParseValidators(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
	}{
		{"single opus", "opus", 1, false},
		{"two", "codex,gemini", 2, false},
		{"three with repeat", "codex,codex,gemini", 3, false},
		{"empty", "", 0, true},
		{"none rejected", "none", 0, true},
		{"NONE upper rejected", "NONE", 0, true},
		{"four rejected", "opus,codex,gemini,opus", 0, true},
		{"unknown agent", "bogus", 0, true},
		{"instance numbering", "codex,codex", 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := ParseValidators(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %v", c.in, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
			if len(out) != c.wantLen {
				t.Fatalf("expected %d validators for %q, got %d", c.wantLen, c.in, len(out))
			}
			// Chequeo de numeración de instancias cuando hay repeticiones.
			if c.in == "codex,codex" {
				if out[0].Instance != 1 || out[1].Instance != 2 {
					t.Fatalf("expected instance numbering 1,2; got %d,%d",
						out[0].Instance, out[1].Instance)
				}
			}
		})
	}
}

func TestParseResponse(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{
			name: "clean approve",
			raw:  `{"verdict":"approve","summary":"todo bien","findings":[]}`,
		},
		{
			name: "changes_requested with finding",
			raw: `{"verdict":"changes_requested","summary":"faltan tests","findings":[` +
				`{"severity":"major","area":"tests","where":"foo.go","issue":"sin tests","needs_human":false,"kind":"technical"}]}`,
		},
		{
			name: "wrapped in code fence",
			raw:  "```json\n{\"verdict\":\"approve\",\"summary\":\"ok\",\"findings\":[]}\n```",
		},
		{
			name: "text before and after",
			raw:  "Acá va:\n{\"verdict\":\"approve\",\"summary\":\"ok\",\"findings\":[]}\nListo",
		},
		{
			name:    "invalid verdict",
			raw:     `{"verdict":"weird","summary":"x","findings":[]}`,
			wantErr: true,
		},
		{
			name: "invalid severity",
			raw: `{"verdict":"changes_requested","summary":"x","findings":[` +
				`{"severity":"catastrophic","area":"code","issue":"oops","needs_human":false}]}`,
			wantErr: true,
		},
		{
			name: "missing finding issue",
			raw: `{"verdict":"changes_requested","summary":"x","findings":[` +
				`{"severity":"major","area":"code","issue":"","needs_human":false}]}`,
			wantErr: true,
		},
		{
			name:    "not JSON",
			raw:     `hola que tal`,
			wantErr: true,
		},
		{
			name: "missing suggestion is tolerated",
			raw: `{"verdict":"changes_requested","summary":"x","findings":[` +
				`{"severity":"minor","area":"docs","issue":"doc falta","needs_human":false}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseResponse(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatalf("expected non-nil response")
			}
		})
	}
}

func TestDetermineIter(t *testing.T) {
	mk := func(body string) PRComment {
		return PRComment{Body: body, CreatedAt: time.Now()}
	}

	cases := []struct {
		name     string
		comments []PRComment
		want     int
	}{
		{
			name:     "no comments",
			comments: nil,
			want:     1,
		},
		{
			name: "comments without header",
			comments: []PRComment{
				mk("plain comment from human"),
				mk("another one"),
			},
			want: 1,
		},
		{
			name: "one previous iter=1",
			comments: []PRComment{
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\n## stuff"),
			},
			want: 2,
		},
		{
			name: "mix of iters",
			comments: []PRComment{
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\n"),
				mk("<!-- claude-cli: flow=validate iter=2 agent=codex instance=1 role=validator -->\n"),
				mk("<!-- claude-cli: flow=validate iter=2 role=summary -->\n"),
			},
			want: 3,
		},
		{
			name: "other flows ignored",
			comments: []PRComment{
				mk("<!-- claude-cli: flow=explore iter=5 agent=opus role=executor -->\n"),
				mk("<!-- claude-cli: flow=execute iter=2 role=pr-link -->\n"),
			},
			want: 1,
		},
		{
			name: "validate mixed with other flows",
			comments: []PRComment{
				mk("<!-- claude-cli: flow=explore iter=5 role=executor -->\n"),
				mk("<!-- claude-cli: flow=validate iter=1 agent=opus instance=1 role=validator -->\n"),
				mk("<!-- claude-cli: flow=execute iter=99 role=pr-link -->\n"),
			},
			want: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetermineIter(c.comments)
			if got != c.want {
				t.Fatalf("expected iter=%d, got %d", c.want, got)
			}
		})
	}
}

func TestParseCommentHeader(t *testing.T) {
	h := ParseCommentHeader("<!-- claude-cli: flow=validate iter=3 agent=codex instance=2 role=validator -->\nrest")
	if h.Flow != "validate" {
		t.Errorf("flow=%q", h.Flow)
	}
	if h.Iter != 3 {
		t.Errorf("iter=%d", h.Iter)
	}
	if h.Agent != AgentCodex {
		t.Errorf("agent=%q", h.Agent)
	}
	if h.Instance != 2 {
		t.Errorf("instance=%d", h.Instance)
	}
	if h.Role != "validator" {
		t.Errorf("role=%q", h.Role)
	}

	empty := ParseCommentHeader("hola humano aquí")
	if empty.Flow != "" || empty.Role != "" {
		t.Errorf("expected empty header for plain text, got %+v", empty)
	}
}

func TestRenderValidatorComment_TitleVisibility(t *testing.T) {
	// Verificamos que el título visible (línea ## ...) incluya la marca "che"
	// para que humanos vean el origen sin abrir el HTML comment.
	r := validatorResult{
		Validator: Validator{Agent: AgentOpus, Instance: 1},
		Response: &Response{
			Verdict: "approve",
			Summary: "todo bien",
		},
	}
	out := renderValidatorComment(r, 2)
	if !strings.Contains(out, "## [che · validate · opus#1 · iter:2 · approve]") {
		t.Fatalf("title missing che marker. Got:\n%s", out)
	}
	if !strings.Contains(out, "<!-- claude-cli: flow=validate iter=2 agent=opus instance=1 role=validator -->") {
		t.Fatalf("header missing. Got:\n%s", out)
	}
}

func TestConsolidateVerdict(t *testing.T) {
	mk := func(verdict string) validatorResult {
		return validatorResult{Response: &Response{Verdict: verdict}}
	}
	errRes := validatorResult{Err: errors.New("timeout")}

	cases := []struct {
		name    string
		results []validatorResult
		want    string
	}{
		{"empty", nil, ""},
		{"all errors", []validatorResult{errRes, errRes}, ""},
		{"single approve", []validatorResult{mk("approve")}, "approve"},
		{"single changes_requested", []validatorResult{mk("changes_requested")}, "changes_requested"},
		{"single needs_human", []validatorResult{mk("needs_human")}, "needs_human"},
		{"approve + changes_requested → changes_requested",
			[]validatorResult{mk("approve"), mk("changes_requested")}, "changes_requested"},
		{"changes_requested + needs_human → needs_human",
			[]validatorResult{mk("changes_requested"), mk("needs_human")}, "needs_human"},
		{"approve + needs_human → needs_human",
			[]validatorResult{mk("approve"), mk("needs_human")}, "needs_human"},
		{"all three → needs_human",
			[]validatorResult{mk("approve"), mk("changes_requested"), mk("needs_human")}, "needs_human"},
		{"approve + error → approve (error ignored)",
			[]validatorResult{mk("approve"), errRes}, "approve"},
		{"error + changes_requested → changes_requested",
			[]validatorResult{errRes, mk("changes_requested")}, "changes_requested"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := consolidateVerdict(c.results)
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestVerdictToLabel(t *testing.T) {
	cases := map[string]string{
		"approve":           labels.ValidatedApprove,
		"changes_requested": labels.ValidatedChangesRequested,
		"needs_human":       labels.ValidatedNeedsHuman,
		"":                  "",
		"bogus":             "",
	}
	for in, want := range cases {
		if got := verdictToLabel(in); got != want {
			t.Errorf("verdictToLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterValidatable(t *testing.T) {
	mk := func(n int, lbls ...string) PullRequest {
		pr := PullRequest{Number: n, Title: fmt.Sprintf("PR %d", n)}
		for _, l := range lbls {
			pr.Labels = append(pr.Labels, struct {
				Name string `json:"name"`
			}{Name: l})
		}
		return pr
	}

	cases := []struct {
		name    string
		in      []PullRequest
		wantNum []int
	}{
		{"empty", nil, nil},
		{"sin labels", []PullRequest{mk(1), mk(2)}, []int{1, 2}},
		{
			"validated:approve excluido",
			[]PullRequest{mk(1, labels.ValidatedApprove), mk(2)},
			[]int{2},
		},
		{
			"changes-requested incluido",
			[]PullRequest{mk(1, labels.ValidatedChangesRequested)},
			[]int{1},
		},
		{
			"needs-human incluido",
			[]PullRequest{mk(1, labels.ValidatedNeedsHuman)},
			[]int{1},
		},
		{
			"mix: solo approve se excluye",
			[]PullRequest{
				mk(1, labels.ValidatedApprove),
				mk(2, labels.ValidatedChangesRequested),
				mk(3, labels.ValidatedNeedsHuman),
				mk(4),
			},
			[]int{2, 3, 4},
		},
		{
			"otros labels no interfieren",
			[]PullRequest{mk(1, "status:executed", "ct:plan")},
			[]int{1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterValidatable(c.in)
			if len(got) != len(c.wantNum) {
				t.Fatalf("want %d candidates, got %d", len(c.wantNum), len(got))
			}
			for i, n := range c.wantNum {
				if got[i].Number != n {
					t.Errorf("candidate %d: want number %d, got %d", i, n, got[i].Number)
				}
			}
		})
	}
}

func TestPullRequest_HasLabel(t *testing.T) {
	pr := &PullRequest{}
	pr.Labels = append(pr.Labels, struct {
		Name string `json:"name"`
	}{Name: labels.ValidatedApprove})
	if !pr.HasLabel(labels.ValidatedApprove) {
		t.Errorf("expected HasLabel(approve)=true")
	}
	if pr.HasLabel(labels.ValidatedNeedsHuman) {
		t.Errorf("expected HasLabel(needs-human)=false")
	}
}

// TestPullRequest_ClosingIssueNumbers protege la proyección al slice de
// ints, consumida por stateref.Resolve.
func TestPullRequest_ClosingIssueNumbers(t *testing.T) {
	var pr *PullRequest
	if got := pr.ClosingIssueNumbers(); got != nil {
		t.Errorf("nil pr should return nil, got %v", got)
	}
	pr = &PullRequest{}
	if got := pr.ClosingIssueNumbers(); len(got) != 0 {
		t.Errorf("empty pr should return empty, got %v", got)
	}
	pr.ClosingIssuesReferences = []struct {
		Number int `json:"number"`
	}{{Number: 122}, {Number: 0}, {Number: 5}}
	got := pr.ClosingIssueNumbers()
	want := []int{122, 0, 5}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d: got %d, want %d", i, got[i], want[i])
		}
	}
}

// TestPullRequest_PRLabelNames protege la proyección al slice de labels.
func TestPullRequest_PRLabelNames(t *testing.T) {
	pr := &PullRequest{}
	pr.Labels = append(pr.Labels, struct {
		Name string `json:"name"`
	}{Name: labels.CheExecuted})
	pr.Labels = append(pr.Labels, struct {
		Name string `json:"name"`
	}{Name: labels.ValidatedApprove})
	got := pr.PRLabelNames()
	if len(got) != 2 || got[0] != labels.CheExecuted || got[1] != labels.ValidatedApprove {
		t.Fatalf("got %v", got)
	}
}

func TestRenderSummaryComment_Table(t *testing.T) {
	results := []validatorResult{
		{
			Validator: Validator{Agent: AgentOpus, Instance: 1},
			Response:  &Response{Verdict: "approve", Summary: "ok"},
		},
		{
			Validator: Validator{Agent: AgentCodex, Instance: 1},
			Response: &Response{
				Verdict:  "changes_requested",
				Summary:  "hay cosas",
				Findings: []Finding{{Severity: "major", Area: "code", Issue: "x"}},
			},
		},
	}
	out := renderSummaryComment(results, 1)
	if !strings.Contains(out, "## 🤖 [che · validate · resumen iter:1]") {
		t.Fatalf("summary title missing. Got:\n%s", out)
	}
	if !strings.Contains(out, "| opus#1 | approve | 0 |") {
		t.Fatalf("summary row for opus missing. Got:\n%s", out)
	}
	if !strings.Contains(out, "| codex#1 | changes_requested | 1 |") {
		t.Fatalf("summary row for codex missing. Got:\n%s", out)
	}
}

// TestResolveRefNumber cubre el helper local que extrae el número sin tocar
// gh. Es la base de detectTarget: fallamos acá si el parsing local no toma
// los formatos que cmd/validate acepta (número, URL de pull o issues,
// owner/repo#N).
func TestResolveRefNumber(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"plain number", "7", 7, false},
		{"large number", "1234", 1234, false},
		{"owner/repo#N", "acme/demo#42", 42, false},
		{"URL pull", "https://github.com/acme/demo/pull/7", 7, false},
		{"URL issues", "https://github.com/acme/demo/issues/42", 42, false},
		{"URL pull with suffix", "https://github.com/acme/demo/pull/7/files", 7, false},
		{"URL pull with query", "https://github.com/acme/demo/pull/7?tab=1", 7, false},
		{"URL pull with fragment", "https://github.com/acme/demo/pull/7#issuecomment-1", 7, false},
		{"unrecognized", "nonsense", 0, true},
		{"# without number", "acme/demo#foo", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveRefNumber(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %d", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}
}

// TestDetectTarget valida el despachador target usando fakes de `gh api`
// inyectados en PATH. No ejecuta runPR/runPlan — solo chequea que el
// campo pull_request discrimina issue vs PR.
func TestDetectTarget(t *testing.T) {
	// Stub de gh que devuelve el fixture según el número pedido. Lo
	// hacemos con un shim bash simple en un tempdir + PATH.
	cases := []struct {
		name        string
		apiResponse string
		want        Target
		wantErr     bool
	}{
		{
			name:        "issue (pull_request null)",
			apiResponse: `{"number": 42, "pull_request": null}`,
			want:        TargetPlan,
		},
		{
			name:        "issue (pull_request absent)",
			apiResponse: `{"number": 42, "title": "foo"}`,
			want:        TargetPlan,
		},
		{
			name:        "PR (pull_request object)",
			apiResponse: `{"number": 7, "pull_request": {"url": "https://api.github.com/repos/acme/demo/pulls/7"}}`,
			want:        TargetPR,
		},
		{
			name:        "malformed JSON",
			apiResponse: `not json`,
			want:        TargetUnknown,
			wantErr:     true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withFakeGh(t, c.apiResponse, 0)
			got, err := detectTarget("42")
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

// TestDetectTarget_GhError verifica que un exit non-zero de gh se
// propague como error (caller debería hacer retry).
func TestDetectTarget_GhError(t *testing.T) {
	withFakeGh(t, "", 1)
	_, err := detectTarget("99")
	if err == nil {
		t.Fatalf("expected error when gh api fails")
	}
}

// withFakeGh instala un fake gh en PATH que siempre responde con stdout y
// exit dado, independiente de los args. Útil para tests donde el contenido
// depende del test pero los args son siempre `api repos/.../issues/<n>`.
func withFakeGh(t *testing.T, stdout string, exit int) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	if stdout != "" {
		// usar printf para evitar problemas con escaping shell de los JSON
		// braces
		script += "printf '%s' " + shellQuote(stdout) + "\n"
	}
	script += fmt.Sprintf("exit %d\n", exit)
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// shellQuote pone el contenido entre comillas simples, escapando las que ya
// estuvieran dentro. Suficiente para los fixtures JSON cortos de estos
// tests.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestVerdictToPlanLabel cubre el mapeo del verdict al label plan-validated:*.
// Es el hermano de TestVerdictToLabel; los dos viven para que un rename o
// cambio de constantes lo rompa de inmediato.
func TestVerdictToPlanLabel(t *testing.T) {
	cases := map[string]string{
		"approve":           labels.PlanValidatedApprove,
		"changes_requested": labels.PlanValidatedChangesRequested,
		"needs_human":       labels.PlanValidatedNeedsHuman,
		"":                  "",
		"bogus":             "",
	}
	for in, want := range cases {
		if got := verdictToPlanLabel(in); got != want {
			t.Errorf("verdictToPlanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildPlanValidatorPrompt chequea que el prompt incluya los campos
// clave del plan parseado (goal, summary, acceptance_criteria, steps) para
// que el validador tenga contexto suficiente. El contrato de respuesta
// (verdict + findings con severity/area/kind) también debe estar presente.
func TestBuildPlanValidatorPrompt(t *testing.T) {
	issue := &Issue{
		Number: 42,
		Title:  "Agregar rate limiting al endpoint /login",
		Body:   "## Plan consolidado\n\ndetalles...",
	}
	cplan := &planpkg.ConsolidatedPlan{
		Summary:            "Rate limit a /login para frenar brute force",
		Goal:               "Bloquear >5 intentos/minuto por IP",
		Approach:           "Middleware in-memory con LRU",
		AcceptanceCriteria: []string{"6to intento devuelve 429", "contador se resetea tras 1min"},
		Steps:              []string{"agregar middleware", "wirearlo a /login", "test"},
		RisksToMitigate: []planpkg.Risk{
			{Risk: "IP share (NAT)", Likelihood: "low", Impact: "medium", Mitigation: "usar IP+useragent"},
		},
		OutOfScope: []string{"rate limit global"},
	}

	prompt := buildPlanValidatorPrompt(issue, cplan)

	// Campos del plan deben aparecer textualmente.
	mustContain := []string{
		"Issue #42",
		"Agregar rate limiting",
		"Rate limit a /login",
		"Bloquear >5 intentos/minuto",
		"Middleware in-memory",
		"6to intento devuelve 429",
		"agregar middleware",
		"IP share (NAT)",
		"rate limit global",
	}
	for _, s := range mustContain {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q", s)
		}
	}

	// Contrato de respuesta: enumera los verdicts válidos y el kind
	// product/technical/documented.
	contract := []string{"approve", "changes_requested", "needs_human", "product", "technical", "documented"}
	for _, s := range contract {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing contract keyword %q", s)
		}
	}
}

// TestBuildPlanValidatorPrompt_Fallback: si el plan parseado viene vacío
// (caller pasó un plan degradado), el prompt incluye el body raw para que
// el validador al menos pueda opinar sobre lo que hay en el issue.
func TestBuildPlanValidatorPrompt_Fallback(t *testing.T) {
	issue := &Issue{
		Number: 9,
		Title:  "legacy issue",
		Body:   "body raw que no parseó bien",
	}
	empty := &planpkg.ConsolidatedPlan{}
	prompt := buildPlanValidatorPrompt(issue, empty)
	if !strings.Contains(prompt, "body raw que no parseó bien") {
		t.Errorf("fallback prompt missing raw body")
	}
	if !strings.Contains(prompt, "fallback") && !strings.Contains(prompt, "Body raw") {
		t.Errorf("fallback marker or raw body section missing")
	}
}

// TestFilterPlanCandidates cubre la regla de exclusión: plan-validated:approve
// desaparece de la lista; los otros plan-validated:* siguen visibles.
func TestFilterPlanCandidates(t *testing.T) {
	mk := func(n int, lbls ...string) Issue {
		i := Issue{Number: n, Title: fmt.Sprintf("Issue %d", n), URL: fmt.Sprintf("https://x/%d", n)}
		for _, l := range lbls {
			i.Labels = append(i.Labels, IssueLabel{Name: l})
		}
		return i
	}

	cases := []struct {
		name    string
		in      []Issue
		wantNum []int
	}{
		{"empty", nil, nil},
		{"sin labels", []Issue{mk(1), mk(2)}, []int{1, 2}},
		{
			"plan-validated:approve excluido",
			[]Issue{mk(1, labels.PlanValidatedApprove), mk(2)},
			[]int{2},
		},
		{
			"changes-requested incluido",
			[]Issue{mk(1, labels.PlanValidatedChangesRequested)},
			[]int{1},
		},
		{
			"needs-human incluido",
			[]Issue{mk(1, labels.PlanValidatedNeedsHuman)},
			[]int{1},
		},
		{
			"mix: solo approve se excluye",
			[]Issue{
				mk(1, labels.PlanValidatedApprove),
				mk(2, labels.PlanValidatedChangesRequested),
				mk(3, labels.PlanValidatedNeedsHuman),
				mk(4),
			},
			[]int{2, 3, 4},
		},
		{
			"otros labels no interfieren",
			[]Issue{mk(1, labels.ChePlan, "ct:plan")},
			[]int{1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterPlanCandidates(c.in)
			if len(got) != len(c.wantNum) {
				t.Fatalf("want %d candidates, got %d", len(c.wantNum), len(got))
			}
			for i, n := range c.wantNum {
				if got[i].Number != n {
					t.Errorf("candidate %d: want number %d, got %d", i, n, got[i].Number)
				}
			}
		})
	}
}

// TestIssue_HasLabel cubre el helper análogo al de PullRequest.
func TestIssue_HasLabel(t *testing.T) {
	i := &Issue{}
	i.Labels = append(i.Labels, IssueLabel{Name: labels.PlanValidatedApprove})
	if !i.HasLabel(labels.PlanValidatedApprove) {
		t.Errorf("expected HasLabel(plan-validated:approve)=true")
	}
	if i.HasLabel(labels.PlanValidatedNeedsHuman) {
		t.Errorf("expected HasLabel(plan-validated:needs-human)=false")
	}
}
