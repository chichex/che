package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	ManifestStatusRunning     = "running"
	ManifestStatusDone        = "done"
	ManifestStatusFailed      = "failed"
	ManifestStatusCancelled   = "cancelled"
	ManifestStatusInterrupted = "interrupted"
)

// Defaults / env keys de H8.
const (
	// runHistoryDefault es el cap default de runs por pipeline-slug. El doc
	// fija 10 (configurable via CHE_RUN_HISTORY).
	runHistoryDefault = 10
	// envRunHistory permite override por env (ej. CHE_RUN_HISTORY=3 para
	// tests; CHE_RUN_HISTORY=0 desactiva GC).
	envRunHistory = "CHE_RUN_HISTORY"
	// envFaultInjectBeforeRename es el switch del fault-injection point del
	// write atomico: cuando vale "1" simulamos un crash entre el write del
	// .tmp y el os.Rename — el .tmp queda huerfano y el manifest viejo
	// intacto. Lo usa el test e2e de atomicidad (H8).
	envFaultInjectBeforeRename = "CHE_FAULT_INJECT_BEFORE_RENAME"
	// recoveryAge es el TTL minimo desde started_at para considerar un
	// manifest con status:running como "interrumpido". El doc fija 1h:
	// menos que eso podria ser otro proceso de che corriendo en paralelo
	// (defensive — no queremos pisar un run vivo del usuario).
	recoveryAge = time.Hour
)

// initManifest arma el manifest minimo al iniciar el run. Steps[] empieza
// pending — UpdateStep reescribe la entrada cuando el spawn arranca / cierra.
// Devuelve el shape ya escrito a disco (write per call, no batching).
//
// startedAt es el timestamp del enterRunning (H8). Lo recibimos del caller
// para mantener consistencia con los snapshots intermedios — el doc fija
// started_at como el inicio del RUN, no del primer step.
func initManifest(p wizard.Pipeline, runID, runDir, pipelinePath, inputKind, inputValue string, startedAt time.Time, steps []StepRun) (Manifest, error) {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	m := Manifest{
		RunID:        runID,
		Pipeline:     p.Name,
		StartedAt:    startedAt,
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

// writeManifest serializa + escribe manifest.yaml de forma atomica (H8):
// escribe a `<runDir>/manifest.yaml.tmp` y luego os.Rename al destino final.
// Si el rename falla a mitad, el manifest viejo queda intacto (rename es
// atomico en POSIX dentro del mismo filesystem). Si el marshal o el write
// del .tmp fallan, no se toca el manifest existente.
//
// Fault injection: cuando CHE_FAULT_INJECT_BEFORE_RENAME=1, escribimos el
// .tmp pero retornamos error ANTES del rename — simula un crash entre los
// dos pasos. El caller no debe ver el .tmp aplicado al destino, y el
// manifest viejo (si existia) queda intacto. Lo usa el test e2e de
// atomicidad de H8.
func writeManifest(runDir string, m Manifest) error {
	data, err := yaml.Marshal(&m)
	if err != nil {
		return fmt.Errorf("manifest: marshal: %w", err)
	}
	path := filepath.Join(runDir, "manifest.yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("manifest: write tmp %s: %w", tmp, err)
	}
	if os.Getenv(envFaultInjectBeforeRename) == "1" {
		// Defensive: NO removemos el .tmp — el doc fija que cleanup de .tmp
		// huerfanos es out-of-scope de H8 (post-v1, no critico). El test
		// se asegura de leer el manifest viejo y verificar que el .tmp
		// efectivamente quedo en disco.
		return fmt.Errorf("manifest: fault inject before rename (CHE_FAULT_INJECT_BEFORE_RENAME=1)")
	}
	if err := os.Rename(tmp, path); err != nil {
		// rename fallido: dejamos el .tmp en disco (mismo criterio que el
		// fault-inject). El manifest viejo queda intacto si existia.
		return fmt.Errorf("manifest: rename %s -> %s: %w", tmp, path, err)
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

// runHistoryCap devuelve el cap de runs por slug-dir aplicando el override
// de CHE_RUN_HISTORY (si esta seteado y >= 0). Default = 10. Un valor 0
// efectivamente desactiva el cap (gcRunHistory hace early-return cuando
// cap<=0 — ver abajo).
func runHistoryCap() int {
	if v := os.Getenv(envRunHistory); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return runHistoryDefault
}

// gcRunHistory aplica el cap de retencion por pipeline-slug: lista los
// subdirs de slugDir, los ordena por mtime descendente y os.RemoveAll todos
// los que sobran al cap. Idempotente — si ya hay <= cap subdirs no toca
// nada. Errores de lectura del dir se reportan (el caller decide si
// abortar); errores de remove individuales se ignoran (un subdir con
// permisos rotos no debe frenar el run nuevo).
//
// cap == 0 desactiva el GC (caso "no quiero history; conserva todo"). El
// doc fija el default en 10 + override por CHE_RUN_HISTORY.
func gcRunHistory(slugDir string, cap int) error {
	if cap <= 0 {
		return nil
	}
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("gc: read %s: %w", slugDir, err)
	}
	type dirInfo struct {
		path  string
		mtime time.Time
	}
	var dirs []dirInfo
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		full := filepath.Join(slugDir, ent.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		dirs = append(dirs, dirInfo{path: full, mtime: info.ModTime()})
	}
	if len(dirs) <= cap {
		return nil
	}
	sort.SliceStable(dirs, func(i, j int) bool {
		return dirs[i].mtime.After(dirs[j].mtime)
	})
	for _, d := range dirs[cap:] {
		_ = os.RemoveAll(d.path)
	}
	return nil
}

// RecoverInterruptedRuns recorre todos los run-dirs bajo
// `<home>/.che/runs/<slug>/<run-id>/manifest.yaml` y reescribe a
// `status: interrupted` los que tienen `status: running` Y
// `started_at` > 1h. La barrera de 1h es defensiva: un manifest con
// started_at reciente puede ser otro proceso de che corriendo en paralelo
// (no queremos pisarle el manifest mid-run).
//
// Lo invoca el lister (`My pipelines`) al boot — antes de mostrar la lista
// — para que los chips reflejen estado consistente. Errores individuales
// (parse, IO) se ignoran: la recovery es best-effort y no debe romper el
// lister.
//
// home == "" cae a os.UserHomeDir (mismo criterio que wizard.PipelinesDir).
func RecoverInterruptedRuns(home string) error {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil // best-effort
		}
		home = h
	}
	root := filepath.Join(home, ".che", "runs")
	slugs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	now := time.Now()
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		slugDir := filepath.Join(root, slug.Name())
		runs, err := os.ReadDir(slugDir)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if !run.IsDir() {
				continue
			}
			runDir := filepath.Join(slugDir, run.Name())
			recoverOne(runDir, now)
		}
	}
	return nil
}

// recoverOne aplica la recovery a un solo run-dir: lee manifest.yaml, si
// status==running y now-started_at > recoveryAge → reescribe a interrupted
// (write atomico). Cualquier error es silencioso.
func recoverOne(runDir string, now time.Time) {
	path := filepath.Join(runDir, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return
	}
	if m.Status != ManifestStatusRunning {
		return
	}
	if m.StartedAt.IsZero() || now.Sub(m.StartedAt) <= recoveryAge {
		return
	}
	m.Status = ManifestStatusInterrupted
	m.FinishedAt = now.UTC()
	_ = writeManifest(runDir, m)
}
