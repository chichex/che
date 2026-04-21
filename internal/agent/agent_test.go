package agent

import (
	"strings"
	"testing"
)

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

func TestAgentBinary(t *testing.T) {
	cases := map[Agent]string{
		AgentOpus:   "claude",
		AgentCodex:  "codex",
		AgentGemini: "gemini",
	}
	for a, want := range cases {
		if got := a.Binary(); got != want {
			t.Errorf("%v.Binary() = %q, want %q", a, got, want)
		}
	}
	if got := Agent("unknown").Binary(); got != "" {
		t.Errorf("unknown.Binary() = %q, want \"\"", got)
	}
}

func TestAgentInvokeArgs_Opus(t *testing.T) {
	// text
	args := AgentOpus.InvokeArgs("hello", OutputText)
	want := []string{"-p", "hello", "--output-format", "text"}
	if !equalStrings(args, want) {
		t.Errorf("opus text: got %v, want %v", args, want)
	}
	// stream-json adds --verbose
	args = AgentOpus.InvokeArgs("hello", OutputStreamJSON)
	want = []string{"-p", "hello", "--output-format", "stream-json", "--verbose"}
	if !equalStrings(args, want) {
		t.Errorf("opus stream-json: got %v, want %v", args, want)
	}
}

func TestAgentInvokeArgs_CodexGemini(t *testing.T) {
	// codex ignora format (siempre exec --full-auto)
	args := AgentCodex.InvokeArgs("hi", OutputStreamJSON)
	want := []string{"exec", "--full-auto", "hi"}
	if !equalStrings(args, want) {
		t.Errorf("codex: got %v, want %v", args, want)
	}
	args = AgentGemini.InvokeArgs("hi", OutputStreamJSON)
	want = []string{"-p", "hi"}
	if !equalStrings(args, want) {
		t.Errorf("gemini: got %v, want %v", args, want)
	}
}

func TestParseValidators_AllowNone(t *testing.T) {
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
		got, err := ParseValidators(c.in, true)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseValidators(%q, true): err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if len(got) != c.wantLen {
			t.Errorf("ParseValidators(%q, true): got %d items", c.in, len(got))
		}
	}

	// Dedupe de instances: "codex,codex,gemini" → codex#1, codex#2, gemini#1.
	got, _ := ParseValidators("codex,codex,gemini", true)
	if got[0].Instance != 1 || got[1].Instance != 2 || got[2].Instance != 1 {
		t.Errorf("instances wrong: %+v", got)
	}
}

func TestParseValidators_DisallowNone(t *testing.T) {
	// allowNone=false → "" y "none" son errores.
	if _, err := ParseValidators("", false); err == nil {
		t.Error("ParseValidators(\"\", false) expected error")
	}
	if _, err := ParseValidators("none", false); err == nil {
		t.Error("ParseValidators(\"none\", false) expected error")
	}
	// válidos iguales que antes.
	got, err := ParseValidators("opus", false)
	if err != nil {
		t.Fatalf("ParseValidators(\"opus\", false): %v", err)
	}
	if len(got) != 1 || got[0].Agent != AgentOpus {
		t.Errorf("got %+v, want [opus]", got)
	}
}

func TestFormatOpusLine_NoPrefix(t *testing.T) {
	// Sin prefijo (comportamiento de execute).
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", false},
		{"plain text", "plain text", true},
		{`{"type":"system","subtype":"init"}`, "sesión lista, arrancando…", true},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/foo.go"}}]}}`, "Edit /tmp/foo.go", true},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"SomethingNew","input":{}}]}}`, "SomethingNew", true},
		{`{"type":"result","subtype":"success"}`, "agente terminó OK", true},
		{`{"type":"result","subtype":"error_max_turns"}`, "agente terminó (error_max_turns)", true},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"…"}]}}`, "", false},
	}
	for _, c := range cases {
		got, ok := FormatOpusLine(c.in, "")
		if ok != c.ok {
			t.Errorf("FormatOpusLine(%q, \"\"): ok=%v want=%v (got=%q)", c.in, ok, c.ok, got)
			continue
		}
		if ok && got != c.want {
			t.Errorf("FormatOpusLine(%q, \"\"): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestFormatOpusLine_WithPrefix(t *testing.T) {
	// Con prefijo "tool: " (comportamiento de iterate). Sólo los tool_use
	// se prefijan; system init y result no.
	got, ok := FormatOpusLine(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/foo.go"}}]}}`, "tool: ")
	if !ok || got != "tool: Edit /tmp/foo.go" {
		t.Errorf("tool_use with prefix: got %q (ok=%v), want %q", got, ok, "tool: Edit /tmp/foo.go")
	}
	got, ok = FormatOpusLine(`{"type":"system","subtype":"init"}`, "tool: ")
	if !ok || got != "sesión lista, arrancando…" {
		t.Errorf("init with prefix: got %q (ok=%v), want no-prefix message", got, ok)
	}
	got, ok = FormatOpusLine(`{"type":"result","subtype":"success"}`, "tool: ")
	if !ok || got != "agente terminó OK" {
		t.Errorf("result success with prefix: got %q (ok=%v), want no-prefix message", got, ok)
	}
}

func TestFormatOpusLine_BashTruncation(t *testing.T) {
	// Bash commands >80 chars se truncan.
	long := strings.Repeat("a", 200)
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + long + `"}}]}}`
	got, ok := FormatOpusLine(line, "")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.HasPrefix(got, "Bash ") {
		t.Errorf("expected prefix \"Bash \", got %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
