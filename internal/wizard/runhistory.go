// Package wizard también expone (H10) la lectura best-effort del historial
// de runs por pipeline-slug. La lectura es minima — solo lo que el lister
// (`My pipelines`) necesita para renderear el chip "last run: X ago" + la
// pantalla "Run history" (lista de runs terminales por pipeline).
//
// El runner persiste manifest.yaml por run en ~/.che/runs/<slug>/<run-id>/;
// definir el shape completo aca duplicaria el contrato. Definimos un struct
// minimo que solo declara los campos que el lister consume — yaml.v3 ignora
// los demas. Si en el futuro el lister necesita mas info (ej. exit codes
// del ultimo step para el detalle), agregamos campos sin tocar el runner.
package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// runsDirName es el subdir de HOME donde el runner persiste los runs (mismo
// path que internal/runner/running.go.defaultRunDirRoot — duplicado por la
// regla del codebase de evitar imports cruzados wizard↔runner).
const runsDirName = ".che/runs"

// RunStatus es el estado terminal del run leido del manifest. Mismos valores
// que internal/runner/manifest.go expone como ManifestStatus*; duplicamos
// para no acoplar el lister al runner.
type RunStatus string

const (
	RunStatusRunning     RunStatus = "running"
	RunStatusDone        RunStatus = "done"
	RunStatusFailed      RunStatus = "failed"
	RunStatusCancelled   RunStatus = "cancelled"
	RunStatusInterrupted RunStatus = "interrupted"
	// RunStatusNever es un valor sintetico que devolvemos cuando el slug
	// no tiene ningun run en disco. El lister lo renderea como chip
	// "never" + omite la sub-linea "last run".
	RunStatusNever RunStatus = "never"
)

// RunSummary es el snapshot que el lister consume por pipeline-slug. Vacio
// (zero-value) si no hay runs.
type RunSummary struct {
	RunID      string
	Status     RunStatus
	StartedAt  time.Time
	FinishedAt time.Time
	// RunDir es el path absoluto del run dir (~/.che/runs/<slug>/<id>).
	// El lister lo usa para abrir el detalle del run en la pantalla "Run
	// history" (que necesita leer step-NN.* files).
	RunDir string
}

// Duration devuelve la duracion del run (FinishedAt - StartedAt) si ambos
// timestamps estan poblados; si el run esta running/interrupted, devuelve 0.
func (r RunSummary) Duration() time.Duration {
	if r.FinishedAt.IsZero() || r.StartedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// runManifestMin es el shape minimo del manifest que parseamos para el
// chip / history. yaml.v3 ignora los campos no declarados, asi que
// extender el manifest del runner no rompe esto.
type runManifestMin struct {
	RunID      string    `yaml:"run_id"`
	Status     string    `yaml:"status"`
	StartedAt  time.Time `yaml:"started_at"`
	FinishedAt time.Time `yaml:"finished_at,omitempty"`
}

// LastRunFor devuelve el RunSummary del run mas reciente (segun StartedAt) del
// pipeline-slug bajo home. Si no hay runs, devuelve un summary con
// Status=RunStatusNever y resto cero. Errores de IO se tratan como "no hay
// runs" — la chip nunca debe romper el lister.
//
// home == "" cae a os.UserHomeDir (mismo criterio que PipelinesDir).
func LastRunFor(home, slug string) RunSummary {
	runs := loadRunSummaries(home, slug)
	if len(runs) == 0 {
		return RunSummary{Status: RunStatusNever}
	}
	return runs[0]
}

// RunHistoryFor devuelve la lista completa de runs del slug, ordenada por
// StartedAt desc (mas reciente primero). Vacia si no hay. Best-effort: runs
// con manifests rotos se skipean.
func RunHistoryFor(home, slug string) []RunSummary {
	return loadRunSummaries(home, slug)
}

// loadRunSummaries es el reader compartido por LastRunFor y RunHistoryFor.
// Lee ~/.che/runs/<slug>/*/manifest.yaml, parsea el shape minimo, ordena
// desc por StartedAt y devuelve la lista. Errores individuales (parse,
// stat) se ignoran — el lister sigue funcionando aunque uno de los runs
// tenga el manifest corrupto.
func loadRunSummaries(home, slug string) []RunSummary {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = h
	}
	if slug == "" {
		return nil
	}
	slugDir := filepath.Join(home, runsDirName, slug)
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		return nil
	}
	var out []RunSummary
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		runDir := filepath.Join(slugDir, ent.Name())
		mPath := filepath.Join(runDir, "manifest.yaml")
		data, err := os.ReadFile(mPath)
		if err != nil {
			continue
		}
		var mm runManifestMin
		if err := yaml.Unmarshal(data, &mm); err != nil {
			continue
		}
		// Si el manifest no declara run_id, fallback al nombre del dir.
		runID := mm.RunID
		if runID == "" {
			runID = ent.Name()
		}
		out = append(out, RunSummary{
			RunID:      runID,
			Status:     RunStatus(mm.Status),
			StartedAt:  mm.StartedAt,
			FinishedAt: mm.FinishedAt,
			RunDir:     runDir,
		})
	}
	// Sort desc por StartedAt; si dos coinciden (granularidad 1s del run-id)
	// desempatamos por RunID desc para que el sort sea estable.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].RunID > out[j].RunID
	})
	return out
}

// ChipForStatus devuelve el chip humano para el status del last-run. El
// lister lo usa al renderear cada row ready. Strings sin estilo aca — el
// caller (renderListRow) aplica el color segun el status.
func ChipForStatus(s RunStatus) string {
	switch s {
	case RunStatusDone:
		return "✓ done"
	case RunStatusFailed:
		return "✗ failed"
	case RunStatusCancelled:
		return "! cancelled"
	case RunStatusInterrupted:
		return "? interrupted"
	case RunStatusRunning:
		return "⏳ running"
	case RunStatusNever, "":
		return "never"
	default:
		return string(s)
	}
}

// formatRunDuration devuelve "1m 23s" / "5s" / "" para una duracion.
// Mismo estilo que el resumen del runner — duplicado aca por la regla
// "no importar runner".
func formatRunDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	if secs == 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%dm %ds", mins, secs)
}
