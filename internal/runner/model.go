// Package runner implementa el flujo de ejecucion de pipelines (R1..R4 +
// modales RC/RP). H1 entrega el skeleton minimo; H2 agrega R1 (InputPrompt)
// con resolucion eager del input segun el kind del step 0 (text / pr / issue
// / file / url / none); H3 agrega R2 (Preflight) con chequeos de CLI / skill
// / gh auth / disk space; H4 agrega R3 spawn basico (1 step, sin streaming),
// R4 placeholder (resumen verde), RF placeholder (resumen rojo) y RC modal
// (cancel) — el manifest minimo se escribe al iniciar y al cerrar el run.
//
// El struct RunModel sigue creciendo de a poco — el doc
// (docs/pipeline-execution-flow.html, seccion "Modelo interno") lista el shape
// completo (LogBuffers, file handles, ValidatorRun.Pause), pero nada de eso
// vive todavia en H4. Cada H siguiente agrega los campos que necesita.
package runner

import (
	"time"

	"github.com/chichex/che/internal/wizard"
)

// RunScreen identifica la pantalla actual del runner. H1 dejo solo
// ScreenSkeleton; H2 agrego ScreenInput (R1); H3 agrego ScreenPreflight (R2);
// H4 reemplaza el placeholder transitorio por ScreenRunning (R3 real) y
// agrega ScreenDone (R4) + ScreenFailed (RF) como pantallas terminales. El
// modal RC (cancel) vive como flag CancelModal sobre ScreenRunning (no es un
// screen aparte) — sigue el patron del doc ("modal sobre R3").
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
	// ScreenRunning (R3) es el screen activo durante el spawn. H4 lo
	// implementa con un solo step, blocking cmd.Run(), dump de logs al
	// final (sin streaming — eso es H5). El layout sigue el mockup del
	// doc: header con step N/M, steps tracker (1 row en H4), log pane
	// con el dump del subprocess, footer con ctrl+c. RC (cancel modal)
	// se renderea encima cuando m.CancelModal=true.
	ScreenRunning
	// ScreenDone (R4) es la pantalla terminal verde: resumen del run,
	// duracion, lista de steps, path al run dir y al result.yaml. enter
	// / esc vuelven al lister.
	ScreenDone
	// ScreenFailed (RF) es la pantalla terminal roja: muestra el step
	// que fallo, exit_code, ultimas lineas de stderr/stdout, path al
	// run dir. enter / esc vuelven al lister.
	ScreenFailed
)

// StepStatus es el estado de cada step durante R3. H4 lo usa para los rows
// del tracker + para escribir el manifest (mismo set de valores que el doc
// fija para steps[].status).
type StepStatus string

const (
	StepStatusPending   StepStatus = "pending"
	StepStatusRunning   StepStatus = "running"
	StepStatusDone      StepStatus = "done"
	StepStatusFailed    StepStatus = "failed"
	StepStatusCancelled StepStatus = "cancelled"
)

// StepRun es el snapshot vivo de un step en runtime. H4 popula Status /
// StartedAt / FinishedAt / ExitCode; H7 agrega Validator (cuando el step
// declara un bloque validator). Idx es 1-indexed para alinear con los
// nombres de los archivos en disco (step-01.stdout.log).
type StepRun struct {
	Idx        int
	Name       string
	CLI        string
	Kind       string
	Status     StepStatus
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	// SpawnError captura cualquier error de exec.Cmd.Start() / Wait() que
	// no sea un exit no-cero "normal". Sirve para diferenciar "el binario
	// no se pudo arrancar" de "el binario corrio y devolvio ≠ 0" en RF.
	SpawnError string
	// Validator es el snapshot del cross-review del step (H7). nil si el
	// step no declaro bloque validator. Se popula apenas el subprocess
	// del step termina con exit 0 y se detecta validator no-nil; cada
	// loop incrementa LoopsRun y sobrescribe LastFeedback. FinalVerdict
	// se setea al salir del loop (ok | fail | human-override).
	Validator *ValidatorRun

	// PermissionDenials = nombres de tools que claude pidio durante el
	// step y le fueron denegadas (no-TTY). Lo poblamos post-mortem
	// leyendo el ultimo `result` event de events.jsonl. Cuando no esta
	// vacio, R4 renderea un chip warn ("⚠ permission denied: tool")
	// porque el step puede haber salido exit 0 sin haber hecho nada.
	// Solo aplica a CLIs que emiten stream-json (claude); para los
	// demas queda nil.
	PermissionDenials []string

	// EventsRun es el contador (1-based) de la corrida activa del step.
	// Se incrementa en cada `runStep` (primer start y cada rerun del
	// validator). El archivo events.jsonl se rota a
	// step-NN.events.RUN-K.jsonl por valor de K, asi cada rerun preserva
	// la traza del anterior en disco (Fix #107). Para CLIs que no emiten
	// stream-json (gemini/opencode/...) el contador igual se incrementa
	// pero no se materializa archivo.
	EventsRun int
}

