package engine

// Aggregator resuelve los markers que emiten N agentes corriendo en paralelo
// dentro del mismo step. PRD §3.d expone 3 presets fijos
// (`majority`/`unanimous`/`first_blocker`) — cada uno implementa esta
// interface.
//
// El motor alimenta al aggregator de forma incremental: por cada agente que
// termina, llama Feed con su resultado y mira el outcome. Apenas el outcome
// reporta Decided=true, el motor cancela el ctx de los agentes restantes y
// no sigue alimentando al aggregator (los markers que lleguen tarde se
// loguean como "cancelled by aggregator", no como error).
//
// Esto permite que aggregators conservadores como `first_blocker` corten
// apenas vean el primer blocker, sin esperar a que los otros terminen — la
// optimización principal de cancelación parcial del PRD §3.d.
//
// Si todos los agentes terminan sin que el aggregator haya decidido, el
// motor llama Finalize para forzar una decisión sobre el set completo.
//
// Las implementaciones DEBEN ser puras (no compartir estado entre runs).
// El motor crea una instancia nueva por step.
type Aggregator interface {
	// Feed registra el resultado de un agente. Devuelve true en Decided
	// apenas el aggregator tiene suficiente info para decidir el marker
	// final del step. Una vez Decided=true, el motor NO vuelve a llamar
	// Feed/Finalize sobre esa instancia.
	Feed(result AgentResult) AggregatorOutcome

	// Finalize se llama cuando todos los agentes terminaron y ningún Feed
	// devolvió Decided=true. El aggregator debe resolver con el set
	// completo de resultados acumulados.
	Finalize() AggregatorOutcome
}

// AgentResult captura el outcome de UN agente dentro de un step multi-agente.
// Es la unidad que consume el Aggregator.
type AgentResult struct {
	// Agent es el nombre del agente (preservado para logs / audit).
	Agent string

	// Marker es el marker resuelto del output. MarkerNone cuando hubo error
	// técnico — el aggregator lo trata como `[stop]` (igual que el motor
	// single-agente: PRD §3.b paso 4).
	Marker Marker

	// Err captura el error técnico del invoker, si hubo. Sólo informativo
	// para el aggregator (los presets actuales no diferencian "stop por
	// error" de "stop explícito" — ambos son MarkerStop a efectos de
	// resolución).
	Err error

	// Cancelled es true si el agente fue cortado por el aggregator antes
	// de terminar (ctx cancel propagado al child process). El aggregator
	// IGNORA estos resultados — no votan. El motor los recibe sólo para el
	// audit log.
	Cancelled bool
}

// AggregatorOutcome es la respuesta del aggregator a Feed/Finalize.
type AggregatorOutcome struct {
	// Decided indica si el aggregator ya resolvió la decisión final. Cuando
	// es true, Marker y Reason son válidos y el motor cancela los agentes
	// restantes.
	Decided bool

	// Marker es el marker final del step (el que decide la transición).
	// Sólo válido cuando Decided=true.
	Marker Marker

	// Reason es un texto corto explicando cómo el aggregator llegó a la
	// decisión (ej. "majority: 2/3 next", "unanimous: divergence next vs
	// stop", "first_blocker: agent-b emitted [stop]"). Útil para audit log
	// y dash UI. Sólo válido cuando Decided=true.
	Reason string
}

// NewAggregator crea una instancia fresca del preset pedido. Si kind está
// vacío, se aplica el default `majority` (PRD §3.d "Default: majority").
// Devuelve nil si el kind es desconocido — el caller (el motor) trata eso
// como error de configuración del pipeline.
func NewAggregator(kind AggregatorKind, expected int) Aggregator {
	if kind == "" {
		kind = AggMajority
	}
	switch kind {
	case AggMajority:
		return &majorityAgg{expected: expected}
	case AggUnanimous:
		return &unanimousAgg{expected: expected}
	case AggFirstBlocker:
		return &firstBlockerAgg{expected: expected}
	}
	return nil
}

// AggregatorKind enumera los presets soportados. Mirror de
// `internal/pipeline.Aggregator` — el motor no importa ese paquete (aún)
// para mantener self-contained la dependencia.
type AggregatorKind string

