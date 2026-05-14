package pipelines

import (
	"os"
	"path/filepath"
)

// pipelinesSubdir es el subdir relativo (sea bajo HOME o bajo cwd) donde
// viven los archivos `.yaml`. Constante compartida entre project y global
// para mantener simetria filesystem-local.
const pipelinesSubdir = ".che/pipelines"

// GlobalPipelinesDir devuelve el directorio absoluto donde viven los
// pipelines globales del usuario (`~/.che/pipelines/`). home="" cae a
// os.UserHomeDir(); error si HOME no se puede resolver.
func GlobalPipelinesDir(home string) (string, error) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = h
	}
	return filepath.Join(home, pipelinesSubdir), nil
}

// ProjectPipelinesDir devuelve el directorio del scope project relativo al
// cwd indicado (`<cwd>/.che/pipelines/`). cwd="" no es valido aca — el
// caller debe resolver el cwd una sola vez al startup. Devolvemos "" en ese
// caso para que Resolve/List interpreten "scope project no disponible".
func ProjectPipelinesDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, pipelinesSubdir)
}

// FindProjectRoot busca el ancestro mas cercano del cwd dado que tenga un
// directorio `.che/pipelines/` adentro. Devuelve el cwd "efectivo" para
// scope project — empezando por el cwd mismo y subiendo hasta encontrar
// uno. Si ninguno tiene `.che/pipelines/`, devuelve cwd tal cual (el
// caller mantiene el comportamiento previo: scope project vacio).
//
// Sirve para que `che dash` arrancado desde una subcarpeta de un proyecto
// igual vea los pipelines guardados en `<root>/.che/pipelines/` sin
// obligar al usuario a hacer `cd` exacto. cwd="" devuelve "".
func FindProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, pipelinesSubdir)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// llegamos a la raiz sin encontrar nada.
			return cwd
		}
		dir = parent
	}
}

// GlobalPathFor compone el path absoluto del archivo del pipeline de scope
// global cuyo slug es slug.
func GlobalPathFor(home, slug string) (string, error) {
	dir, err := GlobalPipelinesDir(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, slug+".yaml"), nil
}

// ProjectPathFor compone el path absoluto del archivo del pipeline de scope
// project. Si cwd="" devuelve "" — usar GlobalPathFor en ese caso.
func ProjectPathFor(cwd, slug string) string {
	dir := ProjectPipelinesDir(cwd)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, slug+".yaml")
}

// PathForScope unifica el calculo de path segun scope. ScopeBuiltin no
// tiene path en disco — devolvemos "".
func PathForScope(scope Scope, cwd, home, slug string) (string, error) {
	switch scope {
	case ScopeProject:
		p := ProjectPathFor(cwd, slug)
		if p == "" {
			return "", errEmptyCwd
		}
		return p, nil
	case ScopeGlobal:
		return GlobalPathFor(home, slug)
	}
	return "", nil
}
