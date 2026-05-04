package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

func TestSavePipelineFile_RejectsDestinationSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	dest := filepath.Join(dir, "pipeline.json")
	if err := os.Symlink(target, dest); err != nil {
		t.Skipf("symlink no soportado: %v", err)
	}

	err := savePipelineFile(dest, pipeline.Default())
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want mention symlink", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink target fue modificado: %q", data)
	}
}

func TestSavePipelineFile_AtomicRenameCleansTemp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "pipeline.json")
	if err := savePipelineFile(dest, pipeline.Default()); err != nil {
		t.Fatalf("savePipelineFile: %v", err)
	}
	if _, err := pipeline.Load(dest); err != nil {
		t.Fatalf("Load: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temp file quedó en destino: %s", entry.Name())
		}
	}
}