const (
	AggMajority     AggregatorKind = "majority"
	AggUnanimous    AggregatorKind = "unanimous"
	AggFirstBlocker AggregatorKind = "first_blocker"
)

// markerKey es la clave canónica para agrupar / comparar markers en los
// aggregators. Diferencia [goto: X] vs [goto: Y] (transiciones distintas)
// pero colapsa MarkerNone con MarkerNext (ambos son "default avanza" según
// PRD §3.b paso 5).
type markerKey struct {
	kind MarkerKind
	dest string // sólo para MarkerGoto
}

func keyOf(m Marker) markerKey {
	if m.Kind == MarkerNone {
		return markerKey{kind: MarkerNext}
	}
	if m.Kind == MarkerGoto {
		return markerKey{kind: MarkerGoto, dest: m.Goto}
	}
	return markerKey{kind: m.Kind}
}

// markerFromKey reconstruye un Marker desde una key. Inverso de keyOf
// (excepto que MarkerNone se mapea a MarkerNext en el round trip).
func markerFromKey(k markerKey) Marker {
	if k.kind == MarkerGoto {
		return Marker{Kind: MarkerGoto, Goto: k.dest}
	}
	return Marker{Kind: k.kind}
}

// effectiveMarker normaliza un AgentResult a un marker votable. Errores
// técnicos cuentan como [stop] (PRD §3.b paso 4). MarkerNone cuenta como
// [next] (PRD §3.b paso 5).
func effectiveMarker(r AgentResult) Marker {
	if r.Err != nil {
		return Marker{Kind: MarkerStop}
	}
	if r.Marker.Kind == MarkerNone {
		return Marker{Kind: MarkerNext}
	}
	return r.Marker
}

// ---------- majority ----------

// majorityAgg implementa AggregatorMajority. PRD §3.d:
//   - "[stop] siempre gana en majority" → si UN agente emite [stop], el
//     aggregator decide [stop] inmediatamente (cancelación temprana del
//     resto, no aporta info que cambie la decisión).
//   - "Empate sin mayoría → [stop] (conservador)" — si al final ningún
//     marker alcanza mayoría estricta (>N/2), el aggregator devuelve [stop]
//     con razón "tie".
//   - Sino, gana el marker más votado.
type majorityAgg struct {
	expected int
	counts   map[markerKey]int
	results  []AgentResult
}

func (a *majorityAgg) Feed(r AgentResult) AggregatorOutcome {
	a.results = append(a.results, r)
	if a.counts == nil {
		a.counts = map[markerKey]int{}
	}
	m := effectiveMarker(r)
	// Short-circuit: cualquier [stop] gana. PRD §3.d.
	if m.Kind == MarkerStop {
		return AggregatorOutcome{
			Decided: true,
			Marker:  Marker{Kind: MarkerStop},
			Reason:  "majority: agent " + r.Agent + " emitted [stop]",
		}
	}
	a.counts[keyOf(m)]++
	// ¿Algún marker ya tiene mayoría estricta (>N/2)?
	for k, c := range a.counts {
		if c*2 > a.expected {
			return AggregatorOutcome{
				Decided: true,
				Marker:  markerFromKey(k),
				Reason:  formatVoteReason("majority", k, c, a.expected),
			}
		}
	}
	return AggregatorOutcome{}
}

func (a *majorityAgg) Finalize() AggregatorOutcome {
	// No hubo mayoría estricta → empate → conservador → [stop].
	// (Si hubiéramos visto un [stop] explícito ya habríamos decidido en
	// Feed; este Finalize sólo se invoca cuando todos terminaron sin
	// short-circuit.)
	var bestKey markerKey
	bestCount := -1
	for k, c := range a.counts {
		if c > bestCount {
			bestCount, bestKey = c, k
		}
	}
	if bestCount*2 > a.expected {
		// Defensa: no debería pasar (Feed ya habría decidido), pero por
		// si el caller pasa expected mal o Feed se saltó el chequeo.
		return AggregatorOutcome{
			Decided: true,
			Marker:  markerFromKey(bestKey),
			Reason:  formatVoteReason("majority", bestKey, bestCount, a.expected),
		}
	}
	return AggregatorOutcome{
		Decided: true,
		Marker:  Marker{Kind: MarkerStop},
		Reason:  "majority: tie without strict majority, defaulting to [stop]",
	}
}

