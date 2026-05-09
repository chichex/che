package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestWriteManifestAtomic_Happy verifica el camino normal: writeManifest
// escribe el .tmp, hace rename, y deja el archivo final con el contenido
// esperado. El .tmp NO sobrevive en disco post-rename (rename mueve el
// inodo).
func TestWriteManifestAtomic_Happy(t *testing.T) {
	t.Parallel()
	runDir := t.TempDir()
	m := Manifest{RunID: "id-1", Pipeline: "p", Status: ManifestStatusRunning, StartedAt: time.Now().UTC()}
	if err := writeManifest(runDir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), "run_id: id-1") {
		t.Errorf("expected run_id in manifest, got:\n%s", data)
	}
	// .tmp no sobrevive (rename atomico).
	if _, err := os.Stat(filepath.Join(runDir, "manifest.yaml.tmp")); !os.IsNotExist(err) {
		t.Errorf("expected .tmp to not exist post-rename, stat err=%v", err)
	}
}

// TestWriteManifestAtomic_FaultInject verifica el switch
// CHE_FAULT_INJECT_BEFORE_RENAME=1: el .tmp se escribe pero el rename NO
// se ejecuta — el manifest viejo (si existia) queda intacto. Es la base
// del test e2e de atomicidad de H8.
func TestWriteManifestAtomic_FaultInject(t *testing.T) {
	runDir := t.TempDir()

	// Pre-poblamos un manifest "viejo" para asertar que NO se pisa.
	old := Manifest{RunID: "viejo", Pipeline: "p", Status: ManifestStatusDone, StartedAt: time.Now().UTC()}
	if err := writeManifest(runDir, old); err != nil {
		t.Fatalf("seed old manifest: %v", err)
	}

	// Activamos el fault inject. Usamos t.Setenv (auto-cleanup post-test).
	t.Setenv("CHE_FAULT_INJECT_BEFORE_RENAME", "1")

	nuevo := Manifest{RunID: "nuevo", Pipeline: "p", Status: ManifestStatusRunning, StartedAt: time.Now().UTC()}
	err := writeManifest(runDir, nuevo)
	if err == nil {
		t.Fatalf("expected error from writeManifest with fault inject, got nil")
	}
	if !strings.Contains(err.Error(), "fault inject") {
		t.Errorf("expected error to mention fault inject, got: %v", err)
	}

	// Manifest viejo intacto.
	data, _ := os.ReadFile(filepath.Join(runDir, "manifest.yaml"))
	if !strings.Contains(string(data), "run_id: viejo") {
		t.Errorf("expected old manifest intact (run_id: viejo), got:\n%s", data)
	}
	// .tmp huerfano en disco.
	if _, err := os.Stat(filepath.Join(runDir, "manifest.yaml.tmp")); err != nil {
		t.Errorf("expected .tmp orphan to exist, stat err=%v", err)
	}
}

