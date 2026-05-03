package runguard

import (
	"testing"

	"github.com/chichex/che/internal/output"
)

// TestAcquireLock_DisabledIsNoop: si CHE_LOCK_HEARTBEAT no está set,
// AcquireLock devuelve nil sin tocar nada — el caller continúa con el
// che:locked legacy.
func TestAcquireLock_DisabledIsNoop(t *testing.T) {
	t.Setenv("CHE_LOCK_HEARTBEAT", "")
	h := AcquireLock("42", "explore", output.New(nil))
	if h != nil {
		t.Errorf("AcquireLock con env vacío devolvió %v, want nil", h)
	}
}

// TestReleaseLock_NilIsNoop: pasar nil a ReleaseLock no panickea.
func TestReleaseLock_NilIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ReleaseLock(nil) panicked: %v", r)
		}
	}()
	ReleaseLock(nil, output.New(nil))
}

// TestAuditAppend_DisabledIsNoop: sin CHE_AUDIT_LOG, AuditAppend no hace
// nada (no shell-out a gh, no error).
func TestAuditAppend_DisabledIsNoop(t *testing.T) {
	t.Setenv("CHE_AUDIT_LOG", "")
	AuditAppend(42, "explore", "from", "to", "", output.New(nil))
	// Si llegamos acá sin error/panic ya tenemos la confirmación.
}

// TestAuditAppend_InvalidNumberIsNoop: number <= 0 es signo de "no
// resolvió un target real" (ej. PR sin closing issue + adopt mode). En
// vez de gritar, AuditAppend salta.
func TestAuditAppend_InvalidNumberIsNoop(t *testing.T) {
	t.Setenv("CHE_AUDIT_LOG", "1")
	// Si llegara a invocar gh, el test fallaría con un timeout o un error
	// de network. Acá esperamos que el guard de number<=0 cortocircuite
	// antes.
	AuditAppend(0, "explore", "from", "to", "", output.New(nil))
}
