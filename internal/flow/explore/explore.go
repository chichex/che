// Package explore implements flow 02 — tomar un issue ya creado por `che
// idea`, leerlo, profundizar con el agente elegido, y persistir el análisis
// (comentario en el issue + plan consolidado escrito en el body +
// transición de label a che:plan). La validación automática vive en un
// flow separado (`che validate`): explore NO dispara validadores ni
// implementa loop humano.
package explore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chichex/che/internal/agent"
	"github.com/chichex/che/internal/comments"
	"github.com/chichex/che/internal/flow/idea"
	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/output"
	"github.com/chichex/che/internal/pipelinelabels"
	"github.com/chichex/che/internal/plan"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh/git falla)
	ExitSemantic ExitCode = 3 // ref vacío, issue sin ct:plan, cerrado, ya explorado, agente inválido
)

// Agent es un alias del enum centralizado en internal/agent. Se re-exporta
// para que cmd/ y la TUI sigan escribiendo `explore.Agent` como antes.
type Agent = agent.Agent

// Re-exports del enum y helpers: cmd/explore.go y la TUI los consumen.
const (
	AgentOpus   = agent.AgentOpus
	AgentCodex  = agent.AgentCodex
	AgentGemini = agent.AgentGemini
)

// DefaultAgent es el ejecutor por defecto si el caller no elige uno.
const DefaultAgent = agent.DefaultAgent

// ValidAgents lista los agentes soportados en orden canónico.
var ValidAgents = agent.ValidAgents

// ParseAgent delega en internal/agent.
func ParseAgent(s string) (Agent, error) { return agent.ParseAgent(s) }

// AgentTimeout es el tiempo máximo que esperamos a que un agente responda
// antes de cancelarlo. Configurable vía env CHE_AGENT_TIMEOUT_SECS. Default
// 60 minutos: holgado para un call a claude/codex/gemini sin dejar flows
// colgados para siempre.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Minute
}()

// runAgentCmd es el adapter local sobre agent.Run que preserva el contrato
// histórico: streamea stdout+stderr al progress con el prefijo dado, aplica
// AgentTimeout, y compone los mismos mensajes de error que la implementación
// previa. El stdout completo (sin transformar) vuelve para que parseResponse
// pueda extraer el JSON.
func runAgentCmd(a Agent, prompt string, progress func(string), progressPrefix string) (string, error) {
	res, err := agent.Run(a, prompt, agent.RunOpts{
		Timeout: AgentTimeout,
		Format:  agent.OutputText,
		OnLine: func(line string) {
			if progress != nil {
				progress(progressPrefix + line)
			}
		},
		OnStderrLine: func(line string) {
			if progress != nil {
				progress(progressPrefix + "stderr: " + line)
			}
		},
	})
	if errors.Is(err, agent.ErrTimeout) {
		return res.Stdout, fmt.Errorf("%s timed out after %s (stderr: %s)",
			a, AgentTimeout, truncate(strings.TrimSpace(res.Stderr), 200))
	}
	var ee *agent.ExitError
	if errors.As(err, &ee) {
		return res.Stdout, fmt.Errorf("exit %d: %s", ee.ExitCode, ee.Stderr)
	}
	if err != nil {
		return res.Stdout, err
	}
	return res.Stdout, nil
}

