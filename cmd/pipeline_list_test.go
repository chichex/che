package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// pipelineFixture es un helper para los tests de subcomandos: arma un
// repo root temporal con un layout ya populado (`.che/pipelines/*.json`
// + opcionalmente un config) y devuelve el Manager + el dir.
//
// No depende de `git`: el Manager toma cualquier path como repo root
// — eso libera a los tests de spawnar procesos.
func pipelineFixture(t *testing.T, files map[string]string, configDefault string) (*pipeline.Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if configDefault != "" {
		cfg := `{"version": 1, "default": "` + configDefault + `"}`
		if err := os.WriteFile(filepath.Join(tmp, ".che", "pipelines.config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	mgr, err := pipeline.NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, tmp
}

const minimalPipeline = `{
  "version": 1,
  "steps": [
    {"name": "explore", "agents": ["claude-opus"]}
  ]
}`

// TestPipelineList_Empty: repo sin pipelines. stdout vacío + mensaje
// informativo en stderr; exit OK.
func TestPipelineList_Empty(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out, errOut bytes.Buffer
	if err := runPipelineList(&out, &errOut, mgr); err != nil {
		t.Fatalf("runPipelineList: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "no hay pipelines") {
		t.Errorf("stderr no menciona ausencia de pipelines: %q", errOut.String())
	}
}

// TestPipelineList_WithDefault: dos pipelines, uno marcado default. La
// columna DEFAULT debe tener "*" sólo en la fila correspondiente.
func TestPipelineList_WithDefault(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast":     minimalPipeline,
		"thorough": minimalPipeline,
	}, "fast")
	var out, errOut bytes.Buffer
	if err := runPipelineList(&out, &errOut, mgr); err != nil {
		t.Fatalf("runPipelineList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "NAME") || !strings.Contains(got, "DEFAULT") {
		t.Errorf("header missing: %q", got)
	}
	// orden alfabético: fast antes que thorough
	idxFast := strings.Index(got, "fast")
	idxThor := strings.Index(got, "thorough")
	if idxFast == -1 || idxThor == -1 || idxFast > idxThor {
		t.Errorf("orden no alfabético: %q", got)
	}
	// La línea de fast debe tener "*", la de thorough no.
	lines := strings.Split(got, "\n")
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "fast") {
			if !strings.Contains(ln, "*") {
				t.Errorf("línea fast sin marker default: %q", ln)
			}
		}
		if strings.HasPrefix(strings.TrimSpace(ln), "thorough") {
			if strings.Contains(ln, "*") {
				t.Errorf("línea thorough con marker default inesperado: %q", ln)
			}
		}
	}
}

// TestPipelineList_DefaultMissingOnDisk: config declara un default que
// no existe → warning en stderr.
func TestPipelineList_DefaultMissingOnDisk(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "missing")
	var out, errOut bytes.Buffer
	if err := runPipelineList(&out, &errOut, mgr); err != nil {
		t.Fatalf("runPipelineList: %v", err)
	}
	if !strings.Contains(errOut.String(), "warn") || !strings.Contains(errOut.String(), "missing") {
		t.Errorf("stderr no warnea sobre default inexistente: %q", errOut.String())
	}
}
