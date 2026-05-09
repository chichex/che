// Package runner implementa el flujo de ejecucion de pipelines (R1..R4 +
// modales RC/RP). H1 entrega el skeleton minimo: screen estatica con titulo
// "Run · <name>" + mensaje "runner pendiente". esc vuelve al lister, q sale
// total. Las screens reales (input/preflight/running/done) llegan en H2+.
//
// El struct RunModel queda chico a proposito en H1 — el doc
// (docs/pipeline-execution-flow.html, seccion "Modelo interno") lista el shape
// completo (Input, Preflight, Steps, LogBuffers, modales, file handles), pero
// nada de eso vive todavia en H1. Cada H siguiente agrega los campos que
// necesita.
package runner

import (
	"github.com/chichex/che/internal/wizard"
)

// RunScreen identifica la pantalla actual del runner. En H1 solo existe
// ScreenSkeleton — el placeholder estatico. R1..R4 + RP/RC se agregan en
// H2..H10.
type RunScreen int

const (
	// ScreenSkeleton es el placeholder de H1: titulo + "runner pendiente"
	// + hint con teclas. Sirve para validar el routing R0 → runner sin
	// implementar funcionalidad real.
	ScreenSkeleton RunScreen = iota
	// TODO H2: ScreenInput (R1).
	// TODO H3: ScreenPreflight (R2).
	// TODO H4: ScreenRunning (R3).
	// TODO H4: ScreenDone (R4).
	// TODO H4: ScreenFailed (RF).
	// TODO H4: ScreenCancel (RC).
	// TODO H7: ScreenPause (RP).
)

// RunModel es la struct unica que vive en el bubbletea program del runner.
// En H1 alcanza con Pipeline + Screen + exitApp; los campos del doc
// (Input/Preflight/Steps/LogBuffers/...) se agregan a partir de H2.
type RunModel struct {
	Screen   RunScreen
	Pipeline wizard.Pipeline

	// path absoluto del archivo del pipeline en disco. H1 lo lee solo para
	// reportar errores razonables; H8+ lo usa para resolver paths del
	// run-dir.
	path string

	// exitApp = true si el usuario pidio salida total (q / ctrl+c). false
	// significa "volver al lister" (esc).
	exitApp bool
}
