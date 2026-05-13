package runner

import (
	"bufio"
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
	"github.com/chichex/che/internal/runner/parser"
	"github.com/chichex/che/internal/wizard"
)

// killGraceDefault es el TTL entre SIGTERM y SIGKILL durante el cancel del
// step. El doc lo deja configurable via env CHE_KILL_GRACE (default 5s).
const killGraceDefault = 5 * time.Second

// stepEventsPath devuelve el archivo events.jsonl activo del step idx
// (1-based) en la corrida K = eventsRun (1-based). Si K <= 0, devuelve el
// nombre legacy sin sufijo de corrida — preserva compat con tests viejos
// que invocan runStep sin contador. Fix #107: cada rerun escribe a su
// propio archivo para no perder la traza de las vueltas previas.
func stepEventsPath(runDir string, idx, eventsRun int) string {
	if eventsRun <= 0 {
		return filepath.Join(runDir, fmt.Sprintf("step-%02d.events.jsonl", idx))
	}
	return filepath.Join(runDir, fmt.Sprintf("step-%02d.events.RUN-%02d.jsonl", idx, eventsRun))
}

// scannerBufferMax es el cap del buffer de bufio.Scanner. El default (64
// KiB) trunca lineas largas SILENCIOSAMENTE — la memoria del proyecto
// "bufio.Scanner 64 KiB silent drop" lo deja explicito. H5 lo sube a 1 MiB
// segun el criterio del doc; cualquier linea por encima de eso se loggea
// como warning + cae a stdout crudo (no es RF — defensive).
const scannerBufferMax = 1024 * 1024

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

	// lineCh es el canal por donde la goroutine del tee emite eventos al
	// program de bubbletea. Cada mensaje es un stepLineMsg (linea humana)
	// o el sentinel stepDoneMsg (cuando el subprocess termina). El
	// program corre un tea.Cmd recursivo (waitForLine) que bloquea sobre
	// este canal y re-emite el siguiente.
	//
	// Buffer generoso (256) para amortiguar bursts de stream-json sin
	// bloquear la goroutine del scanner.
	lineCh chan tea.Msg

	// done indica que la goroutina del wait ya escribio el stepDoneMsg en
	// lineCh — sirve para no doblar el envio si la cancel llega tarde.
	done bool
}

// spawnCmdFn es la factoria swappable usada por R3 para spawnear el step.
// Tests unitarios la reemplazan; los e2e dejan el default que arma el
// exec.Cmd real (con el cli del step apuntando al symlink chefake).
//
// Cambio H5: la firma se mantiene para no romper tests viejos. La logica
// de stream-json va dentro de buildSpawnArgs (ahora claude pasa
// --output-format stream-json --verbose por default).
var spawnCmdFn = defaultSpawnCmd

// defaultSpawnCmd construye el comando segun el cli/kind del step. Tabla
// del doc:
//
//	claude   -p --output-format stream-json --verbose --model <model>
//	codex    exec --json
//	gemini   -p <prompt>  /  /<skill>
//	opencode run <prompt>
//
// El payload del input (R1) se envia por stdin (criterio del doc:
// "todas pasan el input/payload por stdin para evitar problemas de escaping
// de shell"). Antes de armar args, interpolamos `{{INPUT}}` en step.Content
// con el payload resuelto — el payload sigue viajando por stdin tambien para
// preservar la compat (Fix #107).
//
// Fix #114 (E2BIG): para CLI=codex/claude el prompt interpolado viaja por
// stdin en lugar de argv, eliminando el "argument list too long" cuando
// {{INPUT}} se sustituye por payloads grandes. buildSpawnArgs devuelve los
// args sin el content; el content interpolado viaja por stdin. El stdin se
// arma segun si el content original embedia {{INPUT}}:
//   - Si SI embedia {{INPUT}}: stdin = content interpolado (payload ya inline).
//   - Si NO embedia {{INPUT}}: stdin = content + "\n" + payload (payload
//     concatenado para que el CLI lo reciba como bloque <stdin> adicional,
//     necesario p.ej. en validators que no embeben {{INPUT}} pero leen la
//     salida del step previo desde stdin).
//
// Para gemini/opencode no hay cambios: content por argv + payload por stdin.
func defaultSpawnCmd(step wizard.Step, payload string) (*exec.Cmd, error) {
	stepForArgs := step
	stepForArgs.Content = interpolateInput(step.Content, payload)
	args, err := buildSpawnArgs(stepForArgs)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(step.CLI, args...)
	if step.CLI == "codex" || step.CLI == "claude" {
		// Fix #114: prompt via stdin para evitar E2BIG.
		// Si el content original embedia {{INPUT}}, el payload ya quedo
		// sustituido inline — mandamos solo el content interpolado.
		// Si no embedia {{INPUT}}, concatenamos payload al final para que
		// el CLI lo reciba (validators que leen la salida del step previo
		// desde stdin sin placeholder explicit).
		stdinContent := stepForArgs.Content
		if !strings.Contains(step.Content, "{{INPUT}}") {
			stdinContent = stepForArgs.Content + "\n" + payload
		}
		cmd.Stdin = strings.NewReader(stdinContent)
	} else {
		cmd.Stdin = strings.NewReader(payload)
	}
	return cmd, nil
}

