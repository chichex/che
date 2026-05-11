// chefake is a polymorphic fake used by e2e tests to stand in for external
// binaries (gh, claude, codex, gemini, git). Its identity is determined by
// os.Args[0] via symlinks created by the test harness.
//
// On invocation, chefake reads $CHE_FAKE_SCRIPT_DIR/<identity>.json, walks the
// matchers in order, and emits the first match. Every invocation is appended
// (under flock) to $CHE_FAKE_SCRIPT_DIR/_invocations.jsonl so tests can assert
// on calls after the fact.
//
// When no matcher matches, chefake exits 1 with a diagnostic pointing to the
// script file. Tests that invoke a subprocess without scripting a response
// always fail loudly.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type matcher struct {
	ID            string `json:"id"`
	ArgsRegex     string `json:"args_regex,omitempty"`
	StdinContains string `json:"stdin_contains,omitempty"`
	Consume       bool   `json:"consume,omitempty"`
	CaptureStdin  bool   `json:"capture_stdin,omitempty"`
	Stdout        string `json:"stdout,omitempty"`
	StdoutFile    string `json:"stdout_file,omitempty"`
	Stderr        string `json:"stderr,omitempty"`
	Exit          int    `json:"exit,omitempty"`
	Passthrough   bool   `json:"passthrough,omitempty"` // reserved for git passthrough mode
	PassthroughTo string `json:"passthrough_to,omitempty"`
	// TouchFiles lista paths (relativos al cwd donde se ejecutó el fake) que
	// el fake debe crear con el contenido dado al matchear. Usado por tests
	// de execute para simular que el agente modificó archivos en el worktree.
	TouchFiles map[string]string `json:"touch_files,omitempty"`
	// BlockSeconds hace que el fake duerma N segundos después de emitir
	// stdout/stderr antes de salir. Usado por tests de signal handling para
	// simular un agente que tarda (el parent manda SIGTERM y el default
	// handler de Go termina el proceso, interrumpiendo el sleep).
	BlockSeconds int `json:"block_seconds,omitempty"`
	// Stream lista items que el fake emite en orden, cada uno a un stream
	// (stdout/stderr) con un delay opcional entre items. Usado por los
	// tests de H5 (streaming): permite simular un CLI que va emitiendo
	// lineas con sleeps cortos, intercalar stderr+stdout, o enviar lineas
	// > 64 KiB para validar el buffer del scanner. Si Stream esta poblado,
	// el matcher lo emite y luego ignora Stdout / Stderr "estaticos" del
	// matcher (TouchFiles / BlockSeconds / Exit siguen aplicando despues).
	Stream []StreamItem `json:"stream,omitempty"`
	// IgnoreSigterm hace que el fake instale un signal.Notify sobre SIGTERM
	// y descarte la senal: equivalente a `trap '' TERM` en bash. Usado por
	// el test de H9 que valida la escalada SIGTERM → SIGKILL: con
	// IgnoreSigterm + BlockSeconds 10, el parent manda SIGTERM, el fake lo
	// ignora, y el parent debe escalar a SIGKILL al pgid tras CHE_KILL_GRACE.
	// El handler se instala ANTES de cualquier sleep para evitar la race
	// "signal arrives before handler installed".
	IgnoreSigterm bool `json:"ignore_sigterm,omitempty"`
}

// StreamItem es una entrada del Stream del matcher. Sirve para los tests
// que validan streaming de H5: cada item se escribe a stdout o stderr,
// flusheado, con un delay opcional antes (FlushSync evita que el parent
// scanner espere al EOF para ver la linea).
type StreamItem struct {
	// Stream indica el destino: "stdout" (default) o "stderr".
	Stream string `json:"stream,omitempty"`
	// Text es la linea a emitir. El fake appendea "\n" automaticamente
	// (el caller no debe incluirlo).
	Text string `json:"text"`
	// DelayMs es el sleep ANTES de emitir esta linea. Usado para simular
	// un CLI que tarda entre eventos del stream.
	DelayMs int `json:"delay_ms,omitempty"`
}

type script struct {
	Matchers []matcher `json:"matchers"`
	Default  *struct {
		Exit   int    `json:"exit"`
		Stderr string `json:"stderr"`
	} `json:"default,omitempty"`
}

