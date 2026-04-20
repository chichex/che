package labels

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHardcodedLabelsOutsideThisPackage camina el árbol del módulo y falla
// si aparecen strings literales de labels (`"ct:plan"`, `"status:plan"`, …)
// en código de producción fuera de `internal/labels`. La única fuente de
// verdad son las constantes de este paquete — si mañana renombramos
// `status:plan`, hay que cambiarlo acá y nada más.
//
// Scope del check:
//   - solo archivos `.go`,
//   - excluye `_test.go` (fixtures pueden usar strings literales),
//   - excluye `internal/labels` (este paquete es la fuente de verdad).
func TestNoHardcodedLabelsOutsideThisPackage(t *testing.T) {
	root := moduleRoot(t)

	forbidden := []string{
		`"` + StatusIdea + `"`,
		`"` + StatusPlan + `"`,
		`"` + StatusExecuting + `"`,
		`"` + StatusExecuted + `"`,
		`"` + StatusAwaitingHuman + `"`,
		`"` + CtPlan + `"`,
	}

	var violations []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Saltamos dot-dirs (.git, .worktrees, .github) y este paquete.
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if name == "labels" && filepath.Base(filepath.Dir(path)) == "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, lit := range forbidden {
			if strings.Contains(content, lit) {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, rel+": contiene "+lit)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking tree: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("labels hardcoded fuera de internal/labels — usá las constantes del paquete:\n  %s",
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