// interpolateInput reemplaza el placeholder `{{INPUT}}` en content por el
// payload resuelto. El payload sigue enviandose por stdin ademas de quedar
// sustituido en el content — los CLIs que asumen el placeholder en el prompt
// (ej. che-funnel.yaml) lo necesitan inline; el stdin queda como fallback.
//
// Si content no contiene `{{INPUT}}`, devuelve content sin cambios. Sin
// escape adicional: el payload se inserta literal.
func interpolateInput(content, payload string) string {
	if !strings.Contains(content, "{{INPUT}}") {
		return content
	}
	return strings.ReplaceAll(content, "{{INPUT}}", payload)
}

// modelFor resuelve el modelo a pasarle al CLI del step. Si el step declara
// `model:` explicito en el YAML lo respeta; si no, cae al default por CLI
// (ver defaultModelByCLI en models.go). Para CLIs sin default y sin override
// devuelve "" — el caller decide si emitir el flag o no (opencode entra
// por este path: defaultModelByCLI["opencode"] == "" y buildSpawnArgs no
// agrega el flag).
//
// La validacion del whitelist (rechazar modelos no soportados) ocurre en
// preflight, no aca: este helper trabaja con el string ya aceptado.
func modelFor(step wizard.Step) string {
	if m := strings.TrimSpace(step.Model); m != "" {
		return m
	}
	return DefaultModel(step.CLI)
}

