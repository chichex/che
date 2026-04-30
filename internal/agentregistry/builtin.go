package agentregistry

// builtins son los 3 agentes "vanilla" siempre disponibles (§2.b del PRD).
// No tienen system prompt extra ni tools custom — sirven para arrancar
// sin configurar nada y para steps simples.
//
// El Name acá es el id que usan los pipelines (`"agents": ["claude-opus"]`).
// Si el usuario tiene un custom con el mismo nombre, gana el custom y
// che warnea al cargar (ver registry.go).
func builtins() []Agent {
	return []Agent{
		{
			Name:        "claude-opus",
			Description: "Claude Opus general-purpose, sin tools custom ni system prompt extra.",
			Model:       "opus",
			Source:      SourceBuiltin,
		},
		{
			Name:        "claude-sonnet",
			Description: "Claude Sonnet general-purpose, sin tools custom ni system prompt extra.",
			Model:       "sonnet",
			Source:      SourceBuiltin,
		},
		{
			Name:        "claude-haiku",
			Description: "Claude Haiku general-purpose, sin tools custom ni system prompt extra.",
			Model:       "haiku",
			Source:      SourceBuiltin,
		},
	}
}