// Opts agrupa el writer de stdout (payload: "Explored ...", "Comment: ...",
// "Done.") y el logger estructurado (progress + errors). Si Out es nil el
// flow corre silencioso. Si Agent es "", se usa DefaultAgent.
//
// NO hay campo Validators: explore deja de disparar validadores, ese rol
// vive ahora en `che validate` con target plan.
type Opts struct {
	Stdout io.Writer
	Out    *output.Logger
	Agent  Agent
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

// CommentHeader es la metadata parseada del HTML comment que usamos como
// marcador al inicio de cada comment posteado por che. Si Role es "", no es
// un comment de che.
type CommentHeader struct {
	Flow     string
	Iter     int
	Agent    Agent
	Instance int
	Role     string // "executor"
}

// ParseCommentHeader lee la primera línea del body y, si es un HTML comment
// de che, devuelve la metadata. Si no lo es, devuelve un CommentHeader vacío.
func ParseCommentHeader(body string) CommentHeader {
	h := comments.Parse(body)
	if h == (comments.Header{}) {
		return CommentHeader{}
	}
	return CommentHeader{
		Flow:     h.Flow,
		Iter:     h.Iter,
		Agent:    Agent(h.Agent),
		Instance: h.Instance,
		Role:     h.Role,
	}
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

// Response es lo que el agente devuelve después de analizar el issue. Ahora
// incluye ConsolidatedPlan como parte del mismo output: el agente entrega el
// análisis (paths/risks/questions/assumptions para discusión) y el plan
// consolidado que se escribe al body, en una sola llamada. Esto evita una
// segunda invocación solo para consolidar.
//
// Assumptions son decisiones técnicas que el ejecutor tomó por su cuenta
// (aspecto de API, orden de refactor, helper naming, trade-offs que no
// requieren voto humano). Las listamos en el comment del executor para que
// el dueño tenga visibilidad sin interrumpirle.
type Response struct {
	Summary          string                 `json:"summary"`
	Questions        []Question             `json:"questions"`
	Assumptions      []Assumption           `json:"assumptions,omitempty"`
	Risks            []Risk                 `json:"risks"`
	Paths            []Path                 `json:"paths"`
	NextStep         string                 `json:"next_step"`
	ConsolidatedPlan *plan.ConsolidatedPlan `json:"consolidated_plan"`
}

// Question es una pregunta abierta del ejecutor.
//
// Kind clasifica la pregunta para distinguir ambigüedad real de producto
// vs decisiones de ingeniería que el ejecutor debió tomar:
//   - "product": ambigüedad de dominio/producto irreducible.
//   - "technical": decisión de ingeniería (debería ser assumption).
//   - "documented": la respuesta está en el código/body.
//
// El validador (en `che validate`, no acá) usa Kind para dar feedback
// estructurado. Si Kind está vacío asumimos "product" (compat).
type Question struct {
	Q       string `json:"q"`
	Why     string `json:"why"`
	Blocker bool   `json:"blocker"`
	Kind    string `json:"kind,omitempty"`
}

// Assumption es una decisión técnica que el ejecutor tomó sin consultar al
// humano. Se renderiza en el comment como rastro de decisión.
type Assumption struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// Risk es un type alias a plan.Risk para que Response.Risks y el resto del
// código sigan compilando tras la consolidación del shape en el paquete
// internal/plan.
type Risk = plan.Risk

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

// Valores válidos de Kind para Question. "product" es el caso normal; los
// otros dos los usa el validador downstream para marcar bugs del ejecutor.
const (
	KindProduct    = "product"
	KindTechnical  = "technical"
	KindDocumented = "documented"
)

// validKinds es la allowlist de Kind explícitos.
var validKinds = map[string]bool{
	KindProduct:    true,
	KindTechnical:  true,
	KindDocumented: true,
}

// normalizeKind baja a lowercase y si el valor no está en la allowlist lo
// trata como vacío (→ default "product" al aplicar kindOrDefault).
func normalizeKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if validKinds[k] {
		return k
	}
	return ""
}

// kindOrDefault devuelve el Kind efectivo, aplicando el default "product"
// cuando el campo viene vacío.
func kindOrDefault(k string) string {
	if n := normalizeKind(k); n != "" {
		return n
	}
	return KindProduct
}

// QuestionKind expone el Kind efectivo de una Question — default "product"
// para compat con fixtures/modelos que no emiten el campo.
func (q Question) QuestionKind() string { return kindOrDefault(q.Kind) }

// ConsolidatedPlan es un type alias al shape canónico en internal/plan.
type ConsolidatedPlan = plan.ConsolidatedPlan

// Run ejecuta el flow sin ramificaciones: analiza el issue, postea un
// comment con el análisis, escribe el plan consolidado en el body y
// transiciona che:idea → che:planning (lock) → che:plan (éxito) ó
// che:idea (rollback).
func Run(issueRef string, opts Opts) ExitCode {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	log := opts.Out
	if log == nil {
		log = output.New(nil)
	}
	stderr := log.AsWriter(output.LevelError)
	progress := func(s string) { log.Step(s) }

	issueRef = strings.TrimSpace(issueRef)
	if issueRef == "" {
		log.Error("issue ref is empty")
		return ExitSemantic
	}

	log.Info("chequeando repo git y auth de GitHub")
	if err := precheckGitHubRemote(); err != nil {
		log.Error("github remote invalido", output.F{Cause: err})
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		log.Error("gh auth fallo", output.F{Cause: err})
		return ExitRetry
	}

	log.Info("obteniendo issue desde GitHub")
	issue, err := fetchIssue(issueRef)
	if err != nil {
		log.Error("fetching issue failed", output.F{Cause: err})
		return ExitRetry
	}

	// Issues creados a mano (no por `che idea`) no tienen ct:plan. Antes de
	// gatear, intentamos clasificarlos e inyectarles los labels. Saltamos
	// el reclasificador si el issue está cerrado — el gate lo va a rechazar
	// igual y no queremos gastar tokens en algo que no va a avanzar.
	if issue.State == "OPEN" && !issue.HasLabel(labels.CtPlan) {
		if err := reclassifyIssue(issueRef, issue, log); err != nil {
			log.Error("reclasificación del issue falló", output.F{Issue: issue.Number, Cause: err})
			// Un LLM que alucina enums (o devuelve JSON roto) es irremediable:
			// reintentar con la misma entrada va a alucinar de nuevo. Lo
			// diferenciamos de un fallo de invocación (red, binario) que sí es
			// retryable. idea.ErrInvalidResponse es el sentinel.
			if errors.Is(err, idea.ErrInvalidResponse) {
				return ExitSemantic
			}
			return ExitRetry
		}
	}

	if err := gateBasic(issue); err != nil {
		log.Error("gate failed", output.F{Issue: issue.Number, Cause: err})
		return ExitSemantic
	}

	progress("aplicando lock che:locked…")
	if err := labels.Lock(issueRef); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	defer func() {
		if err := labels.Unlock(issueRef); err != nil {
			fmt.Fprintf(stderr, "warning: no se pudo quitar che:locked de %s: %v — corré `che unlock %s`\n", issueRef, err, issueRef)
		}
	}()

	// Transición idea → applying:explore (lock de máquina de estados). El
	// rollback (applying:explore → idea) lo hace el defer si succeeded queda
	// en false.
	progress("transicionando label a " + pipelinelabels.StateApplyingExplore + "…")
	if err := labels.Apply(issueRef, pipelinelabels.StateIdea, pipelinelabels.StateApplyingExplore); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	var succeeded bool
	defer func() {
		if succeeded {
			return
		}
		if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExplore, pipelinelabels.StateIdea); err != nil {
			fmt.Fprintf(stderr, "warning: rollback %s → %s fallo: %v — revisá labels a mano\n",
				pipelinelabels.StateApplyingExplore, pipelinelabels.StateIdea, err)
		}
	}()

	a := opts.Agent
	if a == "" {
		a = DefaultAgent
	}
	if a.Binary() == "" {
		fmt.Fprintf(stderr, "error: unknown agent %q\n", a)
		return ExitSemantic
	}

	progress("consultando a " + string(a) + "…")
	resp, err := callAgent(a, issue, progress)
	if err != nil {
		fmt.Fprintf(stderr, "error: calling %s: %v\n", a, err)
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		fmt.Fprintf(stderr, "error: %s response: %v\n", a, err)
		return ExitSemantic
	}

	progress("posteando comentario con el análisis…")
	comment := renderComment(resp, a, 1)
	commentURL, err := postComment(issueRef, comment)
	if err != nil {
		fmt.Fprintf(stderr, "error: posting comment: %v\n", err)
		return ExitRetry
	}

	progress("escribiendo plan consolidado al body del issue…")
	newBody := plan.Render(resp.ConsolidatedPlan, issue.Body)
	if err := editIssueBody(issueRef, newBody); err != nil {
		fmt.Fprintf(stderr, "error: updating body: %v\n", err)
		return ExitRetry
	}

	progress("transicionando label a " + pipelinelabels.StateExplore + "…")
	if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExplore, pipelinelabels.StateExplore); err != nil {
		fmt.Fprintf(stderr, "error: editing labels: %v\n", err)
		fmt.Fprintf(stderr, "warning: comentario posteado (%s) pero label no cambió; corré de nuevo o editá a mano\n", commentURL)
		return ExitRetry
	}
	succeeded = true

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