// buildSpawnArgs es la parte testeable de defaultSpawnCmd: dado un step,
// devuelve los args para el subprocess (sin tocar exec.Command).
//
// H5: claude pasa --output-format stream-json --verbose (el parser
// de internal/runner/parser/claude.go lo consume). codex se mantiene en
// `exec --json` (parser stub que cae a raw — ver parser/codex.go).
//
// Fix #114 extendido: claude/codex omiten el content del argv — el prompt
// viaja por stdin (ver defaultSpawnCmd) para evitar E2BIG cuando {{INPUT}} se
// expande a payloads grandes.
//
// Issue #142: el flag de modelo se inyecta por CLI cuando hay default o
// override declarado en el YAML:
//   - claude → `--model <X>` (default opus)
//   - codex  → `-m <X>` (default gpt-5.5)
//   - gemini → `-m <X>` (default gemini-2.5-pro)
//   - opencode → no se inyecta (la config vive en el CLI propio)
func buildSpawnArgs(step wizard.Step) ([]string, error) {
	if step.CLI == "" {
		return nil, fmt.Errorf("step sin cli")
	}
	if step.Content == "" {
		return nil, fmt.Errorf("step sin content")
	}
	model := modelFor(step)
	switch step.CLI {
	case "claude":
		// Fix #114 extendido: omitir el content del argv — el prompt
		// viaja por stdin (ver defaultSpawnCmd). claude -p sin arg
		// posicional lee el prompt de stdin (modo pipe declarado en
		// `claude --help`: "-p/--print ... useful for pipes").
		args := []string{
			"-p",
			"--output-format", "stream-json",
			"--verbose",
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		return args, nil
	case "codex":
		// Fix #114: omitir el content como tercer argv — el prompt viaja
		// por stdin (ver defaultSpawnCmd). Esto elimina E2BIG cuando el
		// content interpolado supera ARG_MAX (~256 KB en macOS).
		// codex exec lee el prompt desde stdin cuando se omite el arg
		// posicional (validado contra `codex exec --help`).
		args := []string{"exec", "--json"}
		if model != "" {
			args = append(args, "-m", model)
		}
		return args, nil
	case "gemini":
		// kind=skill se invoca como /<skill> en gemini (alias TOML).
		if step.Kind == wizard.KindSkill {
			args := []string{}
			if model != "" {
				args = append(args, "-m", model)
			}
			return append(args, "/"+step.Content), nil
		}
		args := []string{}
		if model != "" {
			args = append(args, "-m", model)
		}
		return append(args, "-p", step.Content), nil
	case "opencode":
		// opencode no soporta override de modelo desde YAML — la validacion
		// del whitelist en preflight rechaza step.Model != "" para este CLI.
		// Aca no se inyecta flag, queda lo que tenga configurado el propio CLI.
		return []string{"run", step.Content}, nil
	default:
		return nil, fmt.Errorf("cli %q no soportado", step.CLI)
	}
}

// stepLineMsg lo emite la goroutine del tee cuando aparece una nueva linea
// (humana, post-parser) en stdout o stderr del subprocess. El handler de R3
// lo appendea al ring buffer del step + (si stream-json) appendea el evento
// crudo a events.jsonl. Es el msg que viaja decenas-centenas de veces por
// step (vs stepDoneMsg que llega 1 sola vez).
type stepLineMsg struct {
	Idx  int
	Line parser.Line
	// Seq es el numero global asignado por el ring buffer al appendear.
	// Lo usamos para correlacionar con events.jsonl.
	Seq uint64
}

// stepDoneMsg lo emite la goroutine del wait al terminar el subprocess.
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

// runStep arranca el subprocess en MODO STREAMING (cmd.Start()), lanza
// goroutines de tee para stdout/stderr (cada una scanea linea por linea
// con un buffer de 1 MiB y empuja stepLineMsg al lineCh del runState), y
// devuelve un tea.Cmd que es el primer "wait for next line". El program
// vuelve a llamar al cmd retornado por handleStepLine para chainear las
// lineas siguientes; cuando el subprocess termina, la goroutine del wait
// emite stepDoneMsg y cierra el canal.
//
// runDir tiene que existir (lo crea el caller — initRunDir). state es el
// pointer compartido con el modelo: se popula con cmd y los paths de log.
//
// H5 reemplaza el cmd.Run() bloqueante de H4 por este pipeline de
// streaming. Stdin sigue siendo el ResolvedPayload del input (R1).
// runStep arranca el subprocess y rota `events.jsonl` a un archivo por
// corrida (`step-NN.events.RUN-K.jsonl`) — K = eventsRun, 1-based.
// `events.jsonl` se mantiene como alias del archivo activo (copia al cerrar
// el step) para compat con consumidores externos. Si eventsRun <= 0, caemos
// al nombre legacy `step-NN.events.jsonl` para no romper tests viejos que
// pasan idx sin contador.
func runStep(step wizard.Step, payload string, runDir string, idx int, eventsRun int, state *runState) tea.Cmd {
	startedAt := time.Now()
	// Inicializar lineCh ANTES del start: si Start() falla, el caller
	// igual va a leer del canal (waitForLine) y necesita encontrarlo
	// poblado.
	state.lineCh = make(chan tea.Msg, 256)

	cmd, buildErr := spawnCmdFn(step, payload)
	if buildErr != nil {
		// Error de build: emitimos el done sintetico antes de devolver
		// para que el program reciba la transicion a RF.
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx:       idx,
				ExitCode:  -1,
				StartedAt: startedAt,
				EndedAt:   time.Now(),
				SpawnErr:  buildErr.Error(),
			}
		}()
		return waitForLine(state.lineCh)
	}

	// Archivos de log: stdout / stderr / events.jsonl. events.jsonl solo
	// se popula para CLIs con stream-json (claude). Los otros lo dejan
	// vacio — el doc lo deja explicito ("stdout crudo, sin events.jsonl").
	stdoutPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stdout.log", idx))
	stderrPath := filepath.Join(runDir, fmt.Sprintf("step-%02d.stderr.log", idx))
	eventsPath := stepEventsPath(runDir, idx, eventsRun)

	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
				SpawnErr: fmt.Sprintf("create stdout.log: %v", err),
			}
		}()
		return waitForLine(state.lineCh)
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = stdoutFile.Close()
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
				SpawnErr: fmt.Sprintf("create stderr.log: %v", err),
			}
		}()
		return waitForLine(state.lineCh)
	}
	// events.jsonl: solo lo abrimos para CLIs con stream-json. Para
	// otros, eventsFile queda nil y el writer no escribe nada.
	var eventsFile *os.File
	if step.CLI == "claude" {
		eventsFile, err = os.OpenFile(eventsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = stdoutFile.Close()
			_ = stderrFile.Close()
			go func() {
				state.lineCh <- stepDoneMsg{
					Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
					SpawnErr: fmt.Sprintf("create events.jsonl: %v", err),
				}
			}()
			return waitForLine(state.lineCh)
		}
	}

	// Pipes para stdout/stderr (necesarios para scannear linea por
	// linea en modo streaming). cmd.Stdout = MultiWriter no nos sirve aca
	// porque queremos chunks → lineas → parser → events.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		if eventsFile != nil {
			_ = eventsFile.Close()
		}
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
				SpawnErr: fmt.Sprintf("stdout pipe: %v", err),
			}
		}()
		return waitForLine(state.lineCh)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
		if eventsFile != nil {
			_ = eventsFile.Close()
		}
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
				SpawnErr: fmt.Sprintf("stderr pipe: %v", err),
			}
		}()
		return waitForLine(state.lineCh)
	}

	// Process group propio para que SIGTERM al cmd alcance a los hijos
	// del subprocess (defensivo — los CLIs pueden spawnear helpers).
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
		if eventsFile != nil {
			_ = eventsFile.Close()
		}
		go func() {
			state.lineCh <- stepDoneMsg{
				Idx: idx, ExitCode: -1, StartedAt: startedAt, EndedAt: time.Now(),
				SpawnErr: fmt.Sprintf("start %s: %v", step.CLI, startErr),
			}
		}()
		return waitForLine(state.lineCh)
	}

	// Buffers en RAM para el dump terminal (R4/RF reusa el mismo formato
	// que H4). Streaming los popula en paralelo al log pane.
	var stdoutBuf, stderrBuf strings.Builder
	var bufMu sync.Mutex // protege ambos builders (concat raro en e2e parallel)

	p := parser.ForCLI(step.CLI)

	// Goroutine cancel: si requestCancel llega antes de Wait, SIGTERM al
	// pgid; tras grace SIGKILL. Marcamos cancelled=true ANTES de
	// signalCancel para evitar la race con Wait que puede retornar
	// inmediatamente cuando el subprocess muere.
	//
	// H9 race: si el subprocess salio por su cuenta entre el ctrl+c y el
	// abort del modal (waitDone se cierra primero), el select cae al case
	// waitDone y NO seteamos cancelled. handleStepDone va a tratar el step
	// como done/failed segun el ExitCode real del subprocess (criterio del
	// doc: "Si el subprocess salio por si mismo entre el ctrl+c y la
	// decision del modal, tratar como step normal terminado").
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

	// WG para cerrar lineCh recien cuando ambas goroutines de tee
	// terminaron (sino el program podria leer un done con stdout buf
	// incompleto si la goroutina de stderr todavia escribia).
	var teeWG sync.WaitGroup
	teeWG.Add(2)
	go teeStream(idx, stdoutPipe, stdoutFile, &stdoutBuf, &bufMu, p, eventsFile, false, state, &teeWG)
	go teeStream(idx, stderrPipe, stderrFile, &stderrBuf, &bufMu, parser.Raw(), nil, true, state, &teeWG)

	// Goroutina del wait — al terminar emite stepDoneMsg y cierra el
	// canal para que waitForLine devuelva un sentinel y el handler
	// transicione a R4/RF.
	//
	// H9: Sync + Close de TODOS los handles (stdout/stderr/events) antes de
	// emitir stepDoneMsg. El doc lo deja explicito como criterio de
	// aceptacion de cancel ("Flush + close de stdout/stderr/events files")
	// pero aplica a cualquier salida del subprocess: si el handler de R4/RF
	// vuelve al lister, los logs en disco tienen que estar consistentes y
	// el test tiene que poder re-abrir el archivo sin EBADF.
	go func() {
		teeWG.Wait()
		waitErr := cmd.Wait()
		close(waitDone)
		// Sync antes de Close: el doc fija "Logs flusheados antes del exit".
		// Errores ignorados — best-effort; un Close fallido no debe impedir
		// emitir el stepDoneMsg porque el program quedaria colgado.
		_ = stdoutFile.Sync()
		_ = stdoutFile.Close()
		_ = stderrFile.Sync()
		_ = stderrFile.Close()
		if eventsFile != nil {
			_ = eventsFile.Sync()
			_ = eventsFile.Close()
		}
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
		state.done = true
		state.mu.Unlock()

		// H9 acceptance: cuando el step se cancela, exit_code en disco es
		// -1 (no el valor "real" devuelto por exec — que en SIGKILL es -1
		// igual, pero en SIGTERM puede ser distinto segun el handler del
		// subprocess). El doc lo deja explicito: "step en curso status:
		// cancelled, exit_code: -1". Para detectar "cancel real" descartamos
		// la race: si cancelled=true pero el subprocess salio limpio
		// (waitErr==nil, exit 0) trataremos al step como done en
		// handleStepDone — aca dejamos el ExitCode real para que el handler
		// pueda decidir.
		if cancelled && (waitErr != nil || exitCode != 0) {
			exitCode = -1
		}

		bufMu.Lock()
		stdoutCopy := stdoutBuf.String()
		stderrCopy := stderrBuf.String()
		bufMu.Unlock()

		state.lineCh <- stepDoneMsg{
			Idx:       idx,
			ExitCode:  exitCode,
			Stdout:    stdoutCopy,
			Stderr:    stderrCopy,
			StartedAt: startedAt,
			EndedAt:   endedAt,
			SpawnErr:  spawnErr,
			Cancelled: cancelled,
		}
	}()

	return waitForLine(state.lineCh)
}

