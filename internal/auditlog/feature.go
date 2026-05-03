// Feature-flag para roll-out gradual del audit log (PRD §8).
//
// El audit log agrega un comment al issue raíz con la timeline de
// transiciones — útil pero no crítico. Mientras se valida el formato y
// el rendimiento (1 PATCH por transición), se gatea con env var. Cuando
// sea estable se invierte el default en un PR follow-up.
package auditlog

import (
	"os"
	"strings"
)

// Enabled devuelve true si CHE_AUDIT_LOG=1|true|on. Cualquier otro valor
// (incluyendo unset) → false.
func Enabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("CHE_AUDIT_LOG")))
	switch v {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}
