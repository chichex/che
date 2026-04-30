// Package agentregistry descubre los subagents de Claude Code que viven
// en las 4 ubicaciones oficiales (managed, project, user, plugins) y los
// expone junto con los 3 built-in de che (claude-opus / claude-sonnet /
// claude-haiku) bajo un único registro deduplicado por precedencia.
//
// Es la base de los pipelines configurables (§2 del PRD #50): los steps
// referencian agentes por nombre y el orquestador resuelve nombre →
// definición vía Registry.Get. che no inspecciona ni invoca los agentes
// custom directamente — eso lo hace Claude Code; che sólo lee `name`,
// `description` y `model` para listar y dispatchear.
package agentregistry

// Source identifica de dónde viene la definición de un agente. Se usa
// para mostrar origen en `che agents list` y para resolver colisiones
// según la precedencia de §2.a del PRD.
type Source string

const (
	// SourceManaged: directorio gestionado por org admin (más alta).
	SourceManaged Source = "managed"
	// SourceProject: `.claude/agents/` descubierto walking up desde CWD.
	SourceProject Source = "project"
	// SourceUser: `~/.claude/agents/` (todos los proyectos del usuario).
	SourceUser Source = "user"
	// SourcePlugin: subagent provisto por un plugin instalado.
	SourcePlugin Source = "plugin"
	// SourceBuiltin: built-in de che (claude-opus/sonnet/haiku).
	SourceBuiltin Source = "built-in"
)

// sourceRank devuelve la prioridad de una Source: menor = gana. Matchea
// la tabla de §2.a (managed > project > user > plugin > built-in).
func sourceRank(s Source) int {
	switch s {
	case SourceManaged:
		return 1
	case SourceProject:
		return 2
	case SourceUser:
		return 3
	case SourcePlugin:
		return 4
	case SourceBuiltin:
		return 5
	}
	return 99
}

// Agent es la definición de un subagent listo para mostrar/invocar.
//
// Para plugin subagents Name lleva el namespace canónico
// `plugin:name` (o `plugin:subdir:name` si hay subdirs); para el resto
// Name = el campo `name` del frontmatter (o el id del built-in).
type Agent struct {
	Name        string
	Description string
	// Model: opus | sonnet | haiku | inherit (vacío si el frontmatter
	// no lo declaró). che lo respeta sin override.
	Model  string
	Source Source
	// Path al archivo .md de definición. Vacío para built-ins.
	Path string
}
