// Package validate implements flow 04 — tomar un PR abierto, correr N
// validadores (opus/codex/gemini) en paralelo sobre su diff, y postear los
// findings como comments del PR. Es el único flow puramente síncrono del CLI:
// el usuario espera a que todos los validadores terminen y los comments estén
// posteados antes de que `che validate` retorne.
//
// La lógica vive acá (pura, testeable) para que el subcomando `che validate`
// y la TUI compartan la misma implementación.
//
// NOTA: este paquete es deliberadamente una copia adaptada de explore/execute
// (agent enum + prompt builder + postPRComment). La deuda de extraer lo
// común a `internal/flow/common/` queda para un issue futuro.
package validate

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/labels"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh falla)
	ExitSemantic ExitCode = 3 // ref vacío, PR cerrado, validators inválidos
)

// Agent identifica qué CLI invocar como validador.
type Agent string

const (
	AgentOpus   Agent = "opus"
	AgentCodex  Agent = "codex"
	AgentGemini Agent = "gemini"
)

// DefaultValidators es el panel por defecto cuando el caller no pasa uno
// explícito. Coherente con explore v0.0.23: opus como validador base.
const DefaultValidators = "opus"

// ValidAgents lista los agentes soportados (orden preservado para UI).
var ValidAgents = []Agent{AgentOpus, AgentCodex, AgentGemini}

// Binary devuelve el nombre del ejecutable para cada agente.
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

// InvokeArgs devuelve los args de CLI en modo no-interactivo (mismo mapping
// que explore/execute — si cambian los flags de los CLIs, hay que tocar los 3).
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

// ParseAgent normaliza un string a Agent.
func ParseAgent(s string) (Agent, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, a := range ValidAgents {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown agent %q; valid: opus, codex, gemini", s)
}

// AgentTimeout es el tiempo máximo de espera para cada validador individual.
// Configurable con CHE_VALIDATE_AGENT_TIMEOUT_SECS. Default 10min: el diff de
// un PR + review profunda puede tardar más que un explore.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_VALIDATE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 10 * time.Minute
}()

// Validator identifica un validador: agente + instancia (1..N) para distinguir
// cuando el mismo agente aparece varias veces en la lista.
type Validator struct {
	Agent    Agent
	Instance int
}

// ParseValidators parsea la flag `--validators` (ej: "opus", "codex,gemini",
// "codex,codex,gemini"). validate requiere al menos 1 validador — a diferencia
// de execute, aca "none" no tiene sentido (el flow completo se reduce a nada).
func ParseValidators(s string) ([]Validator, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("validators: empty — validate requires at least 1 validator")
	}
	if strings.EqualFold(s, "none") {
		return nil, fmt.Errorf("validators: 'none' is not allowed — validate requires at least 1 validator")
	}
	parts := strings.Split(s, ",")
	if len(parts) < 1 || len(parts) > 3 {
		return nil, fmt.Errorf("validators: need 1-3 items, got %d", len(parts))
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

// Opts agrupa los writers y la lista de validadores.
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string)
	Validators []Validator
}

// PullRequest modela el subset de `gh pr view --json ...` que usamos. Los
// comments se traen con fetchPRComments() porque se usan sólo para calcular
// iter, no son parte del fetch inicial (el diff sí es otra llamada).
type PullRequest struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	HeadBranch string `json:"headRefName"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
	ClosingIssuesReferences []struct {
		Number int `json:"number"`
	} `json:"closingIssuesReferences"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// HasLabel devuelve true si el PR ya tiene el label name.
func (p *PullRequest) HasLabel(name string) bool {
	for _, l := range p.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Candidate es la vista mínima para la TUI al listar PRs abiertos.
type Candidate struct {
	Number         int
	Title          string
	URL            string
	IsDraft        bool
	Author         string
	RelatedIssues  []int // issues referenciados via "Closes #N" / "Fixes #N" en el body del PR
}

// PRComment es un comment del PR; el body puede tener header de claude-cli al
// principio — lo usamos para calcular max(iter) + 1 en runs sucesivos.
type PRComment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
}

