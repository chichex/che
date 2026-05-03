package labels

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/pipelinelabels"
)

// TestNoHardcodedLabelsOutsideThisPackage camina el árbol del módulo y falla
// si aparecen strings literales de labels (`"ct:plan"`, `"che:plan"`, …)
// en código de producción fuera de `internal/labels`. La única fuente de
// verdad son las constantes de este paquete — si mañana renombramos
// `che:plan`, hay que cambiarlo acá y nada más.
//
// Scope del check:
//   - solo archivos `.go`,
//   - excluye `_test.go` (fixtures pueden usar strings literales),
//   - excluye `internal/labels` (este paquete es la fuente de verdad),
//   - excluye `cmd/migrate_labels*.go` — ese subcomando hace migración
//     in-place de los `status:*` viejos a `che:*` nuevos, así que los
//     literales `"status:idea"` etc. son su input, no uso runtime.
func TestNoHardcodedLabelsOutsideThisPackage(t *testing.T) {
	root := moduleRoot(t)

	forbidden := []string{
		`"` + CtPlan + `"`,
		// Máquina (prefix `che:*`).
		`"` + CheIdea + `"`,
		`"` + ChePlanning + `"`,
		`"` + ChePlan + `"`,
		`"` + CheExecuting + `"`,
		`"` + CheExecuted + `"`,
		`"` + CheValidating + `"`,
		`"` + CheValidated + `"`,
		`"` + CheClosing + `"`,
		`"` + CheClosed + `"`,
	}

	// Modelo v2 — los 9 estados nuevos (`che:state:*` / `che:state:applying:*`)
	// solo deben aparecer literales en `internal/pipelinelabels` (su fuente
	// de verdad). El resto del codebase debe usar `pipelinelabels.State*`.
	forbiddenV2 := []string{
		`"` + pipelinelabels.StateIdea + `"`,
		`"` + pipelinelabels.StateApplyingExplore + `"`,
		`"` + pipelinelabels.StateExplore + `"`,
		`"` + pipelinelabels.StateApplyingExecute + `"`,
		`"` + pipelinelabels.StateExecute + `"`,
		`"` + pipelinelabels.StateApplyingValidatePR + `"`,
		`"` + pipelinelabels.StateValidatePR + `"`,
		`"` + pipelinelabels.StateApplyingClose + `"`,
		`"` + pipelinelabels.StateClose + `"`,
	}

	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Saltamos dot-dirs (.git, .worktrees, .github) y los dos paquetes
			// que son fuente de verdad de labels.
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			parent := filepath.Base(filepath.Dir(path))
			if parent == "internal" && (name == "labels" || name == "pipelinelabels") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip cmd/migrate_labels*.go: el subcomando de migración usa
		// literales `"status:*"` como input — son entrada, no runtime.
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, "cmd/migrate_labels") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, lit := range forbidden {
			if strings.Contains(content, lit) {
				violations = append(violations, rel+": contiene "+lit)
			}
		}
		for _, lit := range forbiddenV2 {
			if strings.Contains(content, lit) {
				violations = append(violations, rel+": contiene "+lit+" (usá pipelinelabels.State*)")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking tree: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("labels hardcoded fuera de internal/labels|internal/pipelinelabels — usá las constantes del paquete:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// moduleRoot sube desde el cwd hasta encontrar go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no encontré go.mod subiendo desde cwd")
		}
		dir = parent
	}
}
