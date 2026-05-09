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
