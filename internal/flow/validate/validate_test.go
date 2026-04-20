package validate

import (
	"strings"
	"testing"
	"time"
)

func TestParsePRRef(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"number", "7", false},
		{"large number", "1234", false},
		{"owner/repo#N", "acme/demo#7", false},
		{"github URL", "https://github.com/acme/demo/pull/7", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"plain text", "foo", true},
		{"url without /pull/", "https://github.com/acme/demo/issues/7", true},
		{"# without repo", "#7", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePRRef(c.in)
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
