package pipeline

import (
	"fmt"
	"regexp"
)

// stepNameRe matchea el fragmento permitido para `Step.Name`.
//
// Tiene que coincidir con el regex del parser de markers (PRD §3.c):
// los agentes referencian steps via `[goto: <name>]`, así que el set
// permitido en el JSON tiene que ser un superset del lado parser. El
// schema (`schemas/pipeline.json`) declara el mismo pattern — si cambia
// uno, el otro también.
var stepNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Validate corre las verificaciones semánticas que `encoding/json` no
// cubre por sí solo: versión soportada, mínimo 1 step, nombres válidos,
// agentes no vacíos, aggregator si está presente debe ser uno de los 3
// presets, nombres de steps únicos.
//
// Devuelve *LoadError con Field rellena cuando ubica el problema en una
// ruta JSON específica. La validación es fail-fast: el primer error
// encontrado vuelve. Acumular todos sería más amigable para el wizard
// pero más confuso para el CLI (un humano leyendo `che pipeline
// validate` espera un solo error apuntado, no un dump).
func Validate(p Pipeline) error {
	if p.Version != CurrentVersion {
		return &LoadError{
			Field: "version",
			Reason: fmt.Sprintf(
				"unknown pipeline version %d (loader supports only %d) — upgrade che to load this pipeline",
				p.Version, CurrentVersion,
			),
		}
	}
	if len(p.Steps) == 0 {
		return &LoadError{
			Field:  "steps",
			Reason: "pipeline must declare at least one step",
		}
	}
	if p.Entry != nil {
		if err := validateEntry(*p.Entry); err != nil {
			return err
		}
	}
	seen := map[string]int{}
	for i, s := range p.Steps {
		if err := validateStep(i, s); err != nil {
			return err
		}
		if prev, dup := seen[s.Name]; dup {
			return &LoadError{
				Field: fmt.Sprintf("steps[%d].name", i),
				Reason: fmt.Sprintf(
					"duplicate step name %q (also at steps[%d]); names must be unique within a pipeline so [goto: %s] resolves unambiguously",
					s.Name, prev, s.Name,
				),
			}
		}
		seen[s.Name] = i
	}
	return nil
}

func validateEntry(e Entry) error {
	if len(e.Agents) == 0 {
		return &LoadError{
			Field:  "entry.agents",
			Reason: "entry must declare at least one agent",
		}
	}
	for j, a := range e.Agents {
		if a == "" {
			return &LoadError{
				Field:  fmt.Sprintf("entry.agents[%d]", j),
				Reason: "agent ref cannot be empty",
			}
		}
	}
	if e.Aggregator != "" && !e.Aggregator.IsValid() {
		return &LoadError{
			Field: "entry.aggregator",
			Reason: fmt.Sprintf(
				"unknown aggregator %q (valid: majority, unanimous, first_blocker)",
				e.Aggregator,
			),
		}
	}
	return nil
}

func validateStep(i int, s Step) error {
	if s.Name == "" {
		return &LoadError{
			Field:  fmt.Sprintf("steps[%d].name", i),
			Reason: "step name is required",
		}
	}
	if !stepNameRe.MatchString(s.Name) {
		return &LoadError{
			Field: fmt.Sprintf("steps[%d].name", i),
			Reason: fmt.Sprintf(
				"invalid step name %q (must match [a-z_][a-z0-9_]* — same fragment used by [goto: <name>] markers)",
				s.Name,
			),
		}
	}
	if len(s.Agents) == 0 {
		return &LoadError{
			Field:  fmt.Sprintf("steps[%d].agents", i),
			Reason: fmt.Sprintf("step %q must declare at least one agent", s.Name),
		}
	}
	for j, a := range s.Agents {
		if a == "" {
			return &LoadError{
				Field: fmt.Sprintf("steps[%d].agents[%d]", i, j),
				Reason: fmt.Sprintf(
					"agent ref in step %q cannot be empty",
					s.Name,
				),
			}
		}
	}
	if s.Aggregator != "" && !s.Aggregator.IsValid() {
		return &LoadError{
			Field: fmt.Sprintf("steps[%d].aggregator", i),
			Reason: fmt.Sprintf(
				"unknown aggregator %q in step %q (valid: majority, unanimous, first_blocker)",
				s.Aggregator, s.Name,
			),
		}
	}
	return nil
}

// ValidateAgents chequea que cada referencia a agente en el pipeline
// (entry + steps) exista según el predicado `has`. Pensado para
// componer con el agentregistry: el caller pasa
// `func(name string) bool { _, ok := reg.Get(name); return ok }`.
//
// Mantenemos un closure en vez de importar agentregistry para evitar
// el ciclo (PR1 ya está mergeado, pero el contrato más liviano deja
// al loader testeable sin armar una Registry completa). Si en un PR
// futuro hace falta más metadata del agente (ej. modelo declarado),
// promovemos a una interface dedicada.
//
// Errores: el primero encontrado. Devuelve *LoadError con Field
// apuntando a la posición exacta (`entry.agents[1]`, `steps[2].agents[0]`)
// para que el wizard pueda highlightear la celda culpable.
func ValidateAgents(p Pipeline, has func(name string) bool) error {
	if has == nil {
		return nil
	}
	if p.Entry != nil {
		for j, a := range p.Entry.Agents {
			if !has(a) {
				return &LoadError{
					Field: fmt.Sprintf("entry.agents[%d]", j),
					Reason: fmt.Sprintf(
						"agent %q not found in registry — declare it under .claude/agents/, install a plugin that provides it, or use a built-in (claude-opus, claude-sonnet, claude-haiku)",
						a,
					),
				}
			}
		}
	}
	for i, s := range p.Steps {
		for j, a := range s.Agents {
			if !has(a) {
				return &LoadError{
					Field: fmt.Sprintf("steps[%d].agents[%d]", i, j),
					Reason: fmt.Sprintf(
						"agent %q referenced in step %q not found in registry — declare it under .claude/agents/, install a plugin that provides it, or use a built-in (claude-opus, claude-sonnet, claude-haiku)",
						a, s.Name,
					),
				}
			}
		}
	}
	return nil
}
