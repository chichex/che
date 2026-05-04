// Package pipeline define el modelo declarativo de los pipelines de che:
// la representación on-disk (`.che/pipelines/*.json`) que reemplaza a los
// flows hardcodeados (idea→explore→validate→execute→validate→close) por una
// secuencia configurable de steps con agentes.
//
// Este paquete sólo expone los *tipos* + un `Default()` que reproduce el
// comportamiento actual en formato pipeline. La carga desde disco, la
// validación contra el schema, el motor de ejecución y la integración con
// labels viven en PRs siguientes (ver issue #50, plan §10).
//
// El JSON Schema canónico está en `schemas/pipeline.json`. Los tipos de Go
// son la fuente de verdad para serialización: editar acá implica regenerar
// el schema y los goldens.
package pipeline

// CurrentVersion es la única versión del schema soportada en v1. El loader
// debe rechazar pipelines con versiones distintas pidiendo upgrade — no hay
// auto-migración.
const CurrentVersion = 1

// Pipeline es la representación on-disk de un workflow. Mapea 1:1 a un
// archivo `.che/pipelines/<name>.json`. El nombre del pipeline no vive en
// la struct: lo provee el filename.
type Pipeline struct {
	// Schema es opcional y existe sólo para tooling de editores. El loader lo
	// ignora en runtime, pero debe aceptarlo porque schemas/pipeline.json lo
	// documenta como forma recomendada para autocomplete.
	Schema string `json:"$schema,omitempty"`

	// Version siempre debe ser CurrentVersion en v1. El loader rechaza
	// otras versiones; mantener la verificación explícita en cada llamada
	// permite mensajes de error puntuales.
	Version int `json:"version"`

	// Entry es opcional. Si está presente, sus agentes corren antes de
	// los steps y emiten un marker que define desde qué step arrancar
	// (`[goto: X]`) o si rebotar el input (`[stop]`). Sin entry, che
	// arranca siempre desde el primer step. Ver PRD §5.a.
	Entry *Entry `json:"entry,omitempty"`

	// Steps es la secuencia ordenada de stages que ejecuta el motor.
	// Los saltos (forward o backward) los decide el agente vía marker
	// `[goto: <step_name>]`; no hay metadata de control de flujo en el
	// JSON. v1 rechaza pipelines vacíos: al menos un step.
	Steps []Step `json:"steps"`
}

// Step es un stage del pipeline. Si len(Agents) > 1, los agentes corren en
// paralelo y el aggregator decide cómo resolver markers en conflicto. Un
// agente repetido en la lista equivale a N instancias paralelas — útil para
// estrategias tipo "best-of-3 con el mismo modelo".
type Step struct {
	// Name es identificador libre dentro del pipeline. Los agentes lo
	// referencian en sus markers `[goto: <name>]`. Debe matchear el
	// fragmento `[a-z_][a-z0-9_]*` para coincidir con el regex del parser
	// de markers (PRD §3.c).
	Name string `json:"name"`

	// Agents es la lista de refs a agentes built-in (claude-opus/sonnet/
	// haiku) o custom (auto-descubiertos en `.claude/agents/`). El motor
	// los corre en paralelo si son >1. v1 exige al menos uno.
	Agents []string `json:"agents"`

	// Aggregator selecciona la política para resolver markers cuando
	// len(Agents) > 1. Si está vacío, el motor aplica AggregatorMajority
	// (default conservador). Para 1 agente, el campo se ignora — la
	// validación lo deja pasar para no forzar al wizard a quitarlo al
	// alternar entre 1 y N agentes.
	Aggregator Aggregator `json:"aggregator,omitempty"`

	// Comment es texto libre opcional que el wizard inyecta al generar
	// pipelines (PRD §7.f). El loader lo ignora — sólo sirve para que el
	// usuario que reabre el archivo entienda qué hace el step.
	Comment string `json:"_comment,omitempty"`
}

// Entry corre antes de los steps y decide step inicial o rebote. Comparte la
// semántica de aggregator con Step para validators críticos del input
// (ej. multi-agente en `unanimous` para gates de seguridad).
type Entry struct {
	Agents     []string   `json:"agents"`
	Aggregator Aggregator `json:"aggregator,omitempty"`
}

// Aggregator es la política de resolución de markers cuando un step (o el
// entry) corre con >1 agente. v1 expone 3 presets fijos — agregar uno nuevo
// requiere bumpear CurrentVersion.
type Aggregator string

const (
	// AggregatorMajority es el default. Si alguno dice [stop], gana
	// [stop]; sino, gana el marker más votado; en empate sin mayoría
	// clara, [stop] (conservador). Para validators generales donde
	// confiás en la diversidad de opiniones.
	AggregatorMajority Aggregator = "majority"

	// AggregatorUnanimous exige que todos los agentes coincidan
	// exactamente (mismo marker, mismo destino). Cualquier divergencia
	// → [stop]. Para gates críticos (security, compliance) donde un
	// solo "no" basta para frenar.
	AggregatorUnanimous Aggregator = "unanimous"

	// AggregatorFirstBlocker: si alguno [stop] gana [stop]; si alguno
	// [goto: X] gana ése (y si hay varios distintos, [stop]); si todos
	// [next], [next]. Para pipelines donde "cualquier issue señala
	// problema" — un solo agente que pida revisión es suficiente.
	AggregatorFirstBlocker Aggregator = "first_blocker"
)

// ValidAggregators lista los presets soportados en orden canónico (preserva
// el orden mostrado en `che pipeline create` y en el editor visual del dash).
var ValidAggregators = []Aggregator{
	AggregatorMajority,
	AggregatorUnanimous,
	AggregatorFirstBlocker,
}

// IsValid reporta si a es uno de los presets soportados. El string vacío NO
// es válido como valor explícito; el caller que quiera "usar default" tiene
// que pasar AggregatorMajority directamente o dejar el campo cero en JSON
// (omitempty) y resolverlo en el motor.
func (a Aggregator) IsValid() bool {
	for _, v := range ValidAggregators {
		if a == v {
			return true
		}
	}
	return false
}

// Description devuelve la descripción corta canónica para UI/prompts.
func (a Aggregator) Description() string {
	switch a {
	case AggregatorMajority:
		return "gana el marker más votado; empates paran"
	case AggregatorUnanimous:
		return "todos deben coincidir; divergencias paran"
	case AggregatorFirstBlocker:
		return "cualquier stop bloquea; goto único avanza"
	default:
		return ""
	}
}
