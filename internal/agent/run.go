package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RunOpts configura la invocación del agente. Cada flow setea los campos que
// necesita; los que dejás en zero valor caen al comportamiento default:
//   - Ctx=nil       → context.Background() (sólo el timeout cancela).
//   - Dir=""        → hereda cwd del proceso actual.
//   - Timeout=0     → error de validación (el caller DEBE pasarlo; cada flow
//                     tiene su default configurable por env).
//   - Format=""     → tratado como OutputText.
//   - OnLine=nil    → el output se acumula en RunResult.Stdout pero no se
//                     emite en vivo a nadie.
//   - OnStderrLine=nil → el stderr se acumula en RunResult.Stderr pero no se
//                     emite en vivo.
//   - KillGrace=0   → no hay escalado SIGTERM→SIGKILL; Ctx cancel mata el
//                     PID directo (comportamiento de explore/validate/
//                     iterate). Con KillGrace>0, el agente corre en su
//                     propio process group y al cancelar se le manda SIGTERM
//                     al -pgid; si no termina en KillGrace, cmd.WaitDelay
//                     escala a SIGKILL (comportamiento de execute).
//   - StreamFormatter=nil → cada línea de stdout se emite tal cual. Con
//                     formatter, cada línea pasa por él antes de emitirse
//                     (devuelve msg+ok; si ok=false la línea se omite). Las
//                     líneas NO se guardan transformadas en RunResult.Stdout:
//                     el Stdout bruto queda intacto para parsers posteriores.
type RunOpts struct {
	Ctx             context.Context
	Dir             string
	Timeout         time.Duration
	Format          OutputFormat
	OnLine          func(string)
	OnStderrLine    func(string)
	KillGrace       time.Duration
	StreamFormatter func(line string) (string, bool)
}

// RunResult captura el stdout y stderr completos acumulados durante la
// ejecución. Incluso cuando hay streaming en vivo, el caller necesita el
// stdout completo para parsear el JSON final de explore/validate, y el
// stderr completo para mensajes de error con contexto.
type RunResult struct {
	Stdout string
	Stderr string
}

// ErrTimeout se devuelve cuando el contexto interno (compuesto por el parent
// ctx + Timeout) expira por deadline. Los callers lo distinguen de errores
// de exit code para construir mensajes específicos de timeout.
var ErrTimeout = errors.New("agent: timed out")

// ErrCancelled se devuelve cuando el parent ctx se canceló externamente
// (señal, cancel explícito). Los callers lo distinguen para mapearlo a un
// exit code semántico (ExitCancelled en execute).
var ErrCancelled = errors.New("agent: cancelled")

// ExitError wrapea *exec.ExitError preservando el exit code y el stderr
// para que los callers puedan construir su mensaje de error típico
// ("opus exit 1: auth failed"). Se mantiene ErrorIs(err, ErrTimeout)-style
// sin sacrificar detalle.
type ExitError struct {
	Agent    Agent
	ExitCode int
	Stderr   string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s exit %d: %s", e.Agent, e.ExitCode, e.Stderr)
}

// Run invoca al agente con el prompt dado según opts, streamea el output en
// vivo si el caller lo pidió, y devuelve el stdout/stderr completos junto con
// un error si algo falló. El stdout se devuelve siempre (incluso con error)
// para que el caller pueda intentar parsearlo o loguearlo.
func Run(a Agent, prompt string, opts RunOpts) (RunResult, error) {
	parent := opts.Ctx
	if parent == nil {
		parent = context.Background()
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, opts.Timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	format := opts.Format
	if format == "" {
		format = OutputText
	}

	cmd := exec.CommandContext(ctx, a.Binary(), a.InvokeArgs(prompt, format)...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}

	// KillGrace > 0 activa el modo "process group": el agente corre aislado
	// en su propio pgid, y al cancelar el ctx mandamos SIGTERM al -pgid
	// (mata todo el árbol: tool-use bash/ripgrep que claude pudo forkear) y
	// escalamos a SIGKILL si no termina en KillGrace. Sin KillGrace el
	// comportamiento es el de exec.CommandContext: cancelar ctx → Process.Kill
	// directo al PID, sin escalado.
	if opts.KillGrace > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err != nil {
				return cmd.Process.Signal(syscall.SIGTERM)
			}
			return syscall.Kill(-pgid, syscall.SIGTERM)
		}
		cmd.WaitDelay = opts.KillGrace
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{}, err
	}

	if err := cmd.Start(); err != nil {
		return RunResult{}, fmt.Errorf("starting %s: %w", a.Binary(), err)
	}

	var fullStdout, fullStderr strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go drainPipe(&wg, stdoutPipe, &fullStdout, opts.OnLine, opts.StreamFormatter)
	go drainPipe(&wg, stderrPipe, &fullStderr, opts.OnStderrLine, nil)

	// IMPORTANTE: las goroutines DEBEN terminar antes de cmd.Wait() — los
	// docs de exec.Cmd.StdoutPipe dicen "it is thus incorrect to call Wait
	// before all reads from the pipe have completed". Al revés se pierden
	// bytes de stdout bajo carga y el JSON llega truncado al parser.
	wg.Wait()
	waitErr := cmd.Wait()

	res := RunResult{Stdout: fullStdout.String(), Stderr: fullStderr.String()}

	// Cancelación por parent (señal del caller, ctrl+c en la TUI, etc.)
	// tiene prioridad sobre timeout: si el parent murió, no es "timeout del
	// agente", es cancelación de usuario.
	if parent.Err() != nil {
		return res, ErrCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, ErrTimeout
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return res, &ExitError{
				Agent:    a,
				ExitCode: ee.ExitCode(),
				Stderr:   strings.TrimSpace(res.Stderr),
			}
		}
		return res, waitErr
	}
	return res, nil
}

// drainPipe lee un pipe línea por línea, acumula todo en full (sin
// transformar, para preservar stdout/stderr bruto) y reenvía cada línea al
// callback. Si formatter != nil, la línea pasa por él antes de emitirse;
// formatter puede devolver ok=false para omitir la línea del callback.
func drainPipe(wg *sync.WaitGroup, r io.Reader, full *strings.Builder, cb func(string), formatter func(string) (string, bool)) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	// Buffer 16 MiB: claude con stream-json puede emitir eventos gigantes
	// (tool_result con outputs largos), y queremos que no nos corte mitad de
	// un JSON.
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		full.WriteString(line + "\n")
		out := line
		if formatter != nil {
			msg, ok := formatter(line)
			if !ok {
				continue
			}
			out = msg
		}
		if strings.TrimSpace(out) != "" && cb != nil {
			cb(out)
		}
	}
}