// Candidate es un issue candidato a explorar. Cubre dos buckets que la TUI
// separa visualmente:
//   - ideas de che (ct:plan sin che:planning/plan/executing/executed/...): Raw=false.
//   - issues "crudos" abiertos a mano sin ningún ct:*: Raw=true. explore
//     los reclassifica antes de explorar.
type Candidate struct {
	Number int
	Title  string
	// Raw es true para issues sin ningún label ct:* — los muestra la TUI
	// bajo la sección "Issues sin clasificar".
	Raw bool
}

// ListCandidates devuelve los issues abiertos que la TUI muestra como
// "ideas sin explorar". Cubre dos buckets:
//  1. issues creados por che idea (ct:plan) que todavía no fueron
//     explorados ni avanzaron en el pipeline (sin che:planning, che:plan,
//     che:executing, che:executed, che:validating, che:validated,
//     che:closing, che:closed);
//  2. issues "crudos" abiertos a mano en GitHub — sin ningún label ct:* —
//     que explore reclassifica antes de explorar.
//
// Excluye los bloqueados por otro flow (che:locked). Limita a 50.
func ListCandidates() ([]Candidate, error) {
	raw, err := listOpenIssues()
	if err != nil {
		return nil, err
	}
	return filterCandidates(raw), nil
}

