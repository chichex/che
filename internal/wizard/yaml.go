package wizard

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Marshal serializa un Pipeline al formato canonico YAML. Pipelines ready
// no llevan bloque `status` (Status nil). Drafts si.
func Marshal(p Pipeline) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&p); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("wizard: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("wizard: marshal close: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parsea un Pipeline. Errores tipicos: YAML malformado, status
// con stage desconocido (no se valida aca — quien llama decide).
func Unmarshal(data []byte) (Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Pipeline{}, fmt.Errorf("wizard: unmarshal: %w", err)
	}
	return p, nil
}
