package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Init es no-op: el wizard arranca renderizando con el modelo inicial.
func (m model) Init() tea.Cmd { return nil }

// Update dispatchea al handler de la screen actual. Las transiciones
// entre screens se hacen en cada handler escribiendo m.screen.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// editorFinishedMsg llega tras tea.ExecProcess (H8: `y` en S3 abre
	// $EDITOR). Lo handleamos antes del switch de teclas porque no es un
	// KeyMsg — viene del subproceso del editor al terminar.
	if em, ok := msg.(editorFinishedMsg); ok {
		return m.handleEditorReturn(em)
	}

	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		return m, nil
	}

	if pr, ok := msg.(promptReviewMsg); ok {
		// Solo aplicar si seguimos en el modal — si el usuario lo cancelo
		// con esc mientras claude corria, ignoramos el resultado tardio.
		if m.screen == ScreenStepReview {
			mm, cmd := m.handlePromptReviewResult(pr)
			return mm, cmd
		}
		return m, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch m.screen {
	case ScreenInfo:
		return m.updateInfo(key)
	case ScreenScope:
		return m.updateScope(key)
	case ScreenStep:
		return m.updateStep(key)
	case ScreenStepReview:
		return m.updateStepReview(key)
	case ScreenSummary:
		return m.updateSummary(key)
	case ScreenSaved:
		return m.updateSaved(key)
	case ScreenSaveChoice:
		return m.updateSaveChoice(key)
	case ScreenCancel:
		return m.updateCancel(key)
	case ScreenCollision:
		return m.updateCollision(key)
	case ScreenSummaryConfirmDelete:
		return m.updateSummaryDelete(key)
	}
	return m, nil
}

// View dispatchea al renderer de la screen actual.
func (m model) View() string {
	switch m.screen {
	case ScreenInfo:
		return m.viewInfo()
	case ScreenScope:
		return m.viewScope()
	case ScreenStep:
		return m.viewStep()
	case ScreenStepReview:
		return m.viewStepReview()
	case ScreenSummary:
		return m.viewSummary()
	case ScreenSaved:
		return m.viewSaved()
	case ScreenSaveChoice:
		return m.viewSaveChoice()
	case ScreenCancel:
		return m.viewCancel()
	case ScreenCollision:
		return m.viewCollision()
	case ScreenSummaryConfirmDelete:
		return m.viewSummaryDelete()
	}
	return ""
}

// newModel construye el modelo inicial con el HOME indicado. home=""
// significa "usar $HOME real"; los tests inyectan tmp dir. El cwd se
// resuelve via os.Getwd() — si falla queda vacio y el scope project
// queda deshabilitado en la pantalla S1.5.
func newModel(home string) model {
	cwd, _ := os.Getwd()
	return newModelWithDirs(home, cwd)
}

// newModelWithDirs es el entrypoint testeable que permite inyectar
// tanto home como cwd. El scope default es global para preservar
// back-compat (usuarios existentes que no eligen nada caen al scope
// previo).
func newModelWithDirs(home, cwd string) model {
	return model{
		screen:    ScreenInfo,
		focus:     FocusName,
		nameInput: newSingleLine("ej: Triage checkout flow"),
		descInput: newMultiLine("ej: toma una metrica anomala y dispara un triage"),
		homeDir:   home,
		cwd:       cwd,
		scope:     WizardScopeGlobal,
	}
}

// Run levanta el wizard. Devuelve exitApp=true si el usuario pidio salida
// total (q / ctrl+c en placeholder, o "discard"/"keep" + ctrl+c en SC);
// false si el flujo termino "volviendo al menu". El error solo aparece
// si bubbletea no pudo arrancar (p.ej. stdout no es TTY).
func Run() (bool, error) {
	return runWithHome("")
}

// runWithHome es el entrypoint testeable: permite forzar HomeDir desde
// los tests sin tocar $HOME del proceso.
func runWithHome(home string) (bool, error) {
	return runProgram(newModel(home))
}

// runProgram es el wrapper minimal sobre tea.NewProgram que extrae exitApp
// del modelo final. Lo usan Run / RunResume / RunEditReady para no duplicar
// la logica del cast + fallback.
func runProgram(m model) (bool, error) {
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return false, err
	}
	mm, ok := final.(model)
	if !ok {
		return true, nil
	}
	return mm.exitApp, nil
}

// RunResume reanuda un draft existente (status != nil). Usa $HOME real;
// los tests pueden inyectar HOME via env var (HomeDir lee os.UserHomeDir).
func RunResume(path string) (bool, error) {
	return runResumeWithHome("", path)
}

