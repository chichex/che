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

// AcquireLock toma el lock con heartbeat sobre `ref`. Si la feature está
// off (env CHE_LOCK_HEARTBEAT no=1), devuelve nil sin tocar nada (caller
// debe interpretar nil como "feature off, seguir adelante").
//
// Si la feature está on:
//   - Acquire OK → devuelve un Handle vivo (el caller hace defer Release).
//   - Lock previo vivo (ErrAlreadyLocked) → loggea error y devuelve nil.
//     El caller debe abortar con ExitSemantic. La forma de distinguir
//     "feature off" de "lock contended" es chequear lock.HeartbeatEnabled()
//     después de un nil return.
//   - Cualquier otro error (red, gh) → loggea warn y devuelve nil. El
//     caller puede decidir continuar (el flow no tiene heartbeat pero el
//     binario `che:locked` legacy ya cubrió la sección crítica) o abortar.
//     Conservamos "continuar" como default para no degradar runs por red
//     intermitente.
func AcquireLock(ref, flow string, log *output.Logger) *lock.Handle {
	if !lock.HeartbeatEnabled() {
		return nil
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
		return h
	}
	if errors.Is(err, lock.ErrAlreadyLocked) {
		if log != nil {
			log.Error("ref bloqueado por otro proceso (heartbeat lock)", output.F{Cause: err})
		}
		return nil
	}
	if log != nil {
		log.Warn("no se pudo aplicar heartbeat lock — sigo con che:locked legacy", output.F{Cause: err})
	}
	return nil
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

