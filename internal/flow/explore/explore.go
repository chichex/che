// Package explore implements flow 02 — tomar un issue ya creado por `che
// idea`, leerlo, profundizar con claude, y persistir el análisis (comentario +
// transición de label). La lógica vive acá (pura, testeable) para que el
// subcomando `che explore` y la TUI compartan la misma implementación.
package explore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh/git falla)
	ExitSemantic ExitCode = 3 // ref vacío, issue sin ct:plan, cerrado, ya explorado, agente inválido
)

// Agent identifica qué ejecutor usar para producir el análisis. Cada agente
// corresponde a un binario distinto en el PATH; el mapeo vive en Binary().
type Agent string

const (
	AgentOpus   Agent = "opus"
	AgentCodex  Agent = "codex"
	AgentGemini Agent = "gemini"
)

// DefaultAgent es el ejecutor que usa explore si el caller no elige uno.
const DefaultAgent = AgentOpus

// ValidAgents lista todos los agentes soportados (orden preservado para UI).
var ValidAgents = []Agent{AgentOpus, AgentCodex, AgentGemini}

// Binary devuelve el nombre del ejecutable que se invoca para este agente.
// Opus se mapea a `claude` porque el CLI oficial se llama así; Codex y Gemini
// usan su nombre directo.
func (a Agent) Binary() string {
	switch a {
	case AgentOpus:
		return "claude"
	case AgentCodex:
		return "codex"
	case AgentGemini:
		return "gemini"
	}
	return ""
}

// InvokeArgs devuelve los args de línea de comando para correr al agente en
// modo no-interactivo con el prompt dado. Cada CLI tiene su propia sintaxis:
//   - claude  -p <prompt> --output-format text
//   - codex   exec --full-auto <prompt>      (full-auto evita prompts de
//     confirmación de sandbox que colgarían el proceso sin TTY)
//   - gemini  -p <prompt>                    (text es el default)
func (a Agent) InvokeArgs(prompt string) []string {
	switch a {
	case AgentOpus:
		return []string{"-p", prompt, "--output-format", "text"}
	case AgentCodex:
		return []string{"exec", "--full-auto", prompt}
	case AgentGemini:
		return []string{"-p", prompt}
	}
	return nil
}

// AgentTimeout es el tiempo máximo que esperamos a que un agente responda
// antes de cancelarlo. Valor configurable vía env CHE_AGENT_TIMEOUT_SECS
// (útil para tests lentos o agentes pesados). Default 5 minutos: holgado
// para un call a claude/codex/gemini sin dejar flows colgados para siempre.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 5 * time.Minute
}()

// runAgentCmd invoca al binario del agente con el prompt ya construido.
// Streamea stdout y stderr en vivo al progress (así el usuario ve qué está
// haciendo), aplica AgentTimeout con context cancellation (si se cuelga,
// lo mata), y devuelve el stdout completo o un error con contexto.
//
// Todos los callers específicos (callAgent, callValidator, etc.) pasan por
// acá; lo único que varía es qué prompt construyen y cómo parsean el output.
func runAgentCmd(agent Agent, prompt string, progress func(string), progressPrefix string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), AgentTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agent.Binary(), agent.InvokeArgs(prompt)...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var fullStdout, fullStderr strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdoutPipe, &fullStdout, progress, progressPrefix)
	go streamPipe(&wg, stderrPipe, &fullStderr, progress, progressPrefix+"stderr: ")

	// IMPORTANTE: según docs de exec.Cmd.StdoutPipe, "it is thus incorrect
	// to call Wait before all reads from the pipe have completed". Primero
	// esperamos a que las goroutines drenen los pipes hasta EOF (llega
	// cuando el proceso termina); recién después cmd.Wait() para recoger el
	// exit code. Al revés se pierden bytes de stdout bajo carga y el JSON
	// llega truncado al parser.
	wg.Wait()
	waitErr := cmd.Wait()

	if ctx.Err() == context.DeadlineExceeded {
		return fullStdout.String(), fmt.Errorf("%s timed out after %s (stderr: %s)",
			agent, AgentTimeout, truncate(strings.TrimSpace(fullStderr.String()), 200))
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return fullStdout.String(), fmt.Errorf("exit %d: %s",
				ee.ExitCode(), strings.TrimSpace(fullStderr.String()))
		}
		return fullStdout.String(), waitErr
	}
	return fullStdout.String(), nil
}

// streamPipe lee línea por línea un pipe (stdout o stderr) y la reenvía a
// progress con el prefix dado. Acumula todo en el Builder para que el caller
// pueda usarlo después (parsing, error reporting).
func streamPipe(wg *sync.WaitGroup, r io.Reader, full *strings.Builder, progress func(string), prefix string) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		full.WriteString(line + "\n")
		if strings.TrimSpace(line) != "" && progress != nil {
			progress(prefix + line)
		}
	}
}

// ParseAgent normaliza un string a Agent, o error si no matchea ningún enum.
func ParseAgent(s string) (Agent, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, a := range ValidAgents {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown agent %q; valid: opus, codex, gemini", s)
}

// Validator identifica un validador del plan: agente + instancia (1..N) para
// diferenciar cuando el mismo agente aparece varias veces en la lista.
type Validator struct {
	Agent    Agent
	Instance int
}

// ParseValidators parsea una lista separada por coma ("codex,gemini",
// "codex,codex,gemini"). Acepta "none" (o vacío) para desactivar validación.
// Requiere 2-3 items cuando no es "none".
func ParseValidators(s string) ([]Validator, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "none") {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("validators: need 2-3 items (or `none`), got %d", len(parts))
	}
	counts := map[Agent]int{}
	out := make([]Validator, 0, len(parts))
	for _, p := range parts {
		a, err := ParseAgent(p)
		if err != nil {
			return nil, fmt.Errorf("validators: %w", err)
		}
		counts[a]++
		out = append(out, Validator{Agent: a, Instance: counts[a]})
	}
	return out, nil
}

// Opts agrupa los writers, la callback de progreso, el agente ejecutor y la
// lista de validadores. Si OnProgress es nil, el flow corre silencioso. Si
// Agent es "", se usa DefaultAgent. Si Validators es nil, no se corre la
// etapa de validación (comportamiento v0.0.11).
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string)
	Agent      Agent
	Validators []Validator
}

// Issue modela el subset del output de `gh issue view --json ...` que
// necesitamos para el flow. Los field names matchean las keys que devuelve gh.
type Issue struct {
	Number   int            `json:"number"`
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	URL      string         `json:"url"`
	State    string         `json:"state"`
	Labels   []Label        `json:"labels"`
	Comments []IssueComment `json:"comments,omitempty"`
}

// Label es el shape que gh devuelve para cada label del issue.
type Label struct {
	Name string `json:"name"`
}

// IssueComment es un comment del issue; el body puede tener header de
// claude-cli al principio (parseado en CommentHeader).
type IssueComment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
}

// CommentHeader es la metadata parseada del HTML comment que usamos como
// marcador al inicio de cada comment posteado por che. Si Role es "", no es
// un comment de che (es del humano o de otra herramienta).
type CommentHeader struct {
	Flow     string
	Iter     int
	Agent    Agent
	Instance int
	Role     string // "executor", "validator", "human-request"
}

// headerRe matchea el HTML comment de che al principio del body. Captura
// flow, iter, agent, instance, role como key=value dentro del comment.
var headerRe = regexp.MustCompile(`^<!--\s*claude-cli:\s*(.+?)\s*-->`)
var kvRe = regexp.MustCompile(`(\w+)=(\S+)`)

// ParseCommentHeader lee la primera línea del body y, si es un HTML comment
// de che, devuelve la metadata. Si no lo es, devuelve un CommentHeader vacío.
func ParseCommentHeader(body string) CommentHeader {
	m := headerRe.FindStringSubmatch(strings.TrimSpace(body))
	if m == nil {
		return CommentHeader{}
	}
	h := CommentHeader{}
	for _, kv := range kvRe.FindAllStringSubmatch(m[1], -1) {
		switch kv[1] {
		case "flow":
			h.Flow = kv[2]
		case "iter":
			fmt.Sscanf(kv[2], "%d", &h.Iter)
		case "agent":
			h.Agent = Agent(kv[2])
		case "instance":
			fmt.Sscanf(kv[2], "%d", &h.Instance)
		case "role":
			h.Role = kv[2]
		}
	}
	return h
}

// IsHuman devuelve true cuando el comment NO tiene header de che — asumimos
// entonces que es una respuesta del humano.
func (c *IssueComment) IsHuman() bool {
	return ParseCommentHeader(c.Body).Role == ""
}

// Header parseado del comment (helper para lectores).
func (c *IssueComment) Header() CommentHeader {
	return ParseCommentHeader(c.Body)
}

