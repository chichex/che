// Package wizard implementa el flujo "Create pipeline" (H1+).
//
// H3 entrega: pantalla S2 (StepEditor) en mode=create para el primer step,
// sin validator. Tras "ctrl+s" en S2 el wizard cierra (S3 todavia es
// placeholder); tras "esc" volvemos a S1 con status.stage=info.
package wizard

import (
	"time"

	"github.com/chichex/che/internal/repoctx"
	"github.com/chichex/che/internal/skills"
)

// Screen identifica la pantalla actual del wizard. Cada Screen tiene su
// modulo (info.go, cancel.go, ...) que define como se renderiza y reacciona.
type Screen int

const (
	ScreenInfo                 Screen = iota // S1: nombre + descripcion
	ScreenStep                               // S2: editor de step (H3+)
	ScreenSummary                            // S3: resumen + guardar pipeline final (H6+)
	ScreenSaved                              // S4: post-save, mostrar path (H6+)
	ScreenSaveChoice                         // modal "agregar otro / finalizar / volver"
	ScreenCancel                             // SC: modal keep/discard/back
	ScreenCollision                          // modal "el nombre ya existe"
	ScreenDiscardWarn                        // confirmacion extra antes de discard
	ScreenSummaryConfirmDelete               // modal confirm "borrar step" desde S3 (H7)
	ScreenStepReview                         // modal "Review del prompt" (claude analiza el prompt antes de guardar)
)

// FieldFocus marca cual campo de S1 tiene el foco. tab/shift+tab cyclan
// entre los dos.
type FieldFocus int

const (
	FocusName FieldFocus = iota
	FocusDescription
)

// CancelChoice es la opcion seleccionada dentro del modal SC.
type CancelChoice int

const (
	CancelKeep CancelChoice = iota
	CancelDiscard
	CancelBack
)

// CollisionChoice es la opcion seleccionada en el modal de colision de slug.
type CollisionChoice int

const (
	CollisionOverwrite CollisionChoice = iota
	CollisionCancel
)

// SummaryDeleteChoice es la opcion seleccionada en el modal de confirmacion
// de "borrar step" desde S3 (H7). Default seguro = cancelar; el usuario tiene
// que mover el cursor + enter para confirmar.
type SummaryDeleteChoice int

const (
	SummaryDeleteConfirm SummaryDeleteChoice = iota
	SummaryDeleteCancel
)

// SaveChoice es la opcion seleccionada en el modal "ya termine este step".
// Aparece cuando el usuario presiona enter sobre el ultimo foco de S2 —
// reemplaza el "cierre directo" anterior por una eleccion explicita para
// que el enter no cierre el wizard de pecho.
type SaveChoice int

const (
	SaveAddAnother SaveChoice = iota // ctrl+n equivalent
	SaveFinish                       // ctrl+s equivalent — guarda step + va a S3
	SaveBack                         // cancelar el modal, seguir editando
)

// Pipeline es el draft entero. Status nil = pipeline ready (sin bloque
// status en el YAML); Status no nil = draft con marcador de stage.
type Pipeline struct {
	Status      *Status `yaml:"status,omitempty"`
	Name        string  `yaml:"name"`
	Description string  `yaml:"description,omitempty"`
	Steps       []Step  `yaml:"steps,omitempty"`
}

// Status es el bloque opcional que marca donde quedo el wizard. Solo
// aparece en archivos draft.
type Status struct {
	Stage       string    `yaml:"stage"`
	StepIdx     int       `yaml:"step_idx,omitempty"`
	StepMode    string    `yaml:"step_mode,omitempty"`
	LastSavedAt time.Time `yaml:"last_saved_at"`
}

// Stage values del bloque status.
const (
	StageInfo    = "info"
	StageStep    = "step"
	StageSummary = "summary"
)

