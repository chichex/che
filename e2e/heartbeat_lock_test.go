package e2e_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestHeartbeatLock_ContendedAbortsExplore: con CHE_LOCK_HEARTBEAT=1, el
// flow `che explore` debe detectar un `che:lock:*` vivo presente en el
// issue y abortar con ExitSemantic (3) — sin tocar la transición de
// estado.
//
// Stubeamos `gh api .../issues/<n>` (que internal/lock.Acquire usa para
// listar labels) devolviendo un payload con un lock vivo (timestamp
// reciente). El flow después llama a `gh issue view ...` que devuelve la
// fixture sin lock — los dos endpoints son distintos.
func TestHeartbeatLock_ContendedAbortsExplore(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.SetEnv("CHE_LOCK_HEARTBEAT", "1")
	// Importante: registramos los matchers ESPECÍFICOS antes que los
	// catch-all de scriptExplorePrechecks/scriptCheLockDefault, ya que el
	// harness matchea en orden de registro.
	// Timestamp reciente (30s atrás) → lock VIVO según TTL=5min default.
	liveLockNanos := time.Now().Add(-30 * time.Second).UnixNano()
	env.ExpectGh(`^api repos/\{owner\}/\{repo\}/issues/42$`).
		RespondStdout(fmt.Sprintf(`{"labels":[{"name":"che:lock:%d:777-other-host"}]}`, liveLockNanos), 0)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Errorf("want exit 3 (ExitSemantic) por lock vivo, got %d\nstdout:\n%s\nstderr:\n%s",
			r.ExitCode, r.Stdout, r.Stderr)
	}
	combined := r.Stdout + r.Stderr
	// El error puede aparecer en stderr (output.Logger) — sentinel en
	// español.
	if !strings.Contains(combined, "bloqueado") &&
		!strings.Contains(combined, "already locked") {
		t.Errorf("salida no menciona el lock vivo:\n%s", combined)
	}
}