// HasLabel devuelve true si el issue tiene un label con ese nombre.
func (i *Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// LabelNames devuelve los nombres de labels como slice — útil para inyectar
// en el prompt.
func (i *Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// Response es lo que claude devuelve después de analizar el issue.
type Response struct {
	Summary   string     `json:"summary"`
	Questions []Question `json:"questions"`
	Risks     []Risk     `json:"risks"`
	Paths     []Path     `json:"paths"`
	NextStep  string     `json:"next_step"`
}

type Question struct {
	Q       string `json:"q"`
	Why     string `json:"why"`
	Blocker bool   `json:"blocker"`
}

type Risk struct {
	Risk       string `json:"risk"`
	Likelihood string `json:"likelihood"`
	Impact     string `json:"impact"`
	Mitigation string `json:"mitigation"`
}

type Path struct {
	Title       string   `json:"title"`
	Sketch      string   `json:"sketch"`
	Pros        []string `json:"pros"`
	Cons        []string `json:"cons"`
	Effort      string   `json:"effort"`
	Recommended bool     `json:"recommended"`
}

var (
	validLikelihood = map[string]bool{"low": true, "medium": true, "high": true}
	validImpact     = map[string]bool{"low": true, "medium": true, "high": true}
	validEffort     = map[string]bool{"XS": true, "S": true, "M": true, "L": true, "XL": true}
	validVerdicts   = map[string]bool{"approve": true, "changes_requested": true, "needs_human": true}
	validSeverities = map[string]bool{"blocker": true, "major": true, "minor": true}
)

// ValidatorResponse es el output estructurado que devuelve cada validador
// después de leer el plan del ejecutor.
type ValidatorResponse struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Finding es una observación concreta que un validador encontró sobre el plan.
// NeedsHuman indica que requiere decisión de producto del humano, no de otro
// agente — dispara el escape humano que pausa el flow.
type Finding struct {
	Severity   string `json:"severity"`
	Area       string `json:"area"`
	Where      string `json:"where"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion,omitempty"`
	NeedsHuman bool   `json:"needs_human"`
}

// validatorResult agrupa qué validador corrió, qué devolvió y si falló.
type validatorResult struct {
	Validator Validator
	Response  *ValidatorResponse
	Err       error
}

// hasHumanGaps revisa si alguno de los validators pidió input humano, ya sea
// vía verdict=needs_human o vía un finding con needs_human=true.
func hasHumanGaps(results []validatorResult) bool {
	for _, r := range results {
		if r.Response == nil {
			continue
		}
		if r.Response.Verdict == "needs_human" {
			return true
		}
		for _, f := range r.Response.Findings {
			if f.NeedsHuman {
				return true
			}
		}
	}
	return false
}

// MaxIterations es el tope de iteraciones del loop humano antes de cortar
// con error. 3 es el umbral del design — si después de 3 rondas los
// validadores siguen pidiendo input humano, la conversación requiere
// intervención directa del dueño, no más loops de agentes.
const MaxIterations = 3

// Run ejecuta el flow. Detecta automáticamente el modo según los labels del
// issue: status:awaiting-human dispara reanudación, el resto es exploración
// nueva. status:plan sin awaiting significa "ya explorado" y corta.
func Run(issueRef string, opts Opts) ExitCode {
	stdout, stderr := opts.Stdout, opts.Stderr
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		fmt.Fprintln(stderr, "error: issue ref is empty")
		return ExitSemantic
	}

	progress("chequeando repo git y auth de GitHub…")
	if err := precheckGitHubRemote(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("obteniendo issue desde GitHub…")
	issue, err := fetchIssue(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetching issue: %v\n", err)
		return ExitRetry
	}

	if err := gateBasic(issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitSemantic
	}

	// Ramificación por modo. awaiting-human → resume; status:plan → error
	// de "ya explorado"; default → new.
	if issue.HasLabel("status:awaiting-human") {
		return runResume(issueRef, issue, opts, progress, stdout, stderr)
	}
	if issue.HasLabel("status:plan") {
		fmt.Fprintf(stderr, "error: issue #%d was already explored (status:plan present)\n", issue.Number)
		return ExitSemantic
	}
	return runNew(issueRef, issue, opts, progress, stdout, stderr)
}

// runNew es la exploración desde cero: ejecutor arma el plan, validators
// iter=1 revisan, y se transiciona a status:plan o status:awaiting-human
// según haya o no preguntas humanas.
func runNew(issueRef string, issue *Issue, opts Opts, progress func(string), stdout, stderr io.Writer) ExitCode {
	agent := opts.Agent
	if agent == "" {
		agent = DefaultAgent
	}
	if agent.Binary() == "" {
		fmt.Fprintf(stderr, "error: unknown agent %q\n", agent)
		return ExitSemantic
	}

	progress("consultando a " + string(agent) + "…")
	resp, err := callAgent(agent, issue, progress)
	if err != nil {
		fmt.Fprintf(stderr, "error: calling %s: %v\n", agent, err)
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		fmt.Fprintf(stderr, "error: %s response: %v\n", agent, err)
		return ExitSemantic
	}

	progress("posteando comentario con el análisis…")
	comment := renderComment(resp, agent, 1)
	commentURL, err := postComment(issueRef, comment)
	if err != nil {
		fmt.Fprintf(stderr, "error: posting comment: %v\n", err)
		return ExitRetry
	}

	var validationResults []validatorResult
	if len(opts.Validators) > 0 {
		progress(fmt.Sprintf("corriendo %d validador(es) en paralelo…", len(opts.Validators)))
		validationResults = runValidatorsParallel(issue, resp, opts.Validators, progress)
		if err := validateValidatorResults(validationResults); err != nil {
			fmt.Fprintf(stderr, "error: validator response: %v\n", err)
			return ExitSemantic
		}
		progress("posteando comments de validadores…")
		if err := postValidatorComments(issueRef, 1, validationResults); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
	}

	if hasHumanGaps(validationResults) {
		return pauseForHuman(issueRef, issue, resp, validationResults, 1, progress, stdout, stderr)
	}

	return finalizeToPlan(issueRef, issue, resp, validationResults, agent, opts.Validators, commentURL, progress, stdout, stderr)
}

// runResume reanuda un flow que quedó en status:awaiting-human. Lee los
// comments, extrae las respuestas humanas y re-corre los validadores con
// iteración N+1 incorporando esas respuestas. Si siguen pidiendo humano,
// pausa otra vez; si convergen, consolida el body y cierra.
func runResume(issueRef string, issue *Issue, opts Opts, progress func(string), stdout, stderr io.Writer) ExitCode {
	state := parseConversation(issue)

	if state.ExecutorPlan == nil {
		fmt.Fprintf(stderr, "error: issue #%d tiene status:awaiting-human pero no se encontró el plan del ejecutor en los comments\n", issue.Number)
		return ExitSemantic
	}
	if len(state.HumanAnswers) == 0 {
		fmt.Fprintf(stderr, "error: no hay respuestas humanas posteriores al último human-request en #%d — contestá en el issue antes de re-correr\n", issue.Number)
		return ExitSemantic
	}
	if state.MaxIter >= MaxIterations {
		fmt.Fprintf(stderr, "error: issue #%d excedió %d iteraciones sin converger — resolvé a mano (conversación con validadores en los comments)\n", issue.Number, MaxIterations)
		return ExitRetry
	}

	nextIter := state.MaxIter + 1
	progress(fmt.Sprintf("reanudando iter=%d (executor=%s, validators=%d)…", nextIter, state.ExecutorAgent, len(state.Validators)))

	// Validators para esta iteración: los mismos que en iter=1 si el user no
	// pasó un override; si pasó, los del override. Priorizamos preservar la
	// continuidad del panel de revisión.
	validators := opts.Validators
	if validators == nil {
		validators = state.Validators
	}
	if len(validators) == 0 {
		fmt.Fprintf(stderr, "error: no hay validadores configurados para reanudar — pasá --validators o escriba el flow original con validators\n")
		return ExitSemantic
	}

	progress(fmt.Sprintf("corriendo %d validador(es) con las respuestas humanas…", len(validators)))
	results := runValidatorsResumeParallel(issue, state, validators, progress)
	if err := validateValidatorResults(results); err != nil {
		fmt.Fprintf(stderr, "error: validator response: %v\n", err)
		return ExitSemantic
	}

	progress("posteando comments de validadores…")
	if err := postValidatorComments(issueRef, nextIter, results); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	if hasHumanGaps(results) {
		if nextIter >= MaxIterations {
			fmt.Fprintf(stderr, "error: después de %d iteraciones siguen quedando preguntas sin resolver; resolvé a mano en el issue #%d\n", nextIter, issue.Number)
			fmt.Fprintln(stdout, renderValidationReport(results, true))
			return ExitRetry
		}
		return pauseForHuman(issueRef, issue, state.ExecutorPlan, results, nextIter, progress, stdout, stderr)
	}

	// Convergencia: llamamos al executor en modo consolidación para armar el
	// body final sin ambigüedades.
	progress(fmt.Sprintf("convergencia alcanzada; consolidando plan con %s…", state.ExecutorAgent))
	consolidated, err := callConsolidation(state.ExecutorAgent, issue, state, results, progress)
	if err != nil {
		fmt.Fprintf(stderr, "error: consolidation: %v\n", err)
		return ExitRetry
	}
	if err := validateConsolidated(consolidated); err != nil {
		fmt.Fprintf(stderr, "error: consolidated response: %v\n", err)
		return ExitSemantic
	}

	newBody := renderConsolidatedBody(consolidated, issue.Body)
	progress("actualizando body del issue con plan consolidado…")
	if err := editIssueBody(issueRef, newBody); err != nil {
		fmt.Fprintf(stderr, "error: updating body: %v\n", err)
		return ExitRetry
	}

	progress("asegurando label status:plan…")
	if err := ensureLabel("status:plan", progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	progress("quitando status:awaiting-human, agregando status:plan…")
	if err := closeAwaitingHuman(issueRef); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	fmt.Fprintln(stdout, renderValidationReport(results, false))
	fmt.Fprintf(stdout, "Resumed and consolidated %s\n", issue.URL)
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

// pauseForHuman centraliza lo que hacen new y resume cuando detectan human
// gaps: postean human-request, aseguran status:awaiting-human y salen.
func pauseForHuman(issueRef string, issue *Issue, plan *Response, results []validatorResult, iter int, progress func(string), stdout, stderr io.Writer) ExitCode {
	progress("validadores pidieron input humano; posteando request…")
	humanReq := renderHumanRequest(issue.Number, plan, results, iter)
	if _, err := postComment(issueRef, humanReq); err != nil {
		fmt.Fprintf(stderr, "error: posting human-request comment: %v\n", err)
		return ExitRetry
	}
	progress("asegurando label status:awaiting-human…")
	if err := ensureLabel("status:awaiting-human", progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := setLabelAwaitingHuman(issueRef); err != nil {
		fmt.Fprintf(stderr, "error: editing labels: %v\n", err)
		return ExitRetry
	}
	fmt.Fprintln(stdout, renderValidationReport(results, true))
	fmt.Fprintf(stdout, "Paused %s — contestá en el issue y corré `che explore %d` de nuevo; el flow va a detectar tu respuesta y continuar.\n", issue.URL, issue.Number)
	return ExitOK
}

// finalizeToPlan es el cierre normal del modo new cuando no hay human gaps:
// transiciona status:idea → status:plan y devuelve ExitOK.
func finalizeToPlan(issueRef string, issue *Issue, _ *Response, results []validatorResult, _ Agent, _ []Validator, commentURL string, progress func(string), stdout, stderr io.Writer) ExitCode {
	progress("asegurando label status:plan…")
	if err := ensureLabel("status:plan", progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		fmt.Fprintf(stderr, "warning: comentario posteado (%s) pero label no se pudo crear/actualizar; corré de nuevo\n", commentURL)
		return ExitRetry
	}
	progress("transicionando label a status:plan…")
	if err := transitionLabels(issueRef); err != nil {
		fmt.Fprintf(stderr, "error: editing labels: %v\n", err)
		fmt.Fprintf(stderr, "warning: comentario posteado (%s) pero label no cambió; corré de nuevo o editá a mano\n", commentURL)
		return ExitRetry
	}
	if len(results) > 0 {
		fmt.Fprintln(stdout, renderValidationReport(results, false))
	}
	fmt.Fprintf(stdout, "Explored %s\n", issue.URL)
	if commentURL != "" {
		fmt.Fprintf(stdout, "Comment: %s\n", commentURL)
	}
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

func precheckGitHubRemote() error {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return fmt.Errorf("github remote: no origin configured")
	}
	url := strings.TrimSpace(string(out))
	if !strings.Contains(url, "github.com") {
		return fmt.Errorf("github remote: origin is not github.com: %s", url)
	}
	return nil
}

func precheckGhAuth() error {
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh auth: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Candidate es un issue candidato a explorar: tiene ct:plan, está abierto,
// y todavía no fue explorado (sin status:planned). Es el subset de Issue que
// la TUI necesita para mostrar la lista de selección.
type Candidate struct {
	Number int
	Title  string
}

// ListCandidates devuelve los issues abiertos con label ct:plan que todavía
// no fueron explorados (sin status:plan) y que NO están esperando input
// humano (sin status:awaiting-human). Limita a los 50 más recientes para
// mantener la TUI manejable. Los awaiting-human se listan aparte con
// ListAwaiting() — el usuario los reanuda desde otra sección de la TUI.
func ListCandidates() ([]Candidate, error) {
	raw, err := listIssuesWithCtPlan()
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(raw))
	for _, i := range raw {
		if i.HasLabel("status:plan") || i.HasLabel("status:awaiting-human") {
			continue
		}
		out = append(out, Candidate{Number: i.Number, Title: i.Title})
	}
	return out, nil
}

// ListAwaiting devuelve los issues con status:awaiting-human, candidatos a
// reanudación. Son los que quedaron en pausa porque los validadores pidieron
// input humano en una corrida anterior.
func ListAwaiting() ([]Candidate, error) {
	raw, err := listIssuesWithCtPlan()
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(raw))
	for _, i := range raw {
		if !i.HasLabel("status:awaiting-human") {
			continue
		}
		out = append(out, Candidate{Number: i.Number, Title: i.Title})
	}
	return out, nil
}

// InspectResume fetchea un issue en status:awaiting-human y devuelve qué
// agente ejecutor usó en la corrida anterior y qué validators participaron.
// La TUI lo llama cuando el usuario elige reanudar, para pre-seleccionar
// el mismo panel en las pantallas de agent/validators (el humano puede
// aceptarlo o cambiarlo — no imponemos nada).
func InspectResume(ref string) (Agent, []Validator, error) {
	if err := precheckGitHubRemote(); err != nil {
		return "", nil, err
	}
	if err := precheckGhAuth(); err != nil {
		return "", nil, err
	}
	issue, err := fetchIssue(ref)
	if err != nil {
		return "", nil, err
	}
	state := parseConversation(issue)
	return state.ExecutorAgent, state.Validators, nil
}

// listIssuesWithCtPlan es el fetch compartido: trae todos los issues open con
// ct:plan y deja el filtrado a los callers específicos.
func listIssuesWithCtPlan() ([]Issue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--label", "ct:plan",
		"--state", "open",
		"--json", "number,title,labels",
		"--limit", "50")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var raw []Issue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	return raw, nil
}

// fetchIssue corre `gh issue view <ref> --json ...` y parsea el output. El
// ref puede ser número, URL, o owner/repo#N — gh los normaliza. Incluye
// comments porque el modo resume los necesita para encontrar las respuestas
// humanas y las iteraciones previas.
func fetchIssue(ref string) (*Issue, error) {
	cmd := exec.Command("gh", "issue", "view", ref,
		"--json", "number,title,body,labels,url,state,comments")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parse gh issue view output: %w", err)
	}
	return &issue, nil
}

// gateBasic valida las precondiciones mínimas (open + ct:plan). La decisión
// entre modo new/resume/already-explored se toma después en Run() mirando
// los labels de estado.
func gateBasic(i *Issue) error {
	if i.State != "OPEN" {
		return fmt.Errorf("issue #%d is closed", i.Number)
	}
	if !i.HasLabel("ct:plan") {
		return fmt.Errorf("issue #%d is missing label ct:plan (not created by `che idea`?)", i.Number)
	}
	return nil
}

// callAgent invoca al binario correspondiente al agente elegido con el prompt
// construido para el issue. Usa InvokeArgs para adaptarse a la sintaxis
// específica de cada CLI (opus/codex/gemini usan flags distintos).
func callAgent(agent Agent, issue *Issue, progress func(string)) (*Response, error) {
	prompt := buildPrompt(issue)
	out, err := runAgentCmd(agent, prompt, progress, string(agent)+": ")
	if err != nil {
		return nil, err
	}
	return parseResponse(out)
}

// parseResponse extrae el JSON del output de claude, tolerando code fences
// y texto circundante (mismo algoritmo que `idea.parseResponse`).
func parseResponse(raw string) (*Response, error) {
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "```") {
		if nl := strings.Index(raw, "\n"); nl >= 0 {
			raw = raw[nl+1:]
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if i := strings.LastIndex(raw, "}"); i >= 0 && i < len(raw)-1 {
		raw = raw[:i+1]
	}

	var r Response
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("invalid JSON from claude: %w (raw: %q)", err, truncate(raw, 200))
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func buildPrompt(issue *Issue) string {
	return `Sos un ingeniero senior haciendo exploración técnica de un issue antes de comprometerse a un plan de ejecución.

Te voy a pasar un issue de GitHub ya clasificado (type, size, criterios iniciales). Tu tarea NO es implementar ni planear al detalle — es abrir el espacio de diseño:
1. Parafrasear el issue para confirmar entendimiento.
2. Listar las preguntas abiertas que hay que responder antes de ejecutar.
3. Identificar riesgos con likelihood e impact.
4. Proponer 2-4 caminos de implementación distintos con pros, cons y effort estimado.
5. Marcar EXACTAMENTE UN camino como recomendado.
6. Indicar el próximo paso concreto.

Valores válidos:
- likelihood/impact: low, medium, high
- effort: XS (minutos), S (horas), M (1-2 días), L (1 semana), XL (varias semanas)

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "summary": "Paráfrasis accionable del issue en 1-2 oraciones",
  "questions": [
    {"q": "Pregunta abierta concreta", "why": "Por qué importa para el diseño", "blocker": true}
  ],
  "risks": [
    {"risk": "Descripción del riesgo", "likelihood": "medium", "impact": "high", "mitigation": "Cómo evitarlo"}
  ],
  "paths": [
    {"title": "Nombre corto del camino", "sketch": "2-4 oraciones de qué implica técnicamente", "pros": ["Pro 1"], "cons": ["Con 1"], "effort": "M", "recommended": true}
  ],
  "next_step": "Frase accionable de qué tiene que pasar antes de che execute"
}

Reglas:
- questions[] tiene al menos 1 item. Si no se te ocurre ninguna, el análisis es superficial — forzate a pensar.
- risks[] tiene al menos 1 item.
- paths[] tiene entre 2 y 4 items. Un solo camino = no estás explorando, solo planeando.
- EXACTAMENTE UN path con "recommended": true. Los otros con false.
- Cada path debe tener al menos 1 pro y 1 con.
- No inventes archivos o módulos que no aparecen en el issue.

Issue #` + fmt.Sprint(issue.Number) + `:
Título: ` + issue.Title + `
Labels: ` + strings.Join(issue.LabelNames(), ", ") + `

Body del issue:
<<<
` + issue.Body + `
>>>`
}

func validate(r *Response) error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summary is empty")
	}
	if len(r.Questions) == 0 {
		return fmt.Errorf("questions[] is empty")
	}
	if len(r.Risks) == 0 {
		return fmt.Errorf("risks[] is empty")
	}
	if len(r.Paths) < 2 || len(r.Paths) > 4 {
		return fmt.Errorf("paths[] must have 2-4 items, got %d", len(r.Paths))
	}
	for i, risk := range r.Risks {
		if !validLikelihood[risk.Likelihood] {
			return fmt.Errorf("risk %d: likelihood %q not in [low medium high]", i, risk.Likelihood)
		}
		if !validImpact[risk.Impact] {
			return fmt.Errorf("risk %d: impact %q not in [low medium high]", i, risk.Impact)
		}
	}
	recommended := 0
	for i, p := range r.Paths {
		if strings.TrimSpace(p.Title) == "" {
			return fmt.Errorf("path %d: title is empty", i)
		}
		if !validEffort[strings.ToUpper(p.Effort)] {
			return fmt.Errorf("path %d: effort %q not in [XS S M L XL]", i, p.Effort)
		}
		if p.Recommended {
			recommended++
		}
	}
	if recommended != 1 {
		return fmt.Errorf("paths[]: exactly one path must be recommended, got %d", recommended)
	}
	if strings.TrimSpace(r.NextStep) == "" {
		return fmt.Errorf("next_step is empty")
	}
	return nil
}

// renderComment genera el markdown que se postea como comentario en el issue.
// Arranca con un header HTML que identifica al ejecutor y la iteración, y
// termina con el Response completo en un bloque ```json colapsado — esa
// representación es la fuente de verdad que modo resume re-parsea para
// continuar la conversación sin perder estructura.
func renderComment(r *Response, agent Agent, iter int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=explore iter=%d agent=%s role=executor -->\n", iter, agent))
	sb.WriteString(fmt.Sprintf("## [executor:%s · iter:%d]\n\n", agent, iter))

	sb.WriteString("**Resumen:** ")
	sb.WriteString(r.Summary)
	sb.WriteString("\n\n")

	sb.WriteString("### Preguntas abiertas\n")
	for _, q := range r.Questions {
		marker := "-"
		if q.Blocker {
			marker = "- 🚧"
		}
		sb.WriteString(fmt.Sprintf("%s **%s** — %s\n", marker, q.Q, q.Why))
	}
	sb.WriteString("\n")

	sb.WriteString("### Riesgos\n")
	for _, risk := range r.Risks {
		sb.WriteString(fmt.Sprintf("- **%s** (likelihood=%s, impact=%s) — %s\n",
			risk.Risk, risk.Likelihood, risk.Impact, risk.Mitigation))
	}
	sb.WriteString("\n")

	sb.WriteString("### Caminos posibles\n")
	for _, p := range r.Paths {
		marker := ""
		if p.Recommended {
			marker = " ⭐ _recomendado_"
		}
		sb.WriteString(fmt.Sprintf("\n**%s** (effort=%s)%s\n", p.Title, strings.ToUpper(p.Effort), marker))
		sb.WriteString(p.Sketch + "\n")
		if len(p.Pros) > 0 {
			sb.WriteString("- Pros:\n")
			for _, pro := range p.Pros {
				sb.WriteString("  - " + pro + "\n")
			}
		}
		if len(p.Cons) > 0 {
			sb.WriteString("- Cons:\n")
			for _, con := range p.Cons {
				sb.WriteString("  - " + con + "\n")
			}
		}
	}
	sb.WriteString("\n")

	sb.WriteString("### Próximo paso\n")
	sb.WriteString(r.NextStep + "\n")

	appendEmbeddedJSON(&sb, r, "Plan en JSON")
	return sb.String()
}

// appendEmbeddedJSON escribe al final del comment un bloque <details> con el
// JSON completo de la estructura. Esa es la fuente de verdad para el modo
// resume, que re-parsea sin depender del markdown (que puede perder nesting).
func appendEmbeddedJSON(sb *strings.Builder, v any, title string) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	sb.WriteString("\n---\n\n")
	sb.WriteString(fmt.Sprintf("<details>\n<summary>%s (para re-procesar)</summary>\n\n```json\n", title))
	sb.Write(data)
	sb.WriteString("\n```\n\n</details>\n")
}

// extractEmbeddedJSON busca el primer bloque ```json ... ``` en el body del
// comment y devuelve el contenido. Si no encuentra, devuelve "".
var embeddedJSONRe = regexp.MustCompile("```json\\s*\\n([\\s\\S]*?)\\n```")

func extractEmbeddedJSON(body string) string {
	m := embeddedJSONRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// postComment corre `gh issue comment <ref> --body-file <tmp>` y devuelve la
// URL del comentario creado (primera línea de stdout) si gh la devuelve.
func postComment(ref, body string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "che-explore-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "comment.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return "", err
	}

	cmd := exec.Command("gh", "issue", "comment", ref, "--body-file", bodyFile)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh issue comment: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ensureLabel garantiza que un label exista en el repo antes de aplicarlo.
// Usa `gh label create --force` que es idempotente (crea si no existe,
// actualiza si existe — nunca falla por duplicado).
func ensureLabel(name string, progress func(string)) error {
	progress("asegurando label " + name)
	cmd := exec.Command("gh", "label", "create", name, "--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ensuring label %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// transitionLabels saca `status:idea` y agrega `status:plan`. NO toca
// `ct:plan` (queda como marcador de "fue creado por che idea") ni aplica
// `ct:exec` (eso lo hace `che execute` al arrancar).
func transitionLabels(ref string) error {
	cmd := exec.Command("gh", "issue", "edit", ref,
		"--remove-label", "status:idea",
		"--add-label", "status:plan")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// setLabelAwaitingHuman agrega `status:awaiting-human` manteniendo
// `status:idea` (el issue no transicionó de estado porque el flow quedó en
// pausa esperando respuesta del humano). No toca otros labels.
func setLabelAwaitingHuman(ref string) error {
	cmd := exec.Command("gh", "issue", "edit", ref,
		"--add-label", "status:awaiting-human")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// runValidatorsParallel corre todos los validators en goroutines, cada uno
// con su callAgent propio, y devuelve un slice alineado con el índice de los
// inputs. No cancela los otros si uno falla — los errores se reportan en el
// validatorResult individual.
func runValidatorsParallel(issue *Issue, execResp *Response, validators []Validator, progress func(string)) []validatorResult {
	results := make([]validatorResult, len(validators))
	var wg sync.WaitGroup
	for i, v := range validators {
		wg.Add(1)
		go func(i int, v Validator) {
			defer wg.Done()
			label := fmt.Sprintf("%s#%d", v.Agent, v.Instance)
			progress(label + ": consultando…")
			resp, err := callValidator(v, issue, execResp, func(line string) {
				progress(label + ": " + line)
			})
			results[i] = validatorResult{Validator: v, Response: resp, Err: err}
		}(i, v)
	}
	wg.Wait()
	return results
}

// callValidator invoca al binario del agente validador con un prompt que
// incluye el plan del ejecutor. Devuelve la respuesta parseada o el error.
func callValidator(v Validator, issue *Issue, execResp *Response, progress func(string)) (*ValidatorResponse, error) {
	prompt := buildValidatorPrompt(issue, execResp)
	out, err := runAgentCmd(v.Agent, prompt, progress, "")
	if err != nil {
		return nil, err
	}
	return parseValidatorResponse(out)
}

func parseValidatorResponse(raw string) (*ValidatorResponse, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if nl := strings.Index(raw, "\n"); nl >= 0 {
			raw = raw[nl+1:]
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if i := strings.LastIndex(raw, "}"); i >= 0 && i < len(raw)-1 {
		raw = raw[:i+1]
	}
	var r ValidatorResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("invalid JSON from validator: %w (raw: %q)", err, truncate(raw, 200))
	}
	return &r, nil
}

// validateValidatorResults chequea que cada response cumpla el schema mínimo
// (verdict en enum, severities en enum, issue no vacío). Si alguno es
// inválido devuelve error; otros errores de red/crash (Err != nil) se
// reportan con contexto pero no cortan el flow acá — los posteamos como
// "error: ..." en el comment y el usuario decide.
func validateValidatorResults(results []validatorResult) error {
	for _, r := range results {
		if r.Err != nil || r.Response == nil {
			continue // error en ejecución, no de schema
		}
		if !validVerdicts[r.Response.Verdict] {
			return fmt.Errorf("%s#%d: verdict %q not in [approve changes_requested needs_human]",
				r.Validator.Agent, r.Validator.Instance, r.Response.Verdict)
		}
		if strings.TrimSpace(r.Response.Summary) == "" {
			return fmt.Errorf("%s#%d: summary is empty", r.Validator.Agent, r.Validator.Instance)
		}
		for i, f := range r.Response.Findings {
			if !validSeverities[f.Severity] {
				return fmt.Errorf("%s#%d: finding %d severity %q not in [blocker major minor]",
					r.Validator.Agent, r.Validator.Instance, i, f.Severity)
			}
			if strings.TrimSpace(f.Issue) == "" {
				return fmt.Errorf("%s#%d: finding %d issue is empty",
					r.Validator.Agent, r.Validator.Instance, i)
			}
		}
	}
	return nil
}

// postValidatorComments postea un comment por validator en el issue. Si un
// validator falló (Err != nil), se postea un comment con el error en vez
// del análisis — así queda rastro en el issue.
func postValidatorComments(ref string, iter int, results []validatorResult) error {
	for _, r := range results {
		body := renderValidatorComment(r, iter)
		if _, err := postComment(ref, body); err != nil {
			return fmt.Errorf("posting %s#%d comment: %w", r.Validator.Agent, r.Validator.Instance, err)
		}
	}
	return nil
}

// buildValidatorPrompt arma el prompt para el validador. Le damos el issue
// original + el JSON del plan del ejecutor, y le pedimos que verifique.
func buildValidatorPrompt(issue *Issue, execResp *Response) string {
	planJSON, _ := json.MarshalIndent(execResp, "", "  ")
	return `Sos un validador técnico senior. Otro agente exploró un issue y produjo un plan. Tu tarea es leerlo con criterio y marcar lo que falta o está mal — NO armar un plan alternativo ni implementar nada.

Chequeá específicamente:
1. ¿Faltan riesgos relevantes? (scope creep, acoplamiento, rollback, UX, testing)
2. ¿Faltan preguntas abiertas importantes? (decisiones no tomadas)
3. ¿Los paths son arquitectónicamente distintos o son variantes del mismo tema?
4. ¿Los pros/cons de cada path son realistas? ¿el recommended tiene justificación?
5. ¿Algún punto requiere decisión de PRODUCTO del humano (no técnica)? Marcalo con needs_human=true.

Valores válidos:
- verdict: "approve" (plan suficiente), "changes_requested" (hay que corregir cosas técnicas), "needs_human" (hay preguntas de producto que ni vos ni el ejecutor pueden contestar)
- severity: "blocker", "major", "minor"
- area: "questions", "risks", "paths", "summary", "other"

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "verdict": "changes_requested",
  "summary": "Tu opinión global en 1-2 oraciones",
  "findings": [
    {
      "severity": "major",
      "area": "risks",
      "where": "risks[]",
      "issue": "Descripción concreta del gap",
      "suggestion": "Cómo arreglarlo (opcional)",
      "needs_human": false
    }
  ]
}

Reglas:
- Si el plan está bien, verdict=approve y findings=[].
- needs_human=true SOLO cuando la respuesta depende de una decisión del dueño del producto (ej: "¿idempotente o no?", "¿timeout o esperar para siempre?"). Cosas técnicas (falta manejo de error, el path no compila) van con needs_human=false.
- Un finding con needs_human=true debería escalar el verdict global a "needs_human" si es blocker.
- No inventes gaps — si el plan cubre un riesgo aunque sea brevemente, no lo marques como faltante.

Issue #` + fmt.Sprint(issue.Number) + `:
Título: ` + issue.Title + `
Labels: ` + strings.Join(issue.LabelNames(), ", ") + `

Body del issue:
<<<
` + issue.Body + `
>>>

Plan del ejecutor:
<<<
` + string(planJSON) + `
>>>`
}

// renderValidatorComment genera el markdown del comment de un validator,
// con header HTML estructurado para iteraciones futuras.
func renderValidatorComment(r validatorResult, iter int) string {
	v := r.Validator
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=explore iter=%d agent=%s instance=%d role=validator -->\n",
		iter, v.Agent, v.Instance))

	if r.Err != nil || r.Response == nil {
		sb.WriteString(fmt.Sprintf("## [validator:%s#%d · iter:%d · ERROR]\n\n", v.Agent, v.Instance, iter))
		if r.Err != nil {
			sb.WriteString("El validador falló antes de producir un análisis:\n\n```\n")
			sb.WriteString(r.Err.Error())
			sb.WriteString("\n```\n")
		}
		return sb.String()
	}

	resp := r.Response
	sb.WriteString(fmt.Sprintf("## [validator:%s#%d · iter:%d · %s]\n\n", v.Agent, v.Instance, iter, resp.Verdict))
	sb.WriteString("**Resumen:** " + resp.Summary + "\n\n")

	if len(resp.Findings) == 0 {
		sb.WriteString("_Sin findings._\n")
		return sb.String()
	}

	sb.WriteString("### Findings\n")
	for _, f := range resp.Findings {
		marker := "-"
		if f.NeedsHuman {
			marker = "- 🧑"
		}
		sb.WriteString(fmt.Sprintf("%s **[%s · %s]** %s", marker, f.Severity, f.Area, f.Issue))
		if f.Where != "" {
			sb.WriteString(" _(en: " + f.Where + ")_")
		}
		sb.WriteString("\n")
		if f.Suggestion != "" {
			sb.WriteString("  - sugerencia: " + f.Suggestion + "\n")
		}
	}
	appendEmbeddedJSON(&sb, resp, "Validación en JSON")
	return sb.String()
}

// renderHumanRequest genera el comment que se postea cuando el flow queda en
// pausa esperando input humano. Formato pensado para que el humano pueda
// leer la lista numerada, contestar directo (ej: "1: X. 2: Y.") y que el
// modelo en iter siguiente mapee respuestas con preguntas sin ambigüedad.
//
// Prioriza preguntas del plan marcadas como blocker=true (esas son las que
// el ejecutor identificó como bloqueantes); complementa con findings
// needs_human=true de los validadores que no dupliquen preguntas del plan.
func renderHumanRequest(issueNumber int, plan *Response, results []validatorResult, iter int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=explore iter=%d role=human-request -->\n", iter))
	sb.WriteString("## 🧑 Necesito tu input para seguir\n\n")

	planQs := collectPlanBlockers(plan)
	extraQs := collectValidatorQuestions(results, planQs)

	if len(planQs) == 0 && len(extraQs) == 0 {
		sb.WriteString("_No se identificaron preguntas específicas; revisá los comments de validadores arriba y contestá lo que corresponda._\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("Quedaron %d pregunta(s) abiertas que necesito que resuelvas antes de cerrar el plan. Están numeradas para que puedas contestarlas directo.\n\n", len(planQs)+len(extraQs)))

	n := 1
	if len(planQs) > 0 {
		sb.WriteString("### Preguntas del plan\n\n")
		sb.WriteString("_Las que el ejecutor marcó como bloqueantes para el diseño._\n\n")
		for _, q := range planQs {
			sb.WriteString(fmt.Sprintf("**%d. %s**\n\n", n, q.Q))
			if strings.TrimSpace(q.Why) != "" {
				sb.WriteString("> " + q.Why + "\n\n")
			}
			n++
		}
	}

	if len(extraQs) > 0 {
		sb.WriteString("### Observaciones adicionales de los validadores\n\n")
		sb.WriteString("_Cosas que aparecieron en la validación y también requieren decisión tuya._\n\n")
		for _, e := range extraQs {
			sb.WriteString(fmt.Sprintf("**%d. %s** _(vía %s)_\n\n", n, e.text, e.source))
			if strings.TrimSpace(e.context) != "" {
				sb.WriteString("> " + e.context + "\n\n")
			}
			n++
		}
	}

	sb.WriteString("---\n\n")
	sb.WriteString("### Cómo contestar\n\n")
	sb.WriteString("Dejá un comment nuevo en este issue en cualquiera de estos formatos:\n\n")
	sb.WriteString("- **Numerado** (recomendado): `1: mi respuesta. 2: mi otra respuesta. 3: ...`\n")
	sb.WriteString("- **Prosa libre**: \"Para la 1 vamos con A porque X. Para la 2 preferimos B. La 3 la descartamos...\"\n")
	sb.WriteString("- **Varios comments**: uno por pregunta, como prefieras.\n\n")
	sb.WriteString(fmt.Sprintf("Cuando termines, corré `che explore %d` — el flow detecta tu respuesta, re-valida con los mismos agentes y, si queda sin ambigüedades, consolida el plan final en el body del issue.\n", issueNumber))

	return sb.String()
}

// collectPlanBlockers devuelve las preguntas del plan marcadas como
// blocker=true; son las principales a contestar.
func collectPlanBlockers(plan *Response) []Question {
	if plan == nil {
		return nil
	}
	var out []Question
	for _, q := range plan.Questions {
		if q.Blocker && strings.TrimSpace(q.Q) != "" {
			out = append(out, q)
		}
	}
	return out
}

// extraQuestion es un finding needs_human de un validador que no está
// cubierto por ninguna pregunta del plan.
type extraQuestion struct {
	text    string
	context string
	source  string
}

// collectValidatorQuestions junta los findings con needs_human=true de los
// validadores y descarta los que ya aparecen cubiertos por las preguntas del
// plan. La detección usa 3 heurísticas en cascada porque los validadores
// suelen "citar" o "glosar" las preguntas del plan en vez de repetirlas tal
// cual — con contains simple no las detectábamos y terminaban duplicadas
// debajo de "Observaciones adicionales".
func collectValidatorQuestions(results []validatorResult, planQs []Question) []extraQuestion {
	seen := map[string]bool{}
	var out []extraQuestion
	for _, r := range results {
		if r.Response == nil {
			continue
		}
		label := fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance)
		for _, f := range r.Response.Findings {
			if !f.NeedsHuman {
				continue
			}
			if isMetaFinding(f.Issue) {
				continue
			}
			norm := normalizeQuestion(f.Issue)
			if norm == "" || seen[norm] {
				continue
			}
			if coversSamePlanQuestion(f.Issue, norm, planQs) {
				continue
			}
			seen[norm] = true
			out = append(out, extraQuestion{text: f.Issue, context: f.Suggestion, source: label})
		}
	}
	return out
}

// coversSamePlanQuestion aplica 3 heurísticas para decidir si un finding
// del validador refiere a una pregunta del plan ya listada:
//
//  1. Contains exacto en cualquier dirección (caso trivial — cuando el
//     validador copió la pregunta textualmente).
//  2. Quote match: si el validador puso una sub-pregunta entre comillas
//     (ej. "'¿Cuál es el input?' es decisión de producto..."), y los
//     tokens significativos de esa comilla son un subconjunto de alguna
//     pregunta del plan → cubierta.
//  3. Meta + overlap: si el finding usa una frase típica de "decisión de
//     producto" / "escalar al humano" / "parte del modelo" y comparte
//     3+ tokens significativos con alguna pregunta del plan → cubierta.
//
// La lógica es conservadora: si el validador aporta genuinamente una
// pregunta nueva que NO comparte tokens centrales ni se parafrasea como
// meta-observación de otra, pasa a "Observaciones adicionales".
func coversSamePlanQuestion(findingText, findingNorm string, planQs []Question) bool {
	for _, pq := range planQs {
		pqNorm := normalizeQuestion(pq.Q)
		if pqNorm != "" && (strings.Contains(pqNorm, findingNorm) || strings.Contains(findingNorm, pqNorm)) {
			return true
		}
	}
	findingSig := significantTokens(findingNorm)

	// (2) Quote match.
	for _, quoted := range extractQuotedTexts(findingText) {
		qTokens := significantTokens(normalizeQuestion(quoted))
		if len(qTokens) < 2 {
			continue
		}
		for _, pq := range planQs {
			planSet := toTokenSet(significantTokens(normalizeQuestion(pq.Q)))
			if isTokenSubset(qTokens, planSet) {
				return true
			}
		}
	}

	// (3) Meta phrase + token overlap.
	if containsMetaPhrase(findingNorm) {
		for _, pq := range planQs {
			planSig := significantTokens(normalizeQuestion(pq.Q))
			if countCommonTokens(findingSig, planSig) >= 3 {
				return true
			}
		}
	}

	return false
}

// normalizeQuestion baja a lowercase, quita signos de puntuación y colapsa
// espacios para comparar textos parecidos.
var punctRe = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
var wsRe = regexp.MustCompile(`\s+`)

func normalizeQuestion(s string) string {
	s = strings.ToLower(s)
	s = punctRe.ReplaceAllString(s, " ")
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stopwordsES son palabras vacías frecuentes en español que no aportan
// información sobre el tema de una pregunta. Se usan solo para heurísticas
// de similitud (dedupe de preguntas de validators); no afectan
// normalización ni matching en otros lugares.
var stopwordsES = map[string]bool{
	"a": true, "al": true, "ante": true, "bajo": true, "cabe": true, "con": true,
	"contra": true, "cual": true, "cuales": true, "cuando": true, "de": true,
	"del": true, "desde": true, "donde": true, "durante": true, "e": true, "el": true,
	"en": true, "entre": true, "era": true, "eran": true, "es": true, "esa": true,
	"esas": true, "ese": true, "eso": true, "esos": true, "esta": true, "estas": true,
	"este": true, "esto": true, "estos": true, "ha": true, "han": true, "hasta": true,
	"hay": true, "la": true, "las": true, "le": true, "les": true, "lo": true, "los": true,
	"mas": true, "me": true, "mi": true, "mis": true, "muy": true, "ni": true, "no": true,
	"nos": true, "o": true, "para": true, "pero": true, "por": true, "porque": true,
	"que": true, "quien": true, "quienes": true, "se": true, "segun": true, "ser": true,
	"si": true, "sin": true, "sobre": true, "son": true, "su": true, "sus": true,
	"te": true, "tras": true, "tu": true, "tus": true, "u": true, "un": true, "una": true,
	"uno": true, "unos": true, "y": true, "ya": true,
}

// significantTokens tokeniza el texto normalizado y saca stopwords + palabras
// de 1-2 letras. Devuelve tokens en orden de aparición (con repeticiones).
func significantTokens(normalized string) []string {
	if normalized == "" {
		return nil
	}
	fields := strings.Fields(normalized)
	out := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 3 {
			continue
		}
		if stopwordsES[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// tokenRoot devuelve un "stem" aproximado del token: los primeros 5
// caracteres si el token es suficientemente largo. Es un stemmer naive pero
// suficiente para matchear "label"/"labels" y "transición"/"transiciona"
// como el mismo concepto sin meter una dependencia de stemming real.
func tokenRoot(t string) string {
	if len(t) <= 5 {
		return t
	}
	return t[:5]
}

// toTokenSet convierte un slice de tokens a un set indexado por tokenRoot,
// así el matching es laxo respecto a flexiones morfológicas simples.
func toTokenSet(tokens []string) map[string]bool {
	m := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		m[tokenRoot(t)] = true
	}
	return m
}

// isTokenSubset devuelve true si todos los tokens del subset aparecen en el
// set (comparando por root). Requiere len(subset) >= 2 para evitar matches
// triviales.
func isTokenSubset(subset []string, set map[string]bool) bool {
	if len(subset) < 2 {
		return false
	}
	for _, t := range subset {
		if !set[tokenRoot(t)] {
			return false
		}
	}
	return true
}

// countCommonTokens cuenta cuántos roots únicos de a aparecen en b.
func countCommonTokens(a, b []string) int {
	bSet := toTokenSet(b)
	seen := map[string]bool{}
	n := 0
	for _, t := range a {
		r := tokenRoot(t)
		if seen[r] {
			continue
		}
		if bSet[r] {
			n++
			seen[r] = true
		}
	}
	return n
}

// extractQuotedTexts extrae los segmentos entre comillas simples, dobles o
// angulares. Se usan para detectar cuando un validador cita la pregunta del
// plan para introducir su observación.
var quotedRe = regexp.MustCompile(`['"«»‘’“”](.+?)['"«»‘’“”]`)

func extractQuotedTexts(s string) []string {
	matches := quotedRe.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if strings.TrimSpace(m[1]) != "" {
			out = append(out, m[1])
		}
	}
	return out
}

// metaPhrases son frases tipicas cuando un validador glosa/paraphrasea una
// pregunta del plan diciendo "esto es de producto" en vez de plantear algo
// nuevo. Se usan como señal de que el finding probablemente es eco.
var metaPhrases = []string{
	"decision de producto",
	"decisión de producto",
	"escalar al humano",
	"escalar al usuario",
	"producto del dueño",
	"producto del cli",
	"parte del modelo",
	"modelo de estado",
	"mantener como blocker",
	"requerir decision del",
	"decision del dueño",
}

func containsMetaPhrase(normalized string) bool {
	for _, p := range metaPhrases {
		if strings.Contains(normalized, p) {
			return true
		}
	}
	return false
}

// isMetaFinding detecta cuando un finding del validador es una
// meta-descripción total ("las 3 preguntas bloqueantes...") en lugar de un
// gap concreto. Estos findings nunca aportan info nueva, van directo al
// descarte antes de cualquier dedupe contra el plan.
func isMetaFinding(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	prefixes := []string{
		"las preguntas ",
		"estas preguntas ",
		"los puntos ",
		"escalar ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	if strings.Contains(s, "preguntas bloqueantes") || strings.Contains(s, "preguntas que definen") {
		return true
	}
	return false
}

// renderValidationReport arma el bloque de texto que se imprime en stdout
// después de correr validadores. Lista el verdict de cada uno + hint de qué
// hacer si hay preguntas humanas.
func renderValidationReport(results []validatorResult, humanGaps bool) string {
	var sb strings.Builder
	sb.WriteString("Validation report:\n")
	for _, r := range results {
		label := fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance)
		if r.Err != nil {
			sb.WriteString(fmt.Sprintf("  ✗ %s: error — %v\n", label, r.Err))
			continue
		}
		mark := "✓"
		switch r.Response.Verdict {
		case "changes_requested":
			mark = "⚠"
		case "needs_human":
			mark = "🧑"
		}
		sb.WriteString(fmt.Sprintf("  %s %s: %s — %s (%d findings)\n",
			mark, label, r.Response.Verdict, r.Response.Summary, len(r.Response.Findings)))
	}
	if humanGaps {
		sb.WriteString("\n⚠ Hay preguntas que requieren input tuyo. El issue se marcó con status:awaiting-human y se posteó un comment con las preguntas. Contestá en el issue y corré `che explore <ref>` de nuevo — el flow va a detectar tu respuesta, re-validar, y cerrar el plan si queda sin ambigüedades.\n")
	}
	return sb.String()
}

// conversationState resume lo que extraemos de los comments del issue para
// modo resume: el plan del ejecutor, los validadores que participaron, la
// iteración más alta, las respuestas humanas posteriores al último human
// request.
type conversationState struct {
	ExecutorAgent Agent
	ExecutorPlan  *Response
	Validators    []Validator
	MaxIter       int
	HumanAnswers  []string
	// PreviousFindings son los findings de la iteración más reciente de
	// validators; se pasan al prompt de resume para que el validador vea qué
	// preguntó antes.
	PreviousFindings []validatorResult
}

// parseConversation recorre los comments del issue (en orden cronológico) y
// arma el estado que necesita el modo resume.
func parseConversation(issue *Issue) conversationState {
	st := conversationState{}
	seenValidators := map[string]bool{} // "<agent>#<instance>"
	var lastHumanRequestIdx int = -1
	var lastHumanRequestIter int
	// Agrupo validators por iter para determinar la lista completa.
	iterValidators := map[int][]validatorResult{}

	for idx, c := range issue.Comments {
		h := c.Header()
		if h.Role == "" {
			// Comment humano (sin header) — lo consideramos respuesta solo si
			// está después del último human-request. Lo resolvemos en un
			// segundo pase abajo.
			continue
		}
		if h.Iter > st.MaxIter {
			st.MaxIter = h.Iter
		}
		switch h.Role {
		case "executor":
			// Preservamos siempre el último executor (por si hubo iter=2).
			if raw := extractEmbeddedJSON(c.Body); raw != "" {
				var plan Response
				if err := json.Unmarshal([]byte(raw), &plan); err == nil {
					st.ExecutorPlan = &plan
					st.ExecutorAgent = h.Agent
				}
			}
		case "validator":
			key := fmt.Sprintf("%s#%d", h.Agent, h.Instance)
			if !seenValidators[key] {
				seenValidators[key] = true
				st.Validators = append(st.Validators, Validator{Agent: h.Agent, Instance: h.Instance})
			}
			// Extraemos el ValidatorResponse para poder pasarle feedback al
			// prompt de iter siguiente.
			if raw := extractEmbeddedJSON(c.Body); raw != "" {
				var vr ValidatorResponse
				if err := json.Unmarshal([]byte(raw), &vr); err == nil {
					iterValidators[h.Iter] = append(iterValidators[h.Iter], validatorResult{
						Validator: Validator{Agent: h.Agent, Instance: h.Instance},
						Response:  &vr,
					})
				}
			}
		case "human-request":
			lastHumanRequestIdx = idx
			lastHumanRequestIter = h.Iter
		}
	}

	// Respuestas humanas: todos los comments sin header posteriores al último
	// human-request.
	if lastHumanRequestIdx >= 0 {
		for _, c := range issue.Comments[lastHumanRequestIdx+1:] {
			if c.IsHuman() && strings.TrimSpace(c.Body) != "" {
				st.HumanAnswers = append(st.HumanAnswers, c.Body)
			}
		}
	}

	// PreviousFindings = findings de la iter del último human-request.
	st.PreviousFindings = iterValidators[lastHumanRequestIter]

	return st
}

// runValidatorsResumeParallel corre los validadores con un prompt que
// incluye las respuestas humanas + el plan original. El loop estructural es
// igual que runValidatorsParallel; solo cambia el prompt.
func runValidatorsResumeParallel(issue *Issue, state conversationState, validators []Validator, progress func(string)) []validatorResult {
	results := make([]validatorResult, len(validators))
	var wg sync.WaitGroup
	for i, v := range validators {
		wg.Add(1)
		go func(i int, v Validator) {
			defer wg.Done()
			label := fmt.Sprintf("%s#%d", v.Agent, v.Instance)
			progress(label + ": consultando (resume)…")
			resp, err := callValidatorResume(v, issue, state, func(line string) {
				progress(label + ": " + line)
			})
			results[i] = validatorResult{Validator: v, Response: resp, Err: err}
		}(i, v)
	}
	wg.Wait()
	return results
}

// callValidatorResume es como callValidator pero usa el prompt de reanudación
// que incluye respuestas humanas + findings previos.
func callValidatorResume(v Validator, issue *Issue, state conversationState, progress func(string)) (*ValidatorResponse, error) {
	prompt := buildValidatorResumePrompt(issue, state)
	out, err := runAgentCmd(v.Agent, prompt, progress, "")
	if err != nil {
		return nil, err
	}
	return parseValidatorResponse(out)
}

func buildValidatorResumePrompt(issue *Issue, state conversationState) string {
	planJSON, _ := json.MarshalIndent(state.ExecutorPlan, "", "  ")

	var previousFindings strings.Builder
	for _, r := range state.PreviousFindings {
		if r.Response == nil {
			continue
		}
		previousFindings.WriteString(fmt.Sprintf("- %s#%d (verdict=%s, %d findings)\n",
			r.Validator.Agent, r.Validator.Instance, r.Response.Verdict, len(r.Response.Findings)))
		for _, f := range r.Response.Findings {
			tag := ""
			if f.NeedsHuman {
				tag = " [needs_human]"
			}
			previousFindings.WriteString(fmt.Sprintf("  - [%s · %s]%s %s\n", f.Severity, f.Area, tag, f.Issue))
		}
	}

	humanText := strings.Join(state.HumanAnswers, "\n\n---\n\n")

	return `Sos un validador técnico senior. En una iteración anterior, otros validadores (incluido vos posiblemente) marcaron que el plan necesitaba input humano para ciertas preguntas de producto. El humano contestó. Tu tarea ahora es verificar si las respuestas cubren los gaps, y si queda algo sin responder.

Reglas de esta iteración:
1. Si las respuestas humanas cubren todas las preguntas que tenían needs_human=true en iter anterior, devolvé verdict=approve.
2. Si quedan gaps técnicos menores (no bloqueantes ni de producto), devolvé verdict=changes_requested con findings específicos. Esto NO bloquea convergencia.
3. Si las respuestas son ambiguas, parciales o contradicen algo del plan, devolvé verdict=needs_human con un finding needs_human=true explicando QUÉ falta responder — NO repitas las preguntas si ya fueron contestadas.
4. Sé riguroso pero no inventes gaps — si la respuesta humana es clara aunque breve, aceptala.

Valores válidos: mismo enum que antes (verdict, severity, area).

Devolvé EXCLUSIVAMENTE un objeto JSON con el shape:

{
  "verdict": "approve",
  "summary": "Tu opinión global en 1-2 oraciones",
  "findings": [
    {"severity": "minor", "area": "paths", "where": "...", "issue": "...", "suggestion": "...", "needs_human": false}
  ]
}

Issue #` + fmt.Sprint(issue.Number) + `:
Título: ` + issue.Title + `

Body del issue:
<<<
` + issue.Body + `
>>>

Plan del ejecutor (iter=1):
<<<
` + string(planJSON) + `
>>>

Findings de validadores en iter anterior:
<<<
` + previousFindings.String() + `
>>>

Respuestas del humano:
<<<
` + humanText + `
>>>`
}

// ConsolidatedPlan es el plan final post-convergencia que se escribe al body
// del issue. Sin ambigüedades, listo para ejecutar.
type ConsolidatedPlan struct {
	Summary            string   `json:"summary"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Approach           string   `json:"approach"`
	Steps              []string `json:"steps"`
	RisksToMitigate    []Risk   `json:"risks_to_mitigate"`
	OutOfScope         []string `json:"out_of_scope"`
}

// callConsolidation invoca al ejecutor con un prompt de "consolidación" que
// recibe el plan original, las respuestas humanas y los findings finales, y
// produce el ConsolidatedPlan listo para ser el nuevo body del issue.
func callConsolidation(agent Agent, issue *Issue, state conversationState, finalResults []validatorResult, progress func(string)) (*ConsolidatedPlan, error) {
	prompt := buildConsolidationPrompt(issue, state, finalResults)
	rawOut, err := runAgentCmd(agent, prompt, progress, string(agent)+" (consolidación): ")
	if err != nil {
		return nil, err
	}

	raw := strings.TrimSpace(rawOut)
	if strings.HasPrefix(raw, "```") {
		if nl := strings.Index(raw, "\n"); nl >= 0 {
			raw = raw[nl+1:]
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if i := strings.LastIndex(raw, "}"); i >= 0 && i < len(raw)-1 {
		raw = raw[:i+1]
	}
	var cp ConsolidatedPlan
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		return nil, fmt.Errorf("invalid JSON from consolidation: %w (raw: %q)", err, truncate(raw, 200))
	}
	return &cp, nil
}

func buildConsolidationPrompt(issue *Issue, state conversationState, finalResults []validatorResult) string {
	planJSON, _ := json.MarshalIndent(state.ExecutorPlan, "", "  ")

	var findingsText strings.Builder
	for _, r := range finalResults {
		if r.Response == nil {
			continue
		}
		findingsText.WriteString(fmt.Sprintf("- %s#%d (verdict=%s): %s\n",
			r.Validator.Agent, r.Validator.Instance, r.Response.Verdict, r.Response.Summary))
		for _, f := range r.Response.Findings {
			findingsText.WriteString(fmt.Sprintf("  - [%s · %s] %s", f.Severity, f.Area, f.Issue))
			if f.Suggestion != "" {
				findingsText.WriteString(" — sugerencia: " + f.Suggestion)
			}
			findingsText.WriteString("\n")
		}
	}

	humanText := strings.Join(state.HumanAnswers, "\n\n---\n\n")

	return `Sos un ingeniero senior. Tenés que consolidar un plan de implementación para un issue de GitHub. El plan ya pasó por una ronda de exploración + validación + respuesta del humano a preguntas de producto. Tu tarea es producir el plan FINAL sin ambigüedades — un ingeniero que lea esto tiene que poder arrancar a implementar sin más preguntas.

Reglas:
- Incorporá las respuestas del humano como DECISIONES firmes (no como "a definir").
- Los findings de los validadores son cosas a cubrir: los blockers/majors van como steps o acceptance_criteria, los minors van como risks_to_mitigate.
- Elegí UN approach (el recommended del plan original ajustado por las decisiones del humano si cambió algo).
- Sé concreto: si el humano dijo "no aplica X", sacá X del alcance; si dijo "preferimos A sobre B", elegí A y descartá B.
- No incluyas preguntas abiertas ni ambigüedades — si algo quedó gris, elegí la opción más conservadora y anotá en risks_to_mitigate.

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "summary": "1-2 oraciones del qué y para qué",
  "goal": "Qué logramos cuando esto está implementado",
  "acceptance_criteria": ["Criterio 1 observable", "Criterio 2"],
  "approach": "Descripción del approach elegido, 2-4 oraciones",
  "steps": ["Paso 1 concreto", "Paso 2 concreto"],
  "risks_to_mitigate": [
    {"risk": "...", "likelihood": "low|medium|high", "impact": "low|medium|high", "mitigation": "..."}
  ],
  "out_of_scope": ["Cosa que explícitamente NO hacemos ahora"]
}

Issue #` + fmt.Sprint(issue.Number) + `:
Título: ` + issue.Title + `

Body original:
<<<
` + issue.Body + `
>>>

Plan original del ejecutor:
<<<
` + string(planJSON) + `
>>>

Respuestas del humano a preguntas de producto:
<<<
` + humanText + `
>>>

Findings de validadores en la iteración final:
<<<
` + findingsText.String() + `
>>>`
}

func validateConsolidated(c *ConsolidatedPlan) error {
	if strings.TrimSpace(c.Summary) == "" {
		return fmt.Errorf("summary is empty")
	}
	if strings.TrimSpace(c.Goal) == "" {
		return fmt.Errorf("goal is empty")
	}
	if len(c.AcceptanceCriteria) == 0 {
		return fmt.Errorf("acceptance_criteria is empty")
	}
	if strings.TrimSpace(c.Approach) == "" {
		return fmt.Errorf("approach is empty")
	}
	if len(c.Steps) == 0 {
		return fmt.Errorf("steps is empty")
	}
	for i, r := range c.RisksToMitigate {
		if !validLikelihood[r.Likelihood] {
			return fmt.Errorf("risk %d: likelihood %q not in [low medium high]", i, r.Likelihood)
		}
		if !validImpact[r.Impact] {
			return fmt.Errorf("risk %d: impact %q not in [low medium high]", i, r.Impact)
		}
	}
	return nil
}

// renderConsolidatedBody arma el nuevo body del issue: plan consolidado
// arriba (listo para ejecución), idea original preservada abajo como
// referencia.
func renderConsolidatedBody(c *ConsolidatedPlan, originalBody string) string {
	var sb strings.Builder
	sb.WriteString("## Plan consolidado (post-exploración)\n\n")
	sb.WriteString("**Resumen:** " + c.Summary + "\n\n")
	sb.WriteString("**Goal:** " + c.Goal + "\n\n")

	sb.WriteString("### Criterios de aceptación\n")
	for _, crit := range c.AcceptanceCriteria {
		sb.WriteString("- [ ] " + crit + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("### Approach\n")
	sb.WriteString(c.Approach + "\n\n")

	sb.WriteString("### Pasos\n")
	for i, step := range c.Steps {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, step))
	}
	sb.WriteString("\n")

	if len(c.RisksToMitigate) > 0 {
		sb.WriteString("### Riesgos a mitigar\n")
		for _, r := range c.RisksToMitigate {
			sb.WriteString(fmt.Sprintf("- **%s** (likelihood=%s, impact=%s) — %s\n",
				r.Risk, r.Likelihood, r.Impact, r.Mitigation))
		}
		sb.WriteString("\n")
	}

	if len(c.OutOfScope) > 0 {
		sb.WriteString("### Fuera de alcance\n")
		for _, o := range c.OutOfScope {
			sb.WriteString("- " + o + "\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("## Idea original (de `che idea`)\n\n")
	sb.WriteString(originalBody)
	if !strings.HasSuffix(originalBody, "\n") {
		sb.WriteString("\n")
	}

	return sb.String()
}

// editIssueBody reemplaza el body del issue via `gh issue edit --body-file`.
func editIssueBody(ref, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-explore-body-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "issue", "edit", ref, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit --body-file: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// closeAwaitingHuman saca el label status:awaiting-human y agrega status:plan
// en la misma operación, cerrando el ciclo.
func closeAwaitingHuman(ref string) error {
	cmd := exec.Command("gh", "issue", "edit", ref,
		"--remove-label", "status:awaiting-human",
		"--remove-label", "status:idea",
		"--add-label", "status:plan")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
