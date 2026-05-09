// Package runner implementa el flujo de ejecucion de pipelines (R1..R4 +
// modales RC/RP). H1 entrega el skeleton minimo; H2 agrega R1 (InputPrompt)
// con resolucion eager del input segun el kind del step 0 (text / pr / issue
// / file / url / none); H3 agrega R2 (Preflight) con chequeos de CLI / skill
// / gh auth / disk space.
//
// El struct RunModel sigue creciendo de a poco — el doc
// (docs/pipeline-execution-flow.html, seccion "Modelo interno") lista el shape
// completo (Steps, LogBuffers, modales, file handles), pero nada de eso vive
// todavia. Cada H siguiente agrega los campos que necesita.
package runner

import (
	"github.com/chichex/che/internal/wizard"
)

// RunScreen identifica la pantalla actual del runner. H1 dejo solo
// ScreenSkeleton; H2 agrego ScreenInput (R1); H3 reemplaza el placeholder
// generico de "siguiente" por ScreenPreflight (R2 real) + un
// ScreenRunningPlaceholder transitorio que H4 va a convertir en R3.
type RunScreen int

const (
	// ScreenSkeleton es el placeholder de H1: titulo + "runner pendiente"
	// + hint con teclas. H2/H3 ya no lo usan en el flow real, pero el
	// enum se preserva para que tests viejos que lean el cero-value no
	// rompan; el entry path arranca directo en ScreenInput o (si
	// step[0].input=none) ScreenPreflight.
	ScreenSkeleton RunScreen = iota
	// ScreenInput (R1) recolecta + resuelve eager el input del step 0.
	ScreenInput
	// ScreenPreflight (R2) corre los chequeos de dependencias antes de
	// spawnear nada (H3): CLI installed por cli distinto, skill exists
	// por step/validator kind=skill, gh auth si algun input es pr/issue,
	// file readable defensivo si input=file, disk space ≥ 100 MB en
	// ~/.che/runs (warning amarillo si no llega).
	ScreenPreflight
	// ScreenRunningPlaceholder es el destino "post-preflight ok" de H3.
	// H4 lo reemplaza por ScreenRunning (R3) con spawn real, log pane,
	// steps tracker, etc. Tener un screen explicito (en lugar de tea.Quit
	// con un flag) deja la transicion de H3 testeable end-to-end sin
	// implementar nada que H3 declare out of scope.
	ScreenRunningPlaceholder
	// TODO H4: ScreenRunning (R3) reemplaza ScreenRunningPlaceholder.
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
// H3 agrega Preflight + preflightConfirm; H4+ va a sumar Steps, LogBuffers,
// los file handles del subprocess, etc — segun el "Modelo interno" del doc.
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

	// Preflight es el snapshot del checklist R2: un row por chequeo (CLI
	// installed, skill exists, gh auth, file readable, disk space). Se
	// popula al entrar a ScreenPreflight (enterPreflight) y se reescribe
	// entero al re-correr con `r`. Vacio cuando estamos fuera de R2.
	Preflight []PreflightCheck

	// preflightConfirm = true cuando el verdict de Preflight es "solo
	// warnings" y el usuario ya presiono enter una vez. El proximo enter
	// avanza a la screen siguiente. False en cualquier otro caso (todos
	// verdes, algun rojo, o tras un retry con `r`).
	preflightConfirm bool

	// exitApp = true si el usuario pidio salida total (q / ctrl+c). false
	// significa "volver al lister" (esc).
	exitApp bool
}
