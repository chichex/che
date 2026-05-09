// Package runner implementa el flujo de ejecucion de pipelines (R1..R4 +
// modales RC/RP). H1 entrega el skeleton minimo; H2 agrega R1 (InputPrompt)
// con resolucion eager del input segun el kind del step 0 (text / pr / issue
// / file / url / none).
//
// El struct RunModel queda chico a proposito en H1/H2 — el doc
// (docs/pipeline-execution-flow.html, seccion "Modelo interno") lista el shape
// completo (Preflight, Steps, LogBuffers, modales, file handles), pero nada
// de eso vive todavia. Cada H siguiente agrega los campos que necesita.
package runner

import (
	"github.com/chichex/che/internal/wizard"
)

// RunScreen identifica la pantalla actual del runner. H1 dejo solo
// ScreenSkeleton; H2 agrega ScreenInput (R1) y ScreenSecondary, un
// placeholder mientras R2 (preflight) no exista — H3 lo reemplaza por
// ScreenPreflight real.
type RunScreen int

const (
	// ScreenSkeleton es el placeholder de H1: titulo + "runner pendiente"
	// + hint con teclas. H2 ya no lo usa para el flow real, pero el enum
	// se preserva para que tests viejos que lean el cero-value no
	// rompan; el entry path arranca directo en ScreenInput o (si
	// step[0].input=none) ScreenSecondary.
	ScreenSkeleton RunScreen = iota
	// ScreenInput (R1) recolecta + resuelve eager el input del step 0.
	ScreenInput
	// ScreenSecondary es el placeholder de R2 mientras H3 no lo
	// reemplaza por ScreenPreflight. Renderiza "ok, siguiente:
	// preflight (placeholder)" + hints, asi el smoke manual de H2
	// puede chequear que la transicion sucedio.
	ScreenSecondary
	// TODO H3: ScreenPreflight (R2) reemplaza ScreenSecondary.
	// TODO H4: ScreenRunning (R3).
	// TODO H4: ScreenDone (R4).
	// TODO H4: ScreenFailed (RF).
	// TODO H4: ScreenCancel (RC).
	// TODO H7: ScreenPause (RP).
)

// InputState es el resultado de R1: que se pidio, que tipeo el usuario y
// el payload ya resuelto eager (texto crudo / contenido del file / body
// HTTP / dump de gh). H3+ lo consume desde RunModel.Input para preflight
// y para el spawn del step 0 (CHE_INPUT_PAYLOAD via stdin).
type InputState struct {
	// Kind copia el step[0].Input (wizard.InputText / InputFile / ...).
	// Sirve para que screens posteriores sepan como interpretar Value
	// vs ResolvedPayload sin re-leer el pipeline.
	Kind string
	// Value es lo que el usuario tipeo / seleccionó. Para text es el
	// texto crudo (= ResolvedPayload). Para file/url/pr/issue es la
	// ruta / URL / referencia (no el contenido).
	Value string
	// ResolvedPayload es el contenido eager-fetched. Vacio mientras R1
	// no confirmo. Despues de confirmar:
	//   - text  → texto crudo (igual a Value)
	//   - file  → bytes leidos del archivo
	//   - url   → body de la respuesta HTTP
	//   - pr    → JSON dump de gh pr view
	//   - issue → JSON dump de gh issue view
	//   - none  → ""
	ResolvedPayload string
}

// RunModel es la struct unica que vive en el bubbletea program del runner.
// H1/H2 mantienen el shape chico — el doc tiene el shape completo (Preflight,
// Steps, LogBuffers, modales, file handles), que se va llenando a partir de
// H3.
type RunModel struct {
	Screen   RunScreen
	Pipeline wizard.Pipeline

	// path absoluto del archivo del pipeline en disco. Sirve para reportar
	// errores razonables y, a partir de H8+, para resolver paths del
	// run-dir.
	path string

	// Input es el resultado de R1. Se popula al confirmar (ctrl+s / enter
	// sobre el ultimo foco) con resolucion exitosa.
	Input InputState

	// inputUI es el estado UI puro de R1 (buffer del textInput, cursor del
	// file picker, etc). Lo separamos de InputState para que el modelo
	// "publico" (el campo Input) refleje solo el contrato de salida de
	// R1, sin acoplarse al picker.
	inputUI inputUIState

	// inputErr es el error inline mostrado debajo del input. Se limpia al
	// volver a tipear / al cambiar de seleccion en el picker.
	inputErr string

	// exitApp = true si el usuario pidio salida total (q / ctrl+c). false
	// significa "volver al lister" (esc).
	exitApp bool
}