// CommentHeader es la metadata parseada del HTML comment de che al inicio del
// body del PR comment. Mismo shape que explore, pero con flow="validate".
type CommentHeader struct {
	Flow     string
	Iter     int
	Agent    Agent
	Instance int
	Role     string
}

var headerRe = regexp.MustCompile(`^<!--\s*claude-cli:\s*(.+?)\s*-->`)
var kvRe = regexp.MustCompile(`(\w+)=(\S+)`)

// ParseCommentHeader lee la primera línea del body y si es un HTML comment de
// che devuelve la metadata. Si no, devuelve un header vacío.
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
			if n, err := strconv.Atoi(kv[2]); err == nil {
				h.Iter = n
			}
		case "agent":
			h.Agent = Agent(kv[2])
		case "instance":
			if n, err := strconv.Atoi(kv[2]); err == nil {
				h.Instance = n
			}
		case "role":
			h.Role = kv[2]
		}
	}
	return h
}

// DetermineIter dada una lista de comments devuelve el iter siguiente
// (max(flow=validate) + 1). Si no hay comments previos con flow=validate,
// devuelve 1.
func DetermineIter(comments []PRComment) int {
	max := 0
	for _, c := range comments {
		h := ParseCommentHeader(c.Body)
		if h.Flow != "validate" {
			continue
		}
		if h.Iter > max {
			max = h.Iter
		}
	}
	return max + 1
}

// Response es lo que cada validador devuelve en JSON después de leer el diff.
type Response struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Finding es una observación concreta del validador sobre el diff.
type Finding struct {
	Severity   string `json:"severity"`
	Area       string `json:"area"`
	Where      string `json:"where"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion,omitempty"`
	NeedsHuman bool   `json:"needs_human"`
	Kind       string `json:"kind,omitempty"`
}

var (
	validVerdicts   = map[string]bool{"approve": true, "changes_requested": true, "needs_human": true}
	validSeverities = map[string]bool{"blocker": true, "major": true, "minor": true}
)

// validatorResult agrupa qué validador corrió, qué devolvió y si falló.
type validatorResult struct {
	Validator Validator
	Response  *Response
	Err       error
}

