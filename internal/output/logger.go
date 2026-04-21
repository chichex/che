package output

import (
	"bytes"
	"sync"
	"time"
)

// Logger es la fachada que usan los flows. Wrapper fino sobre Sink.
//
// Se inyecta en Opts por parametro explicito (no context, no singleton):
// coherente con el patron existente de pasar io.Writer a los flows, y
// safe bajo t.Parallel en los tests e2e.
type Logger struct {
	sink Sink
}

// New construye un Logger sobre un sink. Si sink es nil, usa NopSink.
//
// Esto hace safe pasar Opts{Out: nil} desde call-sites legacy (y desde
// tests que no quieran asertar sobre output).
func New(sink Sink) *Logger {
	if sink == nil {
		sink = NopSink{}
	}
	return &Logger{sink: sink}
}

// Info emite un evento de nivel Info.
func (l *Logger) Info(msg string, f ...F) { l.emit(LevelInfo, msg, f) }

// Step emite un evento de nivel Step (sub-paso, dim).
func (l *Logger) Step(msg string, f ...F) { l.emit(LevelStep, msg, f) }

// Success emite un evento de nivel Success.
func (l *Logger) Success(msg string, f ...F) { l.emit(LevelSuccess, msg, f) }

// Warn emite un evento de nivel Warn.
func (l *Logger) Warn(msg string, f ...F) { l.emit(LevelWarn, msg, f) }

// Error emite un evento de nivel Error.
func (l *Logger) Error(msg string, f ...F) { l.emit(LevelError, msg, f) }

func (l *Logger) emit(lv Level, msg string, ff []F) {
	var fields F
	if len(ff) > 0 {
		fields = ff[0]
	}
	l.sink.Emit(Event{
		Time:    time.Now(),
		Level:   lv,
		Message: msg,
		Fields:  fields,
	})
}

// AsWriter devuelve un io.Writer que bufferiza hasta un newline y emite
// cada linea completa como un evento del nivel indicado. Util como
// adapter para helpers legacy que reciben io.Writer (ej. stderr) sin
// reescribir su firma: pasar log.AsWriter(LevelError) y cada línea se
// loguea como error estructurado.
func (l *Logger) AsWriter(lv Level) *LineWriter {
	return &LineWriter{logger: l, level: lv}
}

// LineWriter adapta un Logger a io.Writer. Concurrency-safe: mutex
// interno serializa los flushes de líneas.
type LineWriter struct {
	logger *Logger
	level  Level
	mu     sync.Mutex
	buf    bytes.Buffer
}

// Write bufferiza hasta newline y emite un evento por cada línea completa.
func (w *LineWriter) Write(p []byte) (int, error) {
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
		w.logger.emit(w.level, line, nil)
	}
	return len(p), nil
}