// filterCandidates es la lógica pura detrás de ListCandidates — separada
// para poder testearla sin shell-out a gh. Ver doc de ListCandidates.
//
// El orden de salida es estable: primero las ideas de che (Raw=false) y
// después los issues crudos (Raw=true). La TUI lo aprovecha para inyectar
// un separador "Issues sin clasificar" antes del primer Raw.
func filterCandidates(issues []Issue) []Candidate {
	cheIdeas := make([]Candidate, 0, len(issues))
	raw := make([]Candidate, 0, len(issues))
	for _, i := range issues {
		if i.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		if i.HasLabel(labels.CtPlan) {
			if i.HasLabel(pipelinelabels.StateApplyingExplore) ||
				i.HasLabel(pipelinelabels.StateExplore) ||
				i.HasLabel(pipelinelabels.StateApplyingExecute) ||
				i.HasLabel(pipelinelabels.StateExecute) ||
				i.HasLabel(pipelinelabels.StateApplyingValidatePR) ||
				i.HasLabel(pipelinelabels.StateValidatePR) ||
				i.HasLabel(pipelinelabels.StateApplyingClose) ||
				i.HasLabel(pipelinelabels.StateClose) {
				continue
			}
			cheIdeas = append(cheIdeas, Candidate{Number: i.Number, Title: i.Title})
			continue
		}
		// Sin ct:plan: solo aceptamos issues "crudos". Un ct:* distinto
		// de ct:plan indicaría otra familia del pipeline (hoy no existe
		// ninguno; el check queda como guard para cuando se agreguen).
		if hasCtLabel(i.Labels) {
			continue
		}
		raw = append(raw, Candidate{Number: i.Number, Title: i.Title, Raw: true})
	}
	return append(cheIdeas, raw...)
}

