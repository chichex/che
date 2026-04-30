package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// readConfig recarga `.che/pipelines.config.json` del repo de prueba.
// Útil para verificar el efecto de runPipelineUse.
func readConfig(t *testing.T, root string) pipeline.Config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".che", "pipelines.config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg pipeline.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

// TestPipelineUse_WritesConfig: pipeline existe on-disk → escribe
// config.json con default = name.
func TestPipelineUse_WritesConfig(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "")
	var out bytes.Buffer
	if err := runPipelineUse(&out, mgr, "fast"); err != nil {
		t.Fatalf("runPipelineUse: %v", err)
	}
	cfg := readConfig(t, root)
	if cfg.Default != "fast" {
		t.Errorf("Default = %q, want fast", cfg.Default)
	}
	if cfg.Version != pipeline.ConfigVersion {
		t.Errorf("Version = %d, want %d", cfg.Version, pipeline.ConfigVersion)
	}
	if !strings.Contains(out.String(), "fast") {
		t.Errorf("stdout no menciona el cambio: %q", out.String())
	}
}

// TestPipelineUse_Idempotent: si el config ya marca <name>, no reescribe
// (mtime invariante) y reporta no-op.
func TestPipelineUse_Idempotent(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{
		"fast": minimalPipeline,
	}, "fast")
	cfgPath := filepath.Join(root, ".che", "pipelines.config.json")
	infoBefore, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	var out bytes.Buffer
	if err := runPipelineUse(&out, mgr, "fast"); err != nil {
		t.Fatalf("runPipelineUse: %v", err)
	}
	if !strings.Contains(out.String(), "no-op") {
		t.Errorf("no reporta no-op: %q", out.String())
	}
	infoAfter, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Errorf("mtime cambió en idempotent path")
	}
}

// TestPipelineUse_MissingPipeline: nombre que no existe (y no es
// default) → error explícito.
func TestPipelineUse_MissingPipeline(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	err := runPipelineUse(&out, mgr, "nope")
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error no menciona el nombre: %v", err)
	}
}

// TestPipelineUse_BuiltinDefaultAllowed: name=default sin archivo
// on-disk debe ser válido (built-in implícito).
func TestPipelineUse_BuiltinDefaultAllowed(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	if err := runPipelineUse(&out, mgr, "default"); err != nil {
		t.Fatalf("runPipelineUse: %v", err)
	}
	cfg := readConfig(t, root)
	if cfg.Default != "default" {
		t.Errorf("Default = %q, want default", cfg.Default)
	}
}

// TestPipelineUse_BrokenPipelineRejected: pipeline on-disk roto → use
// se rehúsa.
func TestPipelineUse_BrokenPipelineRejected(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{
		"broken": `{"version": 1, "steps": []}`, // 0 steps → invalid
	}, "")
	var out bytes.Buffer
	err := runPipelineUse(&out, mgr, "broken")
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
}
