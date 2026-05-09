package runner

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// StepResult es el shape de step-NN.result.yaml — el "output efectivo" del
// step. H4 lo escribe SIEMPRE al terminar el subprocess (incluso en exit ≠ 0,
// el doc fija que `output` queda con lo que haya salido en stdout y
// `exit_code` con el codigo real).
//
// H6 va a leer este shape como fuente de previous_output del step siguiente.
type StepResult struct {
	StepIdx  int    `yaml:"step_idx"`
	StepName string `yaml:"step_name,omitempty"`
	ExitCode int    `yaml:"exit_code"`
	Output   string `yaml:"output"`
}

// writeStepResult serializa + escribe el archivo result.yaml del step. El
// nombre sigue la convencion step-NN.result.yaml (NN zero-padded a 2
// digitos para que el orden alfabetico = orden de ejecucion).
func writeStepResult(runDir string, r StepResult) error {
	data, err := yaml.Marshal(&r)
	if err != nil {
		return fmt.Errorf("result: marshal: %w", err)
	}
	path := filepath.Join(runDir, fmt.Sprintf("step-%02d.result.yaml", r.StepIdx))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("result: write %s: %w", path, err)
	}
	return nil
}
