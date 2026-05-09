package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// Manifest es el shape persistido en <run-dir>/manifest.yaml. Sigue el
// "Schema del manifest.yaml" del doc (seccion Persistencia) limitado a lo
// que H4 necesita: header del run + lista de steps con status / exit_code /
// timestamps. H6 va a sumar validator.{loops_run, final_verdict, ...}, H8
// agrega writes atomicos via tmp+rename y los timestamps RFC3339Nano.
//
// Los nombres YAML siguen snake_case para alinearse con el doc; el omitempty
// evita que un FinishedAt cero-value se serialice como "0001-01-01T...".
type Manifest struct {
	RunID        string         `yaml:"run_id"`
	Pipeline     string         `yaml:"pipeline"`
	StartedAt    time.Time      `yaml:"started_at"`
	FinishedAt   time.Time      `yaml:"finished_at,omitempty"`
	Status       string         `yaml:"status"`
	Steps        []ManifestStep `yaml:"steps"`
	InputKind    string         `yaml:"input_kind,omitempty"`
	InputValue   string         `yaml:"input_value,omitempty"`
	PipelinePath string         `yaml:"pipeline_path,omitempty"`
}

// ManifestStep es la entrada por step del manifest. H4 la usa para registrar
// el step 0 (status / exit_code / timestamps); H7 agrega Validator (bloque
// opcional cuando el step declara cross-review).
type ManifestStep struct {
	Idx        int                `yaml:"idx"`
	Name       string             `yaml:"name"`
	CLI        string             `yaml:"cli,omitempty"`
	Kind       string             `yaml:"kind,omitempty"`
	Status     string             `yaml:"status"`
	ExitCode   int                `yaml:"exit_code"`
	StartedAt  time.Time          `yaml:"started_at,omitempty"`
	FinishedAt time.Time          `yaml:"finished_at,omitempty"`
	Error      string             `yaml:"error,omitempty"`
	Validator  *ManifestValidator `yaml:"validator,omitempty"`
}

// ManifestValidator es el bloque persistido en manifest.steps[i].validator
// cuando el step declara cross-review (H7). Sigue el shape del doc
// (seccion "Schema del manifest.yaml"): cli, loops_run, max_loops,
// on_max_loops, final_verdict y last_feedback.
//
// Solo se serializa cuando el step efectivamente corrio el validator (al
// menos un loop cerrado) — si el step fallo antes de llegar al validator,
// Validator queda nil y el bloque no aparece.
type ManifestValidator struct {
	CLI          string `yaml:"cli,omitempty"`
	LoopsRun     int    `yaml:"loops_run"`
	MaxLoops     int    `yaml:"max_loops"`
	OnMaxLoops   string `yaml:"on_max_loops,omitempty"`
	FinalVerdict string `yaml:"final_verdict,omitempty"`
	LastFeedback string `yaml:"last_feedback,omitempty"`
}

// Status values del manifest a nivel run (status del top-level). Los del
// step usan StepStatus (ver model.go).
const (
	ManifestStatusRunning   = "running"
	ManifestStatusDone      = "done"
	ManifestStatusFailed    = "failed"
	ManifestStatusCancelled = "cancelled"
)

// initManifest arma el manifest minimo al iniciar el run. Steps[] empieza
// pending — UpdateStep reescribe la entrada cuando el spawn arranca / cierra.
// Devuelve el shape ya escrito a disco (write per call, no batching).
func initManifest(p wizard.Pipeline, runID, runDir, pipelinePath, inputKind, inputValue string, steps []StepRun) (Manifest, error) {
	m := Manifest{
		RunID:        runID,
		Pipeline:     p.Name,
		StartedAt:    time.Now().UTC(),
		Status:       ManifestStatusRunning,
		Steps:        make([]ManifestStep, 0, len(steps)),
		InputKind:    inputKind,
		InputValue:   inputValue,
		PipelinePath: pipelinePath,
	}
	for _, s := range steps {
		m.Steps = append(m.Steps, ManifestStep{
			Idx:    s.Idx,
			Name:   s.Name,
			CLI:    s.CLI,
			Kind:   s.Kind,
			Status: string(s.Status),
		})
	}
	if err := writeManifest(runDir, m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// writeManifest serializa + escribe manifest.yaml. H4 hace write directo —
// H8 va a cambiarlo por tmp+rename atomico (el doc lo deja explicito como
// scope de H8, no de H4). El error de write se devuelve para que el caller
// decida (R3 lo trata como fatal y va a RF).
func writeManifest(runDir string, m Manifest) error {
	data, err := yaml.Marshal(&m)
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	path := filepath.Join(runDir, "manifest.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("manifest: write %s: %w", path, err)
	}
	return nil
}

// closeManifest reescribe el manifest con el status terminal del run. status
// debe ser uno de ManifestStatus*. steps trae el snapshot final (con
// FinishedAt y ExitCode poblados).
func closeManifest(runDir string, m Manifest, status string, steps []StepRun) error {
	m.Status = status
	m.FinishedAt = time.Now().UTC()
	m.Steps = m.Steps[:0]
	for _, s := range steps {
		entry := ManifestStep{
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
		}
		m.Steps = append(m.Steps, entry)
	}
	return writeManifest(runDir, m)
}

// manifestValidatorFromRun convierte el snapshot vivo (StepRun.Validator) en
// el shape persistido (ManifestValidator). Devuelve nil si no hay validator
// — asi el yaml omite el bloque entero (omitempty del puntero) y el
// manifest queda limpio para steps sin cross-review.
func manifestValidatorFromRun(v *ValidatorRun) *ManifestValidator {
	if v == nil {
		return nil
	}
	return &ManifestValidator{
		CLI:          v.CLI,
		LoopsRun:     v.LoopsRun,
		MaxLoops:     v.MaxLoops,
		OnMaxLoops:   v.OnMaxLoops,
		FinalVerdict: v.FinalVerdict,
		LastFeedback: v.LastFeedback,
	}
}