// Run ejecuta el flow completo sobre un PR. Es sync: espera a que todos los
// validadores terminen y sus comments estén posteados antes de retornar.
// Decisiones:
//   - Preflight: gh auth + remote github.
//   - Fetch del PR: gh pr view --json ...
//   - Diff: gh pr diff <n>
//   - Iter: max(flow=validate) + 1 sobre comments previos.
//   - Validadores: goroutines en paralelo, wait sin timeout global (cada uno
//     tiene su AgentTimeout individual — si se cuelga, muere).
//   - Comments: 1 por validador con título visible + header HTML + 1 resumen
//     final con tabla.
//   - Stdout: reporte resumido (verdict + findings count por validador).
func Run(prRef string, opts Opts) ExitCode {
	stdout, stderr := opts.Stdout, opts.Stderr
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	prRef = strings.TrimSpace(prRef)
	if prRef == "" {
		fmt.Fprintln(stderr, "error: pr ref is empty")
		return ExitSemantic
	}

	if len(opts.Validators) == 0 {
		fmt.Fprintln(stderr, "error: at least 1 validator is required")
		return ExitSemantic
	}

	progress("chequeando auth de GitHub…")
	if err := precheckGitHubRemote(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("obteniendo PR desde GitHub…")
	pr, err := FetchPR(prRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetching PR: %v\n", err)
		return ExitRetry
	}
	if pr.State != "OPEN" {
		fmt.Fprintf(stderr, "error: PR #%d is not OPEN (state=%s)\n", pr.Number, pr.State)
		return ExitSemantic
	}

	progress("descargando diff del PR…")
	diff, err := FetchDiff(prRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetching diff: %v\n", err)
		return ExitRetry
	}
	if strings.TrimSpace(diff) == "" {
		fmt.Fprintf(stderr, "error: diff del PR #%d está vacío — ¿está mergeado o no tiene cambios?\n", pr.Number)
		return ExitSemantic
	}

	progress("leyendo comments previos para calcular iter…")
	comments, err := FetchPRComments(prRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetching comments: %v\n", err)
		return ExitRetry
	}
	iter := DetermineIter(comments)

	progress(fmt.Sprintf("corriendo %d validador(es) en paralelo (iter=%d)…", len(opts.Validators), iter))
	results := runValidatorsParallel(pr, diff, opts.Validators, progress)

	progress("posteando comments de validadores…")
	for _, r := range results {
		body := renderValidatorComment(r, iter)
		if err := postPRComment(prRef, body); err != nil {
			fmt.Fprintf(stderr, "error: posting %s#%d comment: %v\n",
				r.Validator.Agent, r.Validator.Instance, err)
			return ExitRetry
		}
	}

	progress("posteando comment resumen…")
	summary := renderSummaryComment(results, iter)
	if err := postPRComment(prRef, summary); err != nil {
		fmt.Fprintf(stderr, "error: posting summary comment: %v\n", err)
		return ExitRetry
	}

	if verdict := consolidateVerdict(results); verdict != "" {
		target := verdictToLabel(verdict)
		progress("aplicando label " + target + " al PR…")
		if err := applyValidatedLabel(prRef, pr, target); err != nil {
			fmt.Fprintf(stderr, "warning: no pude aplicar label %s al PR: %v\n", target, err)
		}
	}

	fmt.Fprintln(stdout, renderReport(results))
	fmt.Fprintf(stdout, "che ya dejó los comments en el PR %s\n", pr.URL)
	return ExitOK
}

// consolidateVerdict devuelve el verdict consolidado (peor caso) de todos los
// validadores. Precedencia: needs_human > changes_requested > approve.
// Los resultados con error se ignoran — si todos erraron, devuelve "".
func consolidateVerdict(results []validatorResult) string {
	rank := map[string]int{"approve": 1, "changes_requested": 2, "needs_human": 3}
	worst := ""
	for _, r := range results {
		if r.Err != nil || r.Response == nil {
			continue
		}
		if rank[r.Response.Verdict] > rank[worst] {
			worst = r.Response.Verdict
		}
	}
	return worst
}

// verdictToLabel mapea un verdict JSON (snake_case) al label correspondiente
// (kebab-case).
func verdictToLabel(verdict string) string {
	switch verdict {
	case "approve":
		return labels.ValidatedApprove
	case "changes_requested":
		return labels.ValidatedChangesRequested
	case "needs_human":
		return labels.ValidatedNeedsHuman
	}
	return ""
}

// applyValidatedLabel asegura que el label target exista en el repo y lo
// aplica al PR, removiendo primero cualquier otro label validated:* presente
// (son mutuamente excluyentes). Idempotente: si el target ya está y no hay
// otros para remover, no hace nada.
func applyValidatedLabel(prRef string, pr *PullRequest, target string) error {
	if target == "" {
		return fmt.Errorf("empty target label")
	}
	if err := labels.Ensure(target); err != nil {
		return err
	}
	args := []string{"pr", "edit", prRef}
	changes := false
	for _, l := range labels.AllValidated {
		if l == target {
			continue
		}
		if pr.HasLabel(l) {
			args = append(args, "--remove-label", l)
			changes = true
		}
	}
	if !pr.HasLabel(target) {
		args = append(args, "--add-label", target)
		changes = true
	}
	if !changes {
		return nil
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ListOpenPRs devuelve los PRs abiertos del repo actual (todos, sin filtrar
// por autor ni branch prefix). Limita a 50 — la TUI se vuelve inmanejable con
// más. Decisión de producto cerrada: validate actúa sobre cualquier PR abierto,
// no solo los creados por che execute.
//
// Excluye PRs que ya tienen label validated:approve — son verdict final y
// no necesitan reaparecer en la TUI (el usuario igual puede re-validar con
// `che validate <n>` explícito si hiciera falta).
func ListOpenPRs() ([]Candidate, error) {
	cmd := exec.Command("gh", "pr", "list",
		"--state", "open",
		"--json", "number,title,url,isDraft,author,headRefName,closingIssuesReferences,labels",
		"--limit", "50")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var raw []PullRequest
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list output: %w", err)
	}
	return filterValidatable(raw), nil
}

// filterValidatable convierte el raw de gh pr list a Candidates para la TUI,
// dejando afuera los PRs que ya tienen validated:approve. Los otros
// validated:* (changes-requested, needs-human) se mantienen visibles: el
// humano podría re-validar después de un push nuevo.
func filterValidatable(raw []PullRequest) []Candidate {
	res := make([]Candidate, 0, len(raw))
	for _, p := range raw {
		if p.HasLabel(labels.ValidatedApprove) {
			continue
		}
		related := make([]int, 0, len(p.ClosingIssuesReferences))
		for _, r := range p.ClosingIssuesReferences {
			related = append(related, r.Number)
		}
		res = append(res, Candidate{
			Number:        p.Number,
			Title:         p.Title,
			URL:           p.URL,
			IsDraft:       p.IsDraft,
			Author:        p.Author.Login,
			RelatedIssues: related,
		})
	}
	return res
}

// ParsePRRef acepta varios formatos de ref y devuelve una forma que gh
// entiende:
//   - "7" → "7"
//   - "owner/repo#7" → "owner/repo#7" (gh lo soporta nativo)
//   - URL completa "https://github.com/owner/repo/pull/7" → tal cual (gh ok)
//
// El único formato que rechazamos es el vacío. Para el resto delegamos la
// validación a `gh pr view` (si el ref es inválido, gh devuelve error y lo
// propagamos con contexto).
func ParsePRRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("pr ref is empty")
	}
	// "7" o número puro → ok.
	if _, err := strconv.Atoi(ref); err == nil {
		return ref, nil
	}
	// URL de GitHub con /pull/<n> → ok.
	if strings.HasPrefix(ref, "https://github.com/") && strings.Contains(ref, "/pull/") {
		return ref, nil
	}
	// owner/repo#N → ok.
	if strings.Contains(ref, "#") {
		parts := strings.Split(ref, "#")
		if len(parts) == 2 && strings.Contains(parts[0], "/") {
			if _, err := strconv.Atoi(parts[1]); err == nil {
				return ref, nil
			}
		}
	}
	return "", fmt.Errorf("unrecognized PR ref %q — accepted: '7', 'owner/repo#7', URL", ref)
}

// ---- prechecks ----

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

// ---- PR fetch ----

// FetchPR corre `gh pr view <ref> --json ...` y parsea. Acepta los mismos
// formatos de ref que gh (número, URL, owner/repo#N).
func FetchPR(ref string) (*PullRequest, error) {
	cmd := exec.Command("gh", "pr", "view", ref,
		"--json", "number,title,url,state,isDraft,author,headRefName,labels")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var pr PullRequest
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return &pr, nil
}

// FetchDiff corre `gh pr diff <ref>` y devuelve el diff como string. No
// deduplica — si gh lo devuelve dos veces (no pasa), lo pasamos tal cual.
func FetchDiff(ref string) (string, error) {
	cmd := exec.Command("gh", "pr", "diff", ref)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr diff: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// FetchPRComments trae los comments del PR (para calcular iter). Usa una
// llamada separada a gh pr view con --json comments. Si el json wrapper tiene
// campos extra los ignoramos.
func FetchPRComments(ref string) ([]PRComment, error) {
	cmd := exec.Command("gh", "pr", "view", ref, "--json", "comments")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr view comments: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var wrap struct {
		Comments []PRComment `json:"comments"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse gh pr view comments: %w", err)
	}
	return wrap.Comments, nil
}

// ---- agent runner ----

// runValidatorsParallel corre todos los validadores en goroutines separadas y
// devuelve el slice alineado con el input. Ninguno cancela a los otros — los
// errores quedan en el validatorResult individual y se reportan como "ERROR"
// en el comment correspondiente.
func runValidatorsParallel(pr *PullRequest, diff string, validators []Validator, progress func(string)) []validatorResult {
	results := make([]validatorResult, len(validators))
	var wg sync.WaitGroup
	for i, v := range validators {
		wg.Add(1)
		go func(i int, v Validator) {
			defer wg.Done()
			label := fmt.Sprintf("%s#%d", v.Agent, v.Instance)
			progress(label + ": consultando…")
			resp, err := callValidator(v, pr, diff, func(line string) {
				progress(label + ": " + line)
			})
			results[i] = validatorResult{Validator: v, Response: resp, Err: err}
		}(i, v)
	}
	wg.Wait()
	return results
}

// callValidator invoca al binario del agente validador con el prompt del PR
// diff y parsea la respuesta.
func callValidator(v Validator, pr *PullRequest, diff string, progress func(string)) (*Response, error) {
	prompt := buildValidatorPrompt(pr, diff)
	out, err := runAgentCmd(v.Agent, prompt, progress, "")
	if err != nil {
		return nil, err
	}
	return parseResponse(out)
}

// runAgentCmd invoca al binario del agente con el prompt construido. Streamea
// stdout/stderr en vivo al progress y aplica AgentTimeout con context.
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseResponse extrae el JSON del stdout del validador, tolerando code fences
// y texto antes/después (mismo algoritmo que explore.parseResponse).
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
		return nil, fmt.Errorf("invalid JSON from validator: %w (raw: %q)", err, truncate(raw, 200))
	}
	if !validVerdicts[r.Verdict] {
		return nil, fmt.Errorf("verdict %q not in [approve changes_requested needs_human]", r.Verdict)
	}
	for i, f := range r.Findings {
		if !validSeverities[f.Severity] {
			return nil, fmt.Errorf("finding %d severity %q not in [blocker major minor]", i, f.Severity)
		}
		if strings.TrimSpace(f.Issue) == "" {
			return nil, fmt.Errorf("finding %d issue is empty", i)
		}
	}
	return &r, nil
}

// ---- prompt builder ----

// buildValidatorPrompt arma el prompt del validador: le da el título del PR
// y el diff, y le pide un JSON estructurado con el verdict y los findings.
func buildValidatorPrompt(pr *PullRequest, diff string) string {
	var sb strings.Builder
	sb.WriteString(`Sos un validador técnico senior. Otro agente implementó cambios en un PR.
Tu tarea es leer el DIFF del PR y marcar lo que falta o está mal — NO reimplementar nada.

Chequeá específicamente:
1. ¿El diff hace lo que el título del PR dice que hace?
2. ¿Hay regresiones obvias (tests rotos, edge cases no cubiertos, lógica incorrecta)?
3. ¿Hay code smells o problemas de diseño que merezcan escalarse?
4. ¿Los tests cubren los cambios o hay gaps importantes?
5. ¿Hay problemas de seguridad (input validation, auth, injection)?

IMPORTANTE — Clasificación de findings:

- kind="product": decisión de producto/dominio irreducible (política, UX opinada, alcance).
  Puede ir con needs_human=true si requiere decisión del dueño del producto.
- kind="technical": gap técnico (bug, falta manejo de error, test faltante, code smell).
  needs_human=false — es feedback para el implementador, no para el humano.
- kind="documented": el implementador ignoró algo que está en el body del PR o en el código.
  needs_human=false — es bug del ejecutor, se resuelve leyendo.

Valores válidos:
- verdict: "approve" (diff correcto y suficiente), "changes_requested" (hay cosas técnicas a corregir), "needs_human" (hay ambigüedad de producto irreducible)
- severity: "blocker" | "major" | "minor"
- area: "code" | "tests" | "docs" | "security" | "other"
- kind: "product" | "technical" | "documented"

Reglas:
- Si el diff está bien, verdict=approve y findings=[].
- needs_human=true requiere kind=product Y que la respuesta dependa de decisión del dueño.
  Cualquier otro caso debe ir con needs_human=false.
- No inventes gaps — si el diff cubre un caso aunque sea brevemente, no lo marques como faltante.

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "verdict": "approve|changes_requested|needs_human",
  "summary": "Tu opinión global en 1-2 oraciones",
  "findings": [
    {
      "severity": "major",
      "area": "code",
      "where": "path/file.go:line o función",
      "issue": "qué está mal",
      "suggestion": "cómo arreglarlo",
      "needs_human": false,
      "kind": "technical"
    }
  ]
}

DIFF del PR #`)
	sb.WriteString(fmt.Sprint(pr.Number))
	sb.WriteString(` (título: "`)
	sb.WriteString(pr.Title)
	sb.WriteString(`"):
<<<
`)
	sb.WriteString(diff)
	sb.WriteString("\n>>>\n")
	return sb.String()
}

// ---- comment rendering ----

// renderValidatorComment genera el markdown del comment individual de un
// validador. El header HTML (invisible) permite a runs posteriores detectar
// las iteraciones previas; el título visible incluye la marca "che · validate"
// para que humanos vean el origen sin abrir el HTML.
func renderValidatorComment(r validatorResult, iter int) string {
	v := r.Validator
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=validate iter=%d agent=%s instance=%d role=validator -->\n",
		iter, v.Agent, v.Instance))

	if r.Err != nil || r.Response == nil {
		sb.WriteString(fmt.Sprintf("## [che · validate · %s#%d · iter:%d · ERROR]\n\n", v.Agent, v.Instance, iter))
		if r.Err != nil {
			sb.WriteString("El validador falló antes de producir un análisis:\n\n```\n")
			sb.WriteString(r.Err.Error())
			sb.WriteString("\n```\n")
		}
		return sb.String()
	}

	resp := r.Response
	sb.WriteString(fmt.Sprintf("## [che · validate · %s#%d · iter:%d · %s]\n\n",
		v.Agent, v.Instance, iter, resp.Verdict))
	sb.WriteString("**Resumen:** " + resp.Summary + "\n\n")

	if len(resp.Findings) == 0 {
		sb.WriteString("_Sin findings._\n")
		return sb.String()
	}

	sb.WriteString("### Findings\n")
	for _, f := range resp.Findings {
		marker := "-"
		if f.NeedsHuman && (f.Kind == "" || f.Kind == "product") {
			marker = "- 🧑"
		}
		kindTag := ""
		if f.Kind != "" && f.Kind != "product" {
			kindTag = " · " + f.Kind
		}
		sb.WriteString(fmt.Sprintf("%s **[%s · %s%s]** %s", marker, f.Severity, f.Area, kindTag, f.Issue))
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

// renderSummaryComment genera el comment resumen final: tabla con agente →
// verdict → findings count. Se postea después de todos los validadores para
// dar un vistazo rápido sin abrir cada comment individual.
func renderSummaryComment(results []validatorResult, iter int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=validate iter=%d role=summary -->\n", iter))
	sb.WriteString(fmt.Sprintf("## 🤖 [che · validate · resumen iter:%d]\n\n", iter))
	sb.WriteString("| Validador | Verdict | Findings |\n")
	sb.WriteString("|---|---|---|\n")
	for _, r := range results {
		label := fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance)
		if r.Err != nil || r.Response == nil {
			sb.WriteString(fmt.Sprintf("| %s | ERROR | — |\n", label))
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %d |\n", label, r.Response.Verdict, len(r.Response.Findings)))
	}
	return sb.String()
}

// renderReport genera el texto que se imprime en stdout después de postear
// todos los comments. Análogo al "Validation report" de explore, pero en
// formato compacto para validate (un line por validador con verdict + count).
func renderReport(results []validatorResult) string {
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
		sb.WriteString(fmt.Sprintf("  %s %s: %s · %d findings\n",
			mark, label, r.Response.Verdict, len(r.Response.Findings)))
	}
	return sb.String()
}

// ---- PR comment posting ----

// postPRComment postea un comment en el PR via `gh pr comment <ref> --body-file`.
// Usa un archivo temporal para que bodies largos (con el diff embebido o
// findings extensos) no se trunquen por límites de argv.
func postPRComment(prRef, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-validate-prc-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "pr", "comment", prRef, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
