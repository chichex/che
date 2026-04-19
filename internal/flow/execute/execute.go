// Package execute implements flow 03 — tomar un issue en status:plan, armar
// un worktree aislado, invocar al agente para producir el diff, abrir/actualizar
// un PR draft contra main y transicionar el issue a status:executed +
// awaiting-human. La lógica vive acá (pura, testeable) para que el subcomando
// `che execute` y la TUI compartan la misma implementación.
//
// NOTA: este paquete es deliberadamente una copia adaptada de
// `internal/flow/explore/` (no reusa su plumbing todavía) — la deuda de
// extraer lo común a `internal/flow/common/` queda para un issue futuro
// cuando execute esté validado end-to-end contra un issue real.
package execute

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

	"github.com/chichex/che/internal/labels"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh/git falla, rollback aplicado)
	ExitSemantic ExitCode = 3 // ref vacío, issue sin ct:plan, ya ejecutándose, agente inválido
)

// Agent identifica qué ejecutor usar. Replica el enum de explore para no
// acoplar los paquetes; cuando se extraiga `internal/flow/common/`, los dos
// enums migran allá.
type Agent string

const (
	AgentOpus   Agent = "opus"
	AgentCodex  Agent = "codex"
	AgentGemini Agent = "gemini"
)

const DefaultAgent = AgentOpus

var ValidAgents = []Agent{AgentOpus, AgentCodex, AgentGemini}

// Binary devuelve el nombre del ejecutable correspondiente al agente.
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

// InvokeArgs devuelve los args de línea de comando para cada CLI en modo
// no-interactivo.
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

// AgentTimeout para llamadas al CLI del agente. execute tiene un default
// mayor que explore porque generar diff + tool use es típicamente más largo
// que devolver un JSON de análisis.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_EXEC_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 15 * time.Minute
}()

// ValidatorsWaitTimeout es cuánto esperamos a que las goroutines de
// validadores terminen antes de retornar de Run. Es el timeout del wait,
// no del agente validador en sí — cada validador individualmente está
// acotado por AgentTimeout. Configurable con CHE_EXEC_VALIDATORS_WAIT_SECS
// (default 600s = 10min).
var ValidatorsWaitTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_EXEC_VALIDATORS_WAIT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 10 * time.Minute
}()

// Opts agrupa los writers, la callback de progreso y el agente ejecutor.
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string)
	Agent      Agent
	// Validators son los agentes que postean findings en el PR después de
	// crearlo. execute NO espera por ellos (fire-and-forget).
	Validators []Validator
	// SkipValidators puede usarse en tests/CI para omitir el disparo de
	// validadores aunque se hayan pasado en Validators.
	SkipValidators bool
}

// Validator es una re-declaración del enum de explore para no acoplar los
// paquetes. Instance permite repetir tipo (codex×2) aunque execute en v1 no
// lo requiera; queda para futuro.
type Validator struct {
	Agent    Agent
	Instance int
}

// ParseValidators parsea "codex,gemini" o "none".
func ParseValidators(s string) ([]Validator, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "none") {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) < 1 || len(parts) > 3 {
		return nil, fmt.Errorf("validators: need 1-3 items (or `none`), got %d", len(parts))
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

// Issue modela el subset del output de `gh issue view --json ...`.
type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	URL    string  `json:"url"`
	State  string  `json:"state"`
	Labels []Label `json:"labels"`
}

type Label struct {
	Name string `json:"name"`
}

