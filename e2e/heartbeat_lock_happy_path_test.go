package e2e_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestHeartbeatLock_HappyPath_Explore: con CHE_LOCK_HEARTBEAT=1 y un
// issue sin lock vivo, `che explore` debe completar el flujo end-to-end:
// acquire OK → flow completo → release OK.
//
// Es la contraparte simétrica de TestHeartbeatLock_ContendedAbortsExplore
// (lock vivo → abort). Sin esta cobertura, el wireup de
// runguard.AcquireLock se mergea sin que CI vea ningún path donde la
// feature está activa Y el flow completa OK — punto ciego del feature
// flag default-off.
//
// Importante: este test cubre SOLO el flow `explore`. La matriz completa
// para los otros 5 flows (execute, iterate-pr, iterate-plan, validate-pr,
// validate-plan, close) queda como follow-up — ver EXEC_NOTES.md
// "PR7-followup-Y".
//
// Stub strategy:
//   - El primer `gh api repos/.../issues/42` que internal/lock.Acquire
//     hace para listar locks, devuelve un payload SIN labels che:lock:*
//     → fresh acquire.
//   - El POST del lock va al catch-all de scriptCheLockDefault (que ya
//     responde "{}" a cualquier POST de labels al issue).
//   - El segundo `gh api .../issues/42` (post-check de race) también
//     devuelve sin lock vivo nuestro → ganamos.
//   - El DELETE del lock al final (Release) cae en el catch-all de
//     scriptCheLockDefault (que matchea cualquier DELETE de label
//     che:*).
func TestHeartbeatLock_HappyPath_Explore(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.SetEnv("CHE_LOCK_HEARTBEAT", "1")

	// Primer + segundo list (initial check + post-check race detection):
	// devuelven payload SIN locks vivos. Importante: este matcher es
	// non-consumable (matchea las dos veces que internal/lock.Acquire
	// llama al endpoint).
	env.ExpectGh(`^api repos/\{owner\}/\{repo\}/issues/42$`).
		RespondStdout(`{"labels":[]}`, 0)

	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "42")
	harness.AssertContains(t, out, "Explored")

	inv := env.Invocations()

	// Verificación 1: hubo al menos 1 POST de un che:lock:* label (el
	// acquire del heartbeat). El log del Step "lock con heartbeat
	// aplicado" se asserta en TestHeartbeatLock_HappyPath_Explore_RunVariant
	// porque MustRun no expone stderr de manera estructurada.
	posts := inv.FindCalls("gh", "api", "-X", "POST", "issues/42/labels")
	foundLockPost := false
	for _, p := range posts {
		joined := strings.Join(p.Args, " ")
		if strings.Contains(joined, "labels[]=che:lock:") {
			foundLockPost = true
			break
		}
	}
	if !foundLockPost {
		t.Errorf("expected al menos 1 POST de un che:lock:* label (heartbeat acquire); posts=%v", posts)
	}

	// Verificación 2: hubo al menos 1 DELETE de un che:lock:* label (el
	// release al final del flow).
	deletes := inv.FindCalls("gh", "api", "-X", "DELETE")
	foundLockDelete := false
	for _, d := range deletes {
		joined := strings.Join(d.Args, " ")
		if strings.Contains(joined, "/labels/che:lock:") {
			foundLockDelete = true
			break
		}
	}
	if !foundLockDelete {
		t.Errorf("expected al menos 1 DELETE de un che:lock:* label (heartbeat release); deletes=%v", deletes)
	}
}

// TestHeartbeatLock_HappyPath_Explore_RunVariant: variante que usa Run()
// en vez de MustRun() para poder asertar sobre stderr (el logger de
// runguard imprime "lock con heartbeat aplicado" allí). Útil para
// verificar que la feature está realmente activa y no silenciosamente
// disabled.
func TestHeartbeatLock_HappyPath_Explore_RunVariant(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.SetEnv("CHE_LOCK_HEARTBEAT", "1")

	env.ExpectGh(`^api repos/\{owner\}/\{repo\}/issues/42$`).
		RespondStdout(`{"labels":[]}`, 0)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr:\n%s", r.ExitCode, r.Stderr)
	}
	combined := r.Stdout + r.Stderr
	if !strings.Contains(combined, "lock con heartbeat aplicado") {
		t.Errorf("salida no menciona 'lock con heartbeat aplicado' — feature flag puede no estar activo:\nstdout:\n%s\nstderr:\n%s",
			r.Stdout, r.Stderr)
	}
}

// TestHeartbeatLock_StaleEvicted_Explore: simétrico al test contended,
// pero el lock pre-existente está STALE (timestamp viejo, > TTL=5min) →
// internal/lock.Acquire debe evictarlo y proceder con el flow normal,
// devolviendo exit 0.
//
// Este caso ejercita el path donde un proceso anterior crasheó dejando
// un lock huérfano: el siguiente run no debe quedar bloqueado para
// siempre. Es exactamente el path donde el TTL boundary check (`< TTL`
// vs `<= TTL`) podría romper en producción si alguien lo refactorea.
func TestHeartbeatLock_StaleEvicted_Explore(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.SetEnv("CHE_LOCK_HEARTBEAT", "1")

	// Timestamp claramente vencido (1970 + 1ns) → bien por encima del TTL
	// default (5min). El TTL boundary check de internal/lock se asegura
	// de evictar.
	staleNanos := int64(1_000_000_000)
	staleLockLabel := fmt.Sprintf("che:lock:%d:777-other-host", staleNanos)

	// Initial list: devuelve el stale lock.
	env.ExpectGh(`^api repos/\{owner\}/\{repo\}/issues/42$`).
		Consumable().
		RespondStdout(`{"labels":[{"name":"`+staleLockLabel+`"}]}`, 0)
	// Post-check list: ya sin locks (el stale fue evicted por
	// internal/lock antes del POST nuestro, y nuestro lock todavía no
	// está en la respuesta porque el fake no mantiene estado).
	env.ExpectGh(`^api repos/\{owner\}/\{repo\}/issues/42$`).
		RespondStdout(`{"labels":[]}`, 0)

	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0 (stale evicted, flow procede), got %d\nstderr:\n%s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	// Debió haber un DELETE del lock stale (eviction) ANTES del POST
	// nuestro.
	deletes := inv.FindCalls("gh", "api", "-X", "DELETE")
	foundStaleDelete := false
	for _, d := range deletes {
		joined := strings.Join(d.Args, " ")
		if strings.Contains(joined, staleLockLabel) {
			foundStaleDelete = true
			break
		}
	}
	if !foundStaleDelete {
		t.Errorf("expected DELETE del lock stale %q; deletes=%v", staleLockLabel, deletes)
	}
}

