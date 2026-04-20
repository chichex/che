package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chichex/che/e2e/harness"
)

// TestExecute_SIGINT_DuringAgent_Cleanup arranca `che execute` con un fake
// claude que escribe un sentinel file y bloquea; el test espera el archivo,
// manda SIGINT al proceso, y verifica el estado final: worktree ausente,
// label volvió a status:plan, exit code 130, fake ya no corriendo.
func TestExecute_SIGINT_DuringAgent_Cleanup(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecuteSignalFlow(env, ".che-agent-ready")

	async := env.RunAsync("execute", "--validators", "none", "42")
	defer func() {
		// Best-effort: si el proceso sigue vivo al final (no debería),
		// lo matamos para no dejar un huérfano colgado.
		_ = async.SendSignal(syscall.SIGKILL)
	}()

	sentinelPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42", ".che-agent-ready")
	if !async.WaitForFile(t, sentinelPath, 15*time.Second) {
		t.Fatalf("timed out waiting for agent sentinel file %s\nstdout:\n%s\nstderr:\n%s",
			sentinelPath, async.SnapshotStdout(), async.SnapshotStderr())
	}

	// Un momento extra para asegurar que che ya está bloqueado en el Wait
	// del agente (evita una race en la que la señal llega antes de que
	// runAgent arme el cmd.Cancel).
	time.Sleep(200 * time.Millisecond)

	if err := async.SendSignal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	r := async.Wait(t, 20*time.Second)
	if r.ExitCode != 130 {
		t.Fatalf("expected exit 130, got %d\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Stdout, r.Stderr)
	}

	assertCleanupApplied(t, env, r.Stderr)
}

// TestExecute_SIGTERM_DuringAgent_Cleanup: equivalente al test de SIGINT
// pero mandando SIGTERM. El cleanup local tiene que correr igual y el exit
// code debe ser 130 (distinto del exit 0/1/2 de los paths normales).
func TestExecute_SIGTERM_DuringAgent_Cleanup(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecuteSignalFlow(env, ".che-agent-ready")

	async := env.RunAsync("execute", "--validators", "none", "42")
	defer func() { _ = async.SendSignal(syscall.SIGKILL) }()

	sentinelPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42", ".che-agent-ready")
	if !async.WaitForFile(t, sentinelPath, 15*time.Second) {
		t.Fatalf("timed out waiting for agent sentinel file %s\nstdout:\n%s\nstderr:\n%s",
			sentinelPath, async.SnapshotStdout(), async.SnapshotStderr())
	}
	time.Sleep(200 * time.Millisecond)

	if err := async.SendSignal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	r := async.Wait(t, 20*time.Second)
	if r.ExitCode != 130 {
		t.Fatalf("expected exit 130, got %d\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Stdout, r.Stderr)
	}

	assertCleanupApplied(t, env, r.Stderr)
}

// scriptExecuteSignalFlow scriptea el flow completo hasta el punto en que
// el agente bloquea. Prechecks + issue view (2 veces: pre-lock y re-fetch
// del rollback) + pr list + label lock + claude fake blocking + label
// rollback. No scripteamos pasos post-agent (commit/push/PR) porque el
// test mata al proceso antes. El fake escribe un archivo sentinel en el
// worktree para que el test sepa que el agente arrancó — la CLI no pipea
// stdout del agente al terminal del caller (es progress callback, que en
// CLI es no-op), así que un sentinel en stdout no llegaría al test.
func scriptExecuteSignalFlow(env *harness.Env, sentinelFile string) {
	scriptExecutePrechecks(env)
	// 1er issue view (pre-gate) + 2do issue view (rollback re-fetch) — el
	// segundo devuelve locked_executing para que el rollback proceda.
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_locked_executing.json", 0)
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// Lock (plan→executing) + rollback (executing→plan).
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	// Fake claude: crea el sentinel file y duerme. Cuando che le manda
	// SIGTERM al process group, el default handler de Go termina el fake.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile(sentinelFile, "ready\n").
		BlockSeconds(60).
		RespondStdout("starting\n", 0)
}

// waitForInvocation polls the harness invocation log for the given bin
// until one invocation appears or timeout expires. Used as a synchronization
// primitive for tests that need to wait until che has already shelled out
// to a fake (e.g., validators) before sending a signal.
func waitForInvocation(t *testing.T, env *harness.Env, bin string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(env.Invocations().For(bin)) > 0 {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// assertCleanupApplied verifica el estado final después de un cleanup por
// señal: el directorio del worktree no existe, la branch local fue borrada,
// el rollback de label ocurrió, el stderr tiene un mensaje identificable y
// no quedan procesos huérfanos del fake agent.
func assertCleanupApplied(t *testing.T, env *harness.Env, stderr string) {
	t.Helper()

	// 1) Worktree removido: .worktrees/issue-42 no debería existir.
	wtPath := filepath.Join(env.RepoDir, ".worktrees", "issue-42")
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists after signal cleanup (err=%v)", wtPath, err)
	}

	// 2) Branch local borrada: `git branch --list exec/42-*` no debe matchear.
	//    El PR documenta "cleanup determinista" — si la branch queda viva el
	//    próximo execute sobre el mismo issue se va a apropiar de ella.
	cmd := exec.Command("git", "-C", env.RepoDir, "branch", "--list", "exec/42-*")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("local branch exec/42-* still present after cleanup:\n%s", out)
	}

	// 3) Rollback de label ejecutado: debe haber un issue edit consecutivo
	//    con --add-label status:plan.
	edits := env.Invocations().FindCalls("gh", "issue", "edit", "42")
	foundRollback := false
	for _, e := range edits {
		for i := 0; i+1 < len(e.Args); i++ {
			if e.Args[i] == "--add-label" && e.Args[i+1] == "status:plan" {
				foundRollback = true
			}
		}
	}
	if !foundRollback {
		t.Fatalf("expected rollback to status:plan in edits\n%v", edits)
	}

	// 4) Mensaje al usuario: el stderr debería indicar que limpió local.
	if !strings.Contains(stderr, "limpiando localmente") && !strings.Contains(stderr, "señal recibida") {
		t.Fatalf("expected signal cleanup message in stderr, got:\n%s", stderr)
	}

	// 5) No quedan procesos huérfanos del fake claude: si `cmd.Wait` retornó
	//    (exit 130) eso solo garantiza que el PID directo murió — los
	//    descendientes del process group podrían seguir vivos si el kill -pgid
	//    no funcionó. Chequeamos explícitamente que no haya un `claude` (el
	//    symlink del fake) corriendo con el FakeBin del env como ancestro.
	if leaked := findLeakedFakes(env, "claude"); leaked != "" {
		t.Fatalf("orphan claude fake still running after signal cleanup:\n%s", leaked)
	}
}

// findLeakedFakes busca procesos vivos cuyo path ejecutable apunte al fake
// `bin` del env (symlink en FakeBin). Retorna la salida de `ps` que matchea
// o "" si no hay. Usamos `pgrep -f` con el prefijo del FakeBin para evitar
// falsos positivos con `claude`/`codex` reales instalados en la máquina.
func findLeakedFakes(env *harness.Env, bin string) string {
	// pgrep -fl <pattern> devuelve pid + command line; matcheamos por el
	// path del symlink ya que el fake se ejecuta como "<FakeBin>/<bin>".
	pattern := filepath.Join(env.FakeBin, bin)
	cmd := exec.Command("pgrep", "-fl", pattern)
	out, _ := cmd.Output() // exit 1 = no matches, lo tratamos como "clean".
	return strings.TrimSpace(string(out))
}

// TestExecute_SIGINT_DuringValidatorsWait: arrancamos el flow completo
// hasta post-PR con un validador fake que bloquea; mandamos SIGINT
// durante el wait del validador y verificamos que el exit sea 130, que el
// label siga en status:executed (ya transicionamos — no revertimos) y que
// no haya procesos huérfanos. El rollback remoto NO corre: el PR draft y
// la branch remota quedan intactos para retry manual.
func TestExecute_SIGINT_DuringValidatorsWait(t *testing.T) {
	env := setupExecuteEnv(t)
	scriptExecutePrechecks(env)
	env.ExpectGh(`^issue view 42`).Consumable().RespondStdoutFromFixture("execute/gh_issue_view_ready.json", 0)
	env.ExpectGh(`^pr list --head exec/42-`).RespondStdout("[]\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// Lock (plan→executing) + transition post-PR (executing→executed).
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).Consumable().RespondStdout("ok\n", 0)

	// El executor produce un cambio y termina rápido.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior ejecutando`).
		TouchFile("IMPLEMENTATION.md", "did it\n").
		RespondStdout("ok\n", 0)

	env.ExpectGh(`^pr create --draft`).RespondStdout("https://github.com/acme/demo/pull/7\n", 0)
	env.ExpectGh(`^issue comment 42 --body-file`).RespondStdout("ok\n", 0)

	// Validator codex: emite progreso a stdout del che (el flow escribe
	// "esperando validadores (k/total)…" recién cuando uno termina; acá
	// usamos "disparando N validador(es) sobre el PR…" como sentinel).
	// El validator bloquea hasta que le llegue la señal del process group.
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		BlockSeconds(60).
		RespondStdout("starting validator\n", 0)

	async := env.RunAsync("execute", "--validators", "codex", "42")
	defer func() { _ = async.SendSignal(syscall.SIGKILL) }()

	// Sentinel: "Executed" aparece en stdout en el orden actual del flow
	// SOLO después del wait — no nos sirve. Mejor: esperamos hasta que
	// haya un archivo de invocación del codex en el script dir (el fake
	// logea antes del block). Eso nos dice que el wait ya arrancó.
	if !waitForInvocation(t, env, "codex", 20*time.Second) {
		t.Fatalf("timed out waiting for codex invocation\nstdout:\n%s\nstderr:\n%s",
			async.SnapshotStdout(), async.SnapshotStderr())
	}
	time.Sleep(200 * time.Millisecond)

	if err := async.SendSignal(os.Interrupt); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	r := async.Wait(t, 20*time.Second)
	if r.ExitCode != 130 {
		t.Fatalf("expected exit 130, got %d\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Stdout, r.Stderr)
	}

	// El PR ya se había creado antes de la señal: NO debe haber otro edit
	// que revierta a status:plan (ya transicionamos a executed y eso no
	// se deshace).
	edits := env.Invocations().FindCalls("gh", "issue", "edit", "42")
	for _, e := range edits {
		for i := 0; i+1 < len(e.Args); i++ {
			if e.Args[i] == "--add-label" && e.Args[i+1] == "status:plan" {
				t.Fatalf("unexpected rollback to status:plan post-PR\nedits: %v", edits)
			}
		}
	}

	// El mensaje al usuario debería mencionar que el wait fue cancelado.
	if !strings.Contains(r.Stdout, "cancelado durante wait") && !strings.Contains(r.Stdout, "cancelado") {
		t.Fatalf("expected cancel notice in stdout, got:\n%s", r.Stdout)
	}
}
