package parser

import (
	"strings"
	"testing"
)

// TestClaudeAssistantText cubre el caso mas comun del stream: un evento
// type=assistant con un bloque text. El parser tiene que emitir una linea
// "> ...".
func TestClaudeAssistantText(t *testing.T) {
	raw := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hola humano"}]}}`
	lines, ev := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d (%v)", len(lines), lines)
	}
	if lines[0].Text != "> hola humano" {
		t.Errorf("expected '> hola humano', got %q", lines[0].Text)
	}
	if ev.Empty() {
		t.Errorf("expected non-empty event for valid JSON")
	}
}

// TestClaudeToolUse cubre el render del evento tool_use (linea con prefijo
// "· tool: <name>").
func TestClaudeToolUse(t *testing.T) {
	raw := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"read_file","input":{}}]}}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].Text != "· tool: read_file" {
		t.Errorf("expected '· tool: read_file', got %q", lines[0].Text)
	}
}

// TestClaudeToolUseInputSummary cubre que el render anexa un resumen del
// input cuando el tool es conocido (Bash/Read/Grep/Edit/etc). Es la rama
// "punto medio" que enriquece el viewport sin volcar payloads completos.
func TestClaudeToolUseInputSummary(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expected string
	}{
		{
			name:     "Bash con description",
			raw:      `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status","description":"Show working tree status"}}]}}`,
			expected: "· tool: Bash — Show working tree status",
		},
		{
			name:     "Bash sin description cae a command",
			raw:      `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git log --oneline"}}]}}`,
			expected: "· tool: Bash — $ git log --oneline",
		},
		{
			name:     "Read muestra basename",
			raw:      `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/abs/path/to/claude.go"}}]}}`,
			expected: "· tool: Read — claude.go",
		},
		{
			name:     "Grep muestra pattern",
			raw:      `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"tool_use","path":"/x"}}]}}`,
			expected: `· tool: Grep — "tool_use"`,
		},
		{
			name:     "Tool desconocido sin input no agrega sufijo",
			raw:      `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"CustomTool","input":{"foo":"bar"}}]}}`,
			expected: "· tool: CustomTool",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines, _ := Claude().Parse(tc.raw)
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d (%v)", len(lines), lines)
			}
			if lines[0].Text != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, lines[0].Text)
			}
		})
	}
}

// TestClaudeThinking cubre que un bloque thinking emite "· thinking" sin
// volcar el contenido (que viene encriptado en stream-json --verbose).
func TestClaudeThinking(t *testing.T) {
	raw := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"","signature":"..."}]}}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 1 || lines[0].Text != "· thinking" {
		t.Fatalf("expected single '· thinking' line, got %v", lines)
	}
}

// TestClaudeSystemTaskProgress cubre que los eventos de subagentes (Task)
// se renderean con la description, no como "· system task_progress" pelado.
func TestClaudeSystemTaskProgress(t *testing.T) {
	raw := `{"type":"system","subtype":"task_progress","description":"Reading docs/vision.html","task_id":"x"}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].Text != "· task: Reading docs/vision.html" {
		t.Errorf("expected progress with description, got %q", lines[0].Text)
	}
}

// TestClaudeSystemTaskNotification cubre que la notificacion final del
// subagente trae el status + summary.
func TestClaudeSystemTaskNotification(t *testing.T) {
	raw := `{"type":"system","subtype":"task_notification","status":"completed","summary":"Compare docs vs code"}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].Text != "· task completed: Compare docs vs code" {
		t.Errorf("expected status+summary, got %q", lines[0].Text)
	}
}

// TestClaudeUserToolResultError cubre que un tool_result con is_error=true
// sale al viewport con un fragmento del content para que el usuario vea la
// falla sin abrir events.jsonl. Los success se siguen omitiendo.
func TestClaudeUserToolResultError(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","is_error":true,"content":"Exit code 1\nfile not found"}]}}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0].Text, "· tool_result error:") {
		t.Errorf("expected error prefix, got %q", lines[0].Text)
	}
}

// TestClaudeUserToolResultSuccessSilenced cubre que los tool_result OK
// siguen invisibles en el viewport (events.jsonl los conserva igual).
func TestClaudeUserToolResultSuccessSilenced(t *testing.T) {
	raw := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok output"}]}}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 0 {
		t.Errorf("expected 0 lines for success tool_result, got %v", lines)
	}
}

// TestClaudeNonJSONFallback cubre la rama "linea sin JSON valido". El
// parser tiene que mostrarla cruda en el viewport y NO appendearla a
// events.jsonl (event.Empty() == true).
func TestClaudeNonJSONFallback(t *testing.T) {
	raw := "warning: not json"
	lines, ev := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if lines[0].Text != "warning: not json" {
		t.Errorf("expected raw passthrough, got %q", lines[0].Text)
	}
	if !ev.Empty() {
		t.Errorf("expected empty event for non-JSON input, got %q", ev.Raw)
	}
}

// TestClaudeMultilineText cubre que un bloque text con \n se descompone en
// varias Lines (la primera con "> ", las siguientes con indent).
func TestClaudeMultilineText(t *testing.T) {
	raw := `{"type":"assistant","message":{"content":[{"type":"text","text":"linea uno\nlinea dos"}]}}`
	lines, _ := Claude().Parse(raw)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d (%v)", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0].Text, "> linea uno") {
		t.Errorf("expected '> linea uno', got %q", lines[0].Text)
	}
	if !strings.HasPrefix(lines[1].Text, "  linea dos") {
		t.Errorf("expected '  linea dos', got %q", lines[1].Text)
	}
}

// TestClaudeSystemInit cubre el caso del evento system con subtype=init —
// debe emitir 1 linea breve, no 0.
func TestClaudeSystemInit(t *testing.T) {
	raw := `{"type":"system","subtype":"init"}`
	lines, ev := Claude().Parse(raw)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0].Text, "session") {
		t.Errorf("expected session line, got %q", lines[0].Text)
	}
	if ev.Empty() {
		t.Errorf("expected non-empty event for valid system event")
	}
}
