package output

// Sink es el destino final de eventos.
//
// Implementaciones:
//   - writerSink: renderiza a un io.Writer con ANSI (CLI).
//   - NopSink: descarta todo (tests, modo silencioso, fallback).
//   - CapturingSink: colecciona eventos en memoria (tests).
//   - eventSink (en internal/tui): empuja a un channel de tea.Msg.
//
// Las implementaciones DEBEN ser concurrency-safe: execute corre
// validadores async que pueden emitir desde goroutines paralelas.
type Sink interface {
	Emit(Event)
}

// NopSink descarta todos los eventos. Safe por construccion.
type NopSink struct{}

// Emit descarta el evento.
func (NopSink) Emit(Event) {}