type invocation struct {
	Ts          string   `json:"ts"`
	Seq         int      `json:"seq"`
	Bin         string   `json:"bin"`
	Args        []string `json:"args"`
	StdinSHA    string   `json:"stdin_sha256"`
	StdinBytes  int      `json:"stdin_bytes"`
	StdoutBytes int      `json:"stdout_bytes"`
	StderrBytes int      `json:"stderr_bytes"`
	Exit        int      `json:"exit"`
	MatchedID   string   `json:"matched_id,omitempty"`
	DurationMs  int64    `json:"duration_ms"`
}

func main() {
	start := time.Now()
	identity := filepath.Base(os.Args[0])
	args := os.Args[1:]

	scriptDir := os.Getenv("CHE_FAKE_SCRIPT_DIR")
	if scriptDir == "" {
		fmt.Fprintf(os.Stderr, "chefake: CHE_FAKE_SCRIPT_DIR not set (identity=%s)\n", identity)
		os.Exit(2)
	}

	stdin, _ := io.ReadAll(os.Stdin)
	stdinHash := sha256.Sum256(stdin)
	stdinSHA := hex.EncodeToString(stdinHash[:])

	scriptPath := filepath.Join(scriptDir, identity+".json")
	scr, err := loadScript(scriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chefake: loading %s: %v\n", scriptPath, err)
		logInvocation(scriptDir, invocation{
			Ts: start.UTC().Format(time.RFC3339Nano), Bin: identity, Args: args,
			StdinSHA: stdinSHA, StdinBytes: len(stdin), Exit: 2,
			DurationMs: time.Since(start).Milliseconds(),
		})
		os.Exit(2)
	}

	joinedArgs := strings.Join(args, " ")
	// Fix #114 extendido: claude y codex pasan el prompt por stdin, no argv.
	// Los tests historicos hacen WhenArgsMatch("token-del-content") esperando
	// matchear cuando el content viajaba por argv. Para preservarlos, el
	// regex de args se evalua contra "argv + stdin" cuando el bin es uno de
	// los CLIs que migraron a stdin. WhenStdinContains sigue funcionando
	// independiente (es AND, no OR).
	regexHaystack := joinedArgs
	if (identity == "claude" || identity == "codex") && len(stdin) > 0 {
		regexHaystack = joinedArgs + "\n" + string(stdin)
	}
	var matched *matcher
	var matchedIdx int
	for i := range scr.Matchers {
		m := &scr.Matchers[i]
		if m.ArgsRegex != "" {
			re, err := regexp.Compile(m.ArgsRegex)
			if err != nil {
				continue
			}
			if !re.MatchString(regexHaystack) {
				continue
			}
		}
		if m.StdinContains != "" && !strings.Contains(string(stdin), m.StdinContains) {
			continue
		}
		matched = m
		matchedIdx = i
		break
	}

	seq := nextSeq(scriptDir)

	if matched == nil {
		msg := fmt.Sprintf("chefake: no matcher for %q (fake=%s, scriptDir=%s)\n", joinedArgs, identity, scriptDir)
		exit := 1
		if scr.Default != nil {
			if scr.Default.Stderr != "" {
				msg = scr.Default.Stderr
			}
			exit = scr.Default.Exit
		}
		fmt.Fprint(os.Stderr, msg)
		logInvocation(scriptDir, invocation{
			Ts: start.UTC().Format(time.RFC3339Nano), Seq: seq, Bin: identity, Args: args,
			StdinSHA: stdinSHA, StdinBytes: len(stdin), StderrBytes: len(msg), Exit: exit,
			DurationMs: time.Since(start).Milliseconds(),
		})
		os.Exit(exit)
	}

	if matched.CaptureStdin {
		stdinDir := filepath.Join(scriptDir, "stdins")
		_ = os.MkdirAll(stdinDir, 0o755)
		_ = os.WriteFile(filepath.Join(stdinDir, fmt.Sprintf("%d.bin", seq)), stdin, 0o644)
	}

	stdoutBody := matched.Stdout
	if matched.StdoutFile != "" {
		data, err := os.ReadFile(matched.StdoutFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chefake: reading stdout_file %s: %v\n", matched.StdoutFile, err)
			os.Exit(2)
		}
		stdoutBody = string(data)
	}

	// Side effects: touch_files escribe archivos en cwd para simular que
	// el agente produjo cambios.
	for relPath, content := range matched.TouchFiles {
		dir := filepath.Dir(relPath)
		if dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.WriteFile(relPath, []byte(content), 0o644)
	}

	// Stream items: se emiten en orden, cada uno con su delay y su stream
	// destino. Sirve para los tests de H5 (streaming linea por linea).
	// Si Stream esta poblado, el Stdout/Stderr "estaticos" igual se
	// emiten DESPUES (caso comun: header estatico + body streamed).
	for _, item := range matched.Stream {
		if item.DelayMs > 0 {
			time.Sleep(time.Duration(item.DelayMs) * time.Millisecond)
		}
		dst := os.Stdout
		if item.Stream == "stderr" {
			dst = os.Stderr
		}
		_, _ = io.WriteString(dst, item.Text+"\n")
		_ = dst.Sync()
	}

	_, _ = io.WriteString(os.Stdout, stdoutBody)
	_, _ = io.WriteString(os.Stderr, matched.Stderr)
	// Flush explícito: si el matcher bloquea después, el parent necesita
	// haber recibido el sentinel antes de mandar la señal.
	_ = os.Stdout.Sync()

	if matched.Consume {
		markConsumed(scriptPath, matchedIdx)
	}

	// Log ANTES del bloqueo para que el test pueda verificar la invocación
	// aunque el parent termine al fake por señal durante el sleep (los
	// defers de Go no corren con el handler default de SIGTERM/SIGINT).
	logInvocation(scriptDir, invocation{
		Ts: start.UTC().Format(time.RFC3339Nano), Seq: seq, Bin: identity, Args: args,
		StdinSHA: stdinSHA, StdinBytes: len(stdin),
		StdoutBytes: len(stdoutBody), StderrBytes: len(matched.Stderr),
		Exit: matched.Exit, MatchedID: matched.ID,
		DurationMs: time.Since(start).Milliseconds(),
	})

	if matched.IgnoreSigterm {
		// Instalar handler antes del sleep para que SIGTERM no termine
		// el proceso. signal.Notify con buffer 1 + drenado en goroutine
		// "consume y olvida" — es lo mas cercano a `trap '' TERM` en Go.
		// El parent debera escalar a SIGKILL para que el fake muera.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		go func() {
			for range ch {
				// no-op — descartamos la senal a proposito.
			}
		}()
	}

	if matched.BlockSeconds > 0 {
		// Sleep interruptible: el handler default de Go para
		// SIGTERM/SIGINT termina el proceso, así que el bloqueo se corta
		// apenas el parent mata el pgid. Cuando IgnoreSigterm=true el
		// handler instalado arriba se queda con la senal y el sleep
		// completa hasta el final (o hasta que el parent escale a SIGKILL).
		time.Sleep(time.Duration(matched.BlockSeconds) * time.Second)
	}

	os.Exit(matched.Exit)
}

func loadScript(path string) (*script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &script{}, nil
		}
		return nil, err
	}
	var s script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// markConsumed rewrites the script file with the matcher removed. We hold an
// advisory lock so concurrent invocations don't clobber each other.
func markConsumed(scriptPath string, idx int) {
	lock := scriptPath + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	scr, err := loadScript(scriptPath)
	if err != nil || idx >= len(scr.Matchers) {
		return
	}
	scr.Matchers = append(scr.Matchers[:idx], scr.Matchers[idx+1:]...)
	data, _ := json.MarshalIndent(scr, "", "  ")
	_ = os.WriteFile(scriptPath, data, 0o644)
}

// nextSeq returns a monotonic sequence number scoped to scriptDir, via a
// flock-protected counter file.
func nextSeq(scriptDir string) int {
	path := filepath.Join(scriptDir, "_seq")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, _ := io.ReadAll(f)
	var cur int
	if len(data) > 0 {
		_, _ = fmt.Sscanf(string(data), "%d", &cur)
	}
	cur++
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d", cur)
	return cur
}

func logInvocation(scriptDir string, inv invocation) {
	path := filepath.Join(scriptDir, "_invocations.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, _ := json.Marshal(inv)
	_, _ = f.Write(append(data, '\n'))
}
