// Package pipelines centraliza la resolucion, listado y persistencia
// de pipelines de che. Soporta visibilidad dual: pipelines de proyecto
// (cwd-local en `./.che/pipelines/`) y globales (`~/.che/pipelines/`).
// Los builtins embebidos del binario quedan como ultimo fallback.
//
// El scope NO se persiste dentro del YAML — la ubicacion en filesystem
// es la unica fuente de verdad.
package pipelines

// Scope identifica el origen de un pipeline: project, global o builtin.
// El scope es implicito en la ubicacion en filesystem.
type Scope int

const (
	// ScopeUnknown es el zero-value; usado como "no encontrado".
	ScopeUnknown Scope = iota
	// ScopeProject = pipeline cargado desde `./.che/pipelines/` (cwd-local).
	ScopeProject
	// ScopeGlobal = pipeline cargado desde `~/.che/pipelines/`.
	ScopeGlobal
	// ScopeBuiltin = pipeline embebido en el binario.
	ScopeBuiltin
)

// String devuelve la forma serializable (json-safe) del scope.
// "" para ScopeUnknown — el JSON omite el campo via omitempty.
func (s Scope) String() string {
	switch s {
	case ScopeProject:
		return "project"
	case ScopeGlobal:
		return "global"
	case ScopeBuiltin:
		return "builtin"
	}
	return ""
}
