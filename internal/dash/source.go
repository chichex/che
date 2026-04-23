// Package dash — abstracción Source: producer del snapshot de entidades que
// el server le pasa al template. Dos implementaciones: MockSource (datos
// hardcoded del Step 2) y GhSource (poller real que lee labels/PRs vía `gh`).
//
// El server no sabe de dónde vienen los datos — tiene una referencia a una
// Source y llama Snapshot() en cada request. Eso hace trivial:
//   - swappear a un fixture en tests
//   - correr en modo --mock sin depender de gh
//   - que el poller refreshee en background sin bloquear el handler
package dash

import "time"

// Source es el productor de snapshots. Implementaciones deben ser
// concurrency-safe (Snapshot() puede ser llamado en paralelo con un
// refresh interno).
type Source interface {
	Snapshot() Snapshot
}

// Snapshot es un corte instantáneo del estado del repo. Se regenera cada
// vez que el poller hace un refresh exitoso; entre refreshes se devuelve el
// último snapshot conocido (stale si el último refresh falló).
type Snapshot struct {
	// Entities es la lista de issues/PRs gestionados por che (ver filtrado
	// en gh_source.go: issues sin ct:plan se excluyen; PRs sin close-keyword
	// se omiten).
	Entities []Entity
	// LastOK es el timestamp del último refresh que completó sin error. Zero
	// si nunca hubo uno OK (al arrancar, antes del primer tick).
	LastOK time.Time
	// LastErr es el error del último intento de refresh. nil si el último
	// refresh fue OK.
	LastErr error
	// Stale es true cuando LastErr != nil pero hay Entities de un refresh
	// anterior — el board sigue mostrando datos pero el chip avisa que están
	// desactualizados.
	Stale bool
	// Mock indica que el snapshot viene de MockSource (no del repo real).
	// Usado por el topbar para pintar el chip distinto.
	Mock bool
	// NWO es el nameWithOwner del repo (ej: "owner/repo"). Usado por el
	// template para construir URLs absolutas a github.com (refs clickables
	// en cards/drawer). Vacío si MockSource no lo setea.
	NWO string
}

// MockSource es la implementación del Step 2: devuelve las 9 entidades
// hardcoded en mock.go. Útil para demos y para correr el server sin gh.
type MockSource struct{}

// Snapshot siempre devuelve LastOK=now y Mock=true — no hace IO, nunca falla.
// NWO se hardcodea a "demo/che" para que los links del mock apunten a una
// nwo inventada (no importa el destino real — es demo).
func (MockSource) Snapshot() Snapshot {
	return Snapshot{
		Entities: mockEntities(),
		LastOK:   time.Now(),
		Mock:     true,
		NWO:      "demo/che",
	}
}

// Compile-time check: MockSource implementa Source.
var _ Source = MockSource{}
