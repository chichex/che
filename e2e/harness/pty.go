// pty.go runs che under a pseudo-terminal so bubbletea-based TUIs entran en
// raw mode y reaccionan a teclas como en una terminal real. La API estandar
// (Run / RunWithStdin) pipea stdin sin TTY — sirve para subcomandos no
// interactivos pero rompe el menu y los wizards.
//
// Uso tipico:
//
//	p := env.StartPTY()
//	defer p.Close()
//	p.WaitForOutput(t, "Create pipeline", 3*time.Second)
//	mark := p.Mark()
//	p.Send("2")
//	p.WaitForOutputSince(t, mark, "wizard pendiente", 3*time.Second)
//	res := p.Wait(t, 3*time.Second)
package harness

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Bubbletea (vía charmbracelet/x) consulta capacidades del terminal al
// arrancar — sin respuesta queda esperando y nunca renderiza. Respondemos
// con valores por defecto razonables: fondo negro (oscuro) y cursor en 1,1.
var (
	osc11Query = regexp.MustCompile(`\x1b\]11;\?(?:\x07|\x1b\\)`)
	dsrQuery   = regexp.MustCompile(`\x1b\[6n`)
	osc11Reply = []byte("\x1b]11;rgb:0000/0000/0000\x07")
	dsrReply   = []byte("\x1b[1;1R")
)

// PTYRun is a handle over a che invocation running under a pseudo-terminal.
type PTYRun struct {
	cmd  *exec.Cmd
	ptmx io.ReadWriteCloser

	mu     sync.Mutex
	output bytes.Buffer
	done   chan struct{}
	exit   int
	err    error
}

// Mark is an opaque cursor into the captured output, returned by Mark() and
// consumed by WaitForOutputSince. Used to assert "X appears AFTER this point"
// when the same string was rendered earlier (p.ej. menu se redibuja tras esc).
type Mark int

// StartPTY launches che under a pty with the sandbox env. Stdin/stdout/stderr
// del child quedan conectados al slave del pty; escribimos teclas via Send y
// leemos todo lo que el child emite en el buffer interno.
func (e *Env) StartPTY(args ...string) *PTYRun {
	e.t.Helper()
	cmd := exec.Command(chePathOrFail(e.t), args...)
	cmd.Dir = e.RepoDir
	cmd.Env = e.buildEnv()

	ptmx, err := pty.Start(cmd)
	if err != nil {
		e.t.Fatalf("StartPTY: %v", err)
	}

	p := &PTYRun{cmd: cmd, ptmx: ptmx, done: make(chan struct{})}

	// Drain the master fd into the buffer. Cuando el child termina y cerramos
	// el pty, ptmx.Read devuelve EOF / io/fs error y la goroutine corta.
	// En el camino respondemos automaticamente las queries de capabilities
	// (OSC 11 background, DSR cursor pos) — bubbletea espera estas
	// respuestas antes del primer render.
	go func() {
		buf := make([]byte, 4096)
		var pending bytes.Buffer
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				p.mu.Lock()
				p.output.Write(chunk)
				p.mu.Unlock()

				pending.Write(chunk)
				replyToQueries(ptmx, &pending)
			}
			if rerr != nil {
				return
			}
		}
	}()

	go func() {
		werr := cmd.Wait()
		p.mu.Lock()
		p.err = werr
		if werr != nil {
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				p.exit = ee.ExitCode()
			} else {
				p.exit = -1
			}
		}
		p.mu.Unlock()
		close(p.done)
	}()

	return p
}

// Send writes raw bytes al pty master. Para teclas especiales: esc = "\x1b",
// enter = "\r", ctrl+c = "\x03", tab = "\t". Bubbletea aplica un debounce
// interno sobre esc, asi que conviene esperar a que la pantalla redibuje
// antes de mandar la siguiente tecla.
func (p *PTYRun) Send(s string) error {
	_, err := p.ptmx.Write([]byte(s))
	return err
}

