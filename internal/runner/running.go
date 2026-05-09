package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/wizard"
)

// runDirRootFn devuelve ~/.che/runs (o un override). Variable para que los
// tests unitarios puedan apuntar a un tmp sin tocar HOME global.
var runDirRootFn = defaultRunDirRoot

func defaultRunDirRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "che-runs")
	}
	return filepath.Join(home, ".che", "runs")
}

// nowFn es swappable por tests para forzar un RunID determinista. Default
// = time.Now() (ver makeRunID).
var nowFn = time.Now

// makeRunID formatea el timestamp UTC como "2006-01-02T15-04-05" (sortable,
// filename-safe sin colons). Si el dir ya existe (dos runs en el mismo
// segundo) suffijamos -2, -3, ... — el doc lo deja explicito como
// "edge case".
func makeRunID(slugDir string, base time.Time) (string, string) {
	id := base.UTC().Format("2006-01-02T15-04-05")
	candidate := filepath.Join(slugDir, id)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return id, candidate
	}
	for i := 2; i < 1000; i++ {
		alt := fmt.Sprintf("%s-%d", id, i)
		candidate = filepath.Join(slugDir, alt)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return alt, candidate
		}
	}
	// Fallback ultra-defensivo: si por alguna razon hay 1000 runs en un
	// segundo (no debería), usamos un id con nanos para garantizar unico.
	id = base.UTC().Format("2006-01-02T15-04-05.000000000")
	return id, filepath.Join(slugDir, id)
}

// initRunDir crea ~/.che/runs/<slug>/<run-id>/ con permisos 0700 y devuelve
// (runID, runDir, error). H8 dispara el GC del slug-dir ANTES de crear el
// nuevo run-dir: asi el cap de retencion se aplica sobre el snapshot
// historico (sin contar el run que estamos por crear). El doc fija
// `CHE_RUN_HISTORY` (default 10) como cap; con 15 dirs preexistentes y
// cap=10 quedan 10 + 1 nuevo = 11.
//
// El GC es best-effort: errores de read/remove no abortan el run (un run
// fresco con un GC roto sigue siendo mejor que abortar; el doc lo lista
// como criterio de aceptacion solo para "no se acumulen").
func initRunDir(p wizard.Pipeline) (string, string, error) {
	slug := wizard.Slug(p.Name)
	if slug == "" {
		slug = "pipeline"
	}
	root := runDirRootFn()
	slugDir := filepath.Join(root, slug)
	if err := os.MkdirAll(slugDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", slugDir, err)
	}
	// GC del slug-dir antes de crear el nuevo run-dir (H8). best-effort.
	_ = gcRunHistory(slugDir, runHistoryCap())
	id, runDir := makeRunID(slugDir, nowFn())
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	return id, runDir, nil
}

// enterRunning es la transicion R2 → R3 (post-preflight ok / warn confirmado).
// Inicializa el run dir + manifest, prepara el slice de Steps con TODOS los
// steps del pipeline en estado pending, y delega a startStep para arrancar
// el primero. H6 reemplaza el "single-step" de H4 por multi-step en serie:
// tras handleStepDone, si el step termino done y hay siguiente, el handler
// llama a startStep(idx+1) y resuelve el payload via previous_output.
func enterRunning(m RunModel) (RunModel, tea.Cmd) {
	m.Screen = ScreenRunning
	m.CancelModal = false
	m.LogDump = ""
	m.FailedStderr = ""
	// MultiStepWarning ya no aplica post-H6 — multi-step esta soportado.
	// Lo dejamos en false explicitamente por si una transicion previa lo
	// dejo true.
	m.MultiStepWarning = false

	id, runDir, err := initRunDir(m.Pipeline)
	if err != nil {
		// Sin run dir no podemos escribir manifest ni logs — caemos a RF
		// con el error como SpawnError del step 0 (defensivo).
		m.Screen = ScreenFailed
		m.FailedStderr = err.Error()
		m.Steps = []StepRun{{Idx: 1, Name: "init", Status: StepStatusFailed, ExitCode: -1, SpawnError: err.Error()}}
		return m, nil
	}
	m.RunID = id
	m.RunDir = runDir
	// H8: RunStartedAt fija el started_at del manifest a nivel run. Lo
	// usamos en TODOS los snapshots posteriores para que el header del
	// manifest sea consistente entre updates intermedios y el cierre — sin
	// esto, los snapshots usaban step.StartedAt y pisaban el original con
	// cada step, complicando la heuristica de recovery (que mira
	// started_at).
	m.RunStartedAt = time.Now().UTC()

	// Steps slice: H6 inicializa TODOS los steps del pipeline en pending +
	// crea el ring buffer correspondiente para cada uno (LogBuffers idx
	// alineado con Steps idx). Idx empieza en 1 (1-indexed para alinear
	// con los nombres de archivo step-01.*).
	stepRuns := make([]StepRun, 0, len(m.Pipeline.Steps))
	buffers := make([]*RingBuffer, 0, len(m.Pipeline.Steps))
	for i, ps := range m.Pipeline.Steps {
		stepRuns = append(stepRuns, StepRun{
			Idx:    i + 1,
			Name:   ps.Name,
			CLI:    ps.CLI,
			Kind:   ps.Kind,
			Status: StepStatusPending,
		})
		buffers = append(buffers, NewRingBuffer(2000))
	}
	m.Steps = stepRuns
	m.LogBuffers = buffers
	m.Active = 0
	m.LogFocus = 0
	m.StickyBottom = true
	m.LogScrollOffset = 0

	if _, err := initManifest(m.Pipeline, id, runDir, m.path, m.Input.Kind, m.Input.Value, m.RunStartedAt, stepRuns); err != nil {
		m.Screen = ScreenFailed
		m.FailedStderr = err.Error()
		m.Steps[0].Status = StepStatusFailed
		m.Steps[0].SpawnError = err.Error()
		return m, nil
	}

	return startStep(m, 0)
}