// waitForLine es el tea.Cmd recursivo que el program dispara para drenar el
// lineCh del runState. Bloquea hasta que llegue un msg (stepLineMsg o
// stepDoneMsg) y lo devuelve. handleStepLine vuelve a issuear este cmd
// hasta que llegue stepDoneMsg.
func waitForLine(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			// Canal cerrado sin done — defensive: emit un done sintetico
			// para que el program no quede colgado esperando.
			return stepDoneMsg{ExitCode: -1, SpawnErr: "channel closed unexpectedly"}
		}
		return msg
	}
}

// teeStream lee linea por linea de pipe (stdout / stderr del subprocess),
// la escribe al file de log + al builder en RAM (con su mutex), la pasa al
// parser, y empuja el resultado al lineCh como stepLineMsg. Si el parser
// devuelve un evento crudo (no vacio), lo appendea a eventsFile.
//
// Buffer del scanner subido a 1 MiB (memory bufio.Scanner). Si igual se
// excede, scanner.Err() devuelve bufio.ErrTooLong — emit un warning como
// stderr y caer a "stdout crudo" para esa linea (no RF — defensive segun
// la tabla de errores del doc).
func teeStream(
	idx int,
	pipe io.ReadCloser,
	logFile io.Writer,
	bufBuilder *strings.Builder,
	bufMu *sync.Mutex,
	p parser.Parser,
	eventsFile io.Writer,
	isStderr bool,
	state *runState,
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	defer pipe.Close()

	scanner := bufio.NewScanner(pipe)
	// Buffer grande para evitar truncate silencioso en lineas de
	// stream-json largas (memory bufio.Scanner 64 KiB silent drop).
	scanner.Buffer(make([]byte, 64*1024), scannerBufferMax)

	for scanner.Scan() {
		raw := scanner.Text()
		// Disco: append crudo + newline (preservamos el formato original
		// para que stdout.log / stderr.log sean fieles).
		_, _ = io.WriteString(logFile, raw+"\n")
		if syncer, ok := logFile.(*os.File); ok {
			_ = syncer.Sync()
		}
		// RAM (dump terminal — H4 lo usaba; H5 lo conserva para R4/RF).
		bufMu.Lock()
		bufBuilder.WriteString(raw)
		bufBuilder.WriteString("\n")
		bufMu.Unlock()

		// Parser → lineas humanas + evento crudo.
		lines, ev := p.Parse(raw)
		if !ev.Empty() && eventsFile != nil {
			_, _ = io.WriteString(eventsFile, ev.Raw+"\n")
			if syncer, ok := eventsFile.(*os.File); ok {
				_ = syncer.Sync()
			}
		}
		for _, line := range lines {
			line.Stderr = line.Stderr || isStderr
			state.lineCh <- stepLineMsg{Idx: idx, Line: line}
		}
	}
	// Errores del scanner: ErrTooLong (linea > 1 MiB) cae a un warning
	// dimmed; otros errores (pipe cerrado, etc) los ignoramos —
	// generalmente son consecuencia de cmd.Wait() retornando antes.
	if err := scanner.Err(); err != nil && errors.Is(err, bufio.ErrTooLong) {
		state.lineCh <- stepLineMsg{
			Idx: idx,
			Line: parser.Line{
				Text:   "! linea > 1 MiB descartada (parser bufio.ErrTooLong)",
				Stderr: true,
			},
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
