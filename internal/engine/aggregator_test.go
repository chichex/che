package engine

import "testing"

// Tests del aggregator en aislamiento (sin runStep). Verifican la lógica
// pura de las 3 políticas: cuándo deciden temprano, cómo resuelven empates,
// cómo tratan errores técnicos vs markers explícitos.

func mkResult(agent string, kind MarkerKind, dest string) AgentResult {
	m := Marker{Kind: kind}
	if kind == MarkerGoto {
		m.Goto = dest
	}
	return AgentResult{Agent: agent, Marker: m}
}

func mkErrResult(agent string, err error) AgentResult {
	return AgentResult{Agent: agent, Err: err, Marker: Marker{Kind: MarkerNone}}
}

// ---------- majority ----------

func TestMajority_StopGanaSiempre_EarlyCancel(t *testing.T) {
	// 3 agentes, primero emite [stop]. PRD §3.d: "[stop] siempre gana en
	// majority" — el aggregator decide en cuanto ve el primer stop, sin
	// esperar a los demás (cancelación temprana).
	agg := NewAggregator(AggMajority, 3)

	out := agg.Feed(mkResult("a", MarkerStop, ""))
	if !out.Decided {
		t.Fatalf("expected decided=true after first [stop]; got %+v", out)
	}
	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}
}

func TestMajority_TodosNext(t *testing.T) {
	// 3 [next] → mayoría estricta de [next]. Decide al alcanzar 2/3.
	agg := NewAggregator(AggMajority, 3)
	if out := agg.Feed(mkResult("a", MarkerNext, "")); out.Decided {
		t.Fatalf("decided too early after 1/3; want wait")
	}
	out := agg.Feed(mkResult("b", MarkerNext, ""))
	if !out.Decided {
		t.Fatalf("expected decided after 2/3 next (strict majority)")
	}
	if out.Marker.Kind != MarkerNext {
		t.Errorf("Marker.Kind=%v want next", out.Marker.Kind)
	}
}

func TestMajority_DosNextUnoStop(t *testing.T) {
	// next/next/stop — cualquier orden. PRD §3.d: stop gana incluso con
	// 2 next. El aggregator corta apenas ve el stop.
	agg := NewAggregator(AggMajority, 3)
	agg.Feed(mkResult("a", MarkerNext, ""))
	out := agg.Feed(mkResult("b", MarkerNext, ""))
	if !out.Decided || out.Marker.Kind != MarkerNext {
		t.Fatalf("expected decided next after 2/3; got %+v", out)
	}
	// Si el aggregator decidió antes de llegar el stop, el motor no
	// alimentaría ese stop. Pero verificamos el caso al revés: stop
	// llega primero.
	agg2 := NewAggregator(AggMajority, 3)
	agg2.Feed(mkResult("a", MarkerNext, ""))
	out2 := agg2.Feed(mkResult("b", MarkerStop, ""))
	if !out2.Decided || out2.Marker.Kind != MarkerStop {
		t.Fatalf("[stop] debería ganar incluso después de [next]; got %+v", out2)
	}
}

func TestMajority_EmpateGotoStopEsStop(t *testing.T) {
	// PRD §3.d ejemplo del scope del issue: next/goto:X/stop → stop.
	// Como [stop] short-circuitea, el aggregator decide al ver el stop
	// (independiente del orden). Verificamos los 3 órdenes posibles.
	cases := []struct {
		name string
		seq  []AgentResult
	}{
		{"next-goto-stop", []AgentResult{
			mkResult("a", MarkerNext, ""),
			mkResult("b", MarkerGoto, "x"),
			mkResult("c", MarkerStop, ""),
		}},
		{"stop-first", []AgentResult{
			mkResult("a", MarkerStop, ""),
			mkResult("b", MarkerGoto, "x"),
			mkResult("c", MarkerNext, ""),
		}},
		{"goto-stop-next", []AgentResult{
			mkResult("a", MarkerGoto, "x"),
			mkResult("b", MarkerStop, ""),
			mkResult("c", MarkerNext, ""),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agg := NewAggregator(AggMajority, 3)
			var final AggregatorOutcome
			for _, r := range tc.seq {
				final = agg.Feed(r)
				if final.Decided {
					break
				}
			}
			if !final.Decided {
				final = agg.Finalize()
			}
			if final.Marker.Kind != MarkerStop {
				t.Errorf("Marker.Kind=%v want stop (empate sin mayoría → stop)", final.Marker.Kind)
			}
		})
	}
}

