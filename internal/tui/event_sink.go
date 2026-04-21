package tui

import (
	"bytes"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/output"
)

// eventSink empuja Events estructurados del logger al channel del Model
// como eventMsg. Concurrency-safe (channel send + select default para
// no bloquear goroutines del flow si el buffer esta lleno).
//
// Diseño: la TUI NO recibe lineas pre-renderizadas — recibe el Event crudo
// y arma su propio render con lipgloss (eventLineFor) para integrar con
// sus estilos (logLineStyle, truncate, padding).
type eventSink struct {
	ch chan<- tea.Msg
}

func (s *eventSink) Emit(ev output.Event) {
	select {
	case s.ch <- eventMsg{ev: ev}:
	default:
		// Si el buffer esta lleno, dropeamos. Preferible perder una linea
		// de log que bloquear el goroutine del flow.
	}
}

// eventMsg entrega un Event al Update loop.
type eventMsg struct{ ev output.Event }

// payloadMsg entrega una linea de stdout "payload" (URLs creadas,
// "Done.", "Executed", etc.). Se renderiza con logLineStyle para
// matchear el look del runLog preexistente.
type payloadMsg struct{ line string }

// stdoutLineWriter es un io.Writer que bufferiza datos hasta un
// newline y empuja cada linea completa al channel como payloadMsg. Sirve
// para capturar los fmt.Fprintln(stdout, ...) de los flows sin duplicar
// su logica en events. Concurrency-safe.
type stdoutLineWriter struct {
	ch  chan<- tea.Msg
	mu  sync.Mutex
	buf bytes.Buffer
}

func newStdoutLineWriter(ch chan<- tea.Msg) *stdoutLineWriter {
	return &stdoutLineWriter{ch: ch}
}

func (w *stdoutLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		line := string(data[:nl])
		w.buf.Next(nl + 1)
		if line == "" {
			continue
		}
		select {
		case w.ch <- payloadMsg{line: line}:
		default:
		}
	}
	return len(p), nil
}