// Step es una unidad del pipeline. En H2 todavia no se editan, pero el
// struct va completo para que el YAML golden sirva en H3+ sin romper
// backwards-compat.
type Step struct {
	Name       string     `yaml:"name"`
	CLI        string     `yaml:"cli,omitempty"`
	Kind       string     `yaml:"kind,omitempty"`
	Content    string     `yaml:"content,omitempty"`
	Input      string     `yaml:"input,omitempty"`
	Validator  *Validator `yaml:"validator,omitempty"`
	MaxLoops   int        `yaml:"max_loops,omitempty"`
	OnMaxLoops string     `yaml:"on_max_loops,omitempty"`
}

// Validator es el bloque opcional de cross-review de un step.
type Validator struct {
	CLI     string `yaml:"cli"`
	Kind    string `yaml:"kind"`
	Content string `yaml:"content"`
}

// StepFieldFocus indica que campo de S2 tiene el foco.
//
// La lista incluye el toggle del bloque validator (siempre visible) y los
// 5 campos del bloque validator (visibles solo cuando validator on, ver
// B2 en docs/manage-pipelines-flow.html). El orden "vivo" para tab/shift+
// tab se calcula en activeFocusOrder a partir de stepEdit.validatorOn —
// asi el ciclo de foco refleja exactamente lo que el usuario ve, sin
// trampas tipo "skipear este enum si tal flag".
type StepFieldFocus int

const (
	StepFocusName StepFieldFocus = iota
	StepFocusCLI
	StepFocusKind
	StepFocusContent
	StepFocusInput
	// Toggle "¿validar este step?" — siempre visible, esta despues del
	// bloque base.
	StepFocusValToggle
	// Bloque validator (visible solo si validatorOn=true).
	StepFocusValCLI
	StepFocusValKind
	StepFocusValContent
	StepFocusValMaxLoops
	StepFocusValOnMaxLoops
)

// Kind / Input enum values del step. Se mantienen como constantes string
// para que coincidan con la representacion YAML directamente.
const (
	KindPrompt = "prompt"
	KindSkill  = "skill"
)

const (
	InputText           = "text"
	InputPR             = "pr"
	InputIssue          = "issue"
	InputFile           = "file"
	InputURL            = "url"
	InputNone           = "none"
	InputPreviousOutput = "previous_output"
)

// on_max_loops del bloque validator. Default fail (Q cerrado en el flow doc).
const (
	OnMaxLoopsFail     = "fail"
	OnMaxLoopsContinue = "continue"
	OnMaxLoopsPause    = "pause"
)

// Opciones discretas de max_loops del validator. Pills 1..5 con default
// 3. Si el usuario quiere un numero mas grande, el flow doc cubre $EDITOR
// directo sobre el YAML (H8) — no vale la pena exponer un numeric input
// dedicado solo para esto.
var maxLoopsOptions = []int{1, 2, 3, 4, 5}

// Opciones del enum on_max_loops, en el orden en que se renderizan las
// pills.
var onMaxLoopsOptions = []string{OnMaxLoopsFail, OnMaxLoopsContinue, OnMaxLoopsPause}

// stepCLIs es el orden fijo de pills del selector CLI. internal/skills
// devuelve sus entradas en este mismo orden.
var stepCLIs = []string{"claude", "codex", "gemini", "opencode"}

// inputsForStepIdx devuelve las opciones permitidas del enum input segun
// la posicion del step. step 0 no incluye previous_output (no hay step
// previo del cual leer); step 1+ si.
func inputsForStepIdx(idx int) []string {
	base := []string{InputText, InputPR, InputIssue, InputFile, InputURL, InputNone}
	if idx == 0 {
		return base
	}
	return append([]string{InputPreviousOutput}, base...)
}

// inputNeedsRepo reporta si la opcion de input depende de que `gh` reconozca
// el cwd como un repo de github (pr / issue). Se usa para deshabilitar las
// pills cuando repoctx.Detect().InGitHubRepo == false — ofrecer "elegi un PR
// del repo" sin repo actual no tiene sentido y termina rebotando en el
// runner.
func inputNeedsRepo(opt string) bool {
	return opt == InputPR || opt == InputIssue
}