func TestMajority_EmpateNextGotoSinStop_FinalizeDevuelveStop(t *testing.T) {
	// 2 agentes: 1 next, 1 goto:x. NO hay [stop] explícito. Empate sin
	// mayoría estricta (cada uno tiene 1 voto, ninguno >N/2). PRD §3.d:
	// "Empate sin mayoría → [stop] (conservador)".
	agg := NewAggregator(AggMajority, 2)
	if out := agg.Feed(mkResult("a", MarkerNext, "")); out.Decided {
		t.Fatalf("decided too early")
	}
	out := agg.Feed(mkResult("b", MarkerGoto, "x"))
	// Después del segundo, ambos tienen count=1, ninguno >2/2=1 estricto.
	// Feed devuelve no-decided; Finalize lo resuelve a stop.
	if out.Decided {
		// Este caso es ambiguo: 1 voto no es mayoría estricta de 2 (>1).
		// Si Feed decidiera, debería ser stop. Si no decide, Finalize lo
		// hace. Aceptamos cualquiera de los dos siempre que el resultado
		// final sea stop.
		if out.Marker.Kind != MarkerStop {
			t.Errorf("Feed decided=%v Marker=%v; want stop", out.Decided, out.Marker.Kind)
		}
		return
	}
	final := agg.Finalize()
	if final.Marker.Kind != MarkerStop {
		t.Errorf("Finalize Marker.Kind=%v want stop (empate)", final.Marker.Kind)
	}
}

func TestMajority_GotoMismoDestinoGana(t *testing.T) {
	// 3 agentes, 2 con [goto: x] y 1 con [next] → goto:x gana 2/3.
	agg := NewAggregator(AggMajority, 3)
	agg.Feed(mkResult("a", MarkerGoto, "x"))
	agg.Feed(mkResult("b", MarkerNext, ""))
	out := agg.Feed(mkResult("c", MarkerGoto, "x"))
	if !out.Decided {
		t.Fatalf("expected decided after 2/3 goto:x")
	}
	if out.Marker.Kind != MarkerGoto || out.Marker.Goto != "x" {
		t.Errorf("Marker=%+v want goto:x", out.Marker)
	}
}

func TestMajority_DefaultKindEsMajority(t *testing.T) {
	// PRD §3.d: "Default: majority". Cuando se pasa kind="" debe
	// comportarse como majority.
	agg := NewAggregator("", 3)
	if agg == nil {
		t.Fatal("default aggregator nil")
	}
	out := agg.Feed(mkResult("a", MarkerStop, ""))
	if !out.Decided || out.Marker.Kind != MarkerStop {
		t.Errorf("default agg no se comporta como majority; got %+v", out)
	}
}

func TestMajority_ErrorTecnicoCuentaComoStop(t *testing.T) {
	// PRD §3.b paso 4: error técnico = [stop] automático. El aggregator
	// majority debería short-circuitear igual que con un [stop] explícito.
	agg := NewAggregator(AggMajority, 3)
	out := agg.Feed(mkErrResult("a", errExitNonZero))
	if !out.Decided || out.Marker.Kind != MarkerStop {
		t.Errorf("error técnico no fue tratado como stop; got %+v", out)
	}
}

// ---------- unanimous ----------

func TestUnanimous_TodosNext(t *testing.T) {
	agg := NewAggregator(AggUnanimous, 3)
	agg.Feed(mkResult("a", MarkerNext, ""))
	agg.Feed(mkResult("b", MarkerNext, ""))
	out := agg.Feed(mkResult("c", MarkerNext, ""))
	if !out.Decided || out.Marker.Kind != MarkerNext {
		t.Errorf("expected unanimous next; got %+v", out)
	}
}