// TestGCRunHistory_KeepsLatest pre-arma 15 dirs con mtimes escalonados +
// dispara gcRunHistory(cap=10). Tras el GC quedan exactamente los 10 mas
// recientes por mtime.
func TestGCRunHistory_KeepsLatest(t *testing.T) {
	t.Parallel()
	slugDir := t.TempDir()
	base := time.Now()
	// Creamos 15 dirs con mtimes escalonados (1m de diferencia) — del mas
	// viejo (idx 0) al mas nuevo (idx 14).
	for i := 0; i < 15; i++ {
		dir := filepath.Join(slugDir, fmt.Sprintf("run-%02d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mtime := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(dir, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	if err := gcRunHistory(slugDir, 10); err != nil {
		t.Fatalf("gcRunHistory: %v", err)
	}
	// Deberian quedar 10 dirs — los idx 5..14 (mtime mas reciente).
	entries, _ := os.ReadDir(slugDir)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 10 {
		t.Fatalf("expected 10 dirs after GC, got %d: %v", len(dirs), dirs)
	}
	// Los 5 mas viejos (run-00..run-04) debieron borrarse.
	for i := 0; i < 5; i++ {
		path := filepath.Join(slugDir, fmt.Sprintf("run-%02d", i))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected run-%02d to be removed, stat err=%v", i, err)
		}
	}
	// El mas nuevo (run-14) sigue.
	if _, err := os.Stat(filepath.Join(slugDir, "run-14")); err != nil {
		t.Errorf("expected run-14 to survive, stat err=%v", err)
	}
}

// TestGCRunHistory_CapZeroDisables verifica que cap=0 actua como "no GC":
// con 5 dirs preexistentes y cap=0, todos sobreviven.
func TestGCRunHistory_CapZeroDisables(t *testing.T) {
	t.Parallel()
	slugDir := t.TempDir()
	for i := 0; i < 5; i++ {
		dir := filepath.Join(slugDir, fmt.Sprintf("run-%02d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := gcRunHistory(slugDir, 0); err != nil {
		t.Fatalf("gcRunHistory cap=0: %v", err)
	}
	entries, _ := os.ReadDir(slugDir)
	if len(entries) != 5 {
		t.Errorf("expected 5 dirs to survive cap=0, got %d", len(entries))
	}
}

// TestRecoverInterruptedRuns_OldRunningGetsRewritten verifica el camino
// happy de recovery: un manifest con status:running y started_at > 1h se
// reescribe a interrupted.
func TestRecoverInterruptedRuns_OldRunningGetsRewritten(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	runDir := filepath.Join(home, ".che", "runs", "p", "r1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := Manifest{
		RunID:     "r1",
		Pipeline:  "p",
		Status:    ManifestStatusRunning,
		StartedAt: time.Now().Add(-2 * time.Hour).UTC(),
	}
	if err := writeManifest(runDir, stale); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := RecoverInterruptedRuns(home); err != nil {
		t.Fatalf("RecoverInterruptedRuns: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(runDir, "manifest.yaml"))
	var rewritten Manifest
	if err := yaml.Unmarshal(data, &rewritten); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rewritten.Status != ManifestStatusInterrupted {
		t.Errorf("expected status=interrupted, got %q\nraw:\n%s", rewritten.Status, data)
	}
	if rewritten.FinishedAt.IsZero() {
		t.Errorf("expected finished_at populated post-recovery")
	}
}

// TestRecoverInterruptedRuns_RecentRunningSurvives verifica la guard
// defensiva: un manifest con status:running pero started_at < 1h queda
// intacto (puede ser otro proceso de che corriendo en paralelo).
func TestRecoverInterruptedRuns_RecentRunningSurvives(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	runDir := filepath.Join(home, ".che", "runs", "p", "r1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fresh := Manifest{
		RunID:     "r1",
		Pipeline:  "p",
		Status:    ManifestStatusRunning,
		StartedAt: time.Now().Add(-5 * time.Minute).UTC(),
	}
	if err := writeManifest(runDir, fresh); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	if err := RecoverInterruptedRuns(home); err != nil {
		t.Fatalf("RecoverInterruptedRuns: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(runDir, "manifest.yaml"))
	var rewritten Manifest
	_ = yaml.Unmarshal(data, &rewritten)
	if rewritten.Status != ManifestStatusRunning {
		t.Errorf("expected status:running to survive (started_at < 1h), got %q", rewritten.Status)
	}
}

// TestRecoverInterruptedRuns_TerminalUntouched verifica que un manifest ya
// terminal (done/failed/cancelled) no se toca aunque sea viejo.
func TestRecoverInterruptedRuns_TerminalUntouched(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	runDir := filepath.Join(home, ".che", "runs", "p", "r1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	terminal := Manifest{
		RunID:     "r1",
		Pipeline:  "p",
		Status:    ManifestStatusDone,
		StartedAt: time.Now().Add(-72 * time.Hour).UTC(),
	}
	if err := writeManifest(runDir, terminal); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	if err := RecoverInterruptedRuns(home); err != nil {
		t.Fatalf("RecoverInterruptedRuns: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(runDir, "manifest.yaml"))
	var rewritten Manifest
	_ = yaml.Unmarshal(data, &rewritten)
	if rewritten.Status != ManifestStatusDone {
		t.Errorf("expected status:done untouched, got %q", rewritten.Status)
	}
}

// TestRunHistoryCap_EnvOverride verifica el override por
// CHE_RUN_HISTORY: valor numerico ≥0 reemplaza al default; valor vacio /
// invalido cae al default.
func TestRunHistoryCap_EnvOverride(t *testing.T) {
	t.Setenv("CHE_RUN_HISTORY", "3")
	if got := runHistoryCap(); got != 3 {
		t.Errorf("expected cap=3, got %d", got)
	}
	t.Setenv("CHE_RUN_HISTORY", "0")
	if got := runHistoryCap(); got != 0 {
		t.Errorf("expected cap=0 (disabled), got %d", got)
	}
	t.Setenv("CHE_RUN_HISTORY", "garbage")
	if got := runHistoryCap(); got != runHistoryDefault {
		t.Errorf("expected fallback to default with garbage, got %d", got)
	}
}
