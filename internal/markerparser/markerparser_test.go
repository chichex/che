package markerparser

import (
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// Parse — markers válidos
// -----------------------------------------------------------------------------

func TestParse_BasicMarkers(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   Marker
		wantOk bool
	}{
		{"next solo", "[next]", Marker{Kind: Next}, true},
		{"stop solo", "[stop]", Marker{Kind: Stop}, true},
		{"goto sin espacio", "[goto:foo]", Marker{Kind: Goto, Goto: "foo"}, true},
		{"goto con espacio", "[goto: foo]", Marker{Kind: Goto, Goto: "foo"}, true},
		{"goto con underscores", "[goto: validate_pr]", Marker{Kind: Goto, Goto: "validate_pr"}, true},
		{"goto numérico interno", "[goto: step_2_check]", Marker{Kind: Goto, Goto: "step_2_check"}, true},
		{"goto multiple espacios post-colon", "[goto:   foo]", Marker{Kind: Goto, Goto: "foo"}, true},
		{"goto solo letras", "[goto: explore]", Marker{Kind: Goto, Goto: "explore"}, true},
		{"goto empieza con underscore", "[goto: _internal]", Marker{Kind: Goto, Goto: "_internal"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("ok=%v want %v (got=%+v)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestParse_WhitespaceTolerance(t *testing.T) {
	// PRD §3.c: trailing newlines/whitespace ignorados; whitespace pegado
	// al marker dentro de la línea también se tolera.
	cases := []struct {
		name  string
		input string
		want  Marker
	}{
		{"trailing whitespace", "[next]   ", Marker{Kind: Next}},
		{"leading whitespace", "   [next]", Marker{Kind: Next}},
		{"leading + trailing", "  [next]  ", Marker{Kind: Next}},
		{"trailing newlines", "[next]\n\n\n", Marker{Kind: Next}},
		{"leading + trailing newlines", "\n\n[next]\n", Marker{Kind: Next}},
		{"goto trailing whitespace", "[goto: explore]   ", Marker{Kind: Goto, Goto: "explore"}},
		{"goto leading whitespace", "   [goto: explore]", Marker{Kind: Goto, Goto: "explore"}},
		{"stop con tabs", "\t[stop]\t", Marker{Kind: Stop}},
		{"trailing whitespace + newlines mixto", "[next]   \n   \n", Marker{Kind: Next}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse(tc.input)
			if !ok {
				t.Fatalf("expected ok=true; got=%+v", got)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Parse — case sensitivity (PRD §3.c)
// -----------------------------------------------------------------------------

func TestParse_CaseSensitive(t *testing.T) {
	// PRD §3.c: "Case-sensitive: [Next] / [GOTO: x] NO matchean".
	cases := []string{
		"[Next]",
		"[NEXT]",
		"[nExt]",
		"[Stop]",
		"[STOP]",
		"[Goto: foo]",
		"[GOTO: foo]",
		"[goto: FOO]", // step name uppercase está fuera de la regex
		"[goto: Foo]",
		"[goto: foo_BAR]",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got, ok := Parse(in)
			if ok {
				t.Errorf("Parse(%q) = %+v, ok=true; expected ok=false (case-sensitive)", in, got)
			}
			if got.Kind != None {
				t.Errorf("got Kind=%v want None", got.Kind)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Parse — sólo última línea no vacía (PRD §3.c)
// -----------------------------------------------------------------------------

func TestParse_LastLineOnly(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   Marker
		wantOk bool
	}{
		{
			name:   "marker intermedio, prosa al final → no match",
			input:  "Análisis:\n[next] esto es prosa explicativa\nEl plan está OK pero quiero revisar.",
			want:   Marker{Kind: None},
			wantOk: false,
		},
		{
			name:   "marker en última línea, trailing newlines",
			input:  "Análisis: el plan está OK.\n[next]\n\n\n",
			want:   Marker{Kind: Next},
			wantOk: true,
		},
		{
			name:   "marker en última línea con whitespace pegado",
			input:  "Decisión:\n  [stop]  \n",
			want:   Marker{Kind: Stop},
			wantOk: true,
		},
		{
			name:   "marker SOLO en línea intermedia (default a no-marker)",
			input:  "[goto: explore]\nAhora explico por qué.",
			want:   Marker{Kind: None},
			wantOk: false,
		},
		{
			name:   "prosa antes + marker en última línea",
			input:  "lorem ipsum\n[next]",
			want:   Marker{Kind: Next},
			wantOk: true,
		},
		{
			name:   "marker mezclado con prosa en la misma línea final",
			input:  "primer parrafo\nlorem [next] ipsum\nfin sin marker",
			want:   Marker{Kind: None},
			wantOk: false,
		},
		{
			name:   "trailing whitespace lines no afectan",
			input:  "lorem ipsum\n[stop]\n   \n   \n",
			want:   Marker{Kind: Stop},
			wantOk: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("ok=%v want %v (got=%+v)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Parse — formas inválidas: step destino, brackets, vacío
// -----------------------------------------------------------------------------

func TestParse_InvalidShape(t *testing.T) {
	// Casos donde NO hay marker. Parser determinístico, sin fuzzy.
	cases := []string{
		"",
		" ",
		"\n\n  \n",
		"plan ok",
		"goto: foo", // sin brackets
		"[next] ok", // texto post bracket en la misma línea
		"prefix [next]",
		"[]",
		"[foo]",                  // marker desconocido
		"[goto:]",                // goto sin destino
		"[goto: ]",               // goto con sólo whitespace
		"[goto:  ]",              // goto con varios espacios
		"[goto: 123foo]",         // step empieza con dígito (regex exige [a-z_])
		"[goto: 1step]",          // mismo: dígito al inicio
		"[goto: step-name]",      // guión no permitido
		"[goto: step.name]",      // punto no permitido
		"[goto: step name]",      // espacio interno
		"[next ",                 // bracket roto
		"[next\n]",               // marker partido en líneas
		"[next][stop]",           // dos markers seguidos
		"[next]extra",            // texto sin separador
	}
	for _, in := range cases {
		t.Run(strings.ReplaceAll(in, "\n", "\\n"), func(t *testing.T) {
			got, ok := Parse(in)
			if ok {
				t.Errorf("Parse(%q) = %+v, ok=true; want ok=false", in, got)
			}
			if got.Kind != None {
				t.Errorf("got Kind=%v want None", got.Kind)
			}
		})
	}
}

func TestParse_EmptyAndWhitespaceOnly(t *testing.T) {
	cases := []string{
		"",
		" ",
		"   ",
		"\n",
		"\n\n\n",
		"\t\t",
		" \n \n\t ",
	}
	for _, in := range cases {
		t.Run(strings.ReplaceAll(in, "\n", "\\n"), func(t *testing.T) {
			got, ok := Parse(in)
			if ok {
				t.Errorf("Parse(%q) = %+v, ok=true; want ok=false", in, got)
			}
			if got.Kind != None {
				t.Errorf("got Kind=%v want None", got.Kind)
			}
		})
	}
}

func TestParse_ProsaLargaTerminandoEnMarker(t *testing.T) {
	// Caso típico real: el agente escribe párrafos y termina con el marker.
	input := `Revisé el plan propuesto.

Los puntos sólidos:
- Tests cubren casos felices y un par de errores
- El cap de 20 transiciones es defensa razonable

Faltó:
- No vi cómo se valida que el step destino exista
- Pequeño nit en la docstring de Parse

Voy a pedir un ajuste antes de avanzar.

[goto: explore]`
	got, ok := Parse(input)
	if !ok {
		t.Fatalf("expected marker found; got=%+v", got)
	}
	if got.Kind != Goto || got.Goto != "explore" {
		t.Errorf("got %+v, want goto:explore", got)
	}
}

// -----------------------------------------------------------------------------
// ParseLastLine — recibe sólo una línea
// -----------------------------------------------------------------------------

func TestParseLastLine_ReconoceMarkers(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   Marker
		wantOk bool
	}{
		{"next", "[next]", Marker{Kind: Next}, true},
		{"stop", "[stop]", Marker{Kind: Stop}, true},
		{"goto", "[goto: foo]", Marker{Kind: Goto, Goto: "foo"}, true},
		{"con whitespace", "  [next]  ", Marker{Kind: Next}, true},
		{"vacio", "", Marker{Kind: None}, false},
		{"prosa pura", "el plan está OK", Marker{Kind: None}, false},
		{"case-mismatch", "[Next]", Marker{Kind: None}, false},
		{"goto invalido", "[goto: 1foo]", Marker{Kind: None}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseLastLine(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("ok=%v want %v (got=%+v)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestParseLastLine_NoEsperaMultilinea(t *testing.T) {
	// Si el caller le pasa un string con \n adentro, ParseLastLine NO debe
	// matchear: la regex está anclada con ^...$ pensando en una sola línea.
	got, ok := ParseLastLine("blah\n[next]")
	if ok {
		t.Errorf("ParseLastLine multilínea no debería matchear; got=%+v", got)
	}
}

// -----------------------------------------------------------------------------
// ParseStreamJSON — stream NDJSON con tool use
// -----------------------------------------------------------------------------

func TestParseStreamJSON_LastResultEvent(t *testing.T) {
	// Stream típico: system init, varios assistant/tool_use, y al final un
	// evento result con el texto del asistente.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"foo.go"}}]}}`,
		`{"type":"result","subtype":"success","result":"todo OK\n[next]"}`,
	}, "\n")

	got, ok := ParseStreamJSON(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != Next {
		t.Errorf("got %+v want next", got)
	}
}

func TestParseStreamJSON_LastResultWins(t *testing.T) {
	// Si hay múltiples events `result` (escenario raro pero documentable),
	// gana el último — el stream cierra con su outcome final.
	stream := strings.Join([]string{
		`{"type":"result","subtype":"interim","result":"[goto: explore]"}`,
		`{"type":"result","subtype":"success","result":"[stop]"}`,
	}, "\n")

	got, ok := ParseStreamJSON(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != Stop {
		t.Errorf("got %+v want stop (último result gana)", got)
	}
}

func TestParseStreamJSON_NoResultEvent(t *testing.T) {
	// PRD §3.c: si el último output no es texto (LLM cerró con tool_use
	// sin response final), default [next] — pero ese default lo aplica el
	// caller. El parser sólo reporta "no encontró marker".
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	}, "\n")

	got, ok := ParseStreamJSON(stream)
	if ok {
		t.Errorf("expected no marker (sin event result); got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

func TestParseStreamJSON_ResultConTextoVacio(t *testing.T) {
	// Evento result con `result` vacío → tratamos como "no hay marker".
	stream := `{"type":"result","subtype":"success","result":""}`
	got, ok := ParseStreamJSON(stream)
	if ok {
		t.Errorf("expected no marker (result vacío); got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

func TestParseStreamJSON_IgnoraLineasNoJSON(t *testing.T) {
	// Algunos fakes de e2e o headers de claude --verbose escriben texto
	// plano. El parser no debe fallar — sólo ignorar esas líneas.
	stream := strings.Join([]string{
		`>>> claude verbose header`,
		`{"type":"result","subtype":"success","result":"[next]"}`,
		`>>> trailing log noise`,
	}, "\n")

	got, ok := ParseStreamJSON(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != Next {
		t.Errorf("got %+v want next", got)
	}
}

func TestParseStreamJSON_IgnoraLineasJSONInvalido(t *testing.T) {
	// JSON mal formado en el stream no debe colgar el parser. La línea con
	// JSON válido sigue ganando.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{not valid json at all`,
		`{"type":"result","subtype":"success","result":"[goto: validate]"}`,
	}, "\n")

	got, ok := ParseStreamJSON(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != Goto || got.Goto != "validate" {
		t.Errorf("got %+v want goto:validate", got)
	}
}

func TestParseStreamJSON_EmptyStream(t *testing.T) {
	got, ok := ParseStreamJSON("")
	if ok {
		t.Errorf("expected no marker on empty stream; got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

func TestParseStreamJSON_OnlyWhitespace(t *testing.T) {
	got, ok := ParseStreamJSON("   \n\n   \n\t  ")
	if ok {
		t.Errorf("expected no marker on whitespace-only stream; got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

func TestParseStreamJSON_ResultConProsaLargaTerminandoEnMarker(t *testing.T) {
	// El campo `result` viene como texto multilinea — Parse mira la última
	// línea no vacía.
	stream := `{"type":"result","subtype":"success","result":"Revisé el código.\n\nTodo bien.\n\n[goto: validate_pr]"}`
	got, ok := ParseStreamJSON(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != Goto || got.Goto != "validate_pr" {
		t.Errorf("got %+v, want goto:validate_pr", got)
	}
}

func TestParseStreamJSON_ResultSinMarker(t *testing.T) {
	// El último result tiene texto pero la última línea no es un marker —
	// el parser reporta no-match (caller aplicará default [next]).
	stream := `{"type":"result","subtype":"success","result":"todo bien, sin marker explícito"}`
	got, ok := ParseStreamJSON(stream)
	if ok {
		t.Errorf("expected no marker (último result sin marker); got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

func TestParseStreamJSON_ResultConCaseMismatch(t *testing.T) {
	// Stream con [Next] (mayúscula) en el result → no matchea.
	stream := `{"type":"result","subtype":"success","result":"trabajado\n[Next]"}`
	got, ok := ParseStreamJSON(stream)
	if ok {
		t.Errorf("expected no marker (case-mismatch); got=%+v", got)
	}
	if got.Kind != None {
		t.Errorf("got Kind=%v want None", got.Kind)
	}
}

// -----------------------------------------------------------------------------
// Kind.String — sanity check para logs y test failures
// -----------------------------------------------------------------------------

func TestKindString(t *testing.T) {
	cases := []struct {
		kind Kind
		want string
	}{
		{None, "none"},
		{Next, "next"},
		{Goto, "goto"},
		{Stop, "stop"},
		{Kind(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("Kind(%d).String()=%q want %q", tc.kind, got, tc.want)
		}
	}
}
