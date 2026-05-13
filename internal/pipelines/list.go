package pipelines

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chichex/che/internal/wizard"
)

// List enumera todos los pipelines visibles desde cwd+home. Devuelve una
// slice ordenada por slug. En colisiones la regla es:
//   - project gana sobre global y builtin (cwd-local override)
//   - global gana sobre builtin (FS-shadow del builtin)
//
// Archivos corruptos en disco se logean y se skipean — listar igual no
// se vuela porque uno este roto. Builtins que fallan al parsear SI
// rompen el listado (bug del binario).
//
// Si cwd=="" se omite project; si home no se puede resolver se omite
// global. En ambos casos el listado degrada limpiamente.
func List(cwd, home string) ([]Resolved, error) {
	gdir, err := GlobalPipelinesDir(home)
	if err != nil {
		gdir = ""
	}
	return ListInDirs(cwd, gdir)
}

// ListInDirs es la version baja-nivel de List: toma el dir global ya
// resuelto en vez de home. cwd="" desactiva scope project,
// globalDir="" desactiva scope global; en ambos casos los builtins
// quedan siempre disponibles.
func ListInDirs(cwd, globalDir string) ([]Resolved, error) {
	seen := make(map[string]struct{})
	out := make([]Resolved, 0, 8)

	// 1. Project (cwd-local) — primero asi gana en colisiones.
	if projectDir := ProjectPipelinesDir(cwd); projectDir != "" {
		entries := loadFromDir(projectDir)
		for _, e := range entries {
			seen[e.slug] = struct{}{}
			out = append(out, Resolved{
				Slug:     e.slug,
				Pipeline: e.pipeline,
				Scope:    ScopeProject,
				Target:   e.path,
			})
		}
	}

	// 2. Global.
	if globalDir != "" {
		entries := loadFromDir(globalDir)
		for _, e := range entries {
			if _, dup := seen[e.slug]; dup {
				continue
			}
			seen[e.slug] = struct{}{}
			out = append(out, Resolved{
				Slug:     e.slug,
				Pipeline: e.pipeline,
				Scope:    ScopeGlobal,
				Target:   e.path,
			})
		}
	}

	// 3. Builtins.
	builtins, err := wizard.Builtins()
	if err != nil {
		return nil, err
	}
	for _, b := range builtins {
		if _, dup := seen[b.Slug]; dup {
			continue
		}
		seen[b.Slug] = struct{}{}
		out = append(out, Resolved{
			Slug:     b.Slug,
			Pipeline: b.Pipeline,
			Scope:    ScopeBuiltin,
			Target:   wizard.BuiltinTargetPrefix + b.Slug,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

type dirEntry struct {
	slug     string
	path     string
	pipeline wizard.Pipeline
}

// loadFromDir lee *.yaml de dir y devuelve los pipelines parseados.
// Errores de IO (readdir o load) se logean con prefijo [pipelines] y se
// skipean. Dir inexistente se trata como "lista vacia" sin log.
func loadFromDir(dir string) []dirEntry {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[pipelines] readdir %s: %v", dir, err)
		}
		return nil
	}
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(dir, name)
		p, err := wizard.Load(path)
		if err != nil {
			log.Printf("[pipelines] load %s: %v", path, err)
			continue
		}
		out = append(out, dirEntry{
			slug:     strings.TrimSuffix(name, ".yaml"),
			path:     path,
			pipeline: p,
		})
	}
	return out
}
