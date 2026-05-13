package pipelines

import (
	"fmt"

	"github.com/chichex/che/internal/wizard"
)

// SaveScoped serializa el pipeline al scope indicado, devolviendo el path
// resultante. Para ScopeGlobal usa ~/.che/pipelines/<slug>.yaml; para
// ScopeProject usa <cwd>/.che/pipelines/<slug>.yaml. ScopeBuiltin no se
// puede persistir (los builtins viven embebidos) — devuelve error.
//
// slug debe estar pre-computado (wizard.Slug). El nombre/desc/steps del
// pipeline ya tienen que estar materializados — esta funcion delega a
// wizard.Save sin tocar el pipeline.
func SaveScoped(scope Scope, cwd, home, slug string, p wizard.Pipeline) (string, error) {
	if slug == "" {
		return "", fmt.Errorf("pipelines: slug vacio")
	}
	path, err := PathForScope(scope, cwd, home, slug)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("pipelines: scope %q no soporta save", scope)
	}
	if err := wizard.Save(path, p); err != nil {
		return "", err
	}
	return path, nil
}
