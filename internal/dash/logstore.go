// Package dash — logstore: buffer + pub/sub para el stream en vivo de los
// logs del subproceso `che <flow> <id>` disparado desde el dashboard.
//
// Cada entidad tiene su propio canal de historia (ring buffer bounded) y una
// lista de suscriptores activos. Los suscriptores reciben las líneas futuras
// después de que hicieron Subscribe; la historia se consulta aparte con
// Snapshot(). El handler SSE (GET /stream/{id}) hace primero un Snapshot y
// después Subscribe, de modo que los clientes que abren el modal en la mitad
// de un run ven lo ya emitido + lo que venga.
//
// Concurrency model:
//   - Un sync.RWMutex protege el map de entities. RLock para Snapshot/
//     Subscribe (rutas calientes), Lock para Append y Close.
//   - Cada entity mantiene su propio slice ring (no circular: slice con cap
//     fijo; al pasarse, se dropean del frente con copy). Simplifica la vida
//     al leer el snapshot (append al slice destino y listo) a cambio de O(N)
//     por evict, aceptable para N=500.
//   - Los subscribers son chans bufferados. Si un cliente lento no drena,
//     Append hace non-blocking send (select default) y descarta para esa
//     subscripción — no bloquea a los demás.
//   - CloseRun cierra los canales y los retira del map. Idempotente: llamar
//     dos veces no panikea (chequea "ya cerrado" con un flag bajo lock).
package dash

import (
	"sync"
	"time"
)

// LogLine es una línea de output del subproceso. Stream distingue stdout /
// stderr / meta (marcador interno, ej fin de flow). Text NO incluye el "\n"
// trailing — el Scanner lo come. Time es el instante en que se leyó la
// línea del pipe (no el instante en que el subproceso la emitió; hay skew
// típico del orden de microsegundos que no nos importa).
type LogLine struct {
	Time   time.Time
	Stream string // "stdout" | "stderr" | "meta"
	Text   string
}

// ringDefault es la capacidad por defecto del ring buffer por entidad. 500
// líneas alcanza para ~cualquier run razonable de che execute (tool use +
// pushes + etc. suele quedar en decenas). Lo exponemos para que tests que
// exercitan el wraparound no tengan que llenar 500 líneas. Ajustable si en
// producción vemos runs más largos.
const ringDefault = 500

// LogStore es el buffer global per-entity. Guarda historia bounded + un set
// de subscribers por id. Construir con NewLogStore (o NewLogStoreSize para
// tests con capacidad chica).
type LogStore struct {
	mu   sync.RWMutex
	cap  int
	data map[int]*entityLog
}

type entityLog struct {
	// lines es el ring buffer. Cuando len == cap, el próximo Append dropea
	// el primer elemento (copy n-1 → 0, luego append).
	lines []LogLine
	// subs son los canales de los subscribers activos. Se cierran al llamar
	// CloseRun; se remueven por el cancel fn devuelto por Subscribe.
	subs []chan LogLine
	// closed indica que ya se hizo CloseRun — Append se convierte en no-op
	// (excepto por la historia si aún no limpiamos) y Subscribe devuelve
	// un canal ya cerrado (el cliente recibe el snapshot y chau).
	closed bool
}

// NewLogStore devuelve un store vacío con capacidad por defecto.
func NewLogStore() *LogStore {
	return NewLogStoreSize(ringDefault)
}

// NewLogStoreSize devuelve un store con una capacidad ring distinta. Útil
// en tests de wraparound para no tener que llenar 500 entradas.
func NewLogStoreSize(n int) *LogStore {
	if n <= 0 {
		n = ringDefault
	}
	return &LogStore{cap: n, data: map[int]*entityLog{}}
}

// Append agrega una línea a la historia del id y la despacha a todos los
// subscribers activos. Si el canal de un subscriber está lleno, la línea se
// descarta para ESE subscriber (no se bloquea a Append). La historia se
// registra siempre que el entry no esté cerrado.
func (s *LogStore) Append(id int, line LogLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if !ok {
		e = &entityLog{lines: make([]LogLine, 0, s.cap)}
		s.data[id] = e
	}
	if e.closed {
		// Post-close Append es no-op. Evita historia "zombie" después de que
		// el run cerró y un goroutine pipeline demorada intentó escribir.
		return
	}
	if len(e.lines) >= s.cap {
		// Evict front. Slide y reuso del backing array.
		copy(e.lines, e.lines[1:])
		e.lines = e.lines[:len(e.lines)-1]
	}
	e.lines = append(e.lines, line)
	for _, ch := range e.subs {
		select {
		case ch <- line:
		default:
			// Drop. El cliente está lento; el resto sigue servido. El
			// cliente ya tenía el snapshot inicial, así que el worst-case
			// son unos mensajes missing hasta que reabra.
		}
	}
}