// inputDisabled reporta si la pill `opt` debe rendirse deshabilitada (dim,
// cursor la salta) en el contexto actual del proceso. Hoy solo se aplica a
// pr/issue cuando el cwd no esta dentro de un repo conocido por gh; el resto
// es false. La deteccion se cachea en repoctx, asi llamarlo en el render no
// dispara un fork por keystroke.
func inputDisabled(opt string) bool {
	if !inputNeedsRepo(opt) {
		return false
	}
	return !repoctx.Detect().InGitHubRepo
}

// pipelineNeedsRepo reporta si el pipeline tiene algun step cuyo input
// dependa del repo (pr / issue). El lister + el preflight lo usan para
// chipear / sumar el row "git repo context" sin hardcodear la lista de
// inputs en cada caller.
func PipelineNeedsRepo(p Pipeline) bool {
	for _, st := range p.Steps {
		if inputNeedsRepo(st.Input) {
			return true
		}
	}
	return false
}

// stepEditState es el estado UI especifico de S2. Vive como sub-struct del
// model y se reinicia cada vez que entramos a un step (mode=create) o
// cargamos un step existente (mode=edit).
type stepEditState struct {
	idx  int    // posicion del step dentro del pipeline
	mode string // "create" | "edit"

	focus StepFieldFocus

	// inputs editables. Ambos viven aca para que tab pueda cyclar focus
	// sin perder el contenido tipeado al pasar por otros campos.
	nameInput    textInput
	contentInput textInput // multiline, se usa cuando kind=prompt

	// selecciones discretas
	cli   string // claude | codex | gemini | opencode
	kind  string // prompt | skill
	input string // text | pr | issue | file | url | none | previous_output

	// posicion del cursor dentro del skill picker (cuando kind=skill).
	skillCursor int

	// Bloque validator (B2). validatorOn alterna la visibilidad del bloque
	// entero — los 5 campos siguientes solo se renderizan/cyclan cuando
	// validatorOn=true. valCLI/valKind/valMaxLoops/valOnMaxLoops cargan
	// defaults (mismo CLI que el step, prompt, 3, fail) en enterStepCreate
	// para que el primer toggle on muestre el bloque ya razonable; al
	// togglear off no se borran (el usuario puede prender/apagar sin
	// perder lo tipeado).
	validatorOn     bool
	valCLI          string
	valKind         string
	valContentInput textInput // multiline cuando kind=prompt; ignorado cuando kind=skill
	valSkillCursor  int
	valMaxLoops     int
	valOnMaxLoops   string

	// error inline. Se limpia al tipear.
	errMsg string

	// Estado del modal "Review del prompt" (ScreenStepReview). Se dispara
	// al confirmar el step (ctrl+s / ctrl+n / SaveFinish del modal de
	// save choice) cuando kind=prompt + content no vacio. reviewLoading
	// = true mientras claude corre; al volver, populamos review +
	// reviewErr y queda en loading=false. pendingSaveAction guarda que
	// camino tomar despues del modal: "finish" (ctrl+s) o "addanother"
	// (ctrl+n).
	reviewLoading     bool
	review            promptReviewResult
	reviewErr         string
	pendingSaveAction string // "finish" | "addanother"
}

// promptReviewResult es el snapshot tipado del resultado de la review
// que vive en stepEditState. Lo separamos del package promptreview.Review
// para no cargar el import en model.go (los handlers del modal lo
// llenan via type assertion).
type promptReviewResult struct {
	OK        bool
	Issues    []string
	Summary   string
	Suggested string
	Raw       string
}

