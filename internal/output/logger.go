package output

import "time"

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
