package execute

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// writeExecutable escribe un script y lo marca ejecutable (0o755).
func writeExecutable(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}

func getEnv(k string) string  { return os.Getenv(k) }
func setEnv(k, v string) error { return os.Setenv(k, v) }

func TestParseConsolidatedPlan_FullBody(t *testing.T) {
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
	p, err := parseConsolidatedPlan(body)
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

func TestParseConsolidatedPlan_FallbackWhenNoHeader(t *testing.T) {
	body := "Body sin plan consolidado, solo texto libre."
	p, err := parseConsolidatedPlan(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Summary != body {
		t.Errorf("expected summary=body")
	}
	if p.Goal != "" || len(p.Steps) != 0 {
		t.Errorf("expected empty goal/steps in fallback")
	}
}

func TestParseConsolidatedPlan_EmptyBody(t *testing.T) {
	if _, err := parseConsolidatedPlan(""); err == nil {
		t.Fatalf("expected error on empty body")
	}
}

func TestParseConsolidatedPlan_HeaderButNoContent(t *testing.T) {
	body := "## Plan consolidado\n\n(lorem sin sub-secciones parseables)\n"
	if _, err := parseConsolidatedPlan(body); err == nil {
		t.Fatalf("expected error when sections missing")
	}
}

func TestGate(t *testing.T) {
	cases := []struct {
		name    string
		issue   Issue
		wantErr string
	}{
		{
			name: "ok",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "status:plan"},
			}},
			wantErr: "",
		},
		{
			name:    "closed",
			issue:   Issue{Number: 1, State: "CLOSED"},
			wantErr: "closed",
		},
		{
			name:    "missing ct:plan",
			issue:   Issue{Number: 1, State: "OPEN", Labels: []Label{{Name: "status:plan"}}},
			wantErr: "ct:plan",
		},
		{
			name: "executing lock",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "status:executing"},
			}},
			wantErr: "executing",
		},
		{
			name: "awaiting-human",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "status:plan"}, {Name: "status:awaiting-human"},
			}},
			wantErr: "awaiting-human",
		},
		{
			name: "not plan",
			issue: Issue{Number: 1, State: "OPEN", Labels: []Label{
				{Name: "ct:plan"}, {Name: "status:idea"},
			}},
			wantErr: "not status:plan",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := gate(&c.issue)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected err: %v", err)
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

func TestParseAgent(t *testing.T) {
	cases := []struct {
		in      string
		want    Agent
		wantErr bool
	}{
		{"opus", AgentOpus, false},
		{"codex", AgentCodex, false},
		{"gemini", AgentGemini, false},
		{"OPUS", AgentOpus, false},
		{"  codex  ", AgentCodex, false},
		{"foo", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := ParseAgent(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseAgent(%q): err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAgent(%q): got %v want %v", c.in, got, c.want)
		}
	}
}

func TestParseValidators(t *testing.T) {
	cases := []struct {
		in      string
		wantLen int
		wantErr bool
	}{
		{"", 0, false},
		{"none", 0, false},
		{"NONE", 0, false},
		{"codex", 1, false},
		{"codex,gemini", 2, false},
		{"codex,codex,gemini", 3, false},
		{"foo", 0, true},
		{"codex,codex,codex,codex", 0, true},
	}
	for _, c := range cases {
		got, err := ParseValidators(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseValidators(%q): err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if len(got) != c.wantLen {
			t.Errorf("ParseValidators(%q): got %d items", c.in, len(got))
		}
	}
	// Dedupe de instances:
	got, _ := ParseValidators("codex,codex,gemini")
	if got[0].Instance != 1 || got[1].Instance != 2 || got[2].Instance != 1 {
		t.Errorf("instances wrong: %+v", got)
	}
}

func TestBuildPromptIncludesPlanSections(t *testing.T) {
	issue := &Issue{Number: 42, Title: "Implementar foo"}
	plan := &ConsolidatedPlan{
		Summary:            "Resumen X",
		Goal:               "Goal X",
		AcceptanceCriteria: []string{"crit 1", "crit 2"},
		Approach:           "approach X",
		Steps:              []string{"step 1", "step 2"},
		OutOfScope:         []string{"oos 1"},
	}
	got := buildPrompt(issue, plan)
	for _, need := range []string{
		"Issue #42", "Implementar foo",
		"Resumen X", "Goal X",
		"crit 1", "crit 2",
		"approach X",
		"step 1", "step 2",
		"oos 1",
		"EXEC_NOTES.md",
	} {
		if !strings.Contains(got, need) {
			t.Errorf("prompt missing %q", need)
		}
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

// TestWaitValidators_AllFinish: el wait drena N señales y retorna sin timeout,
// emitiendo progreso acumulativo a stdout.
func TestWaitValidators_AllFinish(t *testing.T) {
	done := make(chan int, 2)
	// Simulamos 2 validadores terminando en 10ms/20ms.
	go func() {
		time.Sleep(10 * time.Millisecond)
		done <- 0
		time.Sleep(10 * time.Millisecond)
		done <- 1
	}()
	var buf bytes.Buffer
	start := time.Now()
	waitValidators(context.Background(), &buf, done, 2, 5*time.Second)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("waitValidators took too long: %v", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "(1/2)") || !strings.Contains(out, "(2/2)") {
		t.Errorf("expected progress 1/2 and 2/2, got:\n%s", out)
	}
	if strings.Contains(out, "timeout:") {
		t.Errorf("unexpected timeout message: %s", out)
	}
}

// TestHasRepoScope_RepoPresent: scope 'repo' → pass.
func TestHasRepoScope_RepoPresent(t *testing.T) {
	out := "github.com\n  - Token: gho_xxx\n  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'\n"
	if !hasRepoScope(out) {
		t.Fatalf("expected repo scope to be detected in:\n%s", out)
	}
}

// TestHasRepoScope_PublicRepoPresent: scope 'public_repo' → pass.
func TestHasRepoScope_PublicRepoPresent(t *testing.T) {
	out := "github.com\n  - Token scopes: 'gist', 'public_repo'\n"
	if !hasRepoScope(out) {
		t.Fatalf("expected public_repo scope to be detected in:\n%s", out)
	}
}

// TestHasRepoScope_OnlyReadScopes: sin repo/public_repo, falla.
func TestHasRepoScope_OnlyReadScopes(t *testing.T) {
	out := "github.com\n  - Token scopes: 'read:org', 'read:user', 'repo:status'\n"
	if hasRepoScope(out) {
		t.Fatalf("unexpectedly accepted scopes without repo:\n%s", out)
	}
}

// TestHasRepoScope_EmptyOutput: output vacío → falla.
func TestHasRepoScope_EmptyOutput(t *testing.T) {
	if hasRepoScope("") {
		t.Fatal("expected false for empty output")
	}
}

// TestPrecheckPRScopes_FakeGH_Pass: integración liviana — scripteamos un gh
// fake que devuelve scopes válidos. precheckPRScopes debe pasar.
func TestPrecheckPRScopes_FakeGH_Pass(t *testing.T) {
	tmp := t.TempDir()
	fakeGH := tmp + "/gh"
	script := `#!/bin/sh
echo "github.com"
echo "  - Token: gho_xxx"
echo "  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'"
`
	if err := writeExecutable(fakeGH, script); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	oldPath := getEnv("PATH")
	setEnv("PATH", tmp+":"+oldPath)
	t.Cleanup(func() { setEnv("PATH", oldPath) })

	if err := precheckPRScopes(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// TestPrecheckPRScopes_FakeGH_MissingScope: gh fake sin repo/public_repo →
// error accionable.
func TestPrecheckPRScopes_FakeGH_MissingScope(t *testing.T) {
	tmp := t.TempDir()
	fakeGH := tmp + "/gh"
	script := `#!/bin/sh
echo "github.com"
echo "  - Token scopes: 'read:org', 'repo:status'"
`
	if err := writeExecutable(fakeGH, script); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	oldPath := getEnv("PATH")
	setEnv("PATH", tmp+":"+oldPath)
	t.Cleanup(func() { setEnv("PATH", oldPath) })

	err := precheckPRScopes(context.Background())
	if err == nil {
		t.Fatal("expected error on missing scope")
	}
	if !strings.Contains(err.Error(), "gh auth refresh -s repo") {
		t.Errorf("expected actionable hint, got: %v", err)
	}
}

// TestFindOpenPRForBranch_MultipleMatches: si gh pr list devuelve >1 PRs,
// findOpenPRForBranch debe fallar con un mensaje accionable en vez de
// agarrar el primero silenciosamente. Usamos un PATH temporal con un
// script shell que simula gh — es más barato que armar un harness acá.
func TestFindOpenPRForBranch_MultipleMatches(t *testing.T) {
	tmp := t.TempDir()
	fakeGH := tmp + "/gh"
	script := `#!/bin/sh
cat <<EOF
[{"url":"https://github.com/acme/demo/pull/10","number":10},{"url":"https://github.com/acme/demo/pull/11","number":11}]
EOF
`
	if err := writeExecutable(fakeGH, script); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	// Prepend tmp al PATH.
	oldPath := getEnv("PATH")
	setEnv("PATH", tmp+":"+oldPath)
	t.Cleanup(func() { setEnv("PATH", oldPath) })

	_, err := findOpenPRForBranch(context.Background(), "exec/42-foo")
	if err == nil {
		t.Fatalf("expected error on multiple matches")
	}
	if !strings.Contains(err.Error(), "múltiples PRs") {
		t.Errorf("wrong error: %v", err)
	}
	if !strings.Contains(err.Error(), "pull/10") || !strings.Contains(err.Error(), "pull/11") {
		t.Errorf("error should include both URLs: %v", err)
	}
}

// TestFindOpenPRForBranch_SingleMatch: caso feliz — 1 PR, devuelve la URL.
func TestFindOpenPRForBranch_SingleMatch(t *testing.T) {
	tmp := t.TempDir()
	fakeGH := tmp + "/gh"
	script := `#!/bin/sh
cat <<EOF
[{"url":"https://github.com/acme/demo/pull/7","number":7}]
EOF
`
	if err := writeExecutable(fakeGH, script); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	oldPath := getEnv("PATH")
	setEnv("PATH", tmp+":"+oldPath)
	t.Cleanup(func() { setEnv("PATH", oldPath) })

	got, err := findOpenPRForBranch(context.Background(), "exec/42-foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://github.com/acme/demo/pull/7" {
		t.Errorf("unexpected URL: %q", got)
	}
}

// TestFormatOpusLine cubre los casos relevantes del stream-json del CLI de
// claude: system init, tool_use con las tools que más nos importan, result
// success/error, eventos irrelevantes que se omiten, y el fallback raw
// para líneas no-JSON (lo que emiten los fakes de los e2e).
func TestFormatOpusLine(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		wantOK bool
		want   string // si wantOK, substring esperado
	}{
		{
			name:   "empty",
			line:   "",
			wantOK: false,
		},
		{
			name:   "whitespace",
			line:   "   \t ",
			wantOK: false,
		},
		{
			name:   "non-json raw fallthrough",
			line:   "ok",
			wantOK: true,
			want:   "ok",
		},
		{
			name:   "malformed json falls through as raw",
			line:   `{"type":"assistant"`,
			wantOK: true,
			want:   `{"type":"assistant"`,
		},
		{
			name:   "system init",
			line:   `{"type":"system","subtype":"init","session_id":"abc","tools":["Read","Edit"]}`,
			wantOK: true,
			want:   "sesión lista",
		},
		{
			name:   "assistant tool_use Read",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/repo/foo.go"}}]}}`,
			wantOK: true,
			want:   "Read /repo/foo.go",
		},
		{
			name:   "assistant tool_use Edit",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"internal/bar.go","old_string":"x","new_string":"y"}}]}}`,
			wantOK: true,
			want:   "Edit internal/bar.go",
		},
		{
			name:   "assistant tool_use Bash",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
			wantOK: true,
			want:   "Bash go test ./...",
		},
		{
			name:   "assistant tool_use Bash truncated",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"` + strings.Repeat("a", 200) + `"}}]}}`,
			wantOK: true,
			want:   "…",
		},
		{
			name:   "assistant tool_use Grep",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Grep","input":{"pattern":"foo.*bar","glob":"*.go"}}]}}`,
			wantOK: true,
			want:   "Grep foo.*bar",
		},
		{
			name:   "assistant tool_use unknown tool only shows name",
			line:   `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"SomethingNew","input":{"foo":"bar"}}]}}`,
			wantOK: true,
			want:   "SomethingNew",
		},
		{
			name:   "assistant text-only is skipped",
			line:   `{"type":"assistant","message":{"content":[{"type":"text","text":"Voy a leer el archivo..."}]}}`,
			wantOK: false,
		},
		{
			name:   "user tool_result is skipped",
			line:   `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"..."}]}}`,
			wantOK: false,
		},
		{
			name:   "result success",
			line:   `{"type":"result","subtype":"success","result":"done"}`,
			wantOK: true,
			want:   "OK",
		},
		{
			name:   "result error",
			line:   `{"type":"result","subtype":"error_max_turns"}`,
			wantOK: true,
			want:   "error_max_turns",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := formatOpusLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want=%v (got=%q)", ok, c.wantOK, got)
			}
			if c.wantOK && !strings.Contains(got, c.want) {
				t.Errorf("got %q, want substring %q", got, c.want)
			}
		})
	}
}

// TestWaitValidators_Timeout: si el timeout expira antes de que terminen
// todos, retorna sin bloquear y loguea cuántos quedaron.
func TestWaitValidators_Timeout(t *testing.T) {
	done := make(chan int, 2)
	// Solo 1 de 2 termina dentro del timeout.
	go func() {
		done <- 0
	}()
	var buf bytes.Buffer
	start := time.Now()
	waitValidators(context.Background(), &buf, done, 2, 50*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("waitValidators did not respect timeout: %v", elapsed)
	}
	out := buf.String()
	if !strings.Contains(out, "timeout:") {
		t.Errorf("expected timeout message, got:\n%s", out)
	}
	if !strings.Contains(out, "1/2") {
		t.Errorf("expected 1/2 completaron, got:\n%s", out)
	}
}
