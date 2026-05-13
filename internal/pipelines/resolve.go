package pipelines

import (
	"os"
	"path/filepath"

	"github.com/chichex/che/internal/wizard"
)

// Resolved es el resultado materializado de Resolve / List: pipeline
// parseado + scope + target serializable para el runner. target es el
// path absoluto en disco cuando scope es Project/Global, o
// "builtin:<slug>" cuando scope es Builtin (mismo formato que entiende
// runner.StartHeadless).
type Resolved struct {
	Slug     string
	Pipeline wizard.Pipeline
	Scope    Scope
	Target   string
}

// Resolve busca un pipeline por slug en el orden project → global →
// builtin. Si project y global definen el mismo slug, gana project.
// cwd="" salta el scope project (degradacion silenciosa).
//
// Devuelve found=false si el slug no existe en ningun scope. Errores de
// IO/parse (distintos a "no existe") se propagan tal cual para que el
// caller logee con contexto.
//
// home="" cae a os.UserHomeDir; si falla, scope global queda deshabilitado.
func Resolve(cwd, home, slug string) (Resolved, bool, error) {
	gdir, err := GlobalPipelinesDir(home)
	if err != nil {
		gdir = ""
	}
	return ResolveInDirs(cwd, gdir, slug)
}

// ResolveInDirs es la version baja-nivel de Resolve: toma el directorio
// global ya resuelto (en vez de home). Util para callers que ya tienen
// el dir absoluto cacheado al startup (ej. el dash). cwd="" desactiva
// scope project; globalDir="" desactiva scope global; en ambos casos
// la cascada degrada a builtins.
func ResolveInDirs(cwd, globalDir, slug string) (Resolved, bool, error) {
	if slug == "" {
		return Resolved{}, false, nil
	}

	// 1. Scope project — cwd-local, gana sobre global y builtin.
	if path := ProjectPathFor(cwd, slug); path != "" {
		p, err := tryLoadFromFS(path)
		if err != nil {
			return Resolved{}, false, err
		}
		if p != nil {
			return Resolved{
				Slug:     slug,
				Pipeline: *p,
				Scope:    ScopeProject,
				Target:   path,
			}, true, nil
		}
	}

	// 2. Scope global — home-local, gana sobre builtin.
	if globalDir != "" {
		gpath := filepath.Join(globalDir, slug+".yaml")
		p, err := tryLoadFromFS(gpath)
		if err != nil {
			return Resolved{}, false, err
		}
		if p != nil {
			return Resolved{
				Slug:     slug,
				Pipeline: *p,
				Scope:    ScopeGlobal,
				Target:   gpath,
			}, true, nil
		}
	}

	// 3. Builtin — ultimo fallback.
	b, berr := wizard.BuiltinBySlug(slug)
	if berr != nil {
		return Resolved{}, false, berr
	}
	if b == nil {
		return Resolved{}, false, nil
	}
	return Resolved{
		Slug:     slug,
		Pipeline: b.Pipeline,
		Scope:    ScopeBuiltin,
		Target:   wizard.BuiltinTargetPrefix + slug,
	}, true, nil
}

// tryLoadFromFS lee y parsea un pipeline del path. Devuelve (nil, nil)
// si el archivo no existe (caso esperado durante la cascada de
// resolucion). Otros errores se propagan.
func tryLoadFromFS(path string) (*wizard.Pipeline, error) {
	p, err := wizard.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}
