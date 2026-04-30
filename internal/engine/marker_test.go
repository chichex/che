package engine

import (
	"strings"
	"testing"
)

func TestParseMarker_BasicMarkers(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    Marker
		wantOk  bool
	}{
		{"next solo", "[next]", Marker{Kind: MarkerNext}, true},
		{"stop solo", "[stop]", Marker{Kind: MarkerStop}, true},
		{"goto sin espacio", "[goto:foo]", Marker{Kind: MarkerGoto, Goto: "foo"}, true},
		{"goto con espacio", "[goto: foo]", Marker{Kind: MarkerGoto, Goto: "foo"}, true},
		{"goto con underscores", "[goto: validate_pr]", Marker{Kind: MarkerGoto, Goto: "validate_pr"}, true},
		{"goto numérico", "[goto: step_2_check]", Marker{Kind: MarkerGoto, Goto: "step_2_check"}, true},
		{"trailing whitespace", "[next]   ", Marker{Kind: MarkerNext}, true},
		{"leading whitespace", "   [next]", Marker{Kind: MarkerNext}, true},
		{"multiple goto spaces", "[goto:   foo]", Marker{Kind: MarkerGoto, Goto: "foo"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseMarker(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v (got=%+v)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseMarker_CaseSensitive(t *testing.T) {
	// PRD §3.c: "Case-sensitive: [Next] / [GOTO: x] NO matchean".
	cases := []string{
		"[Next]",
		"[NEXT]",
		"[Stop]",
		"[STOP]",
		"[Goto: foo]",
		"[GOTO: foo]",
		"[goto: FOO]", // step name uppercase también está fuera de la regex
		"[goto: Foo]",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got, ok := ParseMarker(in)
			if ok {
				t.Errorf("ParseMarker(%q) = %+v, ok=true; expected ok=false (case-sensitive)", in, got)
			}
		})
	}
}

func TestParseMarker_LastLineOnly(t *testing.T) {
	// PRD §3.c: "Markers en líneas intermedias se ignoran".
	cases := []struct {
		name   string
		input  string
		want   Marker
		wantOk bool
	}{
		{
			name:   "marker intermedio, prosa al final",
			input:  "Análisis:\n[next] esto es prosa explicativa\nEl plan está OK pero quiero revisar.",
			want:   Marker{Kind: MarkerNone},
			wantOk: false,
		},
		{
			name:   "marker en la última línea no vacía (con trailing newlines)",
			input:  "Análisis: el plan está OK.\n[next]\n\n\n",
			want:   Marker{Kind: MarkerNext},
			wantOk: true,
		},
		{
			name:   "marker en última línea con whitespace pegado",
			input:  "Decisión:\n  [stop]  \n",
			want:   Marker{Kind: MarkerStop},
			wantOk: true,
		},
		{
			name:   "marker SOLO en línea intermedia (default a no-marker)",
			input:  "[goto: explore]\nAhora explico por qué.",
			want:   Marker{Kind: MarkerNone},
			wantOk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseMarker(tc.input)
			if ok != tc.wantOk {
				t.Fatalf("ok=%v want %v (got=%+v)", ok, tc.wantOk, got)
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestParseMarker_InvalidShape(t *testing.T) {
	// Casos donde NO hay marker (parser determinístico, sin fuzzy).
	cases := []string{
		"",
		"\n\n  \n",
		"plan ok",
		"goto: foo",         // sin brackets
		"[next] ok",         // texto post bracket
		"prefix [next]",     // texto pre bracket
		"[]",                // vacío
		"[foo]",             // marker desconocido
		"[goto:]",           // goto sin destino
		"[goto: ]",          // goto con sólo whitespace
		"[goto: 1step]",     // step empieza con dígito (regex exige [a-z_])
		"[goto: step-name]", // guión no permitido
		"[goto: step.name]", // punto no permitido
		"[next ",            // bracket roto
		"[next\n]",          // marker partido en líneas
	}
	for _, in := range cases {
		t.Run(strings.ReplaceAll(in, "\n", "\\n"), func(t *testing.T) {
			got, ok := ParseMarker(in)
			if ok {
				t.Errorf("ParseMarker(%q) = %+v, ok=true; want ok=false", in, got)
			}
		})
	}
}

func TestParseMarker_OutputCompletoConProsaYMarcador(t *testing.T) {
	// Caso típico real: el agente escribe párrafos y termina con el marker.
	input := `Revisé el plan propuesto.

Los puntos sólidos:
- Tests cubren casos felices y un par de errores
- El cap de 20 transiciones es defensa razonable

Faltó:
- No vi cómo se valida que el step destino exista
- Pequeño nit en la docstring de ParseMarker

Voy a pedir un ajuste antes de avanzar.

[goto: explore]`
	got, ok := ParseMarker(input)
	if !ok {
		t.Fatalf("expected marker found; got=%+v", got)
	}
	if got.Kind != MarkerGoto || got.Goto != "explore" {
		t.Errorf("got %+v, want goto:explore", got)
	}
}

func TestParseStreamMarker_LastResultEvent(t *testing.T) {
	// Stream típico: system init, varios assistant/tool_use, y al final un
	// evento result con el texto del asistente.
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working..."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"foo.go"}}]}}`,
		`{"type":"result","subtype":"success","result":"todo OK\n[next]"}`,
	}, "\n")

	got, ok := ParseStreamMarker(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != MarkerNext {
		t.Errorf("got %+v want next", got)
	}
}

func TestParseStreamMarker_LastResultWins(t *testing.T) {
	// Si hay múltiples events `result` (escenario raro pero documentable),
	// gana el último — porque el stream cierra con su outcome final.
	stream := strings.Join([]string{
		`{"type":"result","subtype":"interim","result":"[goto: explore]"}`,
		`{"type":"result","subtype":"success","result":"[stop]"}`,
	}, "\n")

	got, ok := ParseStreamMarker(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != MarkerStop {
		t.Errorf("got %+v want stop (último result gana)", got)
	}
}

func TestParseStreamMarker_NoResultEvent(t *testing.T) {
	// PRD §3.c: "Si el último output no es texto (LLM cerró con tool_use
	// sin response final), default [next]" — el caller resuelve el default,
	// el parser sólo reporta "no encontró marker".
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
	}, "\n")

	got, ok := ParseStreamMarker(stream)
	if ok {
		t.Errorf("expected no marker (sin event result); got=%+v", got)
	}
	if got.Kind != MarkerNone {
		t.Errorf("got Kind=%v want MarkerNone", got.Kind)
	}
}

func TestParseStreamMarker_IgnoraLineasNoJSON(t *testing.T) {
	// Algunos fakes de e2e o headers de claude --verbose escriben texto
	// plano. El parser no debe fallar — sólo ignorar esas líneas.
	stream := strings.Join([]string{
		`>>> claude verbose header`,
		`{"type":"result","subtype":"success","result":"[next]"}`,
		`>>> trailing log noise`,
	}, "\n")

	got, ok := ParseStreamMarker(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != MarkerNext {
		t.Errorf("got %+v want next", got)
	}
}

func TestParseStreamMarker_EmptyStream(t *testing.T) {
	got, ok := ParseStreamMarker("")
	if ok {
		t.Errorf("expected no marker on empty stream; got=%+v", got)
	}
	if got.Kind != MarkerNone {
		t.Errorf("got Kind=%v want MarkerNone", got.Kind)
	}
}

func TestParseStreamMarker_ResultConProsaLargaTerminandoEnMarcador(t *testing.T) {
	// El campo `result` viene como texto multilinea — ParseMarker mira la
	// última línea no vacía.
	stream := `{"type":"result","subtype":"success","result":"Revisé el código.\n\nTodo bien.\n\n[goto: validate_pr]"}`
	got, ok := ParseStreamMarker(stream)
	if !ok {
		t.Fatalf("expected marker; got=%+v", got)
	}
	if got.Kind != MarkerGoto || got.Goto != "validate_pr" {
		t.Errorf("got %+v, want goto:validate_pr", got)
	}
}
