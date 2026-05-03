// Feature-flag para roll-out gradual del lock con heartbeat (PRD §6.d).
//
// Mientras los flows existentes siguen aplicando el binario `che:locked`
// (mutex simple), el nuevo lock con TTL+heartbeat+identidad se activa
// solo cuando CHE_LOCK_HEARTBEAT=1. Esto permite:
//
//   - Tests existentes (incluido e2e) no ven gh calls nuevas; pasan
//     iguales que antes.
//   - Operadores que quieran probar el nuevo modelo en un repo lo activan
//     vía env, sin re-releaser. Si encuentran issues, lo apagan.
//   - El día que el feature sea estable se invierte el default (o se
//     borra el flag) en un PR follow-up.
package lock

import (
	"os"
	"strings"
)

// HeartbeatEnabled devuelve true si CHE_LOCK_HEARTBEAT=1|true|on. Cualquier
// otro valor (incluyendo unset) → false. Mantenido como función (no var)
// para que tests puedan setear/unsetear el env sin importar orden.
func HeartbeatEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("CHE_LOCK_HEARTBEAT")))
	switch v {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}