// Snapshot devuelve una copia de la historia del id. Seguro para leer sin
// mutar. Si el id no existe, devuelve slice vacío (no nil) — el handler SSE
// lo itera directo sin un if-else.
func (s *LogStore) Snapshot(id int) []LogLine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[id]
	if !ok {
		return []LogLine{}
	}
	out := make([]LogLine, len(e.lines))
	copy(out, e.lines)
	return out
}

// Exists reporta si hay entry para el id (aunque esté cerrado). Usado por
// el handler SSE para responder 404 cuando nunca se disparó un flow para
// ese id.
func (s *LogStore) Exists(id int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[id]
	return ok
}

// Closed reporta si el run del id ya terminó (canal de subscribers cerrado).
// Útil para el handler SSE: si ya está cerrado, manda el snapshot y el
// evento done sin suscribirse (no tiene sentido esperar líneas futuras).
func (s *LogStore) Closed(id int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[id]
	return ok && e.closed
}

// Subscribe devuelve un canal que recibe las líneas FUTURAS del id (no
// incluye la historia — eso se consulta con Snapshot). El segundo return es
// la cancel fn: cuando el cliente se desconecta, llamarla libera el slot
// del subscriber. cancel es idempotente.
//
// Si el run ya está cerrado al llamar Subscribe, devuelve un canal ya
// cerrado — el range en el caller sale inmediatamente sin deadlock.
func (s *LogStore) Subscribe(id int) (<-chan LogLine, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if !ok {
		e = &entityLog{lines: make([]LogLine, 0, s.cap)}
		s.data[id] = e
	}
	ch := make(chan LogLine, 64) // buffer chico, absorbe bursts de tool-use
	if e.closed {
		close(ch)
		return ch, func() {}
	}
	e.subs = append(e.subs, ch)
	cancelled := false
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cancelled {
			return
		}
		cancelled = true
		ent, ok := s.data[id]
		if !ok {
			return
		}
		// Remove ch de ent.subs y cerrarlo. Si el run ya fue cerrado
		// globalmente, el canal ya está cerrado: no hacemos doble close.
		for i, c := range ent.subs {
			if c == ch {
				ent.subs = append(ent.subs[:i], ent.subs[i+1:]...)
				if !ent.closed {
					close(ch)
				}
				return
			}
		}
	}
	return ch, cancel
}

// CloseRun marca el run del id como terminado: cierra todos los canales de
// los subscribers vivos y setea closed=true. Appends futuros son no-op.
// Idempotente (llamar dos veces no panikea).
//
// NO borra la historia — queda disponible para clientes que abran el modal
// después (harán Snapshot + Subscribe; Subscribe devuelve un canal cerrado
// inmediatamente, el handler SSE manda `event: done` y sale).
func (s *LogStore) CloseRun(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if !ok {
		// Primer CloseRun sin Append previo: creamos el entry marcado como
		// closed. Handlers que lleguen después ven Closed()=true.
		s.data[id] = &entityLog{lines: make([]LogLine, 0, s.cap), closed: true}
		return
	}
	if e.closed {
		return
	}
	e.closed = true
	for _, ch := range e.subs {
		close(ch)
	}
	e.subs = nil
}

// ResetRun limpia la historia y reabre el entry para un nuevo run del mismo
// id. Llamado al disparar un nuevo flow sobre una entidad que ya tenía un
// run anterior — el buffer es por entidad, no por run, así que los logs del
// run previo no nos interesan más cuando arrancamos uno nuevo.
//
// Si hay subscribers activos, los cierra antes de resetear: sus EventSource
// reciben `done` y se tienen que reconectar con el nuevo id (el browser lo
// hace solo tras el close del stream SSE).
func (s *LogStore) ResetRun(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if ok {
		for _, ch := range e.subs {
			close(ch)
		}
	}
	s.data[id] = &entityLog{lines: make([]LogLine, 0, s.cap)}
}
