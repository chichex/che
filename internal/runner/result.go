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

// readStepResult lee el step-NN.result.yaml del step idx (1-indexed) en
// runDir. H6 lo usa para resolver `input: previous_output` del step N+1: el
// payload del subprocess siguiente es el campo Output del result anterior.
//
// Si el archivo no existe o no parsea, devuelve error — el caller decide si
// es fatal (R3 lo trata como fatal y va a RF: no podemos arrancar el step
// sin el input que pidio).
func readStepResult(runDir string, idx int) (StepResult, error) {
	path := filepath.Join(runDir, fmt.Sprintf("step-%02d.result.yaml", idx))
	data, err := os.ReadFile(path)
	if err != nil {
		return StepResult{}, fmt.Errorf("result: read %s: %w", path, err)
	}
	var r StepResult
	if err := yaml.Unmarshal(data, &r); err != nil {
		return StepResult{}, fmt.Errorf("result: parse %s: %w", path, err)
	}
	return r, nil
}
