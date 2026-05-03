// Package runguard centraliza la adquisición/liberación del lock con
// heartbeat (PRD §6.d) y la escritura del audit log (PRD §8) para que los
// 5 flows (explore, execute, iterate, validate, close) no dupliquen 20+
// líneas idénticas cada uno.
//
// Patrón de uso desde un flow:
//
//	hb := runguard.AcquireLock(issueRef, "explore", log)
//	defer runguard.ReleaseLock(hb, log)
//	if hb == nil && lock.HeartbeatEnabled() {
//	    // Lock vivo de otro proceso — abort.
//	    return ExitSemantic
//	}
//	...
//	runguard.AuditAppend(issueNumber, "explore", from, to, "")
//
// Los dos features están gateados por env vars (CHE_LOCK_HEARTBEAT,
// CHE_AUDIT_LOG): si no están on, las funciones son no-op silencioso.
// Eso permite roll-out gradual sin tocar tests existentes.
package runguard

import (
	"errors"
	"fmt"

	"github.com/chichex/che/internal/auditlog"
	"github.com/chichex/che/internal/lock"
	"github.com/chichex/che/internal/output"
)

// AcquireResult clasifica el resultado de un AcquireLock para que el
// caller pueda ramificar sin parsear errors. Hoy todos los flows usan el
// mismo path (continuar o abortar) pero el enum permite políticas
// distintas en el futuro (ej. validate-pr puede querer skip lock al
// adoptar un PR ajeno).
type AcquireResult int

const (
	// AcquireDisabled — feature flag off (CHE_LOCK_HEARTBEAT != 1). Caller
	// debe seguir adelante sin lock.
	AcquireDisabled AcquireResult = iota
	// AcquireOK — lock aplicado limpio, heartbeat corriendo. Caller debe
	// hacer defer ReleaseLock al final del flow.
	AcquireOK
	// AcquireContended — otro proceso ya tiene lock vivo (o ganó la race
	// por tie-break). Caller debe abortar con ExitSemantic.
	AcquireContended
	// AcquireInfraError — red, gh, o falla del post-check. El lock puede
	// estar parcialmente aplicado. Hoy runguard loggea warn y devuelve
	// AcquireInfraError; los callers actuales lo tratan como "continuar"
	// (el binario `che:locked` legacy cubre la sección crítica). Un
	// caller futuro puede elegir abortar.
	AcquireInfraError
)

// AcquireLock toma el lock con heartbeat sobre `ref`. Devuelve el handle
// (puede ser nil si AcquireResult != AcquireOK) y el resultado clasificado.
//
// Wrapper backward-compat: los callers existentes pueden seguir usando
// la forma "ignorar el segundo return" — pero los nuevos deberían
// ramificar por AcquireResult para distinguir "feature off" de "lock
// contended" sin re-chequear lock.HeartbeatEnabled() después.
func AcquireLock(ref, flow string, log *output.Logger) (*lock.Handle, AcquireResult) {
	if !lock.HeartbeatEnabled() {
		return nil, AcquireDisabled
	}
	h, err := lock.Acquire(ref, lock.Options{
		LogErrf: func(format string, args ...any) {
			if log != nil {
				log.Warn("heartbeat: " + fmt.Sprintf(format, args...))
			}
		},
	})
	if err == nil {
		if log != nil {
			log.Step("lock con heartbeat aplicado", output.F{Labels: []string{h.CurrentLabel()}})
		}
		return h, AcquireOK
	}
	if errors.Is(err, lock.ErrAlreadyLocked) {
		if log != nil {
			log.Error("ref bloqueado por otro proceso (heartbeat lock)", output.F{Cause: err})
		}
		return nil, AcquireContended
	}
	if errors.Is(err, lock.ErrPostCheckFailed) {
		// El lock SÍ está aplicado pero la verificación de race falló.
		// Política actual: warn + continuar (el handle queda vivo para
		// que el caller pueda hacer Release al terminar, manteniendo el
		// invariante de que el lock no quede huérfano hasta que expire
		// el TTL).
		if log != nil {
			log.Warn("post-check del heartbeat lock falló — sigo con lock aplicado pero sin verificación de race", output.F{Cause: err})
		}
		return h, AcquireInfraError
	}
	if log != nil {
		log.Warn("no se pudo aplicar heartbeat lock — sigo con che:locked legacy", output.F{Cause: err})
	}
	return nil, AcquireInfraError
}

// ReleaseLock libera el handle si no es nil. Idempotente. Loggea warn si
// la liberación falla (el flow ya terminó, no podemos abortar).
func ReleaseLock(h *lock.Handle, log *output.Logger) {
	if h == nil {
		return
	}
	if err := h.Release(); err != nil {
		if log != nil {
			log.Warn("no se pudo liberar heartbeat lock", output.F{Cause: err})
		}
	}
}

// AuditAppend agrega una entrada al audit log del issue/PR. No-op si
// CHE_AUDIT_LOG no está on. Errores se loggean como warn pero no abortan
// el flow (el audit log es valor agregado, no crítico).
//
// `note` es opcional — pasalo como "rollback", "stale-evicted", etc. para
// distinguir transiciones de éxito de las correctivas.
func AuditAppend(number int, flow, from, to, note string, log *output.Logger) {
	if !auditlog.Enabled() {
		return
	}
	if number <= 0 {
		return
	}
	_, err := auditlog.Append(number, auditlog.Entry{
		Flow: flow,
		From: from,
		To:   to,
		Note: note,
	}, auditlog.Options{})
	if err != nil && log != nil {
		log.Warn("audit log append falló", output.F{Cause: err})
	}
}

