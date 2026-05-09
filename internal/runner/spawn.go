package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/wizard"
)

// killGraceDefault es el TTL entre SIGTERM y SIGKILL durante el cancel del
// step. El doc lo deja configurable via env CHE_KILL_GRACE (default 5s).
const killGraceDefault = 5 * time.Second

// runState agrupa los handles vivos del spawn actual. Como los modelos
// bubbletea se pasan por valor, lo mantenemos como puntero y se lo damos al
// modelo via RunModel.runState — asi el handler de cancel puede leer cmd /
// signalCancel desde cualquier copia del Update.
type runState struct {
	mu            sync.Mutex
	cmd           *exec.Cmd
	stdoutPath    string
	stderrPath    string
	requestCancel chan struct{}
	cancelled     bool
}

// spawnCmdFn es la factoria swappable usada por R3 para spawnear el step.
// Tests unitarios la reemplazan; los e2e dejan el default que arma el
// exec.Cmd real (con el cli del step apuntando al symlink chefake).
var spawnCmdFn = defaultSpawnCmd

// defaultSpawnCmd construye el comando segun el cli/kind del step. H4
// soporta el subset minimo (cli del step, kind prompt o skill, content
// crudo). Tabla del doc:
//
//	claude   -p <skill-or-prompt> --output-format stream-json --verbose
//	codex    exec --json <skill-or-prompt>
//	gemini   -p <prompt>  /  /<skill>
//	opencode run <prompt>
//
// H4 no implementa stream-json (out of scope del doc — es H5). Usamos
// invocaciones blocking-friendly: para claude/codex caemos a `<cli> -p
// <content>`, para gemini el mismo flag, para opencode `run`. El parser
// de stream-json llega en H5 — por eso H4 no agrega --output-format.
//
// El payload del input (R1) se envia por stdin (criterio del doc:
// "todas pasan el input/payload por stdin para evitar problemas de escaping
// de shell").
func defaultSpawnCmd(step wizard.Step, payload string) (*exec.Cmd, error) {
	args, err := buildSpawnArgs(step)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(step.CLI, args...)
	cmd.Stdin = strings.NewReader(payload)
	return cmd, nil
}

// buildSpawnArgs es la parte testeable de defaultSpawnCmd: dado un step,
// devuelve los args para el subprocess (sin tocar exec.Command). H4 solo
// cubre los 4 CLIs canonicos; cualquier otro cae en error explicito en vez
// de un default silencioso.
func buildSpawnArgs(step wizard.Step) ([]string, error) {
	if step.CLI == "" {
		return nil, fmt.Errorf("step sin cli")
	}
	if step.Content == "" {
		return nil, fmt.Errorf("step sin content")
	}
	switch step.CLI {
	case "claude":
		// H4 usa text mode (stream-json llega en H5). `-p` toma el
		// prompt o el nombre de la skill — claude resuelve ambos.
		return []string{"-p", step.Content}, nil
	case "codex":
		return []string{"exec", step.Content}, nil
	case "gemini":
		// kind=skill se invoca como /<skill> en gemini (alias TOML).
		if step.Kind == wizard.KindSkill {
			return []string{"/" + step.Content}, nil
		}
		return []string{"-p", step.Content}, nil
	case "opencode":
		return []string{"run", step.Content}, nil
	default:
		return nil, fmt.Errorf("cli %q no soportado en H4", step.CLI)
	}
}

// stepDoneMsg lo emite la goroutine del spawn al terminar el subprocess.
// Lo consume R3 para transicionar a R4 / RF segun el ExitCode + error.
type stepDoneMsg struct {
	Idx       int
	ExitCode  int
	Stdout    string
	Stderr    string
	StartedAt time.Time
	EndedAt   time.Time
	// SpawnErr captura un error de Start() (binario inexistente, permisos)
	// o un Wait() roto (no-ExitError). Exit code "normal" no-cero NO
	// llega aca — va en ExitCode.
	SpawnErr string
	// Cancelled = true cuando el Wait() devolvio porque el caller mando
	// SIGTERM/SIGKILL desde RC. R3 lo trata como "manifest cancelled".
	Cancelled bool
}

// stepLogTickMsg podria usarse para refrescar el log pane mientras el
// subprocess corre. H4 no la emite (blocking) — H5 la va a usar para
// streaming. Se deja declarada para que el TODO sea trivial.
//
// type stepLogTickMsg struct { Idx int }

