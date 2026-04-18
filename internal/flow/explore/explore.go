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
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh/git falla)
	ExitSemantic ExitCode = 3 // ref vacío, issue sin ct:plan, cerrado, ya explorado, claude inválido
)

// Opts agrupa los writers y la callback de progreso. Si OnProgress es nil,
// el flow corre silencioso (modo CI).
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string)
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
)

// Run ejecuta el flow completo.
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

	if err := gateIssue(issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitSemantic
	}

	progress("consultando a claude…")
	resp, err := callClaude(issue, progress)
	if err != nil {
		fmt.Fprintf(stderr, "error: calling claude: %v\n", err)
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		fmt.Fprintf(stderr, "error: claude response: %v\n", err)
		return ExitSemantic
	}

	progress("posteando comentario con el análisis…")
	comment := renderComment(resp)
	commentURL, err := postComment(issueRef, comment)
	if err != nil {
		fmt.Fprintf(stderr, "error: posting comment: %v\n", err)
		return ExitRetry
	}

	progress("asegurando label status:planned…")
	if err := ensureLabel("status:planned", progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		fmt.Fprintf(stderr, "warning: comentario posteado (%s) pero label no se pudo crear/actualizar; volvé a correr con --no-comment\n", commentURL)
		return ExitRetry
	}

	progress("transicionando label a status:planned…")
	if err := transitionLabels(issueRef); err != nil {
		fmt.Fprintf(stderr, "error: editing labels: %v\n", err)
		fmt.Fprintf(stderr, "warning: comentario posteado (%s) pero label no cambió; corré de nuevo o editá a mano\n", commentURL)
		return ExitRetry
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
// no fueron explorados (status:planned ausente). Limita a los 50 más
// recientes para mantener la TUI manejable.
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
		if i.HasLabel("status:planned") {
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
	if i.HasLabel("status:planned") {
		return fmt.Errorf("issue #%d was already explored (status:planned present)", i.Number)
	}
	return nil
}

func callClaude(issue *Issue, progress func(string)) (*Response, error) {
	prompt := buildPrompt(issue)
	cmd := exec.Command("claude", "-p", prompt, "--output-format", "text")
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
			progress("claude: " + line)
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
func renderComment(r *Response) string {
	var sb strings.Builder
	sb.WriteString("## Exploración (che explore)\n\n")

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

// transitionLabels saca `status:idea` y agrega `status:planned`. NO toca
// `ct:plan` (queda como marcador de "fue creado por che idea") ni aplica
// `ct:exec` (eso lo hace `che execute` al arrancar).
func transitionLabels(ref string) error {
	cmd := exec.Command("gh", "issue", "edit", ref,
		"--remove-label", "status:idea",
		"--add-label", "status:planned")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