func (i *Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Candidate es la vista mínima usada por la TUI al listar issues.
type Candidate struct {
	Number int
	Title  string
}

// ConsolidatedPlan es el subset del body consolidado que execute usa para
// armar el prompt del ejecutor. Mantiene los mismos nombres que
// explore.ConsolidatedPlan pero evita importar explore para no acoplar.
type ConsolidatedPlan struct {
	Summary            string
	Goal               string
	AcceptanceCriteria []string
	Approach           string
	Steps              []string
	OutOfScope         []string
}

// Run ejecuta el flow completo sobre un issue. Decisiones claves:
//   - Preflight: repo git + gh auth + gh pr list (scope check).
//   - Transition status:plan → status:executing (lock).
//   - Crear worktree .worktrees/issue-N sobre branch exec/N-<slug>.
//   - Invocar agente, commit en el worktree, push.
//   - Crear o actualizar PR draft contra main con Closes #<n>.
//   - Fire-and-forget validadores sobre el diff del PR.
//   - Transition status:executing → status:executed + awaiting-human.
//   - Comentario al issue con link al PR.
//   - Rollback: si algo falla después del lock, revertir a status:plan y
//     limpiar worktree.
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

	agent := opts.Agent
	if agent == "" {
		agent = DefaultAgent
	}
	if agent.Binary() == "" {
		fmt.Fprintf(stderr, "error: unknown agent %q\n", agent)
		return ExitSemantic
	}

	progress("chequeando repo git y auth de GitHub…")
	repoRoot, err := repoToplevel()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGitHubRemote(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckPRScopes(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("obteniendo issue desde GitHub…")
	issue, err := fetchIssue(issueRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetching issue: %v\n", err)
		return ExitRetry
	}

	if err := gate(issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitSemantic
	}

	plan, err := parseConsolidatedPlan(issue.Body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitSemantic
	}

	// Transition plan → executing. Desde acá se lockea el issue.
	progress("transicionando issue a status:executing…")
	if err := labels.Apply(issueRef, labels.StatusPlan, labels.StatusExecuting); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	// Defer de rollback: si algo falla, volver a plan.
	var (
		succeeded bool
		wt        *Worktree
	)
	defer func() {
		if succeeded {
			return
		}
		if wt != nil {
			_ = wt.Cleanup(repoRoot, false)
		}
		// Rollback ownership-aware: re-fetch el issue y verificar que
		// status:executing siga siendo nuestro lock antes de revertirlo.
		// Si otra instancia ya transitó, pisaríamos su estado.
		current, fetchErr := fetchIssue(issueRef)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "warning: rollback no aplicado: no se pudo re-fetch el issue (%v) — revisá labels a mano\n", fetchErr)
			return
		}
		if !current.HasLabel(labels.StatusExecuting) {
			fmt.Fprintln(stderr, "rollback abortado: el issue ya no está en status:executing (owner=otro)")
			return
		}
		if err := labels.Apply(issueRef, labels.StatusExecuting, labels.StatusPlan); err != nil {
			fmt.Fprintf(stderr, "warning: rollback failed: %v — revisá labels del issue a mano\n", err)
		}
	}()

	slug := Slugify(issue.Title)
	progress(fmt.Sprintf("creando worktree .worktrees/issue-%d (branch exec/%d-%s)…", issue.Number, issue.Number, slug))
	wt, err = CreateWorktree(WorktreeOpts{
		RepoRoot: repoRoot,
		IssueNum: issue.Number,
		Slug:     slug,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	// Antes de invocar al agente, chequeamos si ya hay un PR abierto para
	// esta branch — si lo hay, estamos en modo "re-ejecutar" (idempotente)
	// y vamos a actualizarlo después de pushear los nuevos commits.
	existingPR, err := findOpenPRForBranch(wt.Branch)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress(fmt.Sprintf("invocando a %s en el worktree…", agent))
	if err := runAgent(agent, wt.Path, buildPrompt(issue, plan), progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("chequeando cambios en el worktree…")
	hasChanges, err := worktreeHasChanges(wt.Path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if !hasChanges && existingPR == "" {
		fmt.Fprintf(stderr, "error: agente terminó sin cambios en el worktree y no hay PR existente — nada que commitear\n")
		return ExitRetry
	}

	if hasChanges {
		progress("armando commit en el worktree…")
		if err := commitAll(wt.Path, fmt.Sprintf("feat(#%d): %s\n\nCloses #%d", issue.Number, issue.Title, issue.Number)); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}

		progress("pusheando branch " + wt.Branch + "…")
		if err := pushBranch(wt.Path, wt.Branch); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
	}

	var prURL string
	if existingPR != "" {
		progress("actualizando PR existente " + existingPR + "…")
		prURL = existingPR
	} else {
		progress("creando PR draft contra main…")
		prURL, err = createDraftPR(wt.Path, wt.Branch, issue)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
	}

	// Disparar validadores y esperar con timeout acotado. Originalmente esto
	// era fire-and-forget, pero cmd/execute.go hace os.Exit(code) apenas Run
	// retorna — eso mataba las goroutines antes de que postearan comments.
	// Ahora esperamos hasta ValidatorsWaitTimeout (env configurable); si
	// expira, logeamos y retornamos igual (los validadores que queden siguen
	// en background pero el proceso se va a cortar).
	var validatorsDone <-chan int
	validatorsTotal := 0
	if !opts.SkipValidators && len(opts.Validators) > 0 {
		progress(fmt.Sprintf("disparando %d validador(es) sobre el PR…", len(opts.Validators)))
		validatorsDone = fireValidators(prURL, issue, plan, opts.Validators)
		validatorsTotal = len(opts.Validators)
	}

	progress("transicionando issue a status:executed + awaiting-human…")
	if err := labels.Apply(issueRef, labels.StatusExecuting, labels.StatusExecuted); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("posteando comment en el issue con link al PR…")
	if err := commentIssue(issueRef, renderIssueComment(prURL, opts.Validators)); err != nil {
		// No es fatal — ya hicimos todo el trabajo. Lo logueamos y seguimos.
		fmt.Fprintf(stderr, "warning: no se pudo comentar el issue: %v\n", err)
	}

	succeeded = true

	// Esperar a los validadores (si los hay) antes de retornar, para que el
	// os.Exit del caller no los mate. Feedback incremental a stdout.
	if validatorsDone != nil && validatorsTotal > 0 {
		waitValidators(stdout, validatorsDone, validatorsTotal, ValidatorsWaitTimeout)
	}

	fmt.Fprintf(stdout, "Executed %s\n", issue.URL)
	fmt.Fprintf(stdout, "PR: %s\n", prURL)
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

// waitValidators lee del canal una señal por validador que terminó y emite
// progreso "esperando validadores (k/total)…" hasta total o hasta timeout.
// Si expira el timeout, imprime cuántos quedaron y retorna — los que sigan
// corriendo van a morir cuando el proceso termine.
func waitValidators(stdout io.Writer, done <-chan int, total int, timeout time.Duration) {
	completed := 0
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for completed < total {
		select {
		case _, ok := <-done:
			if !ok {
				// Canal cerrado antes de tiempo (no debería pasar, pero defendernos).
				return
			}
			completed++
			fmt.Fprintf(stdout, "esperando validadores (%d/%d)…\n", completed, total)
		case <-timer.C:
			fmt.Fprintf(stdout, "timeout: %d/%d validadores completaron, el resto sigue corriendo en background\n", completed, total)
			return
		}
	}
}

// ListCandidates devuelve los issues abiertos con ct:plan + status:plan que
// no tienen awaiting-human. Estos son los candidatos a ejecutar.
func ListCandidates() ([]Candidate, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--label", "ct:plan",
		"--label", "status:plan",
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
		if i.HasLabel(labels.StatusAwaitingHuman) {
			continue
		}
		out2 = append(out2, Candidate{Number: i.Number, Title: i.Title})
	}
	return out2, nil
}

// ---- prechecks ----

func repoToplevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git repo: not inside a git repository")
	}
	return strings.TrimSpace(string(out)), nil
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

// precheckPRScopes verifica que el token de gh tenga permisos para listar
// PRs (proxy razonable para "puede crear PRs"). Un fallo acá es accionable:
// el usuario sabe que tiene que `gh auth refresh -s repo`.
func precheckPRScopes() error {
	cmd := exec.Command("gh", "pr", "list", "--limit", "1", "--json", "number")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr list (scope check): %s — probá `gh auth refresh -s repo`", strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- issue fetch / gate ----

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

// gate valida las precondiciones para ejecutar:
//   - Issue OPEN.
//   - Tiene label ct:plan.
//   - Tiene label status:plan.
//   - NO tiene label status:awaiting-human (hay algo humano sin resolver).
//   - NO tiene label status:executing (hay otro run en curso o quedó colgado).
func gate(i *Issue) error {
	if i.State != "OPEN" {
		return fmt.Errorf("issue #%d is closed", i.Number)
	}
	if !i.HasLabel(labels.CtPlan) {
		return fmt.Errorf("issue #%d is missing label ct:plan (not created by `che idea`?)", i.Number)
	}
	if i.HasLabel(labels.StatusExecuting) {
		return fmt.Errorf("issue #%d is already status:executing — otro run en curso o quedó colgado; quitá el label a mano si es lo segundo", i.Number)
	}
	if i.HasLabel(labels.StatusAwaitingHuman) {
		return fmt.Errorf("issue #%d tiene status:awaiting-human — resolvé primero lo que falta antes de ejecutar", i.Number)
	}
	if !i.HasLabel(labels.StatusPlan) {
		return fmt.Errorf("issue #%d is not status:plan — corré `che explore %d` primero", i.Number, i.Number)
	}
	return nil
}

// ---- parseo del plan consolidado ----

// parseConsolidatedPlan extrae las secciones del body consolidado que escribe
// `che explore`. Es tolerante: si la sección "Plan consolidado" no existe,
// arma un plan con summary=body y todas las demás secciones vacías — el
// agente puede trabajar con eso aunque sea menos guiado.
func parseConsolidatedPlan(body string) (*ConsolidatedPlan, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("issue body is empty")
	}

	// Si no hay header de plan consolidado, devolvemos fallback.
	if !strings.Contains(body, "## Plan consolidado") {
		return &ConsolidatedPlan{Summary: body}, nil
	}

	// Extrae cada sección buscando headers conocidos.
	p := &ConsolidatedPlan{}
	if v := extractSection(body, "## Plan consolidado"); v != "" {
		// La primera línea suele ser "**Resumen:** ..."
		if idx := strings.Index(v, "**Resumen:**"); idx >= 0 {
			rest := v[idx+len("**Resumen:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Summary = strings.TrimSpace(rest[:nl])
			} else {
				p.Summary = strings.TrimSpace(rest)
			}
		}
		if idx := strings.Index(v, "**Goal:**"); idx >= 0 {
			rest := v[idx+len("**Goal:**"):]
			if nl := strings.Index(rest, "\n\n"); nl >= 0 {
				p.Goal = strings.TrimSpace(rest[:nl])
			} else {
				p.Goal = strings.TrimSpace(rest)
			}
		}
	}
	if v := extractSection(body, "### Criterios de aceptación"); v != "" {
		p.AcceptanceCriteria = parseChecklist(v)
	}
	if v := extractSection(body, "### Approach"); v != "" {
		p.Approach = strings.TrimSpace(v)
	}
	if v := extractSection(body, "### Pasos"); v != "" {
		p.Steps = parseNumbered(v)
	}
	if v := extractSection(body, "### Fuera de alcance"); v != "" {
		p.OutOfScope = parseBullets(v)
	}

	if p.Summary == "" && p.Goal == "" && len(p.Steps) == 0 {
		return nil, fmt.Errorf("issue body has a '## Plan consolidado' header but no parseable content — revisá que `che explore` haya terminado bien")
	}

	return p, nil
}

// extractSection devuelve el texto entre un header (ej. "## X") y el próximo
// header de nivel <= al del header dado. Devuelve "" si no encuentra.
func extractSection(body, header string) string {
	idx := strings.Index(body, header)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(header):]
	// Busca el próximo "#..." a principio de línea.
	lines := strings.Split(rest, "\n")
	var out []string
	// Determinar el nivel del header (# count).
	level := 0
	for _, c := range header {
		if c == '#' {
			level++
		} else {
			break
		}
	}
	for i, line := range lines {
		if i == 0 {
			out = append(out, line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			// Contar nivel.
			n := 0
			for _, c := range trimmed {
				if c == '#' {
					n++
				} else {
					break
				}
			}
			if n > 0 && n <= level {
				break
			}
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// parseChecklist extrae items de un bloque "- [ ] foo\n- [ ] bar".
var checklistRe = regexp.MustCompile(`^\s*-\s*\[.\]\s*(.+)$`)

func parseChecklist(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := checklistRe.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// parseNumbered extrae items de "1. foo\n2. bar".
var numberedRe = regexp.MustCompile(`^\s*\d+\.\s+(.+)$`)

func parseNumbered(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := numberedRe.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

// parseBullets extrae items de "- foo\n- bar".
var bulletRe = regexp.MustCompile(`^\s*-\s+(.+)$`)

func parseBullets(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			text := strings.TrimSpace(m[1])
			// Saltarse checklist items que ya matchean el otro regex.
			if !strings.HasPrefix(text, "[") {
				out = append(out, text)
			}
		}
	}
	return out
}

// ---- prompt builder ----

func buildPrompt(issue *Issue, plan *ConsolidatedPlan) string {
	var sb strings.Builder
	sb.WriteString(`Sos un ingeniero senior ejecutando un plan ya consolidado. Tenés acceso al filesystem del worktree actual — tu tarea es implementar el plan editando archivos directamente.

Reglas:
1. Trabajá SOLO dentro del scope del plan. No toques cosas que estén explícitamente fuera de alcance.
2. Si el plan dice "crear archivo X", creá X. Si dice "modificar Y", modificá Y.
3. Usá tus herramientas de edición de archivos (Read/Write/Edit) para aplicar los cambios.
4. No commitees — eso lo hace el harness después. Tu único trabajo es dejar el worktree con los cambios listos.
5. Si al final hay cosas del plan que no pudiste hacer (falta info, dependencia bloqueada), dejá un archivo `)
	sb.WriteString("`EXEC_NOTES.md`")
	sb.WriteString(` con lo que quedó pendiente — esa info va al PR body.

Issue #`)
	sb.WriteString(fmt.Sprint(issue.Number))
	sb.WriteString(`:
Título: ` + issue.Title + `

`)
	if plan.Summary != "" {
		sb.WriteString("## Resumen del plan\n" + plan.Summary + "\n\n")
	}
	if plan.Goal != "" {
		sb.WriteString("## Goal\n" + plan.Goal + "\n\n")
	}
	if len(plan.AcceptanceCriteria) > 0 {
		sb.WriteString("## Criterios de aceptación\n")
		for _, c := range plan.AcceptanceCriteria {
			sb.WriteString("- " + c + "\n")
		}
		sb.WriteString("\n")
	}
	if plan.Approach != "" {
		sb.WriteString("## Approach\n" + plan.Approach + "\n\n")
	}
	if len(plan.Steps) > 0 {
		sb.WriteString("## Pasos\n")
		for i, s := range plan.Steps {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
		}
		sb.WriteString("\n")
	}
	if len(plan.OutOfScope) > 0 {
		sb.WriteString("## Fuera de alcance (NO toques esto)\n")
		for _, s := range plan.OutOfScope {
			sb.WriteString("- " + s + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Arrancá. Cuando termines, el harness va a chequear el diff y crear el PR.\n")
	return sb.String()
}

// ---- agente runner ----

// runAgent invoca al CLI del agente con el prompt construido, corriendo con
// cwd en el worktree para que cualquier herramienta de file edit afecte ese
// directorio. Usa StdoutPipe + streaming igual que explore.
func runAgent(agent Agent, cwd, prompt string, progress func(string)) error {
	ctx, cancel := context.WithTimeout(context.Background(), AgentTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agent.Binary(), agent.InvokeArgs(prompt)...)
	cmd.Dir = cwd
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", agent.Binary(), err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var stderrBuf strings.Builder
	go streamPipe(&wg, stdoutPipe, nil, progress, string(agent)+": ")
	go streamPipe(&wg, stderrPipe, &stderrBuf, progress, string(agent)+" stderr: ")
	wg.Wait()

	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %s", agent, AgentTimeout)
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return fmt.Errorf("%s exit %d: %s", agent, ee.ExitCode(), strings.TrimSpace(stderrBuf.String()))
		}
		return waitErr
	}
	return nil
}

// streamPipe lee un pipe y reenvía las líneas al progress con un prefix.
func streamPipe(wg *sync.WaitGroup, r io.Reader, acc *strings.Builder, progress func(string), prefix string) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if acc != nil {
			acc.WriteString(line + "\n")
		}
		if strings.TrimSpace(line) != "" && progress != nil {
			progress(prefix + line)
		}
	}
}

// ---- git ops sobre el worktree ----

// worktreeHasChanges devuelve true si `git status --porcelain` devuelve
// líneas (es decir, hay archivos modificados/nuevos).
func worktreeHasChanges(wtPath string) (bool, error) {
	cmd := exec.Command("git", "-C", wtPath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return false, fmt.Errorf("git status: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// commitAll hace `git add -A && git commit -F <tmp>` en el worktree.
func commitAll(wtPath, message string) error {
	if err := runGitIn(wtPath, "add", "-A"); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "che-exec-commit-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	msgFile := filepath.Join(tmpDir, "msg.txt")
	if err := os.WriteFile(msgFile, []byte(message), 0o644); err != nil {
		return err
	}
	if err := runGitIn(wtPath, "commit", "-F", msgFile); err != nil {
		return err
	}
	return nil
}

// pushBranch empuja la branch a origin. Usa --force-with-lease para
// re-ejecuciones idempotentes (caso de actualizar un PR existente) sin
// pisar cambios ajenos.
func pushBranch(wtPath, branch string) error {
	if err := runGitIn(wtPath, "push", "--force-with-lease", "--set-upstream", "origin", branch); err != nil {
		return err
	}
	return nil
}

func runGitIn(dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- PR ops ----

// findOpenPRForBranch busca un PR abierto cuyo head-branch sea el dado.
// Devuelve la URL si lo encuentra, "" si no. Cualquier error de gh (red,
// permisos) se propaga.
func findOpenPRForBranch(branch string) (string, error) {
	cmd := exec.Command("gh", "pr", "list", "--head", branch, "--state", "open", "--json", "url,number")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	var prs []struct {
		URL    string `json:"url"`
		Number int    `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return "", fmt.Errorf("parse gh pr list: %w", err)
	}
	if len(prs) == 0 {
		return "", nil
	}
	return prs[0].URL, nil
}

// createDraftPR crea un PR draft contra main con Closes #<n> en el body.
func createDraftPR(wtPath, branch string, issue *Issue) (string, error) {
	tmpDir, err := os.MkdirTemp("", "che-exec-pr-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	body := fmt.Sprintf("Implementa el plan consolidado de #%d.\n\nCloses #%d\n", issue.Number, issue.Number)
	bodyFile := filepath.Join(tmpDir, "pr-body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return "", err
	}

	title := fmt.Sprintf("feat(#%d): %s", issue.Number, issue.Title)

	cmd := exec.Command("gh", "pr", "create",
		"--draft",
		"--base", "main",
		"--head", branch,
		"--title", title,
		"--body-file", bodyFile)
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr create: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ---- validadores (fire-and-forget) ----

// fireValidators lanza una goroutine por validator que invoca al agente con
// el prompt de validación de diff y postea el resultado como PR comment.
// Devuelve un canal que recibe una señal (el índice del validator) cada vez
// que una goroutine termina. El caller puede leer hasta len(validators)
// señales y aplicar su propio timeout. El canal tiene buffer = len(validators)
// así las goroutines nunca se bloquean si el caller deja de drenarlo.
func fireValidators(prURL string, issue *Issue, plan *ConsolidatedPlan, validators []Validator) <-chan int {
	done := make(chan int, len(validators))
	for i, v := range validators {
		i, v := i, v
		go func() {
			defer func() {
				// Non-blocking send: si el canal está lleno (no debería con
				// buffer = len(validators)), no nos colgamos.
				select {
				case done <- i:
				default:
				}
			}()
			prompt := buildValidatorPrompt(issue, plan)
			ctx, cancel := context.WithTimeout(context.Background(), AgentTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, v.Agent.Binary(), v.Agent.InvokeArgs(prompt)...)
			out, _ := cmd.Output()
			body := fmt.Sprintf("## Validator %s#%d\n\n%s\n", v.Agent, v.Instance, strings.TrimSpace(string(out)))
			_ = postPRComment(prURL, body)
		}()
	}
	return done
}

func buildValidatorPrompt(issue *Issue, plan *ConsolidatedPlan) string {
	var sb strings.Builder
	sb.WriteString("Sos un validador técnico senior. Un agente acaba de implementar un plan y abrió un PR draft. Tu tarea es revisarlo y marcar problemas — NO reimplementar nada.\n\n")
	sb.WriteString("Chequeá específicamente:\n")
	sb.WriteString("1. ¿El diff cubre los criterios de aceptación del plan?\n")
	sb.WriteString("2. ¿Hay regresiones obvias, tests faltantes, o quebró builds?\n")
	sb.WriteString("3. ¿Se metió con cosas fuera del scope del plan?\n\n")
	sb.WriteString(fmt.Sprintf("Issue #%d — %s\n\n", issue.Number, issue.Title))
	if plan.Summary != "" {
		sb.WriteString("## Resumen del plan\n" + plan.Summary + "\n\n")
	}
	if len(plan.AcceptanceCriteria) > 0 {
		sb.WriteString("## Criterios de aceptación\n")
		for _, c := range plan.AcceptanceCriteria {
			sb.WriteString("- " + c + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Podés ver el diff del PR corriendo `gh pr diff " + issue.URL + "` — revisalo y devolvé un resumen en markdown con hallazgos numerados.\n")
	return sb.String()
}

// postPRComment postea un comment en el PR via `gh pr comment <url>`.
func postPRComment(prURL, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-exec-prc-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "pr", "comment", prURL, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- issue comment al final ----

func renderIssueComment(prURL string, validators []Validator) string {
	var sb strings.Builder
	sb.WriteString("<!-- claude-cli: flow=execute role=pr-link -->\n")
	sb.WriteString("## Ejecución completada\n\n")
	sb.WriteString("Se abrió un PR draft con los cambios:\n\n")
	sb.WriteString("- PR: " + prURL + "\n\n")
	if len(validators) > 0 {
		sb.WriteString("Los siguientes validadores están corriendo sobre el diff:\n")
		for _, v := range validators {
			sb.WriteString(fmt.Sprintf("- %s#%d\n", v.Agent, v.Instance))
		}
		sb.WriteString("\nSus findings van a aparecer como comments del PR (no de este issue).\n\n")
	}
	sb.WriteString("El issue quedó en `status:executed` + `status:awaiting-human` — revisá el PR + CI y mergealo cuando esté listo.\n")
	return sb.String()
}

func commentIssue(ref, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-exec-ic-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "issue", "comment", ref, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
