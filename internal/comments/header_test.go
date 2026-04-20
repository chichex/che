package comments

import (
	"strings"
	"testing"
)

func TestParseValidatorHeader(t *testing.T) {
	body := "<!-- claude-cli: flow=execute iter=1 agent=opus instance=1 role=validator -->\nfindings..."
	h := Parse(body)
	if h.Flow != "execute" || h.Iter != 1 || h.Agent != "opus" || h.Instance != 1 || h.Role != "validator" {
		t.Fatalf("unexpected header: %+v", h)
	}
}

func TestParseExecutorHeader(t *testing.T) {
	body := "<!-- claude-cli: flow=explore iter=2 agent=opus role=executor -->\nplan..."
	h := Parse(body)
	if h.Flow != "explore" || h.Iter != 2 || h.Agent != "opus" || h.Role != "executor" {
		t.Fatalf("unexpected header: %+v", h)
	}
	if h.Instance != 0 {
		t.Fatalf("Instance should be zero when omitted, got %d", h.Instance)
	}
}

func TestParseHumanRequestHeader(t *testing.T) {
	body := "<!-- claude-cli: flow=explore iter=1 role=human-request -->\npreguntas..."
	h := Parse(body)
	if h.Flow != "explore" || h.Iter != 1 || h.Role != "human-request" {
		t.Fatalf("unexpected header: %+v", h)
	}
	if h.Agent != "" {
		t.Fatalf("Agent should be empty, got %q", h.Agent)
	}
}

func TestParseNoHeader(t *testing.T) {
	h := Parse("hola humano aquí")
	if (h != Header{}) {
		t.Fatalf("expected empty header, got %+v", h)
	}
}

func TestParseMalformedFieldsSkipped(t *testing.T) {
	// iter no numérico debe quedar en 0, el resto se respeta.
	body := "<!-- claude-cli: flow=execute iter=abc agent=codex instance=xx role=validator -->"
	h := Parse(body)
	if h.Flow != "execute" || h.Agent != "codex" || h.Role != "validator" {
		t.Fatalf("unexpected header: %+v", h)
	}
	if h.Iter != 0 || h.Instance != 0 {
		t.Fatalf("expected iter/instance zeroed, got iter=%d instance=%d", h.Iter, h.Instance)
	}
}

func TestFormatValidator(t *testing.T) {
	h := Header{Flow: "execute", Iter: 1, Agent: "opus", Instance: 1, Role: "validator"}
	got := h.Format()
	want := "<!-- claude-cli: flow=execute iter=1 agent=opus instance=1 role=validator -->"
	if got != want {
		t.Fatalf("Format:\n got %q\nwant %q", got, want)
	}
}

func TestFormatOmitsZeroFields(t *testing.T) {
	h := Header{Flow: "explore", Iter: 1, Role: "human-request"}
	got := h.Format()
	want := "<!-- claude-cli: flow=explore iter=1 role=human-request -->"
	if got != want {
		t.Fatalf("Format:\n got %q\nwant %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []Header{
		{Flow: "execute", Iter: 1, Agent: "opus", Instance: 1, Role: "validator"},
		{Flow: "execute", Iter: 3, Agent: "codex", Instance: 2, Role: "validator"},
		{Flow: "execute", Iter: 1, Agent: "gemini", Instance: 1, Role: "validator"},
		{Flow: "explore", Iter: 2, Agent: "opus", Role: "executor"},
		{Flow: "explore", Iter: 1, Role: "human-request"},
	}
	for _, h := range cases {
		body := h.Format() + "\nbody text"
		got := Parse(body)
		if got != h {
			t.Errorf("round-trip failed:\n orig %+v\n got  %+v\n wire %q", h, got, h.Format())
		}
	}
}

func TestParseIgnoresLeadingWhitespace(t *testing.T) {
	body := "   \n<!-- claude-cli: flow=execute iter=1 agent=opus instance=1 role=validator -->\n"
	h := Parse(body)
	if h.Role != "validator" {
		t.Fatalf("expected validator role, got %+v", h)
	}
}

// TestHeaderIsFirstLine documenta que el header debe estar al principio del
// body: si hay texto antes, Parse no lo detecta. Esto matchea el contrato de
// explore.ParseCommentHeader y evita que texto libre del humano dispare un
// falso match del regex.
func TestHeaderIsFirstLine(t *testing.T) {
	body := "texto libre\n<!-- claude-cli: flow=execute iter=1 agent=opus role=validator -->"
	h := Parse(body)
	if (h != Header{}) {
		t.Fatalf("expected empty header when not at start, got %+v", h)
	}
}

func TestFormatResultStartsWithMarker(t *testing.T) {
	h := Header{Flow: "execute", Iter: 1, Agent: "opus", Instance: 1, Role: "validator"}
	if !strings.HasPrefix(h.Format(), "<!-- claude-cli:") {
		t.Fatalf("Format must start with claude-cli marker: %q", h.Format())
	}
}
