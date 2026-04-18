// Package explore implements flow 02 — tomar un issue ya creado por `che
// idea`, leerlo, profundizar con claude, y persistir el análisis (comentario +
// transición de label). La lógica vive acá (pura, testeable) para que el
// subcomando `che explore` y la TUI compartan la misma implementación.
package explore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	URL    string  `json:"url"`
	State  string  `json:"state"`
	Labels []Label `json:"labels"`
}

// Label es el shape que gh devuelve para cada label del issue.
type Label struct {
	Name string `json:"name"`
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

// Run ejecuta el flow completo.
func Run(issueRef string, opts Opts) ExitCode {
	stdout, stderr := opts.Stdout, opts.Stderr
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}
	agent := opts.Agent
	if agent == "" {
		agent = DefaultAgent
	}
	if agent.Binary() == "" {
		fmt.Fprintf(stderr, "error: unknown agent %q\n", agent)
		return ExitSemantic
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

	if err := gateIssue(issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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

	// --- etapa de validación (opt-in vía opts.Validators) ---
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

	humanGaps := hasHumanGaps(validationResults)
	if humanGaps {
		progress("validadores pidieron input humano; posteando request…")
		humanReq := renderHumanRequest(validationResults, 1)
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
		fmt.Fprintln(stdout, renderValidationReport(validationResults, true))
		fmt.Fprintf(stdout, "Paused %s — contestá en el issue y corré de nuevo (v0.0.13 detecta la respuesta).\n", issue.URL)
		return ExitOK
	}

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

	if len(validationResults) > 0 {
		fmt.Fprintln(stdout, renderValidationReport(validationResults, false))
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
// no fueron explorados (status:plan ausente). Limita a los 50 más recientes
// para mantener la TUI manejable.
func ListCandidates() ([]Candidate, error) {
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
	out2 := make([]Candidate, 0, len(raw))
	for _, i := range raw {
		if i.HasLabel("status:plan") {
			continue
		}
		out2 = append(out2, Candidate{Number: i.Number, Title: i.Title})
	}
	return out2, nil
}

// fetchIssue corre `gh issue view <ref> --json ...` y parsea el output. El
// ref puede ser número, URL, o owner/repo#N — gh los normaliza.
func fetchIssue(ref string) (*Issue, error) {
	cmd := exec.Command("gh", "issue", "view", ref,
		"--json", "number,title,body,labels,url,state")
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

// gateIssue valida que el issue sea candidato para explorar.
func gateIssue(i *Issue) error {
	if i.State != "OPEN" {
		return fmt.Errorf("issue #%d is closed", i.Number)
	}
	if !i.HasLabel("ct:plan") {
		return fmt.Errorf("issue #%d is missing label ct:plan (not created by `che idea`?)", i.Number)
	}
	if i.HasLabel("status:plan") {
		return fmt.Errorf("issue #%d was already explored (status:plan present)", i.Number)
	}
	return nil
}

// callAgent invoca al binario correspondiente al agente elegido con el prompt
// construido para el issue. Mantiene el patrón -p + streaming por stdout
// establecido en el flow idea.
func callAgent(agent Agent, issue *Issue, progress func(string)) (*Response, error) {
	prompt := buildPrompt(issue)
	cmd := exec.Command(agent.Binary(), "-p", prompt, "--output-format", "text")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var fullOutput strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fullOutput.WriteString(line + "\n")
		if strings.TrimSpace(line) != "" {
			progress(string(agent) + ": " + line)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderrBuf.String()))
		}
		return nil, err
	}

	return parseResponse(fullOutput.String())
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
// Arranca con un header HTML que identifica al ejecutor y la iteración — lo
// van a leer iteraciones futuras (validadores, correcciones) para saber qué
// comments corresponden a qué agente y ronda.
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

	return sb.String()
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
	cmd := exec.Command(v.Agent.Binary(), "-p", prompt, "--output-format", "text")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var fullOutput strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fullOutput.WriteString(line + "\n")
		if strings.TrimSpace(line) != "" {
			progress(line)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderrBuf.String()))
		}
		return nil, err
	}

	return parseValidatorResponse(fullOutput.String())
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
	return sb.String()
}

// renderHumanRequest genera el comment que se postea cuando el flow queda en
// pausa esperando input humano. Lista las preguntas que los validadores
// marcaron con needs_human=true.
func renderHumanRequest(results []validatorResult, iter int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=explore iter=%d role=human-request -->\n", iter))
	sb.WriteString("## 🧑 Input humano requerido\n\n")
	sb.WriteString("Los validadores marcaron preguntas que requieren decisión de producto — no técnicas — y no pueden contestarlas por sí solos. Contestalas en un comment nuevo en este issue (lenguaje libre, una o varias respuestas cubriendo todo). Al re-correr `che explore`, el flow va a leer tus respuestas y continuar desde donde quedó.\n\n")
	sb.WriteString("### Preguntas\n")
	for _, r := range results {
		if r.Response == nil {
			continue
		}
		wrote := false
		for _, f := range r.Response.Findings {
			if !f.NeedsHuman {
				continue
			}
			sb.WriteString(fmt.Sprintf("- **[%s#%d · %s]** %s\n",
				r.Validator.Agent, r.Validator.Instance, f.Area, f.Issue))
			if f.Suggestion != "" {
				sb.WriteString("  - contexto: " + f.Suggestion + "\n")
			}
			wrote = true
		}
		if !wrote && r.Response.Verdict == "needs_human" {
			sb.WriteString(fmt.Sprintf("- **[%s#%d]** %s\n",
				r.Validator.Agent, r.Validator.Instance, r.Response.Summary))
		}
	}
	return sb.String()
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
		sb.WriteString("\n⚠ Hay preguntas que requieren input tuyo. El issue se marcó con status:awaiting-human y se posteó un comment con las preguntas. Contestá en el issue y corré `che explore <ref>` de nuevo (la reanudación automática llega en v0.0.13).\n")
	}
	return sb.String()
}
