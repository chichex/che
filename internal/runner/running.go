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
// (runID, runDir, error).
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
	id, runDir := makeRunID(slugDir, nowFn())
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	return id, runDir, nil
}

// enterRunning es la transicion R2 → R3 (post-preflight ok / warn confirmado).
// Inicializa el run dir + manifest, prepara el slice de Steps, y devuelve un
// tea.Cmd que arranca el spawn del step 0. H4 solo soporta 1 step — si el
// pipeline tiene N>1 mostramos el banner MultiStepWarning pero igual corremos
// SOLO el step 0 (defensive, segun el criterio de aceptacion del doc).
func enterRunning(m RunModel) (RunModel, tea.Cmd) {
	m.Screen = ScreenRunning
	m.CancelModal = false
	m.LogDump = ""
	m.FailedStderr = ""

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

	// Steps slice: H4 solo el step 0. Idx empieza en 1 (1-indexed para
	// alinear con los nombres de archivo step-01.*).
	stepRuns := make([]StepRun, 0, len(m.Pipeline.Steps))
	step0 := m.Pipeline.Steps[0]
	stepRuns = append(stepRuns, StepRun{
		Idx:    1,
		Name:   step0.Name,
		CLI:    step0.CLI,
		Kind:   step0.Kind,
		Status: StepStatusPending,
	})
	m.Steps = stepRuns
	m.Active = 0
	m.MultiStepWarning = len(m.Pipeline.Steps) > 1

	if _, err := initManifest(m.Pipeline, id, runDir, m.path, m.Input.Kind, m.Input.Value, stepRuns); err != nil {
		m.Screen = ScreenFailed
		m.FailedStderr = err.Error()
		m.Steps[0].Status = StepStatusFailed
		m.Steps[0].SpawnError = err.Error()
		return m, nil
	}

	// Marcamos el step 0 como running antes de spawnear (el render del
	// tracker refleja "⏳ running" mientras el subprocess esta vivo).
	m.Steps[0].Status = StepStatusRunning
	m.Steps[0].StartedAt = time.Now()

	// runState compartido entre Update + spawn goroutine (cancel handler).
	m.runState = &runState{requestCancel: make(chan struct{}, 1)}
	cmd := runStep(step0, m.Input.ResolvedPayload, runDir, 1, m.runState)
	return m, cmd
}

// updateRunning maneja teclas + msgs durante R3. La transicion a R4/RF se
// activa cuando llega stepDoneMsg (lo emite la goroutine del spawn).
func (m RunModel) updateRunning(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		}
		// Resto de teclas (g / ctrl+l / tab / arrows) las dejamos para H5.
		return m, nil
	case stepDoneMsg:
		return m.handleStepDone(msg)
	}
	return m, nil
}

// handleStepDone procesa el mensaje de fin del subprocess. Escribe el
// result.yaml del step + el manifest cerrado, y transiciona a R4 / RF segun
// exit_code + cancelled. Es el unico punto donde se decide R4 vs RF en H4.
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

	// LogDump para el render terminal — concat stdout+stderr resumido.
	m.LogDump = msg.Stdout
	if msg.Stderr != "" {
		// stderr indented + prefix `! ` para distinguirlo en R4/RF.
		m.LogDump += "\n" + indentStderr(msg.Stderr)
	}

	// result.yaml siempre se escribe — el doc fija que `output` queda con
	// lo que haya en stdout incluso en exit ≠ 0. Ignoramos el error de
	// write: si falla, el screen RF/R4 igual va a renderear; el manifest
	// posterior captura el mismo problema.
	_ = writeStepResult(m.RunDir, StepResult{
		StepIdx:  step.Idx,
		StepName: step.Name,
		ExitCode: msg.ExitCode,
		Output:   msg.Stdout,
	})

	// Manifest cerrado con el status terminal del run. Para H4 (1 step)
	// el run-status = el status del unico step.
	runStatus := ManifestStatusDone
	switch step.Status {
	case StepStatusFailed:
		runStatus = ManifestStatusFailed
	case StepStatusCancelled:
		runStatus = ManifestStatusCancelled
	}
	currentManifest := Manifest{
		RunID:        m.RunID,
		Pipeline:     m.Pipeline.Name,
		StartedAt:    msg.StartedAt,
		PipelinePath: m.path,
		InputKind:    m.Input.Kind,
		InputValue:   m.Input.Value,
	}
	_ = closeManifest(m.RunDir, currentManifest, runStatus, m.Steps)

	// Limpieza del runState — el handle ya no es vivo.
	m.runState = nil

	// Transicion segun status.
	switch step.Status {
	case StepStatusFailed:
		m.Screen = ScreenFailed
		m.FailedStderr = msg.Stderr
		if step.SpawnError != "" && m.FailedStderr == "" {
			m.FailedStderr = step.SpawnError
		}
	case StepStatusCancelled:
		// El doc dice "tono amarillo, screen tipo RF". Para H4 caemos a
		// ScreenFailed marcando que fue cancel via SpawnError; el view
		// usa ese flag para el banner.
		m.Screen = ScreenFailed
		if m.FailedStderr == "" {
			m.FailedStderr = "run cancelado por el usuario"
		}
	default:
		m.Screen = ScreenDone
	}
	return m, nil
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
	b.WriteString("\n")

	if m.MultiStepWarning {
		b.WriteString(warnStyle.Render(fmt.Sprintf("multi-step viene en H6 — corriendo solo el step 1/%d", len(m.Pipeline.Steps))))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Steps tracker — un row por step del pipeline. Marcamos el activo
	// con icono segun status.
	for i, step := range m.Pipeline.Steps {
		var run StepRun
		if i < len(m.Steps) {
			run = m.Steps[i]
		}
		b.WriteString(renderStepRow(i+1, step, run, i == m.Active))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Log pane — H4 muestra "ejecutando ..." mientras corre; el dump real
	// llega cuando handleStepDone setea m.LogDump (en ese momento ya
	// estamos en R4/RF, no en R3 — pero por consistencia del modelo lo
	// renderizamos si esta presente).
	if m.LogDump != "" {
		b.WriteString(dimStyle.Render(m.LogDump))
		b.WriteString("\n")
	} else {
		b.WriteString(dimStyle.Render("> ejecutando " + m.Pipeline.Steps[m.Active].CLI + "..."))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("ctrl+c cancelar"))
	b.WriteString("\n")

	if m.CancelModal {
		b.WriteString("\n")
		b.WriteString(viewCancelModal(m))
		b.WriteString("\n")
	}
	return b.String()
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
