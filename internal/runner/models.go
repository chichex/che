package runner

import "github.com/chichex/che/internal/runnermodels"

// DefaultModel es un re-export thin sobre runnermodels.DefaultModel — la
// tabla de modelos vive en runnermodels para que el wizard pueda
// importarla sin crear un ciclo (runner ya importa wizard).
func DefaultModel(cli string) string {
	return runnermodels.DefaultModel(cli)
}

// ValidateModel re-exporta runnermodels.ValidateModel para no romper los
// call sites internos del runner (preflight).
func ValidateModel(cli, model string) error {
	return runnermodels.ValidateModel(cli, model)
}