// ---------- unanimous ----------

// unanimousAgg implementa AggregatorUnanimous. Si todos los agentes
// coinciden exactamente (mismo Marker.Kind y mismo Goto cuando aplica),
// devuelve ese marker. Cualquier divergencia → [stop].
//
// Cancelación temprana: apenas vemos 2 markers distintos, decidimos [stop]
// — los markers restantes no pueden volver atrás la divergencia.
type unanimousAgg struct {
	expected int
	first    *markerKey
	results  []AgentResult
}

func (a *unanimousAgg) Feed(r AgentResult) AggregatorOutcome {
	a.results = append(a.results, r)
	m := effectiveMarker(r)
	k := keyOf(m)
	if a.first == nil {
		a.first = &k
	} else if *a.first != k {
		return AggregatorOutcome{
			Decided: true,
			Marker:  Marker{Kind: MarkerStop},
			Reason:  "unanimous: divergence (" + describeKey(*a.first) + " vs " + describeKey(k) + ")",
		}
	}
	// Mientras todos coincidan, esperamos al resto antes de declarar
	// unanimidad. Sólo decidimos cuando llegó el último.
	if len(a.results) == a.expected {
		return AggregatorOutcome{
			Decided: true,
			Marker:  markerFromKey(*a.first),
			Reason:  "unanimous: all " + itoa(a.expected) + " agents agreed on " + describeKey(*a.first),
		}
	}
	return AggregatorOutcome{}
}

func (a *unanimousAgg) Finalize() AggregatorOutcome {
	// Caso defensivo: el motor llamó Finalize sin completar expected. Tomá
	// lo que tengas.
	if a.first == nil {
		return AggregatorOutcome{
			Decided: true,
			Marker:  Marker{Kind: MarkerStop},
			Reason:  "unanimous: no agents reported",
		}
	}
	return AggregatorOutcome{
		Decided: true,
		Marker:  markerFromKey(*a.first),
		Reason:  "unanimous: all reporting agents agreed on " + describeKey(*a.first),
	}
}

// ---------- first_blocker ----------

// firstBlockerAgg implementa AggregatorFirstBlocker. PRD §3.d: "primer
// `[stop]` o `[goto: X]` define la transición". Si todos `[next]` → `[next]`.
type firstBlockerAgg struct {
	expected int
	results  []AgentResult
}

func (a *firstBlockerAgg) Feed(r AgentResult) AggregatorOutcome {
	a.results = append(a.results, r)
	m := effectiveMarker(r)
	if m.Kind == MarkerStop || m.Kind == MarkerGoto {
		return AggregatorOutcome{
			Decided: true,
			Marker:  m,
			Reason:  "first_blocker: agent " + r.Agent + " emitted " + describeKey(keyOf(m)),
		}
	}
	if len(a.results) == a.expected {
		return AggregatorOutcome{
			Decided: true,
			Marker:  Marker{Kind: MarkerNext},
			Reason:  "first_blocker: all agents emitted [next]",
		}
	}
	return AggregatorOutcome{}
}

func (a *firstBlockerAgg) Finalize() AggregatorOutcome {
	// Caso defensivo: motor llamó Finalize sin que ningún agente bloqueara
	// y sin completar expected. Tratamos como [next] si todos fueron [next].
	for _, r := range a.results {
		m := effectiveMarker(r)
		if m.Kind == MarkerStop || m.Kind == MarkerGoto {
			return AggregatorOutcome{
				Decided: true,
				Marker:  m,
				Reason:  "first_blocker: agent " + r.Agent + " emitted " + describeKey(keyOf(m)),
			}
		}
	}
	return AggregatorOutcome{
		Decided: true,
		Marker:  Marker{Kind: MarkerNext},
		Reason:  "first_blocker: no blockers seen",
	}
}

// ---------- helpers ----------

func describeKey(k markerKey) string {
	if k.kind == MarkerGoto {
		return "[goto: " + k.dest + "]"
	}
	return "[" + k.kind.String() + "]"
}

func formatVoteReason(prefix string, k markerKey, count, total int) string {
	return prefix + ": " + itoa(count) + "/" + itoa(total) + " votes for " + describeKey(k)
}

// itoa local (evita pull de strconv para una función trivial).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
