package agent

import (
	"fmt"
	"strings"
)

// Validator identifica un validador: agente + instancia 1..N (para distinguir
// cuando el mismo agente aparece varias veces, ej "codex,codex,gemini" → los
// dos codex tienen Instance 1 y 2).
type Validator struct {
	Agent    Agent
	Instance int
}

// ParseValidators parsea una lista separada por coma ("codex,gemini",
// "codex,codex,gemini"). Acepta 1-3 items.
//
// allowNone=true (explore/execute): "" o "none" → nil, []Validator vacío
// permitido, no se corre validación.
// allowNone=false (validate): "" o "none" → error — validate no tiene sentido
// sin al menos un validador.
func ParseValidators(s string, allowNone bool) ([]Validator, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if allowNone {
			return nil, nil
		}
		return nil, fmt.Errorf("validators: empty — validate requires at least 1 validator")
	}
	if strings.EqualFold(s, "none") {
		if allowNone {
			return nil, nil
		}
		return nil, fmt.Errorf("validators: 'none' is not allowed — validate requires at least 1 validator")
	}
	parts := strings.Split(s, ",")
	if len(parts) < 1 || len(parts) > 3 {
		if allowNone {
			return nil, fmt.Errorf("validators: need 1-3 items (or `none`), got %d", len(parts))
		}
		return nil, fmt.Errorf("validators: need 1-3 items, got %d", len(parts))
	}
	counts := map[Agent]int{}
	out := make([]Validator, 0, len(parts))
	for _, p := range parts {
		a, err := ParseAgent(p)
		if err != nil {
			return nil, fmt.Errorf("validators: %w", err)
		}
		counts[a]++
		out = append(out, Validator{Agent: a, Instance: counts[a]})
	}
	return out, nil
}
