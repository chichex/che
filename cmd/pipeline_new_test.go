package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// TestPipelineNew_CreatesDefault: en repo limpio, escribe el built-in
// como `.che/pipelines/<name>.json` y el archivo parsea + matchea
// pipeline.Default().
func TestPipelineNew_CreatesDefault(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	var out bytes.Buffer
	if err := runPipelineNew(&out, mgr, "mine", false); err != nil {
		t.Fatalf("runPipelineNew: %v", err)
	}
	dest := filepath.Join(root, ".che", "pipelines", "mine.json")
	got, err := pipeline.Load(dest)
	if err != nil {
		t.Fatalf("Load %s: %v", dest, err)
	}
	if !reflect.DeepEqual(got, pipeline.Default()) {
		t.Errorf("contenido drifteó vs Default()")
	}
	if !strings.Contains(out.String(), "mine.json") {
		t.Errorf("stdout no menciona el path: %q", out.String())
	}
}

// TestPipelineNew_ConflictWithoutForce: archivo ya existe, sin --force →
// error.
func TestPipelineNew_ConflictWithoutForce(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{
		"taken": minimalPipeline,
	}, "")
	dest := filepath.Join(root, ".che", "pipelines", "taken.json")
	infoBefore, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	var out bytes.Buffer
	err = runPipelineNew(&out, mgr, "taken", false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "ya existe") {
		t.Errorf("error no menciona conflicto: %v", err)
	}
	// El archivo no debe haberse tocado.
	infoAfter, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if infoBefore.Size() != infoAfter.Size() {
		t.Errorf("tamaño cambió en path de conflicto")
	}
}

// TestPipelineNew_ForceOverwrites: con --force sobrescribe.
func TestPipelineNew_ForceOverwrites(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{
		"taken": minimalPipeline,
	}, "")
	var out bytes.Buffer
	if err := runPipelineNew(&out, mgr, "taken", true); err != nil {
		t.Fatalf("runPipelineNew --force: %v", err)
	}
	dest := filepath.Join(root, ".che", "pipelines", "taken.json")
	got, err := pipeline.Load(dest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, pipeline.Default()) {
		t.Errorf("contenido tras --force no es Default()")
	}
}
