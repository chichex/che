package iterate

import (
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
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
		{"changes-requested incluido",
			[]validate.PullRequest{mk(1, labels.ValidatedChangesRequested)}, []int{1}},
		{"approve excluido (no pidió cambios)",
			[]validate.PullRequest{mk(1, labels.ValidatedApprove)}, nil},
		{"needs-human excluido (decisión humana, no técnica)",
			[]validate.PullRequest{mk(1, labels.ValidatedNeedsHuman)}, nil},
		{"mix",
			[]validate.PullRequest{
				mk(1, labels.ValidatedApprove),
				mk(2, labels.ValidatedChangesRequested),
				mk(3, labels.ValidatedNeedsHuman),
				mk(4, labels.ValidatedChangesRequested),
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