// startStep arranca el subprocess del step en idx (0-based). Resuelve el
// payload segun el input del step (R1 para el step 0, previous_output para
// idx ≥ 1) y emite el spawn. Si la resolucion falla, marca el step como
// failed con SpawnError + transiciona a RF (no podemos arrancar el step sin
// su input). Devuelve el (model, cmd) listo para que el caller (enterRunning
// o handleStepDone) lo encadene.
func startStep(m RunModel, idx int) (RunModel, tea.Cmd) {
	if idx < 0 || idx >= len(m.Pipeline.Steps) {
		// Defensive: idx fuera de rango — no deberia pasar (handleStepDone
		// solo invoca cuando hay siguiente). Lo tratamos como done para
		// caer al render terminal.
		m.Screen = ScreenDone
		return m, nil
	}
	step := m.Pipeline.Steps[idx]

	payload, perr := resolvePayloadForStep(m, idx)
	if perr != nil {
		// Sin payload no podemos spawnear — marcamos el step como failed +
		// vamos a RF directo. Manifest se cierra con failed para que el
		// estado en disco refleje el problema.
		m.Steps[idx].Status = StepStatusFailed
		m.Steps[idx].StartedAt = time.Now()
		m.Steps[idx].FinishedAt = time.Now()
		m.Steps[idx].ExitCode = -1
		m.Steps[idx].SpawnError = perr.Error()
		m.Active = idx
		m.LogFocus = idx
		_ = closeManifest(m.RunDir, Manifest{
			RunID:        m.RunID,
			Pipeline:     m.Pipeline.Name,
			StartedAt:    time.Now().UTC(),
			PipelinePath: m.path,
			InputKind:    m.Input.Kind,
			InputValue:   m.Input.Value,
		}, ManifestStatusFailed, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		m.FailedStderr = perr.Error()
		return m, nil
	}

	// Marcar el step activo + ponerlo en running antes de spawnear (el
	// tracker refleja "⏳ running" mientras el subprocess vive).
	m.Active = idx
	m.LogFocus = idx
	m.StickyBottom = true
	m.LogScrollOffset = 0
	m.Steps[idx].Status = StepStatusRunning
	m.Steps[idx].StartedAt = time.Now()

	// H8: snapshot atomico del manifest al ARRANCAR el step (start de cada
	// step, segun los criterios de aceptacion). Asi si el proceso muere
	// mientras el subprocess corre, el manifest refleja steps[idx].status:
	// running con started_at populado — la recovery del lister lo va a
	// reescribir a interrupted post-1h.
	_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, m.Steps[idx].StartedAt))

	// runState fresco por step (cancel + lineCh exclusivos del subprocess
	// actual). Sobreescribe cualquier remanente del step anterior — ese ya
	// fue limpiado por handleStepDone.
	m.runState = &runState{requestCancel: make(chan struct{}, 1)}
	cmd := runStep(step, payload, m.RunDir, idx+1, m.runState)
	return m, cmd
}

// resolvePayloadForStep devuelve el payload (stdin del subprocess) segun el
// input del step. Para step 0 es el ResolvedPayload de R1 (lo mismo que H4
// hacia ya). Para step ≥ 1:
//   - input == previous_output → leer step-(N-1).result.yaml/output.
//   - cualquier otro kind → defensive: el wizard fuerza previous_output como
//     default para steps ≥ 1; si llegara otro, reusamos el ResolvedPayload
//     de R1 (raro pero no fatal segun el doc).
func resolvePayloadForStep(m RunModel, idx int) (string, error) {
	if idx == 0 {
		return m.Input.ResolvedPayload, nil
	}
	step := m.Pipeline.Steps[idx]
	if step.Input != wizard.InputPreviousOutput {
		// Defensive: no deberia pasar — wizard.IsValid + el flow de R1
		// solo dejan previous_output (o uno equivalente "consumido") para
		// steps ≥ 1. Caemos al payload original de R1 para no abortar.
		return m.Input.ResolvedPayload, nil
	}
	prev, err := readStepResult(m.RunDir, idx) // idx 1-based para el archivo (step-NN)
	if err != nil {
		return "", fmt.Errorf("previous_output del step %d: %w", idx+1, err)
	}
	return prev.Output, nil
}

// updateRunning maneja teclas + msgs durante R3. Las transiciones:
//
//   - ctrl+c       → abre RC (cancel modal).
//   - ↑/↓ / k/j    → scroll del log pane (desactiva sticky).
//   - g            → re-stick al fondo + reset scroll.
//   - ctrl+l       → limpia el ring buffer del step activo (no toca disco).
//   - tab          → cyclea LogFocus entre los steps del pipeline. El log
//     pane pasa a renderear los logs del step seleccionado
//     (sin frenar el subprocess en curso). H6 lo agrega
//     porque multi-step necesita ver logs de steps ya
//     terminados o pendientes.
//
// stepLineMsg appendea al ring buffer del step indicado y vuelve a issuear
// waitForLine. stepDoneMsg cierra el step y, segun el resultado:
//   - done + hay siguiente → arranca el step siguiente (startStep).
//   - done + ultimo step   → R4.
//   - failed / cancelled   → RF inmediato (stop-on-error).
//
// validatorDoneMsg (H7) lo emite la goroutine del validator subprocess al
// terminar — handleValidatorDone decide la transicion (siguiente step /
// re-run del step / RF / RP modal) segun verdict + on_max_loops.
func (m RunModel) updateRunning(msg tea.Msg) (tea.Model, tea.Cmd) {
	// PauseModal tiene prioridad sobre cancel — mientras hay una pausa
	// activa, el modal RP se queda abierto hasta que el humano elija. RC
	// y RP son mutuamente exclusivos por construccion (no se abre uno
	// mientras el otro esta visible).
	if m.PauseModal != nil {
		return m.updatePauseModal(msg)
	}
	if m.CancelModal {
		return m.updateCancelModal(msg)
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			// Abre RC en vez de salir directo. El doc lo deja explicito:
			// "ctrl+c en R3 (siempre) — el run no es salida accidental".
			m.CancelModal = true
			m.CancelChoice = CancelChoiceAbort
			return m, nil
		case "up", "k":
			// Desactivar sticky + scrollear arriba 1 linea. El cap del
			// scroll se mide al render (clamp segun visible/total).
			m.StickyBottom = false
			m.LogScrollOffset++
			return m, nil
		case "down", "j":
			// Scrollear abajo. Si llegamos al fondo, re-activamos sticky.
			if m.LogScrollOffset > 0 {
				m.LogScrollOffset--
			}
			if m.LogScrollOffset == 0 {
				m.StickyBottom = true
			}
			return m, nil
		case "g":
			// Re-sticky al fondo.
			m.StickyBottom = true
			m.LogScrollOffset = 0
			return m, nil
		case "ctrl+l":
			// Limpiar viewport (ring buffer del step activo). Disco intacto.
			if rb := m.activeBuffer(); rb != nil {
				rb.Clear()
			}
			m.LogScrollOffset = 0
			m.StickyBottom = true
			return m, nil
		case "tab":
			// Cycle LogFocus entre 0..N-1. Solo aplica si hay >1 step
			// (con 1 solo el cycle es no-op y evita un re-render inutil).
			if len(m.Pipeline.Steps) > 1 {
				m.LogFocus = (m.LogFocus + 1) % len(m.Pipeline.Steps)
				m.LogScrollOffset = 0
				m.StickyBottom = true
			}
			return m, nil
		}
		return m, nil
	case stepLineMsg:
		// Append al ring buffer del step indicado (H5: validamos contra
		// LogBuffers; idx 1-based). Si por alguna razon el buffer no
		// existe (race, defensive), lo creamos al vuelo.
		bufIdx := msg.Idx - 1
		if bufIdx < 0 {
			bufIdx = 0
		}
		for len(m.LogBuffers) <= bufIdx {
			m.LogBuffers = append(m.LogBuffers, NewRingBuffer(2000))
		}
		kind := LogLineStdout
		if msg.Line.Stderr {
			kind = LogLineStderr
		}
		m.LogBuffers[bufIdx].Append(kind, msg.Line.Text)
		// Re-issuear el wait para drenar la siguiente linea.
		if m.runState != nil {
			return m, waitForLine(m.runState.lineCh)
		}
		return m, nil
	case stepDoneMsg:
		return m.handleStepDone(msg)
	case validatorDoneMsg:
		return m.handleValidatorDone(msg)
	}
	return m, nil
}

// activeBuffer devuelve el ring buffer del step en LogFocus, o nil si no
// existe. Defensive — el render lo usa para no panic-ear si el slice
// quedo desfasado.
func (m RunModel) activeBuffer() *RingBuffer {
	if m.LogFocus < 0 || m.LogFocus >= len(m.LogBuffers) {
		return nil
	}
	return m.LogBuffers[m.LogFocus]
}

// handleStepDone procesa el mensaje de fin del subprocess del step. Escribe
// el result.yaml del step + actualiza el manifest, y decide la transicion:
//
//   - failed / cancelled       → RF inmediato (stop-on-error de H6).
//   - done + validator nil     → H6 path: avanza al siguiente step (o R4 si
//     era el ultimo).
//   - done + validator declared → H7 path: spawnea el validator (loop K=
//     LoopsRun+1) y espera validatorDoneMsg.
//
// Manifest se reescribe en cada step (no atomico hasta H8) para que un crash
// mid-pipeline deje el estado en disco coherente con lo ya ejecutado.
func (m RunModel) handleStepDone(msg stepDoneMsg) (tea.Model, tea.Cmd) {
	// Update del slice de Steps con el resultado.
	idx := msg.Idx - 1
	if idx < 0 || idx >= len(m.Steps) {
		idx = m.Active
	}
	step := &m.Steps[idx]
	step.FinishedAt = msg.EndedAt
	step.ExitCode = msg.ExitCode
	step.SpawnError = msg.SpawnErr

	switch {
	case msg.Cancelled:
		step.Status = StepStatusCancelled
	case msg.SpawnErr != "" || msg.ExitCode != 0:
		step.Status = StepStatusFailed
	default:
		step.Status = StepStatusDone
	}

	// LogDump para el render terminal — concat stdout+stderr resumido del
	// step que acaba de terminar. Para multi-step done, se va sobreescribiendo
	// en cada step; el render terminal R4 lo usa para el bloque "ultimas
	// lineas" del ultimo step ejecutado.
	m.LogDump = msg.Stdout
	if msg.Stderr != "" {
		// stderr indented + prefix `! ` para distinguirlo en R4/RF.
		m.LogDump += "\n" + indentStderr(msg.Stderr)
	}

	// result.yaml siempre se escribe — el doc fija que `output` queda con
	// lo que haya en stdout incluso en exit ≠ 0. H6 lo necesita ademas
	// para resolver previous_output del step siguiente.
	_ = writeStepResult(m.RunDir, StepResult{
		StepIdx:  step.Idx,
		StepName: step.Name,
		ExitCode: msg.ExitCode,
		Output:   msg.Stdout,
	})

	// Limpieza del runState — el handle ya no es vivo.
	m.runState = nil

	// Decision de transicion. Stop-on-error: cualquier failed / cancelled
	// corta el pipeline; los steps subsiguientes nunca se inician y quedan
	// en pending en el manifest.
	switch step.Status {
	case StepStatusFailed:
		m.FailedStderr = msg.Stderr
		if step.SpawnError != "" && m.FailedStderr == "" {
			m.FailedStderr = step.SpawnError
		}
		// Stop-on-error: solo persistimos en el manifest los steps que
		// efectivamente arrancaron (m.Steps[:idx+1]). Los siguientes nunca
		// se ejecutan y el doc explicita en el test e2e de H6 que NO
		// aparecen en el array — el run dir no tiene step-NN.* files
		// para ellos tampoco.
		_ = closeManifest(m.RunDir, baseManifestForRun(m, msg.StartedAt), ManifestStatusFailed, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		return m, nil
	case StepStatusCancelled:
		if m.FailedStderr == "" {
			m.FailedStderr = "run cancelado por el usuario"
		}
		// Mismo criterio que failed: solo los steps que llegaron a correr.
		_ = closeManifest(m.RunDir, baseManifestForRun(m, msg.StartedAt), ManifestStatusCancelled, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		return m, nil
	}

	// H7: si el step tiene bloque validator, dejamos el step done pero NO
	// avanzamos — spawneamos el validator en su proximo loop.
	// handleValidatorDone (al recibir validatorDoneMsg) decide si el step
	// se considera validado (ok → siguiente step) o si necesita re-correr
	// (fail + loops < max). El step queda en status done en el manifest
	// hasta que el loop se resuelva (es lo que el doc fija — el
	// subprocess del step si termino, lo que sigue es la auditoria).
	//
	// IMPORTANTE: el ValidatorRun se inicializa SOLO la primera vez (cuando
	// es nil). En las re-vueltas del step (rerunStepWithFeedback) volvemos
	// aca con el mismo ValidatorRun ya poblado — startValidator incrementa
	// LoopsRun para reflejar la nueva vuelta. Si reseteasemos el bloque
	// aca, el contador nunca avanzaria y el loop seria infinito.
	pipelineStep := m.Pipeline.Steps[idx]
	if pipelineStep.Validator != nil {
		if m.Steps[idx].Validator == nil {
			m.Steps[idx].Validator = newValidatorRun(pipelineStep)
		}
		_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, msg.StartedAt))
		return startValidator(m, idx, msg.Stdout)
	}

	return m.advanceAfterValidator(idx, msg.StartedAt)
}

// advanceAfterValidator es el path comun de "step OK, listo para avanzar".
// Lo invoca handleStepDone cuando el step no tiene validator y
// handleValidatorDone (o resolvePauseChoice) cuando el validator termino
// con ok / human-override / fail-but-continued. fallbackStart es el
// timestamp del primer step.StartedAt (para el header del manifest).
func (m RunModel) advanceAfterValidator(idx int, fallbackStart time.Time) (RunModel, tea.Cmd) {
	nextIdx := idx + 1
	if nextIdx < len(m.Pipeline.Steps) {
		// Persistimos el manifest "en progreso" con el step recien cerrado
		// + los pendientes intactos antes de spawnear el proximo. Asi un
		// crash entre dos steps deja el manifest reflejando lo real.
		_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, fallbackStart))
		return startStep(m, nextIdx)
	}

	// Ultimo step done → cerrar manifest done + transicionar a R4.
	_ = closeManifest(m.RunDir, baseManifestForRun(m, fallbackStart), ManifestStatusDone, m.Steps)
	m.Screen = ScreenDone
	return m, nil
}

// newValidatorRun arma el ValidatorRun inicial al detectar que el step
// declaro validator. MaxLoops cae a 1 si el yaml no lo define (defensive
// — el wizard fuerza 1..5 con default 3 pero un yaml editado a mano
// podria omitir el campo). OnMaxLoops cae a "fail" por el mismo motivo.
func newValidatorRun(step wizard.Step) *ValidatorRun {
	max := step.MaxLoops
	if max <= 0 {
		max = 1
	}
	on := step.OnMaxLoops
	if on == "" {
		on = wizard.OnMaxLoopsFail
	}
	return &ValidatorRun{
		CLI:        step.Validator.CLI,
		MaxLoops:   max,
		OnMaxLoops: on,
	}
}

// startValidator dispara el subprocess del validator del step idx (0-based)
// con stepStdout como input (criterio del doc: "spawn validator con el
// output del step + un preambulo que pide bloque YAML verdict"). Marca
// ValidatorActive=true (el render muestra "loop K/max" en el row del step)
// y devuelve el (model, cmd) listo para que el program drene el lineCh
// hasta el validatorDoneMsg.
func startValidator(m RunModel, idx int, stepStdout string) (RunModel, tea.Cmd) {
	step := m.Pipeline.Steps[idx]
	if step.Validator == nil || m.Steps[idx].Validator == nil {
		// Defensive: handleStepDone deberia haberlo prevenido. Si llega
		// igual, caemos al advance "limpio".
		return m.advanceAfterValidator(idx, m.Steps[idx].StartedAt)
	}
	m.Steps[idx].Validator.LoopsRun++
	loop := m.Steps[idx].Validator.LoopsRun

	m.Active = idx
	m.LogFocus = idx
	m.ValidatorActive = true

	// H8: snapshot atomico al arrancar el validator loop (start de cada
	// validator loop, segun criterio de aceptacion). El bloque
	// validator.loops_run del manifest refleja la nueva vuelta apenas
	// arranca.
	_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, m.Steps[idx].StartedAt))

	m.runState = &runState{requestCancel: make(chan struct{}, 1)}
	cmd := runValidator(step, stepStdout, m.RunDir, idx+1, loop, m.runState)
	return m, cmd
}

// handleValidatorDone procesa el msg de fin del subprocess del validator.
// Escribe el verdict.yaml + actualiza el ValidatorRun del step, y decide
// la siguiente transicion segun verdict.Status + on_max_loops:
//
//   - verdict ok                              → final_verdict=ok, advanceAfterValidator.
//   - verdict fail + loops < max              → re-spawn del step con feedback.
//   - hit max + on_max_loops=fail             → final_verdict=fail, RF.
//   - hit max + on_max_loops=continue         → final_verdict=fail-but-continued, advance.
//   - hit max + on_max_loops=pause            → modal RP, espera decision humana.
func (m RunModel) handleValidatorDone(msg validatorDoneMsg) (tea.Model, tea.Cmd) {
	idx := msg.StepIdx - 1
	if idx < 0 || idx >= len(m.Steps) {
		return m, nil
	}
	if m.Steps[idx].Validator == nil {
		// Defensive: ValidatorRun deberia existir si llegamos aca.
		return m.advanceAfterValidator(idx, m.Steps[idx].StartedAt)
	}

	m.ValidatorActive = false
	m.runState = nil

	// Persistir verdict.yaml + actualizar feedback en el ValidatorRun.
	_ = writeVerdict(m.RunDir, VerdictRecord{
		StepIdx:   msg.StepIdx,
		Loop:      msg.Loop,
		Verdict:   msg.Verdict.Status,
		Feedback:  msg.Verdict.Feedback,
		RawStdout: truncateForRecord(msg.RawStdout),
	})
	m.Steps[idx].Validator.LastFeedback = msg.Verdict.Feedback

	// H8: snapshot atomico al CERRAR el validator loop (end de cada
	// validator loop). Hace que el bloque validator.last_feedback +
	// loops_run quede persistido apenas se conoce — independientemente del
	// branch siguiente (ok, retry, max_loops...). advanceAfterValidator y
	// rerunStepWithFeedback van a re-escribir despues con su propia
	// transicion; esta linea garantiza que un crash entre las dos NO deje
	// el manifest sin la auditoria del loop que acaba de cerrar.
	_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, m.Steps[idx].StartedAt))

	if msg.Verdict.Status == VerdictOk {
		m.Steps[idx].Validator.FinalVerdict = FinalVerdictOk
		return m.advanceAfterValidator(idx, m.Steps[idx].StartedAt)
	}

	// verdict: fail.
	val := m.Steps[idx].Validator
	if val.LoopsRun < val.MaxLoops {
		// Retry: re-correr EL STEP (no el validator) con el feedback como
		// contexto extra. El loop counter no se resetea — la siguiente
		// vuelta del step incrementa al validator.LoopsRun via
		// startValidator.
		return rerunStepWithFeedback(m, idx)
	}

	// Hit max_loops — decidir segun on_max_loops.
	switch val.OnMaxLoops {
	case wizard.OnMaxLoopsContinue:
		val.FinalVerdict = FinalVerdictFailButContinued
		return m.advanceAfterValidator(idx, m.Steps[idx].StartedAt)
	case wizard.OnMaxLoopsPause:
		// Modal RP — espera decision humana. NO escribimos manifest
		// terminal aca: resolvePauseChoice se encarga segun la opcion.
		m.PauseModal = &PauseState{
			StepIdx:      idx,
			LastFeedback: val.LastFeedback,
			Choice:       PauseChoiceContinue,
		}
		_ = writeManifest(m.RunDir, manifestRunningSnapshot(m, m.Steps[idx].StartedAt))
		return m, nil
	default:
		// fail (default + valor desconocido — defensive).
		val.FinalVerdict = FinalVerdictFail
		m.Steps[idx].Status = StepStatusFailed
		m.FailedStderr = "validator agoto max_loops · " + val.LastFeedback
		_ = closeManifest(m.RunDir, baseManifestForRun(m, m.Steps[idx].StartedAt), ManifestStatusFailed, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		return m, nil
	}
}

// rerunStepWithFeedback re-spawnea el step idx (0-based) con el ultimo
// feedback del validator appendeado al payload original. El payload original
// se re-resuelve con resolvePayloadForStep (mismo path que la primera vuelta
// — para step 0 viene de R1, para step ≥ 1 viene de previous_output).
//
// El status del step vuelve a "running" + StartedAt se re-setea (auditoria:
// la duracion final reflejara la ULTIMA vuelta exitosa, no la suma de todas
// — el doc lo deja explicito como criterio v1). El ring buffer se limpia
// para que el log pane muestre solo el output de la nueva vuelta.
func rerunStepWithFeedback(m RunModel, idx int) (RunModel, tea.Cmd) {
	step := m.Pipeline.Steps[idx]
	basePayload, perr := resolvePayloadForStep(m, idx)
	if perr != nil {
		// Mismo manejo que startStep: sin payload no podemos arrancar.
		m.Steps[idx].Status = StepStatusFailed
		m.Steps[idx].FinishedAt = time.Now()
		m.Steps[idx].SpawnError = perr.Error()
		_ = closeManifest(m.RunDir, baseManifestForRun(m, m.Steps[idx].StartedAt), ManifestStatusFailed, m.Steps[:idx+1])
		m.Screen = ScreenFailed
		m.FailedStderr = perr.Error()
		return m, nil
	}

	feedback := ""
	if m.Steps[idx].Validator != nil {
		feedback = m.Steps[idx].Validator.LastFeedback
	}
	payload := mergeFeedbackIntoPayload(basePayload, feedback)

	m.Active = idx
	m.LogFocus = idx
	m.StickyBottom = true
	m.LogScrollOffset = 0
	if idx < len(m.LogBuffers) && m.LogBuffers[idx] != nil {
		m.LogBuffers[idx].Clear()
	}
	m.Steps[idx].Status = StepStatusRunning
	m.Steps[idx].StartedAt = time.Now()
	m.Steps[idx].FinishedAt = time.Time{}
	m.Steps[idx].ExitCode = 0
	m.Steps[idx].SpawnError = ""

	m.runState = &runState{requestCancel: make(chan struct{}, 1)}
	cmd := runStep(step, payload, m.RunDir, idx+1, m.runState)
	return m, cmd
}

// mergeFeedbackIntoPayload prependea un bloque "FEEDBACK del validator" al
// payload original para que el modelo del step lo consuma como contexto
// extra al re-correr. Si no hay feedback, devuelve el payload tal cual.
func mergeFeedbackIntoPayload(payload, feedback string) string {
	feedback = strings.TrimSpace(feedback)
	if feedback == "" {
		return payload
	}
	var b strings.Builder
	b.WriteString("--- FEEDBACK del validator (intenta corregir esto): ---\n")
	b.WriteString(feedback)
	b.WriteString("\n--- FIN FEEDBACK ---\n\n")
	b.WriteString(payload)
	return b.String()
}

// baseManifestForRun arma el header del manifest con los campos que no
// cambian a lo largo del run. closeManifest le pisa Status/FinishedAt y
// regenera Steps[] desde m.Steps. H8 prefiere m.RunStartedAt (capturado en
// enterRunning) sobre fallbackStart — si por alguna razon RunStartedAt
// quedo zero (defensive: tests viejos / paths edge), caemos al fallback.
func baseManifestForRun(m RunModel, fallbackStart time.Time) Manifest {
	started := m.RunStartedAt
	if started.IsZero() {
		started = fallbackStart
	}
	return Manifest{
		RunID:        m.RunID,
		Pipeline:     m.Pipeline.Name,
		StartedAt:    started,
		PipelinePath: m.path,
		InputKind:    m.Input.Kind,
		InputValue:   m.Input.Value,
	}
}

// manifestRunningSnapshot arma el shape "en curso" con steps actualizados +
// status running. Lo escribimos entre steps para que si el proceso muere
// inesperadamente, el manifest refleje los steps cerrados (done/failed) y
// los pendientes (pending) — no quede el snapshot inicial obsoleto. H8
// reemplazara el write directo por tmp+rename atomico.
func manifestRunningSnapshot(m RunModel, fallbackStart time.Time) Manifest {
	mf := baseManifestForRun(m, fallbackStart)
	mf.Status = ManifestStatusRunning
	mf.Steps = make([]ManifestStep, 0, len(m.Steps))
	for _, s := range m.Steps {
		mf.Steps = append(mf.Steps, ManifestStep{
			Idx:        s.Idx,
			Name:       s.Name,
			CLI:        s.CLI,
			Kind:       s.Kind,
			Status:     string(s.Status),
			ExitCode:   s.ExitCode,
			StartedAt:  s.StartedAt,
			FinishedAt: s.FinishedAt,
			Error:      s.SpawnError,
			Validator:  manifestValidatorFromRun(s.Validator),
		})
	}
	return mf
}

// indentStderr prefija cada linea con `! ` para que el dump del log pane
// distinga visualmente stderr de stdout (sin streaming aun, pero el
// resumen final aprovecha el mismo lipgloss del render).
func indentStderr(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("! ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// viewRunning renderiza R3: header + steps tracker + log pane (vacio
// mientras el subprocess corre — H5 lo va a animar) + footer. Si el modal
// RC esta abierto, lo superpone al final (sigue el patron del doc:
// "modal sobre R3").
func (m RunModel) viewRunning() string {
	name := m.Pipeline.Name
	if name == "" {
		name = "(sin nombre)"
	}

	var b strings.Builder
	header := fmt.Sprintf("Run · %s    step %d/%d", name, m.Active+1, len(m.Pipeline.Steps))
	b.WriteString(titleStyle.Render(header))
	b.WriteString("\n\n")

	// Steps tracker — un row por step del pipeline. Marcamos el activo
	// con icono segun status. Cuando el validator del step activo esta
	// corriendo (H7), el row appendea " · loop K/max".
	for i, step := range m.Pipeline.Steps {
		var run StepRun
		if i < len(m.Steps) {
			run = m.Steps[i]
		}
		row := renderStepRow(i+1, step, run, i == m.Active)
		if i == m.Active && m.ValidatorActive && run.Validator != nil {
			row += dimStyle.Render(fmt.Sprintf("  · loop %d/%d",
				run.Validator.LoopsRun, run.Validator.MaxLoops))
		}
		b.WriteString(row)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Log pane — H5: ring buffer del step en LogFocus, sticky-bottom por
	// default. Si el buffer esta vacio (todavia no llego ninguna linea),
	// caemos al placeholder "ejecutando ...".
	b.WriteString(renderLogPane(m))
	b.WriteString("\n")
	hint := "ctrl+c cancelar · ↑/↓ scroll · g fondo · ctrl+l clear"
	if len(m.Pipeline.Steps) > 1 {
		hint += " · tab cambiar logs"
	}
	b.WriteString(hintStyle.Render(hint))
	b.WriteString("\n")

	if m.CancelModal {
		b.WriteString("\n")
		b.WriteString(viewCancelModal(m))
		b.WriteString("\n")
	}
	if m.PauseModal != nil {
		b.WriteString("\n")
		b.WriteString(viewPauseModal(m))
		b.WriteString("\n")
	}
	return b.String()
}

// logViewportLines es el cap de lineas visibles en el log pane (height del
// viewport). H5 lo deja fijo en 18 — H6 va a sumar resize dinamico segun
// tea.WindowSizeMsg. 18 es suficiente para mostrar ~media pantalla en una
// terminal estandar sin ahogar el header + tracker + footer.
const logViewportLines = 18

// renderLogPane dibuja el viewport del log pane: header del step en LogFocus
// + las ultimas N lineas del ring buffer del mismo step (segun StickyBottom
// / LogScrollOffset). Lineas de stderr en rojo dimmed, intercaladas con
// stdout (criterio del doc para H5). H6 desacopla LogFocus de Active: tab
// cyclea LogFocus para que el usuario pueda inspeccionar logs de steps ya
// terminados o pendientes mientras el subprocess activo sigue.
func renderLogPane(m RunModel) string {
	var b strings.Builder
	focus := m.LogFocus
	if focus < 0 || focus >= len(m.Pipeline.Steps) {
		focus = m.Active
	}
	if focus < len(m.Pipeline.Steps) {
		step := m.Pipeline.Steps[focus]
		head := fmt.Sprintf("[%d/%d] %s (%s / %s)",
			focus+1, len(m.Pipeline.Steps), step.Name, step.CLI, step.Kind)
		b.WriteString(labelStyle.Render(head))
		if focus != m.Active {
			b.WriteString("  ")
			b.WriteString(dimStyle.Render("· viendo logs (tab para cycle)"))
		}
		b.WriteString("\n")
	}
	rb := m.activeBuffer()
	if rb == nil || rb.Len() == 0 {
		// Sin lineas todavia: placeholder igual que H4. Para steps no
		// activos (focus != Active) el placeholder explicita el motivo
		// (pendiente / sin output) para evitar la ilusion de "ejecutando".
		var placeholder string
		if focus < len(m.Pipeline.Steps) {
			cli := m.Pipeline.Steps[focus].CLI
			switch {
			case focus < m.Active:
				placeholder = "> step ya terminado · sin lineas en buffer"
			case focus > m.Active:
				placeholder = "> step pendiente · todavia no arranco"
			case cli != "":
				placeholder = "> ejecutando " + cli + "..."
			default:
				placeholder = "> ejecutando..."
			}
		} else {
			placeholder = "> ejecutando..."
		}
		b.WriteString(dimStyle.Render(placeholder))
		b.WriteString("\n")
		return b.String()
	}
	snap := rb.Snapshot()
	// Slice visible: tomar las ultimas logViewportLines lineas, ajustando
	// con LogScrollOffset cuando el usuario scrolleo arriba.
	visible := windowLines(snap, logViewportLines, m.LogScrollOffset)
	for _, line := range visible {
		switch line.Kind {
		case LogLineStderr:
			b.WriteString(stderrStyle.Render(line.Text))
		default:
			b.WriteString(line.Text)
		}
		b.WriteString("\n")
	}
	// Indicador de scroll arriba si hay mas lineas no visibles.
	if !m.StickyBottom && m.LogScrollOffset > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("(scroll · %d lineas mas abajo · g para ir al fondo)", m.LogScrollOffset)))
		b.WriteString("\n")
	}
	return b.String()
}

// windowLines selecciona las lineas visibles del snapshot segun el scroll
// offset. offset=0 → ultimas `size` lineas (sticky-bottom). offset>0 →
// ventana corrida hacia atras `offset` lineas.
//
// Clampamos para evitar leer fuera de rango: si offset > len-size, la
// ventana queda al inicio del slice.
func windowLines(snap []LogLine, size, offset int) []LogLine {
	if size <= 0 || len(snap) == 0 {
		return nil
	}
	if size >= len(snap) {
		return snap
	}
	end := len(snap) - offset
	if end < size {
		end = size
	}
	if end > len(snap) {
		end = len(snap)
	}
	start := end - size
	if start < 0 {
		start = 0
	}
	return snap[start:end]
}

// renderStepRow es el row de un step en el tracker. icono + nombre + cli +
// (si terminó) duracion.
func renderStepRow(idx int, step wizard.Step, run StepRun, active bool) string {
	icon := "  "
	switch run.Status {
	case StepStatusDone:
		icon = okStyle.Render("✓ ")
	case StepStatusFailed:
		icon = errorStyle.Render("✗ ")
	case StepStatusCancelled:
		icon = warnStyle.Render("! ")
	case StepStatusRunning:
		icon = warnStyle.Render("⏳ ")
	default:
		icon = dimStyle.Render("· ")
	}
	label := fmt.Sprintf("%d. %s", idx, step.Name)
	if step.CLI != "" {
		label += fmt.Sprintf("  (%s)", step.CLI)
	}
	if active && run.Status == StepStatusRunning {
		label = labelStyle.Render(label)
	}
	if !run.FinishedAt.IsZero() {
		dur := run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond)
		label += dimStyle.Render(fmt.Sprintf("  %s", dur))
	}
	return "  " + icon + label
}