// runStep arranca el subprocess, redirige stdout/stderr a los archivos de
// log + buffers en RAM, y devuelve un tea.Cmd que bloquea hasta el Wait().
// El log pane se renderea con los buffers al terminar (no streaming en H4).
//
// runDir tiene que existir (lo crea el caller — initRunDir). state es el
// pointer compartido con el modelo: se popula con cmd y los paths de log.
func runStep(step wizard.Step, payload string, runDir string, idx int, state *runState) tea.Cmd {
	return func() tea.Msg {
		startedAt := time.Now()
		cmd, err := spawnCmdFn(step, payload)
		if err != nil {
			return stepDoneMsg{
				Idx:       idx,
				ExitCode:  -1,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				SpawnErr:  err.Error(),
			}
		}
		// Los archivos de log viven en runDir/step-NN.{stdout,stderr}.log
		// segun el "Layout en disco" del doc. H4 los crea con permisos
		// 0600 (file) — el dir 0700 lo crea initRunDir.
		stdoutPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stdout.log", idx))
		stderrPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stderr.log", idx))
		stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return stepDoneMsg{
				Idx:       idx,
				ExitCode:  -1,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				SpawnErr:  fmt.Sprintf("create stdout.log: %v", err),
			}
		}
		stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = stdoutFile.Close()
			return stepDoneMsg{
				Idx:       idx,
				ExitCode:  -1,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				SpawnErr:  fmt.Sprintf("create stderr.log: %v", err),
			}
		}

		// Tee a buffer en RAM + archivo. H4 hace dump al final, asi que el
		// buffer queda en memoria y se vuelca al modelo via stepDoneMsg.
		var stdoutBuf, stderrBuf strings.Builder
		cmd.Stdout = io.MultiWriter(stdoutFile, &stdoutBuf)
		cmd.Stderr = io.MultiWriter(stderrFile, &stderrBuf)
		// Process group propio para que SIGTERM al cmd alcance a los
		// hijos del subprocess (defensivo — los CLIs pueden spawnear
		// helpers).
		setProcAttrs(cmd)

		state.mu.Lock()
		state.cmd = cmd
		state.stdoutPath = stdoutPath
		state.stderrPath = stderrPath
		state.mu.Unlock()

		startErr := cmd.Start()
		if startErr != nil {
			_ = stdoutFile.Close()
			_ = stderrFile.Close()
			return stepDoneMsg{
				Idx:       idx,
				ExitCode:  -1,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				SpawnErr:  fmt.Sprintf("start %s: %v", step.CLI, startErr),
			}
		}

		// Goroutine de cancel: si requestCancel llega antes de Wait,
		// SIGTERM al pgid; tras grace SIGKILL. Marcamos cancelled=true
		// ANTES de signalCancel para evitar la race con Wait() que puede
		// retornar inmediatamente cuando el subprocess muere por SIGTERM:
		// si el flag se setea despues, handleStepDone lee false y trata
		// el run como failed en vez de cancelled.
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-state.requestCancel:
				state.mu.Lock()
				state.cancelled = true
				state.mu.Unlock()
				signalCancel(cmd, killGrace())
			case <-waitDone:
			}
		}()

		waitErr := cmd.Wait()
		close(waitDone)
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		endedAt := time.Now()

		exitCode := 0
		spawnErr := ""
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
				spawnErr = waitErr.Error()
			}
		}

		state.mu.Lock()
		cancelled := state.cancelled
		state.mu.Unlock()

		return stepDoneMsg{
			Idx:       idx,
			ExitCode:  exitCode,
			Stdout:    stdoutBuf.String(),
			Stderr:    stderrBuf.String(),
			StartedAt: startedAt,
			EndedAt:   endedAt,
			SpawnErr:  spawnErr,
			Cancelled: cancelled,
		}
	}
}

// signalCancel manda SIGTERM al process group y, si no salio en grace,
// SIGKILL. Se asume que setProcAttrs puso al cmd en su propio pgid (asi
// matamos al hijo + sus eventuales sub-hijos). En tests donde la setpgid
// falla (p.ej. fake instantaneo), igual cae al Kill() directo del cmd
// para no quedarse colgado.
func signalCancel(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid != 0 {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	// Esperamos hasta `grace` por una salida ordenada antes de SIGKILL.
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			if pgid != 0 {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				_ = cmd.Process.Kill()
			}
			return
		case <-tick.C:
			// ProcessState != nil → ya termino — Wait() en la otra
			// goroutine lo va a recoger.
			if cmd.ProcessState != nil {
				return
			}
		}
	}
}

// killGrace lee CHE_KILL_GRACE (segundos enteros). Default killGraceDefault.
// Cualquier valor invalido cae al default.
func killGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CHE_KILL_GRACE"))
	if raw == "" {
		return killGraceDefault
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		return killGraceDefault
	}
	return time.Duration(n) * time.Second
}
