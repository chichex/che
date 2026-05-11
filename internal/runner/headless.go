package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// HeadlessRun es el handle de un run iniciado en modo headless: el run-dir y
// el manifest inicial ya estan en disco con status=running; los steps todavia
// no se ejecutaron. El caller invoca Execute() (sincronico) para correr la
// maquina de steps; el dash lo hace en una goroutine separada al server
// para responder 201 con el RunID apenas StartHeadless retorna.
//
// El modo headless es una simplificacion deliberada del runner TUI: corre los
// steps en serie, escribe los mismos artefactos en disco (manifest.yaml,
// step-NN.stdout.log, step-NN.stderr.log, step-NN.result.yaml) y respeta el
// schema del Manifest. NO soporta validator loops, retry, cancel ni
// streaming hacia el caller — el flow de dash POST + SSE per-run no los
// necesita (el watcher en runs_watcher.go traduce los writes a SSE).
type HeadlessRun struct {
	RunID  string
	RunDir string

	p       wizard.Pipeline
	target  string
	input   InputState
	started time.Time
	steps   []StepRun
}

// StartHeadless prepara un run headless: carga el pipeline desde target,
// valida el shape (wizard.IsValid), crea el run-dir y persiste el manifest
// inicial con status=running. Devuelve el handle listo para Execute().
//
// target acepta los mismos shapes que Run: "builtin:<slug>" o un path en
// disco. inputValue es el valor crudo del input (equivalente a lo que R1
// resuelve en la TUI); para pipelines con steps[0].input == "none" se
// ignora. runsRoot overridea ~/.che/runs/<slug>/ cuando no esta vacio
// (override de tests + opcional para el dash si queremos sandboxear).
func StartHeadless(target, inputValue, runsRoot string) (*HeadlessRun, error) {
	p, err := loadPipelineForRun(target)
	if err != nil {
		return nil, err
	}
	if verr := wizard.IsValid(p); verr != nil {
		return nil, fmt.Errorf("runner: pipeline invalido: %w", verr)
	}
	return startHeadlessFromPipeline(p, target, inputValue, runsRoot)
}

// startHeadlessFromPipeline es el core de StartHeadless sin la validacion de
// wizard.IsValid + el load del target. Lo usan los tests que quieren saltar
// la deteccion de CLIs instalados (validate.go usa skills.Detect, dependent
// del PATH del host) y construir un pipeline directamente con un CLI fake.
func startHeadlessFromPipeline(p wizard.Pipeline, target, inputValue, runsRoot string) (*HeadlessRun, error) {
	kind := firstInputKind(p)
	is := InputState{Kind: kind}
	if kind != wizard.InputNone {
		is.Value = inputValue
		is.ResolvedPayload = inputValue
	}

	id, runDir, err := initRunDirAt(p, runsRoot)
	if err != nil {
		return nil, err
	}

	started := time.Now().UTC()
	stepRuns := make([]StepRun, 0, len(p.Steps))
	for i, ps := range p.Steps {
		stepRuns = append(stepRuns, StepRun{
			Idx:    i + 1,
			Name:   ps.Name,
			CLI:    ps.CLI,
			Kind:   ps.Kind,
			Status: StepStatusPending,
		})
	}

	if _, err := initManifest(p, id, runDir, target, is.Kind, is.Value, started, stepRuns); err != nil {
		return nil, err
	}

	return &HeadlessRun{
		RunID:   id,
		RunDir:  runDir,
		p:       p,
		target:  target,
		input:   is,
		started: started,
		steps:   stepRuns,
	}, nil
}

// Execute corre los steps del pipeline en serie. Bloquea hasta que termine
// (done | failed). Errores fatales se reflejan en el manifest con status
// terminal y se devuelven al caller (que en el dash los logea con prefijo
// [dash]). Un recover() defensivo cierra el manifest como failed si algo
// panickea durante la ejecucion — asi un crash deja el manifest auditado
// en vez de huerfano con status=running.
func (h *HeadlessRun) Execute() (err error) {
	defer func() {
		if r := recover(); r != nil {
			_ = closeManifest(h.RunDir, h.baseManifest(), ManifestStatusFailed, h.steps)
			err = fmt.Errorf("runner.headless: panic: %v", r)
		}
	}()

	for i := range h.p.Steps {
		step := h.p.Steps[i]
		payload, perr := h.payloadForStep(i)
		if perr != nil {
			h.steps[i].StartedAt = time.Now()
			h.steps[i].FinishedAt = time.Now()
			h.steps[i].ExitCode = -1
			h.steps[i].SpawnError = perr.Error()
			h.steps[i].Status = StepStatusFailed
			_ = closeManifest(h.RunDir, h.baseManifest(), ManifestStatusFailed, h.steps[:i+1])
			return perr
		}

		h.steps[i].StartedAt = time.Now()
		h.steps[i].Status = StepStatusRunning
		_ = writeManifest(h.RunDir, h.runningSnapshot())

		exitCode, stdout, _, spawnErr := runStepHeadless(step, payload, h.RunDir, i+1)
		h.steps[i].FinishedAt = time.Now()
		h.steps[i].ExitCode = exitCode
		h.steps[i].SpawnError = spawnErr

		_ = writeStepResult(h.RunDir, StepResult{
			StepIdx:  i + 1,
			StepName: step.Name,
			ExitCode: exitCode,
			Output:   stdout,
		})

		if spawnErr != "" || exitCode != 0 {
			h.steps[i].Status = StepStatusFailed
			_ = closeManifest(h.RunDir, h.baseManifest(), ManifestStatusFailed, h.steps[:i+1])
			if spawnErr != "" {
				return fmt.Errorf("step %d (%s): %s", i+1, step.Name, spawnErr)
			}
			return fmt.Errorf("step %d (%s) exit %d", i+1, step.Name, exitCode)
		}

		h.steps[i].Status = StepStatusDone
	}

	return closeManifest(h.RunDir, h.baseManifest(), ManifestStatusDone, h.steps)
}