func TestUnanimous_DivergenciaEsStop_EarlyCancel(t *testing.T) {
	// Scope del issue: unanimous con next/next/goto:X → stop.
	// El aggregator detecta divergencia en el segundo voto (next vs goto).
	agg := NewAggregator(AggUnanimous, 3)
	agg.Feed(mkResult("a", MarkerNext, ""))
	out := agg.Feed(mkResult("b", MarkerGoto, "x"))
	if !out.Decided {
		t.Fatalf("expected early decide on divergence after 2/3")
	}
	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop (divergence)", out.Marker.Kind)
	}
}

func TestUnanimous_GotoMismoDestino(t *testing.T) {
	// goto:x / goto:x / goto:x → unanimous goto:x.
	agg := NewAggregator(AggUnanimous, 3)
	agg.Feed(mkResult("a", MarkerGoto, "x"))
	agg.Feed(mkResult("b", MarkerGoto, "x"))
	out := agg.Feed(mkResult("c", MarkerGoto, "x"))
	if !out.Decided {
		t.Fatalf("expected decided after unanimous")
	}
	if out.Marker.Kind != MarkerGoto || out.Marker.Goto != "x" {
		t.Errorf("Marker=%+v want goto:x", out.Marker)
	}
}

func TestUnanimous_GotoDestinosDistintosEsStop(t *testing.T) {
	// goto:x / goto:y → divergencia.
	agg := NewAggregator(AggUnanimous, 2)
	agg.Feed(mkResult("a", MarkerGoto, "x"))
	out := agg.Feed(mkResult("b", MarkerGoto, "y"))
	if !out.Decided || out.Marker.Kind != MarkerStop {
		t.Errorf("goto destinos distintos deberían divergir; got %+v", out)
	}
}

// ---------- first_blocker ----------

func TestFirstBlocker_PrimerStopGana(t *testing.T) {
	// Scope del issue: first_blocker con [stop] primero cancela los demás.
	agg := NewAggregator(AggFirstBlocker, 3)
	out := agg.Feed(mkResult("a", MarkerStop, ""))
	if !out.Decided {
		t.Fatalf("first_blocker no decidió en el primer stop")
	}
	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}
}

func TestFirstBlocker_PrimerGotoGana(t *testing.T) {
	agg := NewAggregator(AggFirstBlocker, 3)
	agg.Feed(mkResult("a", MarkerNext, ""))
	out := agg.Feed(mkResult("b", MarkerGoto, "x"))
	if !out.Decided {
		t.Fatalf("first_blocker no decidió con goto")
	}
	if out.Marker.Kind != MarkerGoto || out.Marker.Goto != "x" {
		t.Errorf("Marker=%+v want goto:x", out.Marker)
	}
}

func TestFirstBlocker_TodosNext(t *testing.T) {
	agg := NewAggregator(AggFirstBlocker, 3)
	agg.Feed(mkResult("a", MarkerNext, ""))
	agg.Feed(mkResult("b", MarkerNext, ""))
	out := agg.Feed(mkResult("c", MarkerNext, ""))
	if !out.Decided || out.Marker.Kind != MarkerNext {
		t.Errorf("expected next after all next; got %+v", out)
	}
}

// ---------- helpers / edge cases ----------

func TestNewAggregator_KindDesconocidoEsNil(t *testing.T) {
	if a := NewAggregator("bogus_kind", 3); a != nil {
		t.Errorf("NewAggregator(unknown) = %T want nil", a)
	}
}

func TestKeyOf_NoneSeMapeaANext(t *testing.T) {
	// Salud del helper: MarkerNone (output sin marker) cuenta como
	// MarkerNext a efectos de voto. Esto preserva la regla "default-next"
	// del PRD §3.b paso 5 incluso en multi-agente.
	if keyOf(Marker{Kind: MarkerNone}) != keyOf(Marker{Kind: MarkerNext}) {
		t.Error("MarkerNone debería mapearse a MarkerNext en keyOf")
	}
}

// errExitNonZero es un error técnico fake (no constructor explícito para
// no acoplar el test al exit-code wrapping de internal/agent).
var errExitNonZero = &fakeErr{msg: "exit 1: auth failed"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }
