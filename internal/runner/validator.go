// validator.go implementa el loop de cross-review (H7) que corre despues de
// cada step con bloque `validator` declarado en el pipeline. La idea es:
//
//  1. el step termina con exit_code: 0 → spawneamos un subprocess validator
//     con el output del step + un preambulo que pide un bloque YAML
//     `verdict: ok|fail` + opcional `feedback`.
//  2. parseamos el stdout del validator buscando el ULTIMO bloque YAML que
//     contenga la clave `verdict` (parser tolerante: ignora basura antes,
//     intenta varios formatos).
//  3. verdict ok → siguiente step (final_verdict=ok, manifest cierra el
//     bloque validator).
//  4. verdict fail + loops_run < max_loops → re-corremos EL STEP (no el
//     validator) con el feedback como contexto extra al payload.
//  5. hit max_loops → aplicamos on_max_loops:
//     fail     → FinalVerdictFail   → RF.
//     continue → FinalVerdictFailButContinued → siguiente step con el
//     ultimo output.
//     pause    → modal RP (decision humana en pause.go).
//
// Out of scope H7: editar el prompt mid-run (post-v1), atomic manifest
// writes (H8), timeout del validator (cae al killGrace existente cuando
// se cancela el run).
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// validatorPreamble es el texto que se prependea al output del step antes
// de pasarlo por stdin al validator. Le pedimos al modelo que termine su
// respuesta con un bloque YAML con clave `verdict` (ok|fail) + opcional
// `feedback`. El parser de parseVerdict es tolerante con basura antes y con
// formatos sin fences (verdict: ok / feedback: ... directo en stdout).
const validatorPreamble = `Sos un validador. A continuacion vas a recibir el output de un step de un pipeline.
Tu tarea: revisar el output y emitir un veredicto. Al final de tu respuesta, terminala con un bloque YAML asi:

verdict: ok      # o "fail"
feedback: |     # opcional, requerido si verdict=fail
  explica brevemente que mejorar.

--- OUTPUT DEL STEP ---
`

// Verdict es el shape parseado del verdict.yaml (o de cualquier bloque
// equivalente que aparezca en el stdout del validator). Status es "ok" o
// "fail" (lower-cased + trimeado por parseVerdict). Feedback es opcional.
//
// Cuando el parser no encuentra ningun bloque con clave verdict, devuelve
// un Verdict{Status: "fail", Feedback: "no verdict block"} para que el
// loop trate ese caso como un fail "tolerante" y haga retry (criterio
// del doc: "sin bloque → fail + feedback: 'no verdict block'").
type Verdict struct {
	Status   string `yaml:"verdict"`
	Feedback string `yaml:"feedback,omitempty"`
}

// VerdictStatus values.
const (
	VerdictOk   = "ok"
	VerdictFail = "fail"
)

// parseVerdict busca el ultimo bloque YAML con clave `verdict` en el stdout
// del validator. Estrategia tolerante (criterio del doc):
//
//  1. partir el stdout en bloques separados por lineas vacias o lineas con
//     `---` (separador YAML estandar).
//  2. de atras hacia adelante (queremos el ultimo), intentar
//     yaml.Unmarshal sobre cada bloque; el primero que parsee y tenga
//     verdict no-vacio gana.
//  3. fallback: pasar el stdout entero por yaml.Unmarshal — para casos
//     donde el modelo emitio solo `verdict: ok` sin nada mas.
//
// Si nada matchea: devolvemos Verdict{Status: VerdictFail, Feedback: "no
// verdict block"} (loop equivalente a un fail tolerante).
//
// Verdict.Status se normaliza a lower-case + trim y luego se mapea a las
// dos ramas que entiende el loop (ok|fail). Tokens reconocidos:
//   - "ok", "approve"                                  → VerdictOk
//   - "fail", "changes_requested", "needs_human", o
//     cualquier otro no-vacio (defensivo)              → VerdictFail
//
// Cuando un token 3-vias (changes_requested/needs_human) cae a fail sin
// feedback explicito, parseVerdict guarda el token original en Feedback
// para que el modal RP / merge-feedback muestre que tipo de fail fue.
func parseVerdict(stdout string) Verdict {
	// Si el validator es claude o codex, su stdout es NDJSON stream-json:
	// el verdict del agente viene anidado en el `text` de un evento, no
	// como YAML top-level. extractValidatorMessages devuelve solo el
	// texto del agente concatenado; si nada matchea, devuelve el stdout
	// tal cual (validators raw como gemini/opencode siguen funcionando).
	stdout = extractValidatorMessages(stdout)
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return Verdict{Status: VerdictFail, Feedback: "no verdict block"}
	}

	// Recolectar bloques candidatos. El doble newline o `---` separa
	// bloques YAML conceptuales — partimos por ambos. Tambien probamos
	// el stdout entero como ultimo intento.
	candidates := splitVerdictBlocks(stdout)
	candidates = append(candidates, stdout)

	// Recorrer de atras hacia adelante: queremos el ULTIMO bloque que
	// matchee. Asi, si el modelo emitio varios borradores antes del
	// veredicto final, gana el final.
	for i := len(candidates) - 1; i >= 0; i-- {
		body := strings.TrimSpace(candidates[i])
		if body == "" {
			continue
		}
		if v, ok := tryParseVerdictBlock(body); ok {
			return v
		}
	}

	return Verdict{Status: VerdictFail, Feedback: "no verdict block"}
}

// splitVerdictBlocks parte el stdout en bloques separados por lineas en
// blanco o por separadores yaml `---`. Mantiene el orden original — el
// caller los itera de atras hacia adelante para preferir el ultimo bloque.
func splitVerdictBlocks(stdout string) []string {
	// Normalizamos line endings + reemplazamos separadores `---` por una
	// linea en blanco (asi el split por doble newline alcanza para ambos).
	normalized := strings.ReplaceAll(stdout, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	var b strings.Builder
	for _, l := range lines {
		trim := strings.TrimSpace(l)
		if trim == "---" {
			b.WriteString("\n\n")
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	parts := strings.Split(b.String(), "\n\n")
	return parts
}

// tryParseVerdictBlock intenta deserializar el bloque como Verdict. Devuelve
// (verdict, true) si parseo OK y Status no esta vacio. Cualquier otro caso
// devuelve (_, false) — el caller sigue con el siguiente candidato.
//
// Status se normaliza a lower-case + trim y luego se canonicaliza via
// canonicalVerdict (ok/approve → ok; resto → fail).
func tryParseVerdictBlock(body string) (Verdict, bool) {
	var v Verdict
	if err := yaml.Unmarshal([]byte(body), &v); err != nil {
		return Verdict{}, false
	}
	raw := strings.ToLower(strings.TrimSpace(v.Status))
	if raw == "" {
		return Verdict{}, false
	}
	v.Status = canonicalVerdict(raw)
	v.Feedback = strings.TrimSpace(v.Feedback)
	// Si un token 3-vias (changes_requested/needs_human, o cualquier valor
	// raro) cae a fail y el validator no emitio feedback explicito,
	// guardamos el token original para que el merge-feedback / modal RP
	// muestre por que fallo y no quede vacio. raw=="fail" se omite porque
	// "verdict: fail" como feedback no agrega informacion.
	if v.Status == VerdictFail && v.Feedback == "" && raw != VerdictFail {
		v.Feedback = "verdict: " + raw
	}
	return v, true
}

// extractValidatorMessages destila el stdout de un validator stream-json
// (claude o codex) al texto del agente concatenado, descartando metadatos
// (turn.started, command_execution, usage…) y outputs de tools. Sin esto,
// `verdict: approve` queda enterrado dentro de `item.text` y yaml.Unmarshal
// lo lee como un objeto con key "type"/"item", no como un bloque verdict.
//
// Shapes reconocidos (cualquier linea que matchee aporta texto al output):
//
//	codex  : {"type":"item.completed","item":{"type":"agent_message","text":...}}
//	claude : {"type":"assistant","message":{"content":[{"type":"text","text":...}, ...]}}
//
// Si NINGUNA linea matchea (validator raw — gemini/opencode/text), devuelve
// el stdout intacto: el parser cae al path tradicional de YAML directo.
func extractValidatorMessages(stdout string) string {
	var sb strings.Builder
	found := false
	for _, raw := range strings.Split(stdout, "\n") {
		trim := strings.TrimSpace(raw)
		if trim == "" || !strings.HasPrefix(trim, "{") {
			continue
		}
		text := tryExtractAgentText(trim)
		if text == "" {
			continue
		}
		if found {
			sb.WriteString("\n\n")
		}
		sb.WriteString(text)
		found = true
	}
	if !found {
		return stdout
	}
	return sb.String()
}

// tryExtractAgentText devuelve el texto del agente de una sola linea JSON
// del stdout del validator. Soporta los dos shapes (claude / codex) y
// devuelve "" si la linea no es JSON o no calza con ningun shape conocido.
//
// Para claude `assistant.message.content[]`, concatena los bloques con
// type=text separados por newline. Para codex `item.agent_message.text`,
// devuelve el text plano. Otros tipos (tool_use, command_execution,
// reasoning, turn.completed, etc) se ignoran — son metadata, no veredicto.
func tryExtractAgentText(line string) string {
	var ev struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Item    struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
		return ev.Item.Text
	}
	if ev.Type == "assistant" && len(ev.Message) > 0 {
		var msg struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(ev.Message, &msg); err == nil {
			var sb strings.Builder
			first := true
			for _, c := range msg.Content {
				if c.Type != "text" || c.Text == "" {
					continue
				}
				if !first {
					sb.WriteString("\n")
				}
				sb.WriteString(c.Text)
				first = false
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
	}
	return ""
}

// canonicalVerdict normaliza el token YAML emitido por el validator a las
// dos ramas que entiende el loop. Acepta los tokens 3-vias usados por
// pipelines tipo che-funnel (approve/changes_requested/needs_human) y los
// tokens binarios del contrato base (ok/fail). Cualquier otro valor cae a
// fail (defensivo: un modelo confundido que emite "approved" o "KO" no
// debe avanzar el loop).
func canonicalVerdict(token string) string {
	switch token {
	case VerdictOk, "approve":
		return VerdictOk
	default:
		return VerdictFail
	}
}

// VerdictRecord es el shape persistido en
// step-NN.validator.0K.verdict.yaml. Es el mismo Verdict + un par de
// metadatos (loop K, raw stdout) para auditoria. El doc lo lista en
// "Layout en disco".
type VerdictRecord struct {
	StepIdx  int    `yaml:"step_idx"`
	Loop     int    `yaml:"loop"`
	Verdict  string `yaml:"verdict"`
	Feedback string `yaml:"feedback,omitempty"`
	// RawStdout es el stdout entero del validator (truncado a 4 KiB para
	// no inflar el archivo). Sirve para debuggear cuando el parser
	// devuelve "no verdict block" y el usuario quiere ver que emitio el
	// modelo.
	RawStdout string `yaml:"raw_stdout,omitempty"`
}

// validatorVerdictPath devuelve el path del verdict.yaml para el step idx
// (1-based) y el loop K (1-based). Sigue la convencion del doc:
// step-NN.validator.0K.verdict.yaml — NN y K zero-padded a 2 digitos.
func validatorVerdictPath(runDir string, stepIdx, loop int) string {
	return filepath.Join(runDir, fmt.Sprintf("step-%02d.validator.%02d.verdict.yaml", stepIdx, loop))
}

// validatorStdoutPath devuelve el path del stdout.log del validator para el
// step idx + loop K. Sigue la convencion del doc.
func validatorStdoutPath(runDir string, stepIdx, loop int) string {
	return filepath.Join(runDir, fmt.Sprintf("step-%02d.validator.%02d.stdout.log", stepIdx, loop))
}

// validatorStderrPath devuelve el path del stderr.log del validator.
func validatorStderrPath(runDir string, stepIdx, loop int) string {
	return filepath.Join(runDir, fmt.Sprintf("step-%02d.validator.%02d.stderr.log", stepIdx, loop))
}

// truncateForRecord limita el raw stdout que se persiste en verdict.yaml
// a 4 KiB para no inflar el manifest dir con dumps gigantes. El stdout
// completo igual queda en step-NN.validator.0K.stdout.log.
func truncateForRecord(s string) string {
	const max = 4 * 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncado)"
}

// validatorDoneMsg es el msg que la goroutine del validator subprocess
// emite al terminar. El handler de R3 lo consume para decidir la siguiente
// transicion del loop (siguiente step / re-run del step / RF / RP modal).
type validatorDoneMsg struct {
	StepIdx int // 1-based
	Loop    int // 1-based
	Verdict Verdict
	// SpawnErr captura un error fatal del subprocess (no se pudo
	// arrancar / wait roto). El doc lo trata como un fail "tolerante" en
	// el loop — equivale a verdict: fail con feedback="error: <msg>" para
	// que el retry haga sentido si el problema fue transitorio.
	SpawnErr string
	// RawStdout es el stdout completo del subprocess (se persiste en
	// verdict.yaml truncado, pero el msg lo lleva entero por si el
	// handler lo necesita para algo).
	RawStdout string
	StartedAt time.Time
	EndedAt   time.Time
}

// validatorSpawnCmdFn es la factoria swappable para el validator spawn.
// Tests unitarios la reemplazan; los e2e dejan el default que arma el
// exec.Cmd con el cli del validator. La firma es identica a spawnCmdFn
// (defaultSpawnCmd) — reusamos buildSpawnArgs para no duplicar la tabla
// de CLIs (claude/codex/gemini/opencode).
var validatorSpawnCmdFn = defaultValidatorSpawnCmd

// defaultValidatorSpawnCmd construye el exec.Cmd del validator: mismo
// payload-via-stdin que el step principal, mismos args por CLI. La unica
// diferencia practica con un step normal es que el "step" sintetico que
// le pasamos a buildSpawnArgs viene del bloque validator del step
// original (CLI/Kind/Content) — esto fuerza la misma logica de routing.
func defaultValidatorSpawnCmd(step wizard.Step, payload string) (*exec.Cmd, error) {
	return defaultSpawnCmd(step, payload)
}

// runValidator arranca el subprocess del validator del step y devuelve un
// tea.Cmd que se resuelve en validatorDoneMsg. En vez de streaming linea
// por linea (como runStep), corremos el validator en modo blocking via
// goroutine: el output completo lo tenemos al final, lo parseamos y
// emitimos un solo msg al canal del runState.
//
// Fail modes (todos terminan en validatorDoneMsg con un Verdict resuelto):
//   - cmd.Start error → SpawnErr poblado, Verdict fail con feedback "spawn error: ..."
//   - exit ≠ 0        → Verdict fail con feedback="validator exit %d" (ignoramos
//     stdout en ese caso — el shape es indeterminado)
//   - exit 0 + parser → parseVerdict sobre el stdout (tolerante)
//
// El stdout/stderr SIEMPRE se persiste (stdout.log + stderr.log) — el
// verdict.yaml lo escribe el handler en R3 al recibir el msg, no esta
// goroutine, para mantener writes secuenciales y atomicos en el thread
// del program.
func runValidator(step wizard.Step, payload string, runDir string, stepIdx, loop int, state *runState) tea.Cmd {
	startedAt := time.Now()
	state.lineCh = make(chan tea.Msg, 8)

	// El "step" sintetico para buildSpawnArgs: copiamos el bloque
	// validator del step original como si fuera un step independiente.
	validatorStep := wizard.Step{
		CLI:     step.Validator.CLI,
		Kind:    step.Validator.Kind,
		Content: step.Validator.Content,
	}

	full := validatorPreamble + payload
	cmd, buildErr := validatorSpawnCmdFn(validatorStep, full)
	if buildErr != nil {
		go func() {
			state.lineCh <- validatorDoneMsg{
				StepIdx: stepIdx,
				Loop:    loop,
				Verdict: Verdict{
					Status:   VerdictFail,
					Feedback: fmt.Sprintf("spawn error: %v", buildErr),
				},
				SpawnErr:  buildErr.Error(),
				StartedAt: startedAt,
				EndedAt:   time.Now(),
			}
		}()
		return waitForLine(state.lineCh)
	}

	state.mu.Lock()
	state.cmd = cmd
	state.mu.Unlock()

	go func() {
		// Capturamos stdout/stderr en buffers (modo blocking — el
		// validator no necesita streaming linea por linea: lo unico que
		// importa es el verdict final). Output no escala — los validators
		// son CLIs tipicas que devuelven 1-2 KB.
		var stdoutBuf, stderrBuf strings.Builder
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		setProcAttrs(cmd)

		runErr := cmd.Run()
		endedAt := time.Now()
		stdoutCopy := stdoutBuf.String()
		stderrCopy := stderrBuf.String()

		// Persistimos stdout/stderr a disco sincronicamente — el verdict
		// se escribe en handleValidatorDone (en el thread del program).
		_ = writeFileSync(validatorStdoutPath(runDir, stepIdx, loop), stdoutCopy)
		_ = writeFileSync(validatorStderrPath(runDir, stepIdx, loop), stderrCopy)

		exitCode := 0
		spawnErr := ""
		if runErr != nil {
			var ee *exec.ExitError
			if errors.As(runErr, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
				spawnErr = runErr.Error()
			}
		}

		var verdict Verdict
		switch {
		case spawnErr != "":
			verdict = Verdict{Status: VerdictFail, Feedback: fmt.Sprintf("spawn error: %s", spawnErr)}
		case exitCode != 0:
			verdict = Verdict{Status: VerdictFail, Feedback: fmt.Sprintf("validator exit %d", exitCode)}
		default:
			verdict = parseVerdict(stdoutCopy)
		}

		state.mu.Lock()
		state.done = true
		state.mu.Unlock()

		state.lineCh <- validatorDoneMsg{
			StepIdx:   stepIdx,
			Loop:      loop,
			Verdict:   verdict,
			SpawnErr:  spawnErr,
			RawStdout: stdoutCopy,
			StartedAt: startedAt,
			EndedAt:   endedAt,
		}
	}()

	return waitForLine(state.lineCh)
}

// writeFileSync escribe data en path con permisos 0600 (alineado con el
// resto de los archivos del run dir) y devuelve el primer error.
func writeFileSync(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o600)
}

// writeVerdict serializa + escribe step-NN.validator.0K.verdict.yaml. El
// shape sigue VerdictRecord (incluye el raw_stdout truncado para auditoria).
// Lo invoca handleValidatorDone en el thread del program — el spawn
// goroutine NO escribe verdict.yaml (mantenemos writes secuenciales para
// que el state on disk sea coherente con el state in-memory).
func writeVerdict(runDir string, rec VerdictRecord) error {
	data, err := yaml.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("verdict: marshal: %w", err)
	}
	path := validatorVerdictPath(runDir, rec.StepIdx, rec.Loop)
	return os.WriteFile(path, data, 0o600)
}
