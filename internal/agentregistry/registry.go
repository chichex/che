package agentregistry

import (
	"fmt"
	"sort"
)

// Registry es el resultado del Discover: una lista de agentes resueltos
// (ya con la precedencia aplicada) más los shadows que quedaron tapados
// para que `che agents list --verbose` (futuro) o tests puedan revisarlos.
type Registry struct {
	resolved []Agent
	shadows  []Agent
}

// All devuelve todos los agentes resueltos, ordenados por nombre.
// Cada nombre aparece una sola vez — el de mayor precedencia.
func (r *Registry) All() []Agent {
	if r == nil {
		return nil
	}
	out := make([]Agent, len(r.resolved))
	copy(out, r.resolved)
	return out
}

// Get busca un agente por nombre canónico. Devuelve la entry resuelta
// (la de mayor precedencia que reclamó el nombre) y true si existe.
func (r *Registry) Get(name string) (Agent, bool) {
	if r == nil {
		return Agent{}, false
	}
	for _, a := range r.resolved {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Shadows devuelve los agentes que no ganaron el nombre por colisión.
// Útil para `che agents list --verbose` o para testear que la
// precedencia se aplicó como dice §2.a del PRD.
func (r *Registry) Shadows() []Agent {
	if r == nil {
		return nil
	}
	out := make([]Agent, len(r.shadows))
	copy(out, r.shadows)
	return out
}

// CollisionWarning describe que un agente custom le ganó al built-in
// del mismo nombre. Discover lo emite como error (no fatal) para que
// el caller decida si printearlo.
type CollisionWarning struct {
	Name           string
	Winner         Source
	WinnerPath     string
	ShadowedSource Source
	ShadowedPath   string
}

func (c CollisionWarning) Error() string {
	loc := c.WinnerPath
	if loc == "" {
		loc = string(c.Winner)
	}
	return fmt.Sprintf(
		"agentregistry: name %q from %s (%s) shadows %s entry; rename one to disambiguate",
		c.Name, c.Winner, loc, c.ShadowedSource,
	)
}

// buildRegistry aplica la precedencia y devuelve el Registry final.
//
// Para cada nombre canónico colisionado, gana el de menor sourceRank;
// los demás se guardan en shadows. Si el ganador es un Source distinto
// de built-in y tapa al built-in, se emite un CollisionWarning (§2.a:
// "Si el usuario tiene un agente custom llamado claude-opus, gana sobre
// el built-in con warning al cargar para evitar confusión").
func buildRegistry(all []Agent) (*Registry, []error) {
	byName := map[string][]Agent{}
	for _, a := range all {
		byName[a.Name] = append(byName[a.Name], a)
	}

	var (
		resolved []Agent
		shadows  []Agent
		warns    []error
	)
	for _, group := range byName {
		sort.SliceStable(group, func(i, j int) bool {
			return sourceRank(group[i].Source) < sourceRank(group[j].Source)
		})
		winner := group[0]
		resolved = append(resolved, winner)
		for _, loser := range group[1:] {
			shadows = append(shadows, loser)
			if loser.Source == SourceBuiltin {
				warns = append(warns, CollisionWarning{
					Name:           winner.Name,
					Winner:         winner.Source,
					WinnerPath:     winner.Path,
					ShadowedSource: loser.Source,
					ShadowedPath:   loser.Path,
				})
			}
		}
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].Name < resolved[j].Name })
	sort.Slice(shadows, func(i, j int) bool {
		if shadows[i].Name != shadows[j].Name {
			return shadows[i].Name < shadows[j].Name
		}
		return sourceRank(shadows[i].Source) < sourceRank(shadows[j].Source)
	})
	return &Registry{resolved: resolved, shadows: shadows}, warns
}