// ValidatorRun es el estado vivo del loop del validator de un step. Sigue
// el shape del doc (seccion "Modelo interno"): LoopsRun crece en cada
// iteracion, MaxLoops y OnMaxLoops vienen del bloque validator del step,
// FinalVerdict queda vacio mientras el loop sigue activo y se setea al
// resolver (ok | fail | human-override).
type ValidatorRun struct {
	CLI          string
	LoopsRun     int
	MaxLoops     int
	OnMaxLoops   string
	FinalVerdict string
	LastFeedback string
	// LastFeedbackRawOnly indica que LastFeedback es solo el fallback
	// `"verdict: " + raw` (sin texto explicito del validator). En ese caso
	// mergeFeedbackIntoPayload NO lo prependea al payload del rerun: el
	// string pelado es ruido al modelo. Sigue siendo util para el modal RP
	// y last_feedback del manifest (Fix #107).
	LastFeedbackRawOnly bool
}

// Final verdict values registrados en manifest.steps[i].validator.final_verdict.
// Los 3 estados terminales del loop del validator (H7).
const (
	// FinalVerdictOk indica que el validator emitio verdict: ok en algun
	// loop antes de agotar max_loops. El step se considera valido y el
	// pipeline avanza al siguiente.
	FinalVerdictOk = "ok"
	// FinalVerdictFail indica que el validator agoto max_loops sin ok y
	// on_max_loops=fail (o el modal RP eligio abort). El run termina en RF.
	FinalVerdictFail = "fail"
	// FinalVerdictFailButContinued indica que se agoto max_loops y
	// on_max_loops=continue: el pipeline acepto el ultimo output y
	// avanzo al siguiente step. El doc lo deja explicito como valor
	// distinto a "fail" (asi el manifest queda auditable).
	FinalVerdictFailButContinued = "fail-but-continued"
	// FinalVerdictHumanOverride indica que el modal RP eligio "continuar":
	// el humano acepto el ultimo output a pesar del fail del validator.
	FinalVerdictHumanOverride = "human-override"
)

// PauseChoice indexa las opciones del modal RP. H7 expone tres opciones:
// continuar (acepta el ultimo output, manifest registra
// final_verdict=human-override), retry (resetea el contador y pide otra
// pasada de loops), abort (equivalente a fail → RF). El cero-value apunta a
// "continuar" para que el cursor inicial sea la opcion que tipicamente el
// usuario quiere despues de revisar el feedback (matchea el orden del doc).
type PauseChoice int

const (
	PauseChoiceContinue PauseChoice = iota
	PauseChoiceRetry
	PauseChoiceAbort
)

// PauseState es el estado del modal RP (Paused). Vive en RunModel.PauseModal
// como puntero — nil cuando el modal no esta activo. Cuando el validator
// agota max_loops con on_max_loops=pause, handleValidatorDone lo crea con
// el ultimo feedback + el step que lo gatillo, y la screen sigue siendo
// ScreenRunning (el modal se renderea encima — patron del doc).
type PauseState struct {
	// StepIdx es el idx (0-based) del step que agoto los loops. El handler
	// lo usa para retomar el flow segun la decision del usuario.
	StepIdx int
	// LastFeedback es el feedback del ultimo verdict.fail (string crudo
	// del campo `feedback` del verdict.yaml). Se renderea en el modal
	// como "ultimo feedback del validator".
	LastFeedback string
	// Choice es la opcion seleccionada actualmente en el modal (cursor).
	Choice PauseChoice
}

// CancelChoice indexa las opciones del modal RC. H4 expone tres opciones
// (abort & save, back to run, exit che). El cero-value apunta a la primera
// opcion para que el cursor inicial sea siempre el "destructivo" (matchea
// el mockup donde abort esta arriba).
type CancelChoice int

