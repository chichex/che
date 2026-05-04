package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/chichex/che/internal/pipeline"
)

// savePipelineFile serializa un pipeline a JSON indentado y lo escribe
// en path, creando el dir padre si falta.
func savePipelineFile(path string, p pipeline.Pipeline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