// payloadForStep devuelve el stdin del subprocess para el step en idx
// (0-based). Step 0 usa ResolvedPayload del input (R1 equivalente); steps
// >= 1 leen el output del step anterior via readStepResult (mismo path que
// resolvePayloadForStep en running.go).
func (h *HeadlessRun) payloadForStep(idx int) (string, error) {
	if idx == 0 {
		return h.input.ResolvedPayload, nil
	}
	prev, err := readStepResult(h.RunDir, idx)
	if err != nil {
		return "", fmt.Errorf("previous_output step %d: %w", idx+1, err)
	}
	return prev.Output, nil
}

// baseManifest arma el header del Manifest que comparte todos los snapshots
// del run. closeManifest le sobrescribe Status/FinishedAt + Steps al cerrar.
func (h *HeadlessRun) baseManifest() Manifest {
	return Manifest{
		RunID:        h.RunID,
		Pipeline:     h.p.Name,
		StartedAt:    h.started,
		PipelinePath: h.target,
		InputKind:    h.input.Kind,
		InputValue:   h.input.Value,
	}
}

// runningSnapshot arma el shape "en curso" del manifest segun el estado vivo
// de h.steps. Misma idea que manifestRunningSnapshot en running.go pero sin
// depender del RunModel (que es TUI-specific).
func (h *HeadlessRun) runningSnapshot() Manifest {
	mf := h.baseManifest()
	mf.Status = ManifestStatusRunning
	mf.Steps = make([]ManifestStep, 0, len(h.steps))
	for _, s := range h.steps {
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
		})
	}
	return mf
}

// runStepHeadless ejecuta el subprocess de un step en modo blocking: arma el
// cmd via spawnCmdFn (la misma factoria que usa la TUI, asi los tests
// pueden seguir mockeandola), redirecciona stdout/stderr a archivos +
// builders en memoria, y devuelve el resultado del Wait sincronicamente.
//
// La rama del exec.ExitError vs SpawnError sigue el mismo criterio que
// stepDoneMsg en spawn.go: ExitCode "real" cuando el subprocess corrio y
// salio no-cero; SpawnErr cuando el binario no se pudo arrancar o Wait
// devolvio un error no-Exit.
func runStepHeadless(step wizard.Step, payload, runDir string, idx int) (exitCode int, stdout, stderr, spawnErr string) {
	cmd, err := spawnCmdFn(step, payload)
	if err != nil {
		return -1, "", "", err.Error()
	}

	stdoutPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stdout.log", idx))
	stderrPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stderr.log", idx))

	stdoutFile, ferr := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if ferr != nil {
		return -1, "", "", fmt.Sprintf("create stdout.log: %v", ferr)
	}
	defer stdoutFile.Close()

	stderrFile, ferr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if ferr != nil {
		return -1, "", "", fmt.Sprintf("create stderr.log: %v", ferr)
	}
	defer stderrFile.Close()

	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = io.MultiWriter(stdoutFile, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(stderrFile, &stderrBuf)

	runErr := cmd.Run()
	_ = stdoutFile.Sync()
	_ = stderrFile.Sync()

	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return ee.ExitCode(), stdoutBuf.String(), stderrBuf.String(), ""
		}
		return -1, stdoutBuf.String(), stderrBuf.String(), runErr.Error()
	}
	return 0, stdoutBuf.String(), stderrBuf.String(), ""
}

// initRunDirAt es la version parametrizada de initRunDir. Cuando runsRoot
// esta vacio, cae al default (~/.che/runs via runDirRootFn). El extra
// parametro permite que el dash use un override sin tocar la variable
// global swappable de tests.
func initRunDirAt(p wizard.Pipeline, runsRoot string) (string, string, error) {
	slug := wizard.Slug(p.Name)
	if slug == "" {
		slug = "pipeline"
	}
	root := runsRoot
	if root == "" {
		root = runDirRootFn()
	}
	slugDir := filepath.Join(root, slug)
	if err := os.MkdirAll(slugDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", slugDir, err)
	}
	_ = gcRunHistory(slugDir, runHistoryCap())
	id, runDir := makeRunID(slugDir, nowFn())
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	return id, runDir, nil
}
