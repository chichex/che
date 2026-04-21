// Package agent centraliza la invocación de los CLIs externos que actúan como
// ejecutores y validadores de los flows de che: `claude` (opus), `codex` y
// `gemini`. Antes vivía duplicado en explore/execute/validate/iterate — cada
// paquete tenía su copia del enum Agent, ParseAgent, ParseValidators, y del
// plumbing de exec.Cmd con streaming por pipes. Este paquete absorbe todo eso
// para que los flows sólo decidan QUÉ prompt mandar y cómo parsear la
// respuesta; el CÓMO invocar al binario vive acá.
//
// Diferencias de comportamiento por flow que este paquete respeta:
//   - Output format: text (explore/validate) vs stream-json --verbose
//     (execute/iterate), seleccionado por RunOpts.Format.
//   - Timeouts: cada flow configura el suyo vía RunOpts.Timeout (el env var
//     que lo respalda sigue viviendo en cada paquete flow).
//   - Signal handling: execute aísla al agente en su propio process group y
//     escala SIGTERM→SIGKILL con gracia de 5s; iterate/explore/validate usan
//     cancelación simple por context. RunOpts.KillGrace controla esto.
//   - cwd: execute e iterate fijan el cwd al worktree; explore/validate
//     heredan. RunOpts.Dir lo expone.
//   - allowNone en ParseValidators: explore/execute permiten "none" → nil;
//     validate lo rechaza. El flag vive en ParseValidators(s, allowNone).
package agent

import (
	"fmt"
	"strings"
)

// Agent identifica qué binario invocar. Los strings subyacentes son parte del
// contrato con los usuarios (aparecen en flags, labels de comments, etc.), así
// que cambiarlos es una break visible.
type Agent string

const (
	AgentOpus   Agent = "opus"
	AgentCodex  Agent = "codex"
	AgentGemini Agent = "gemini"
)

// DefaultAgent es el ejecutor por defecto cuando el caller no elige uno.
const DefaultAgent = AgentOpus

// ValidAgents lista los agentes soportados en orden canónico (preservado para
// UI: el iterador de la TUI depende del orden).
var ValidAgents = []Agent{AgentOpus, AgentCodex, AgentGemini}

// Binary devuelve el nombre del ejecutable asociado al agente. Opus se mapea
// a `claude` porque el CLI oficial de Anthropic se llama así; Codex y Gemini
// usan su nombre directo.
func (a Agent) Binary() string {
	switch a {
	case AgentOpus:
		return "claude"
	case AgentCodex:
		return "codex"
	case AgentGemini:
		return "gemini"
	}
	return ""
}

// ParseAgent normaliza un string a Agent, tolerando mayúsculas/espacios y
// devolviendo error si no matchea ningún enum.
func ParseAgent(s string) (Agent, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, a := range ValidAgents {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown agent %q; valid: opus, codex, gemini", s)
}