// model es la unica struct viva en el bubbletea program. Cada screen lee
// y escribe sobre la misma instancia; al salir por SC keep/discard se
// finaliza la persistencia y se cierra el program.
type model struct {
	screen Screen

	// pipeline en construccion
	pipeline Pipeline

	// path absoluto del archivo en disco. "" hasta que S1 setea name por
	// primera vez. A partir de ahi, save sincronico antes de cada
	// transicion lo mantiene actualizado.
	path string

	// foco dentro de S1
	focus FieldFocus

	// buffers de los inputs de S1. Se materializan a pipeline.Name /
	// pipeline.Description al validar.
	nameInput textInput
	descInput textInput

	// estado UI de S2 (vacio mientras estemos en S1).
	stepEdit stepEditState

	// CLIs detectados (lazy: solo se llena al entrar a S2 por primera
	// vez). Se reusa para B4 (CLI no instalado) y para el skill picker
	// de B3.
	skillsCache    []skills.CLI
	skillsCacheSet bool

	// estado del modal SC
	cancelCursor CancelChoice
	// pantalla a la que volvemos si el usuario elige "back" en SC.
	cancelReturn Screen

	// estado del modal de colision
	collisionCursor CollisionChoice

	// estado del modal "ya termine el step" (enter en ultimo foco de S2).
	saveCursor SaveChoice

	// cursor de S3: indice del step apuntado por ↑/↓. H7 lo usa para
	// e/d/shift+↑↓/+. Se reinicia a 0 al entrar a S3 si quedo fuera de
	// rango (delete que dejo al cursor mas alla del ultimo step).
	summaryCursor int

	// cursor del modal de confirmacion de "borrar step" (H7). Default
	// seguro = SummaryDeleteCancel, seteado por openSummaryDelete.
	summaryDelCursor SummaryDeleteChoice

	// errores de IsValid mostrados en S3 cuando ctrl+s falla. Se popula
	// entrando con un error multi-linea; queda visible hasta el proximo
	// ctrl+s.
	summaryErrs []string

	// error inline (S1: nombre vacio, slug invalido, write failed)
	errMsg string

	// HomeDir a usar. "" significa $HOME real; los tests inyectan tmp dir.
	homeDir string

	// originalReadySnapshot guarda los bytes del archivo ready original
	// cuando entramos al wizard via RunEditReady. Si el usuario elige
	// "discard" en el modal SC, restauramos el archivo desde este
	// snapshot en lugar de borrarlo — "discard" en edit-ready = "tirar
	// mis cambios y volver al estado original", no "borrar el pipeline".
	// nil cuando entramos por cualquier otro flow (Run / RunResume).
	originalReadySnapshot []byte

	// exitApp = true si el wizard pidio salida total al menu (q / ctrl+c
	// confirmados). false significa "volver al menu principal".
	exitApp bool

	// width es el ancho del terminal en columnas, refrescado via
	// tea.WindowSizeMsg. Se usa para wrappear inputs de texto largos
	// (prompt del step) y evitar overflow horizontal.
	width int
}

// installedCLIs returns the names of the detected CLIs that are installed.
// Used by S2 to disable pills (B4) — no installed cli == cannot save.
func (m *model) installedCLIs() []string {
	if !m.skillsCacheSet {
		return nil
	}
	out := make([]string, 0, len(m.skillsCache))
	for _, c := range m.skillsCache {
		if c.Installed {
			out = append(out, c.Name)
		}
	}
	return out
}

// skillsForCLI looks up the detected skills for a given cli name. Returns
// nil if the cli isn't installed or wasn't detected.
func (m *model) skillsForCLI(name string) []skills.Skill {
	for _, c := range m.skillsCache {
		if c.Name == name {
			return c.Skills
		}
	}
	return nil
}

// isInstalled reports whether the given cli appears as installed in the
// detection cache. Returns false if the cache hasn't been loaded yet.
func (m *model) isInstalled(name string) bool {
	for _, c := range m.skillsCache {
		if c.Name == name {
			return c.Installed
		}
	}
	return false
}