// hasCtLabel reporta si el issue tiene algún label con prefijo ct:*.
func hasCtLabel(lbls []Label) bool {
	for _, l := range lbls {
		if strings.HasPrefix(l.Name, "ct:") {
			return true
		}
	}
	return false
}

// listOpenIssues trae todos los issues open del repo — sin filtro de
// label, para poder surface tanto las ideas de che como las abiertas a
// mano. El filtrado queda del lado de filterCandidates.
func listOpenIssues() ([]Issue, error) {
	cmd := exec.Command("gh", "issue", "list",
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

// reclassifyIssue clasifica un issue que no fue creado por `che idea` (no
// tiene ct:plan) delegando en idea.Classify, y le aplica los labels de
// pipeline (ct:plan + che:idea + type/size inferidos). Si el issue ya
// tenía type:* o size:* manualmente, los preserva y solo agrega los que
// falten — respetamos lo que el humano decidió a mano.
//
// Todos los labels se agregan en una sola llamada a `gh issue edit` para
// que GitHub los aplique de forma atómica: si falla, el issue queda como
// estaba (sin labels parciales). Actualiza issue.Labels in-place para que
// el resto del flow vea los labels nuevos sin re-fetchear.
func reclassifyIssue(ref string, issue *Issue, log *output.Logger) error {
	log.Info("issue sin ct:plan; clasificando con el LLM antes de explorar",
		output.F{Issue: issue.Number})

	text := strings.TrimSpace(issue.Title)
	if body := strings.TrimSpace(issue.Body); body != "" {
		if text != "" {
			text += "\n\n"
		}
		text += body
	}

	resp, err := idea.Classify(text, log)
	if err != nil {
		return fmt.Errorf("clasificación: %w", err)
	}
	if len(resp.Items) == 0 {
		return fmt.Errorf("clasificación: el LLM no devolvió items")
	}
	it := resp.Items[0]

	// Si el issue ya tiene un type:* o size:*, lo respetamos. La
	// clasificación del LLM es un fallback para issues sin nada. Ídem con
	// che:* — si alguien editó labels a mano y dejó el issue con
	// che:plan pero sin ct:plan, no queremos sumar che:idea arriba
	// (GitHub aceptaría ambos y el flow quedaría con dos che:*).
	hasType, hasSize, hasStatus := false, false, false
	for _, l := range issue.Labels {
		if strings.HasPrefix(l.Name, "type:") {
			hasType = true
		}
		if strings.HasPrefix(l.Name, "size:") {
			hasSize = true
		}
		if strings.HasPrefix(l.Name, "che:") && l.Name != labels.CheLocked {
			hasStatus = true
		}
	}

	toAdd := []string{labels.CtPlan}
	if !hasStatus {
		toAdd = append(toAdd, pipelinelabels.StateIdea)
	} else {
		log.Warn("issue con che:* preexistente sin ct:plan; preservando el estado actual",
			output.F{Issue: issue.Number})
	}
	if !hasType {
		toAdd = append(toAdd, "type:"+it.Type)
	}
	if !hasSize {
		toAdd = append(toAdd, "size:"+strings.ToLower(it.Size))
	}

	for _, l := range toAdd {
		log.Step("asegurando label", output.F{Labels: []string{l}})
		if err := labels.Ensure(l); err != nil {
			return fmt.Errorf("ensuring label %s: %w", l, err)
		}
	}

	number, err := labels.RefNumber(ref)
	if err != nil {
		return fmt.Errorf("reclassify %s: %w", ref, err)
	}
	if err := labels.AddLabels(number, toAdd...); err != nil {
		return err
	}

	for _, l := range toAdd {
		issue.Labels = append(issue.Labels, Label{Name: l})
	}
	log.Success("issue clasificado y etiquetado", output.F{
		Issue:  issue.Number,
		Labels: toAdd,
	})
	return nil
}

// gateBasic valida las precondiciones: open + ct:plan + NO está más allá
// de che:idea (planning/plan/executing/executed/validating/validated/
// closing/closed → el issue ya avanzó en el pipeline, explore no aplica).
//
// También rechaza issues con labels del modelo viejo (`che:idea`,
// `che:plan`, …): el flow migrado a v2 escribe `che:state:*` y no sabe
// hacer migración in-place del label viejo, así que dejarlo correr
// produciría un estado mixto (v1 + v2 simultáneos) ilegal en ambas
// máquinas. La migración de repos vivos vive en `migrate-labels-v2`
// (subcomando dedicado, fuera del scope de PR6b).
func gateBasic(i *Issue) error {
	if i.State != "OPEN" {
		return fmt.Errorf("issue #%d is closed", i.Number)
	}
	if !i.HasLabel(labels.CtPlan) {
		return fmt.Errorf("issue #%d is missing label ct:plan (not created by `che idea`?)", i.Number)
	}
	// Detectar labels v1 (modelo viejo) antes de avanzar — si el repo
	// no corrió migrate-labels-v2, mezclar v1+v2 deja al issue en estado
	// inconsistente.
	for _, v1 := range []string{
		labels.CheIdea,
		labels.ChePlanning,
		labels.ChePlan,
		labels.CheExecuting,
		labels.CheExecuted,
		labels.CheValidating,
		labels.CheValidated,
		labels.CheClosing,
		labels.CheClosed,
	} {
		if i.HasLabel(v1) {
			return fmt.Errorf("issue #%d tiene labels v1 (%s); este flow opera sobre el modelo v2 (`che:state:*`). Corré `che migrate-labels-v2` antes de explorar, o ajustá los labels a mano", i.Number, v1)
		}
	}
	for _, beyond := range []string{
		pipelinelabels.StateApplyingExplore,
		pipelinelabels.StateExplore,
		pipelinelabels.StateApplyingExecute,
		pipelinelabels.StateExecute,
		pipelinelabels.StateApplyingValidatePR,
		pipelinelabels.StateValidatePR,
		pipelinelabels.StateApplyingClose,
		pipelinelabels.StateClose,
	} {
		if i.HasLabel(beyond) {
			return fmt.Errorf("issue #%d ya avanzó en el pipeline (%s presente) — explore no aplica", i.Number, beyond)
		}
	}
	if i.HasLabel(labels.CheLocked) {
		return fmt.Errorf("issue #%d tiene che:locked — otro flow lo tiene agarrado, o quedó colgado. Si es lo segundo: `che unlock %d`", i.Number, i.Number)
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

// parseResponse extrae el JSON del output del agente, tolerando code fences
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
		return nil, fmt.Errorf("invalid JSON from agent: %w (raw: %q)", err, truncate(raw, 200))
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// buildPrompt arma el prompt que le pedimos al agente. A diferencia del
// modelo anterior (análisis + segundo call de consolidación), acá el agente
// produce las dos cosas en el mismo JSON: el análisis para el comment del
// issue y el plan consolidado que se escribe al body. La consolidación
// depende solo del issue + assumptions del propio ejecutor, así que no
// necesita un round-trip con validadores.
func buildPrompt(issue *Issue) string {
	return `Sos un ingeniero senior haciendo exploración técnica de un issue antes de comprometerte a un plan de ejecución.

Te voy a pasar un issue de GitHub ya clasificado (type, size, criterios iniciales). Tu tarea tiene dos partes que devolvés EN EL MISMO JSON:

PARTE A — ANÁLISIS (para discusión y revisión futura):
1. Parafrasear el issue para confirmar entendimiento.
2. Tomar decisiones técnicas por tu cuenta y declararlas como assumptions.
3. Listar las preguntas abiertas que SOLO el humano puede contestar (kind=product).
4. Identificar riesgos con likelihood e impact.
5. Proponer al menos 2 caminos de implementación distintos (idealmente 2-4) con pros, cons y effort estimado.
6. Marcar EXACTAMENTE UN camino como recomendado.
7. Indicar el próximo paso concreto.

PARTE B — PLAN CONSOLIDADO (listo para ejecutar):
Produce un plan accionable basado en el camino recomendado. Este plan se escribe directo al body del issue y es lo que va a leer ` + "`che execute`" + ` cuando el issue entre en ejecución. Tiene que ser autocontenido: un ingeniero que lea SOLO ese plan tiene que poder arrancar a implementar.

Reglas del plan consolidado:
- Incorporá TUS assumptions como decisiones firmes (no como "a definir").
- Elegí el approach del path recommended y descartá los demás.
- Si hay ambigüedad de producto irreducible (una question kind=product blocker=true), listala también en questions[] de la Parte A. En el plan consolidado, elegí la opción más conservadora y anotá en risks_to_mitigate que esa decisión está pendiente de confirmación humana.
- No incluyas preguntas abiertas ni ambigüedades EN EL PLAN — ese ruido lo toma ` + "`che validate`" + ` en una etapa aparte si el humano lo pide.
- Sé concreto y accionable: criterios de aceptación observables, pasos numerados que un agente pueda ejecutar.

IMPORTANTE — Clasificación de todo lo que no sabés antes de decidir si preguntar:

- kind="product": ambigüedad IRREDUCIBLE del dominio o del producto. Política del proyecto, trade-off de UX/negocio, alcance opinado. El código y el body del issue no la resuelven. SOLO estas van como "question" en Parte A.
- kind="technical": decisión de ingeniería (API shape, orden de refactor, naming de helpers, trade-off de implementación). NO es una question. Tomá vos la decisión y anotala como "assumption".
- kind="documented": la respuesta ya está en el body del issue, en el código del repo, en design docs, memory, README o criterios de aceptación. LEELA y resolvé; no generes ni question ni assumption.

Regla práctica: si alguien con contexto del proyecto podría contestar con un grep, leyendo el body del issue, o aplicando best practice razonable, NO es una "question". Es una decisión tuya que va como "assumption" (o ni siquiera — no todo vale la pena declarar).

Valores válidos:
- likelihood/impact: low, medium, high
- effort: XS (minutos), S (horas), M (1-2 días), L (1 semana), XL (varias semanas)
- kind: product, technical, documented

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "summary": "Paráfrasis accionable del issue en 1-2 oraciones",
  "questions": [
    {"q": "Pregunta abierta concreta para el humano", "why": "Por qué NO puede responderse con código ni con el body", "blocker": true, "kind": "product"}
  ],
  "assumptions": [
    {"what": "Decisión técnica que tomaste", "why": "Justificación breve (código leído, best practice, precedente del repo)"}
  ],
  "risks": [
    {"risk": "Descripción del riesgo", "likelihood": "medium", "impact": "high", "mitigation": "Cómo evitarlo"}
  ],
  "paths": [
    {"title": "Nombre corto del camino", "sketch": "2-4 oraciones de qué implica técnicamente", "pros": ["Pro 1"], "cons": ["Con 1"], "effort": "M", "recommended": true}
  ],
  "next_step": "Frase accionable de qué tiene que pasar antes de che execute",
  "consolidated_plan": {
    "summary": "1-2 oraciones del qué y para qué",
    "goal": "Qué logramos cuando esto está implementado",
    "acceptance_criteria": ["Criterio 1 observable", "Criterio 2"],
    "approach": "Descripción del approach elegido (basado en el path recommended), 2-4 oraciones",
    "steps": ["Paso 1 concreto", "Paso 2 concreto"],
    "risks_to_mitigate": [
      {"risk": "...", "likelihood": "low|medium|high", "impact": "low|medium|high", "mitigation": "..."}
    ],
    "out_of_scope": ["Cosa que explícitamente NO hacemos ahora"]
  }
}

Reglas:
- questions[] puede estar vacío si no hay ambigüedad de producto real. Un array vacío es preferible a rellenar con preguntas técnicas inventadas.
- assumptions[] idealmente tiene 2-5 items si tomaste decisiones técnicas. Cero assumptions con cero questions es sospechoso.
- Toda question DEBE tener kind="product". Si te sale kind="technical" o "documented", MOVELA a assumptions.
- risks[] tiene al menos 1 item.
- paths[] tiene al menos 2 items. EXACTAMENTE UN path con "recommended": true.
- Cada path debe tener al menos 1 pro y 1 con.
- consolidated_plan DEBE estar presente y completo (summary, goal, acceptance_criteria con >=1 item, approach, steps con >=1 item).
- No inventes archivos o módulos que no aparecen en el issue.

Issue #` + fmt.Sprint(issue.Number) + `:
Título: ` + issue.Title + `
Labels: ` + strings.Join(issue.LabelNames(), ", ") + `

Body del issue:
<<<
` + issue.Body + `
>>>`
}

// validate chequea que la Response cumpla el contrato mínimo. El plan
// consolidado se valida con validateConsolidated.
func validate(r *Response) error {
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("summary is empty")
	}
	if len(r.Risks) == 0 {
		return fmt.Errorf("risks[] is empty")
	}
	for i, q := range r.Questions {
		if strings.TrimSpace(q.Q) == "" {
			return fmt.Errorf("question %d: q is empty", i)
		}
		if k := normalizeKind(q.Kind); k != "" && k != KindProduct {
			// El ejecutor clasificó una question como technical/documented
			// pero la dejó en questions[]. Rechazamos para que el ejecutor
			// se corrija en vez de filtrar silenciosamente.
			return fmt.Errorf("question %d: kind=%q en questions[] — mover a assumptions[] o remover (solo product va acá)", i, q.Kind)
		}
	}
	for i, a := range r.Assumptions {
		if strings.TrimSpace(a.What) == "" {
			return fmt.Errorf("assumption %d: what is empty", i)
		}
	}
	if len(r.Paths) < 2 {
		return fmt.Errorf("paths[] must have at least 2 items, got %d", len(r.Paths))
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
	if r.ConsolidatedPlan == nil {
		return fmt.Errorf("consolidated_plan is missing")
	}
	if err := validateConsolidated(r.ConsolidatedPlan); err != nil {
		return fmt.Errorf("consolidated_plan: %w", err)
	}
	return nil
}

// validateConsolidated asegura que el plan consolidado sea procesable por
// `che execute`: summary/goal/approach no vacíos, al menos un step y un
// criterio de aceptación, enums de riesgos dentro del rango.
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

// renderComment genera el markdown que se postea como comentario en el issue.
// Arranca con un header HTML que identifica al ejecutor y la iteración.
// NO incluye el plan consolidado (ese va al body del issue via editIssueBody)
// para evitar duplicar información en el issue.
func renderComment(r *Response, agent Agent, iter int) string {
	var sb strings.Builder
	sb.WriteString(comments.Header{Flow: "explore", Iter: iter, Agent: string(agent), Role: "executor"}.Format() + "\n")
	sb.WriteString(fmt.Sprintf("## [executor:%s · iter:%d]\n\n", agent, iter))

	sb.WriteString("**Resumen:** ")
	sb.WriteString(r.Summary)
	sb.WriteString("\n\n")

	if len(r.Assumptions) > 0 {
		sb.WriteString("### Decisiones técnicas tomadas\n")
		sb.WriteString("_El ejecutor resolvió esto por su cuenta (decisión de ingeniería, no de producto). Si alguna no te cuadra, marcala en un comment y corré `che validate` sobre este issue._\n\n")
		for _, a := range r.Assumptions {
			sb.WriteString(fmt.Sprintf("- **%s** — %s\n", a.What, a.Why))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("### Preguntas abiertas\n")
	if len(r.Questions) == 0 {
		sb.WriteString("_Sin ambigüedades de producto irreducibles; el ejecutor no necesita input humano para arrancar._\n")
	}
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