const (
	CancelChoiceAbort CancelChoice = iota
	CancelChoiceBack
	CancelChoiceExit
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

	// R3 / R4 / RF state — todo poblado por H4 al iniciar el run.
	//
	// RunID y RunDir se calculan al spawnear: RunID es el timestamp UTC
	// formateado como "2006-01-02T15-04-05" (sortable, sin colons). RunDir
	// es ~/.che/runs/<slug>/<RunID>/.
	RunID  string
	RunDir string

	// RunStartedAt es el timestamp del enterRunning (cuando arranca el
	// run, no el step). H8 lo usa para popular el header del manifest de
	// forma consistente entre snapshots intermedios y el cierre — antes de
	// H8 los snapshots intermedios usaban fallbackStart=step.StartedAt y
	// pisaban el started_at original del manifest con cada step.
	RunStartedAt time.Time

	// Steps es el slice 1-indexed (Steps[0].Idx==1) que vive durante R3 +
	// se renderea en R4/RF. H4 lo crea con un solo elemento (el step 0 del
	// pipeline); H6 va a llenar con los N steps reales.
	Steps []StepRun

	// Active es el indice (0-based) del step en curso. H4 lo deja siempre
	// en 0 — es 1 step por pipeline. H6 lo va a incrementar.
	Active int

	// LogDump guarda el stdout del step en curso (concat dump al final).
	// H4 lo escribe entero al terminar el subprocess; H5 lo sigue
	// poblando al cerrar el step para que R4/RF mantengan el resumen
	// terminal — durante R3, sin embargo, el render usa LogBuffers (ring
	// buffer en RAM) para streaming linea por linea.
	LogDump string

	// LogBuffers es el ring buffer por step (2000 lineas / step segun el
	// doc). H5 lo crea al spawnear el step y lo va llenando desde la
	// goroutine de tee del subprocess. El render de R3 lee el buffer del
	// step en LogFocus (default = step activo). H6 va a sumar mas
	// elementos cuando lleguen multi-step.
	LogBuffers []*RingBuffer

	// LogFocus es el indice del step cuyos logs se renderean en el log
	// pane. H5 lo deja igual a Active (1 step) — H6 lo va a desacoplar via
	// tab.
	LogFocus int

	// StickyBottom = true cuando el viewport hace auto-scroll al fondo
	// (default). Se desactiva cuando el usuario scrollea con ↑/↓; `g` lo
	// reactiva (criterio del doc).
	StickyBottom bool

	// LogScrollOffset es la cantidad de lineas que el viewport esta
	// scrolleado HACIA ARRIBA (desde el fondo). 0 significa "fondo" (que
	// junto con StickyBottom=true es la posicion default). Solo es
	// relevante cuando StickyBottom=false.
	LogScrollOffset int

	// FailedStderr almacena el stderr del step que fallo, para que RF
	// pueda mostrarlo en la pantalla terminal. H4 lo lee del archivo
	// step-NN.stderr.log al detectar exit ≠ 0.
	FailedStderr string

	// MultiStepWarning = true cuando el pipeline tiene N>1 steps. R3 lo
	// muestra como banner amarillo recordando que multi-step llega en H6;
	// el run igual ejecuta solo el step 0 (defensive — el doc lo deja
	// explicito como criterio de aceptacion de H4).
	MultiStepWarning bool

	// CancelModal indica si el modal RC esta abierto sobre R3. Mientras
	// es true, las teclas se interpretan como navegacion del modal
	// (up/down + enter), no del log pane.
	CancelModal  bool
	CancelChoice CancelChoice

	// PauseModal es el estado del modal RP (Paused). nil cuando no hay
	// pause activa; no-nil cuando el validator agoto max_loops con
	// on_max_loops=pause y esperamos decision humana. Cuando es no-nil,
	// las teclas se interpretan como navegacion del modal (up/down +
	// enter), igual que CancelModal. No coexisten — el doc fija que RC
	// y RP son mutuamente exclusivos (no se abre uno mientras el otro
	// esta visible).
	PauseModal *PauseState

	// ValidatorActive = true mientras el subprocess del validator esta
	// vivo (entre el spawn y el validatorDoneMsg). El renderer lo usa para
	// mostrar "loop K/max" en el row del step y para que el ring buffer
	// del paso muestre "validating..." mientras tanto. Cuando la
	// transicion del loop manda otro spawn (re-run del step o nuevo
	// validator), se reactiva.
	ValidatorActive bool

	// runState es el handle compartido (puntero) entre el modelo y la
	// goroutine del spawn. Vive desde enterRunning hasta handleStepDone
	// — el modelo se pasa por valor en bubbletea pero el puntero
	// sobrevive las copias, asi cancel handler + spawn goroutine ven el
	// mismo *exec.Cmd / canal de cancel. Lo limpia handleStepDone.
	runState *runState

	// exitApp = true si el usuario pidio salida total (q / ctrl+c). false
	// significa "volver al lister" (esc).
	exitApp bool

	// retryRequested = true si el usuario presiono `r` en RF (failed o
	// cancelled). El caller (runner.Run) detecta el flag al cierre del
	// program y arma una nueva pasada del runner desde R1 con el input ya
	// resuelto pre-cargado. El doc fija "crea un run-id nuevo" — se hace
	// reseteando RunID/RunDir + dejando el ResolvedPayload del Input
	// intacto para que enterRunning vuelva a generar el run-dir.
	retryRequested bool

	// terminalWidth / terminalHeight son el tamaño del terminal segun el
	// ultimo tea.WindowSizeMsg recibido (H10). Lo usa renderLogPane para
	// dimensionar dinamicamente el viewport del log pane sobre R3 — el cap
	// fijo de logViewportLines (18) sigue siendo el fallback cuando el
	// program todavia no recibio un WindowSizeMsg (tests sin pty / antes
	// del primer render).
	terminalWidth  int
	terminalHeight int
}