// Snapshot devuelve una copia del output capturado hasta ahora. Incluye
// secuencias ANSI (lipgloss + bubbletea) — para sustring matching alcanza
// porque los estilos envuelven el texto sin cortarlo char-a-char.
func (p *PTYRun) Snapshot() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.output.String()
}

// Mark devuelve un cursor al final del output actual. Sirve para asserts
// del estilo "tras esc el menu se redibuja" cuando el mismo texto ya
// aparecio antes en el buffer.
func (p *PTYRun) Mark() Mark {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Mark(p.output.Len())
}

// Since devuelve el output desde la marca dada en adelante.
func (p *PTYRun) Since(m Mark) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.output.String()
	if int(m) >= len(s) {
		return ""
	}
	return s[m:]
}

// WaitForOutput bloquea hasta que el output total contenga substr o expire
// el timeout. Devuelve true si lo encontro.
func (p *PTYRun) WaitForOutput(t *testing.T, substr string, timeout time.Duration) bool {
	t.Helper()
	return p.waitFor(timeout, func() bool {
		return strings.Contains(p.Snapshot(), substr)
	})
}

// WaitForOutputSince es como WaitForOutput pero solo mira lo que llego despues
// de la marca dada.
func (p *PTYRun) WaitForOutputSince(t *testing.T, m Mark, substr string, timeout time.Duration) bool {
	t.Helper()
	return p.waitFor(timeout, func() bool {
		return strings.Contains(p.Since(m), substr)
	})
}

func (p *PTYRun) waitFor(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		select {
		case <-p.done:
			return pred()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false
}

// Wait bloquea hasta que el child termine o expire el timeout. Si timeoutea,
// mata el proceso y devuelve un Result con ExitCode=-1.
func (p *PTYRun) Wait(t *testing.T, timeout time.Duration) Result {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Result{Stdout: p.output.String(), ExitCode: p.exit, Err: p.err}
}

// Close libera el pty master. Cerrar el master mientras el child corre
// suele triggerar SIGHUP en el child; usar despues de Wait o cuando se
// quiere abortar el run.
func (p *PTYRun) Close() {
	_ = p.ptmx.Close()
}

// replyToQueries consume del buffer cualquier query terminal completa y
// envia la respuesta al pty master. Si una query queda partida entre dos
// reads, la deja en pending para que el proximo read la complete. Solo
// removemos del buffer las queries que matchean en orden de aparicion;
// bytes intermedios se descartan (no nos interesan para el responder).
func replyToQueries(w io.Writer, pending *bytes.Buffer) {
	data := pending.Bytes()
	consumed := 0
	for consumed < len(data) {
		rest := data[consumed:]
		idx, match := earliest(rest, osc11Query, dsrQuery)
		if idx < 0 {
			break
		}
		var reply []byte
		switch match {
		case osc11Query:
			reply = osc11Reply
		case dsrQuery:
			reply = dsrReply
		}
		_, _ = w.Write(reply)
		// Avanzamos hasta el final del match.
		loc := match.FindIndex(rest)
		if loc == nil {
			break
		}
		consumed += loc[1]
	}
	// Conservamos lo que sobre (puede contener inicio de una query parcial).
	leftover := append([]byte{}, data[consumed:]...)
	pending.Reset()
	pending.Write(leftover)
}

// earliest devuelve el indice del match mas temprano y el regex que matcheo.
// -1 si ninguno matchea.
func earliest(data []byte, candidates ...*regexp.Regexp) (int, *regexp.Regexp) {
	bestIdx := -1
	var bestRe *regexp.Regexp
	for _, re := range candidates {
		loc := re.FindIndex(data)
		if loc == nil {
			continue
		}
		if bestIdx < 0 || loc[0] < bestIdx {
			bestIdx = loc[0]
			bestRe = re
		}
	}
	return bestIdx, bestRe
}
