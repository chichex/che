package output

import (
	"strings"
	"sync"
)

// CapturingSink colecciona Events en memoria. Concurrency-safe.
//
// Pensado para tests unitarios de flows: despues de correr el flow con
// un Logger sobre un CapturingSink, se puede asertar sobre los eventos
// estructurados (sin tener que parsear ANSI).
type CapturingSink struct {
	mu     sync.Mutex
	events []Event
}

// Emit colecciona el evento.
func (s *CapturingSink) Emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// Events devuelve una copia de los eventos capturados hasta el momento.
func (s *CapturingSink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Reset vacia la lista de eventos. Util entre sub-tests.
func (s *CapturingSink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

// FindByMessage devuelve el primer evento cuyo Message contenga substr.
// El bool indica si se encontro.
func (s *CapturingSink) FindByMessage(substr string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if strings.Contains(ev.Message, substr) {
			return ev, true
		}
	}
	return Event{}, false
}

// CountByLevel devuelve cuantos eventos hay del nivel dado.
func (s *CapturingSink) CountByLevel(lv Level) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev.Level == lv {
			n++
		}
	}
	return n
}
