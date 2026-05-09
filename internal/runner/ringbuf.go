package runner

import "sync"

// LogLineKind distingue stdout (linea humana / texto crudo) de stderr (linea
// de error, renderea en rojo dimmed). El parser de stream-json emite ambas:
// la linea formateada va a Kind=stdout; eventos crudos opcionales se
// guardan aparte (events.jsonl) y no entran al ring buffer.
type LogLineKind int

const (
	LogLineStdout LogLineKind = iota
	LogLineStderr
)

// LogLine es una entrada del ring buffer del log pane. H5 las acumula por
// step en RAM (cap 2000 segun el doc) y el render las pinta en orden de
// llegada — stderr en rojo dimmed, intercalado con stdout (criterio del
// doc: "lineas de stderr en rojo dimmed, intercaladas").
type LogLine struct {
	Kind LogLineKind
	Text string
	// Seq es el numero de orden global (entre stdout y stderr). Sirve para
	// debug + para rendereos futuros que quieran ordenar a un timestamp.
	Seq uint64
}

// RingBuffer guarda las ultimas Cap lineas del subprocess. Es thread-safe
// porque las goroutines del tee (1 por stream) appendean concurrentemente
// y el render (en el goroutine de bubbletea) las lee al View().
//
// El doc fija Cap=2000 por step. Cuando se llena, el append mas nuevo pisa
// el mas viejo (FIFO clasico de "ultimas N").
type RingBuffer struct {
	mu    sync.Mutex
	cap   int
	lines []LogLine
	// nextSeq es el contador monotonico global del buffer. No reinicia con
	// el wrap-around (sirve para que el View detecte cambios y para tests).
	nextSeq uint64
}

// NewRingBuffer construye un buffer con capacidad fija. Cap <=0 cae a 1
// (defensivo — un buffer de cero lineas no tiene sentido).
func NewRingBuffer(cap int) *RingBuffer {
	if cap <= 0 {
		cap = 1
	}
	return &RingBuffer{cap: cap}
}

// Append agrega una linea. Si el buffer esta lleno, descarta la mas vieja.
// Devuelve la Seq asignada — sirve para correlacionar con events.jsonl.
func (r *RingBuffer) Append(kind LogLineKind, text string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSeq++
	line := LogLine{Kind: kind, Text: text, Seq: r.nextSeq}
	if len(r.lines) < r.cap {
		r.lines = append(r.lines, line)
	} else {
		// Drop oldest: shift via copy. Cap pequenita (2000) — un copy en
		// cada wrap es aceptable y mantiene Snapshot() trivial.
		copy(r.lines, r.lines[1:])
		r.lines[len(r.lines)-1] = line
	}
	return r.nextSeq
}

// Snapshot devuelve una copia del slice actual (orden cronologico,
// vieja → nueva). Safe para usar fuera del lock.
func (r *RingBuffer) Snapshot() []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogLine, len(r.lines))
	copy(out, r.lines)
	return out
}

// Len devuelve la cantidad actual de lineas guardadas (entre 0 y Cap).
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

// Cap devuelve la capacidad maxima del buffer.
func (r *RingBuffer) Cap() int {
	return r.cap
}

// Clear vacia el buffer. ctrl+l de R3 lo invoca para limpiar el viewport
// (los archivos en disco no se tocan — el doc lo deja explicito).
func (r *RingBuffer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
}