// runResumeWithHome construye un model pre-cargado desde el archivo y arranca
// el wizard en la screen indicada por status.stage. Status ausente o
// step_idx fuera de rango caen a S1 con un warning visible (m.errMsg) — el
// usuario sigue desde S1 con name+desc cargados, lo demas se reconstruye.
func runResumeWithHome(home, path string) (bool, error) {
	m := newModel(home)
	m.path = path
	m.scope = scopeFromPath(path, m.cwd, home)
	p, err := Load(path)
	if err != nil {
		// Fallback duro: archivo ilegible. Arrancamos en S1 sin path para
		// no pisarlo accidentalmente — el usuario decide si guarda con un
		// nombre nuevo o cancela.
		m.path = ""
		m.errMsg = "no se pudo cargar el pipeline (" + err.Error() + ") — arrancando desde S1"
		return runProgram(m)
	}
	m.pipeline = p
	if p.Name != "" {
		m.nameInput.SetValue(p.Name)
	}
	if p.Description != "" {
		m.descInput.SetValue(p.Description)
	}

	if p.Status == nil {
		// Llamar a RunResume sobre un ready es responsabilidad del caller
		// (en general usa RunEditReady). Si llegamos aca asumimos draft
		// corrupto: fallback S1 con warning.
		m.errMsg = "el archivo no tiene status — arrancando desde S1"
		return runProgram(m)
	}

	stage := p.Status.Stage
	stepIdx := p.Status.StepIdx
	switch stage {
	case StageInfo:
		m.screen = ScreenInfo
		return runProgram(m)
	case StageStep:
		// stepIdx valido: [0, len(Steps)] — len(Steps) corresponde a un
		// step nuevo que el usuario estaba creando (mode=create idx ==
		// len). Cualquier cosa fuera de [0, len] es corrupcion.
		if stepIdx < 0 || stepIdx > len(p.Steps) {
			m.errMsg = fmt.Sprintf("step_idx %d fuera de rango — arrancando desde S1", stepIdx)
			return runProgram(m)
		}
		if p.Status.StepMode == "edit" && stepIdx < len(p.Steps) {
			m, _ = m.enterStepEdit(stepIdx)
		} else {
			// step_mode=create o vacio: enterStepCreate funciona tanto
			// para idx existente (lo trata como nuevo encima del idx) como
			// para idx == len (append).
			m, _ = m.enterStepCreate(stepIdx)
		}
		return runProgram(m)
	case StageSummary:
		if len(p.Steps) == 0 {
			m.errMsg = "summary sin steps — arrancando desde S1"
			return runProgram(m)
		}
		m.summaryCursor = 0
		m.screen = ScreenSummary
		return runProgram(m)
	default:
		m.errMsg = "stage desconocido (" + stage + ") — arrancando desde S1"
		return runProgram(m)
	}
}

// RunEditReady toma un pipeline ready (status nil), siembra status.stage=
// summary EN RAM, y arranca el wizard en S3 mode=edit. NO persiste el
// status al disco — si el usuario no toca nada y sale con SC keep el
// archivo sigue ready. La primera mutacion dentro del wizard (e/d/+/
// shift+↑↓/y en S3, o cualquier ctrl+s/ctrl+n en S2 al editar un step)
// persiste como draft via los Save() habituales; ctrl+s en S3 lo vuelve
// a ready strippeando el status.
func RunEditReady(path string) (bool, error) {
	return runEditReadyWithHome("", path)
}

// scopeFromPath infiere el WizardScope mirando el path: si esta bajo
// `<cwd>/.che/pipelines/` es project; cualquier otra cosa cae a global.
// Sirve para que RunResume / RunEditReady levanten el scope correcto al
// rehidratar un draft existente sin pasar por ScreenScope.
func scopeFromPath(path, cwd, _ string) WizardScope {
	if cwd == "" || path == "" {
		return WizardScopeGlobal
	}
	projectDir, err := filepath.Abs(filepath.Join(cwd, projectPipelinesSubdir))
	if err != nil {
		return WizardScopeGlobal
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return WizardScopeGlobal
	}
	rel, err := filepath.Rel(projectDir, abs)
	if err != nil {
		return WizardScopeGlobal
	}
	// rel sin ".." significa que el path vive dentro del project dir.
	if rel != "" && !strings.HasPrefix(rel, "..") && rel != "." {
		return WizardScopeProject
	}
	return WizardScopeGlobal
}

func runEditReadyWithHome(home, path string) (bool, error) {
	p, err := Load(path)
	if err != nil {
		// fallback: arrancamos un wizard limpio con el aviso.
		m := newModel(home)
		m.errMsg = "no se pudo cargar el pipeline (" + err.Error() + ") — arrancando desde S1"
		return runProgram(m)
	}
	if len(p.Steps) == 0 {
		// ready sin steps no deberia existir (IsValid lo rechaza al
		// finalizar), pero por si acaso: caemos a S1 con el name cargado.
		m := newModel(home)
		m.path = path
		m.pipeline = p
		if p.Name != "" {
			m.nameInput.SetValue(p.Name)
		}
		if p.Description != "" {
			m.descInput.SetValue(p.Description)
		}
		m.errMsg = "el pipeline ready no tiene steps — arrancando desde S1"
		return runProgram(m)
	}
	// Snapshot del archivo ready en disco ANTES de seedearle status —
	// sirve para que SC discard restaure el archivo byte-a-byte en vez de
	// borrarlo. Leemos los bytes crudos en vez de Marshal(p) para
	// preservar formato (indent / comentarios) si el usuario edito el
	// YAML a mano antes de abrir desde el lister.
	origSnapshot, sErr := os.ReadFile(path)
	if sErr != nil {
		m := newModel(home)
		m.errMsg = "no se pudo preparar edit-ready (" + sErr.Error() + ") — arrancando desde S1"
		return runProgram(m)
	}
	// Status en RAM solamente: el archivo en disco sigue ready hasta que
	// haya un cambio real. Sin esto, abrir + esc + keep convertia un ready
	// en draft sin que el usuario hubiese tocado nada — bug confuso para
	// el flujo "miro el resumen y salgo".
	p.Status = &Status{
		Stage:       StageSummary,
		LastSavedAt: time.Now(),
	}
	m := newModel(home)
	m.path = path
	m.scope = scopeFromPath(path, m.cwd, home)
	m.pipeline = p
	m.originalReadySnapshot = origSnapshot
	if p.Name != "" {
		m.nameInput.SetValue(p.Name)
	}
	if p.Description != "" {
		m.descInput.SetValue(p.Description)
	}
	m.summaryCursor = 0
	m.screen = ScreenSummary
	return runProgram(m)
}
