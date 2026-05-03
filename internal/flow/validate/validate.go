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
	"encoding/json"
	"errors"
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

	"github.com/chichex/che/internal/agent"
	"github.com/chichex/che/internal/flow/runguard"
	"github.com/chichex/che/internal/flow/stateref"
	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/lock"
	"github.com/chichex/che/internal/output"
	"github.com/chichex/che/internal/pipelinelabels"
	planpkg "github.com/chichex/che/internal/plan"
)

// rejectV1Labels devuelve un error accionable si la lista contiene algún
// label v1 del modelo viejo. Wirea ValidateNoMixedLabels para detectar
// mezclas v1+v2 (caso intermedio: alguien aplicó che:state:* a mano sobre
// un issue v1) y, si todo es v1-only, devuelve un error apuntando a
// migrate-labels-v2.
//
// REMOVE IN PR6d junto con `labels.V1LegacyStates`/`ValidateNoMixedLabels`.
func rejectV1Labels(kind string, number int, current []string) error {
	if err := labels.ValidateNoMixedLabels(current); err != nil {
		return fmt.Errorf("%s #%d: %w", kind, number, err)
	}
	for _, v1 := range labels.V1LegacyStates() {
		for _, l := range current {
			if l == v1 {
				return fmt.Errorf("%s #%d tiene labels v1 (%s); este flow opera sobre el modelo v2 (`che:state:*`). Corré `che migrate-labels-v2` antes de validar, o ajustá los labels a mano", kind, number, v1)
			}
		}
	}
	return nil
}

// Target discrimina el tipo de ref que recibió `che validate`. La detección
// corre contra `gh api repos/{owner}/{repo}/issues/{n}`: ese endpoint devuelve
// el mismo shape para issues y PRs, con la diferencia de que los PRs incluyen
// un campo `pull_request` no-null. Ambos targets comparten el runner de
// validadores en paralelo, la consolidación de verdict y el render de
// comments; lo que cambia es qué se valida (diff vs plan del body), dónde
// se postea el comment (pr vs issue) y qué label se aplica (validated:* vs
// plan-validated:*).
type Target int

const (
	// TargetUnknown es el valor cero; no debería retornarlo detectTarget
	// salvo que haya un bug.
	TargetUnknown Target = iota
	// TargetPlan indica que el ref apunta a un issue (no-PR) y el modo
	// plan del flow aplica: valida el body consolidado del issue.
	TargetPlan
	// TargetPR indica que el ref apunta a un pull request; modo PR
	// (comportamiento histórico: valida el diff).
	TargetPR
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh falla)
	ExitSemantic ExitCode = 3 // ref vacío, PR cerrado, validators inválidos
)

// Agent es un alias del enum centralizado en internal/agent. Re-exportado
// para que cmd/validate.go y la TUI sigan escribiendo `validate.Agent`.
type Agent = agent.Agent

const (
	AgentOpus   = agent.AgentOpus
	AgentCodex  = agent.AgentCodex
	AgentGemini = agent.AgentGemini
)

// DefaultValidators es el panel por defecto cuando el caller no pasa uno
// explícito. Coherente con explore v0.0.23: opus como validador base.
const DefaultValidators = "opus"

// ValidAgents lista los agentes soportados (orden preservado para UI).
var ValidAgents = agent.ValidAgents

// ParseAgent delega en internal/agent.
func ParseAgent(s string) (Agent, error) { return agent.ParseAgent(s) }

// AgentTimeout es el tiempo máximo de espera para cada validador individual.
// Configurable con CHE_VALIDATE_AGENT_TIMEOUT_SECS. Default 60min: el diff de
// un PR + review profunda puede tardar más que un explore.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_VALIDATE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Minute
}()

// Validator es un alias del struct centralizado.
type Validator = agent.Validator

// ParseValidators parsea la flag `--validators` (ej: "opus", "codex,gemini",
// "codex,codex,gemini"). validate requiere al menos 1 validador — a diferencia
// de execute, aca "none" no tiene sentido (el flow completo se reduce a nada),
// así que lo rechazamos explícitamente con allowNone=false.
func ParseValidators(s string) ([]Validator, error) {
	return agent.ParseValidators(s, false /* allowNone */)
}

// Opts agrupa el writer de stdout (payload: reporte final) y el logger
// estructurado (progress + errors), más la lista de validadores.
type Opts struct {
	Stdout     io.Writer
	Out        *output.Logger
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

// ClosingIssueNumbers proyecta pr.ClosingIssuesReferences al slice de
// números en el orden que vino de la API. Vacío si el PR no cierra issues.
// Usado por stateref.Resolve para decidir dónde viven los labels che:*.
func (p *PullRequest) ClosingIssueNumbers() []int {
	if p == nil {
		return nil
	}
	out := make([]int, 0, len(p.ClosingIssuesReferences))
	for _, r := range p.ClosingIssuesReferences {
		out = append(out, r.Number)
	}
	return out
}

// PRLabelNames proyecta pr.Labels al slice de nombres. Fallback para
// stateref.Resolve cuando el PR no tiene issue linkeado.
func (p *PullRequest) PRLabelNames() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		out = append(out, l.Name)
	}
	return out
}

// ResolveStateRef devuelve el ref donde viven los labels che:* de estado
// para este PR. Preferencia: primer closing issue con che:* → issueRef;
// fallback: el PR mismo (compat con PRs ajenos). Thin wrapper sobre
// stateref.Resolve para el caller típico que ya tiene el *PullRequest.
func (p *PullRequest) ResolveStateRef(prRef string) stateref.Resolution {
	return stateref.Resolve(prRef, p.PRLabelNames(), p.ClosingIssueNumbers())
}

// Candidate es la vista mínima para la TUI al listar PRs abiertos.
type Candidate struct {
	Number        int
	Title         string
	URL           string
	IsDraft       bool
	Author        string
	RelatedIssues []int // issues referenciados via "Closes #N" / "Fixes #N" en el body del PR
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

// Run ejecuta el flow despachando por tipo de ref. Detecta si <ref> es un
// issue (modo plan) o un PR (modo PR actual) usando `gh api` y delega. La
// API pública del paquete no cambia — cmd/validate.go sigue llamando a
// validate.Run(ref, opts) sin saber qué target tocó.
//
// Preflight común (gh auth + remote) vive en el despachador: es idéntico en
// ambos modos y no queremos repetirlo en runPR/runPlan. El preflight también
// corre ANTES de detectTarget para que errores de entorno (no auth, no
// remote github) no disparen una llamada a `gh api` que igual va a fallar —
// así el usuario ve el error accionable primero.
func Run(ref string, opts Opts) ExitCode {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	log := opts.Out
	if log == nil {
		log = output.New(nil)
	}

	ref = strings.TrimSpace(ref)
	if ref == "" {
		log.Error("ref is empty")
		return ExitSemantic
	}
	if len(opts.Validators) == 0 {
		log.Error("at least 1 validator is required")
		return ExitSemantic
	}

	log.Info("chequeando auth de GitHub")
	if err := precheckGitHubRemote(); err != nil {
		log.Error("github remote invalido", output.F{Cause: err})
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		log.Error("gh auth fallo", output.F{Cause: err})
		return ExitRetry
	}

	log.Step("detectando si el ref es un issue o un PR")
	target, err := detectTarget(ref)
	if err != nil {
		// resolveRefNumber falla solo si el formato del ref es irreparable
		// (no número, no URL con /pull/N o /issues/N, no owner/repo#N). Eso
		// es input inválido del usuario → ExitSemantic. El resto de los
		// fallos vienen de `gh api` (red o 404) y son potencialmente
		// remediables → ExitRetry. Nota: tras el fix de ParseRef, la rama
		// semantic es prácticamente inalcanzable desde cmd/validate porque
		// ya validamos antes; queda como defensa en profundidad para otros
		// callers (TUI, futuros) que invoquen Run sin pre-validar.
		if errors.Is(err, errInvalidRef) {
			log.Error("ref invalido", output.F{Cause: err})
			return ExitSemantic
		}
		log.Error("no se pudo determinar si es issue o PR", output.F{Cause: err})
		return ExitRetry
	}
	switch target {
	case TargetPR:
		return runPR(ref, opts, stdout, log)
	case TargetPlan:
		return runPlan(ref, opts, stdout, log)
	default:
		log.Error(fmt.Sprintf("tipo de ref desconocido: %v", target))
		return ExitRetry
	}
}

// DetectTarget decide si un ref apunta a un issue (TargetPlan) o a un PR
// (TargetPR). Expuesto para que otros flows (ej. iterate) reusen exactamente
// la misma detección (mismo endpoint `gh api` + manejo de errores) sin
// duplicar lógica.
//
// Errores: devuelve un error que matchea `errors.Is(err, ErrInvalidRef)`
// cuando el ref tiene formato irreparable (no número, no URL con /pull/ o
// /issues/, no owner/repo#N). El resto de los errores vienen de `gh api`
// (red, 404) — el caller los trata como remediables.
func DetectTarget(ref string) (Target, error) {
	return detectTarget(ref)
}

// ErrInvalidRef es el error que DetectTarget devuelve cuando el ref tiene
// formato irreparable. Expuesto para que los callers distingan "input del
// usuario malformado" (ExitSemantic) de "gh api falló" (ExitRetry) vía
// errors.Is.
var ErrInvalidRef = errInvalidRef

// runPR es el flow histórico: valida el diff de un PR abierto, postea
// comments en el PR y aplica validated:*. El preflight (auth + remote)
// ya corrió en Run.
//
// Gates: PR open, NO che:locked. La transición de máquina de estados
// (che:executed → che:validating → che:validated, con rollback) corre
// sobre el issue linkeado al PR (via closingIssuesReferences). Execute
// escribe los che:* sobre el issue, no sobre el PR — si leyéramos del PR
// nunca veríamos `che:executed` y la transición se saltearía silenciosa.
// Si el PR no tiene issue linkeado (PR ajeno, no creado por `che execute`)
// caemos al PR, preservando compat.
//
// Los labels validated:* (verdict) y che:locked (lock del recurso) se
// siguen aplicando sobre el PR: el verdict es relevante al diff, el lock
// al recurso tocado.
func runPR(prRef string, opts Opts, stdout io.Writer, log *output.Logger) ExitCode {
	log.Info("obteniendo PR desde GitHub")
	pr, err := FetchPR(prRef)
	if err != nil {
		log.Error("fetching PR failed", output.F{Cause: err})
		return ExitRetry
	}
	if pr.State != "OPEN" {
		log.Error(fmt.Sprintf("PR #%d is not OPEN (state=%s)", pr.Number, pr.State))
		return ExitSemantic
	}
	if pr.HasLabel(labels.CheLocked) {
		log.Error(fmt.Sprintf("PR #%d tiene che:locked — otro flow lo tiene agarrado, o quedó colgado. Si es lo segundo: `che unlock %d`", pr.Number, pr.Number))
		return ExitSemantic
	}

	// Rechazar labels v1: si el PR tiene `che:executed` viejo (modelo v1)
	// el flow no puede transicionar (no hay key v1+v2 cruzada en
	// validTransitions, además aplicar v2 dejaría mixto). Mensaje accionable.
	if err := rejectV1Labels("PR", pr.Number, pr.PRLabelNames()); err != nil {
		log.Error("gate v1 falló", output.F{PR: pr.Number, Cause: err})
		return ExitSemantic
	}

	log.Step("aplicando lock che:locked", output.F{PR: pr.Number})
	if err := labels.Lock(prRef); err != nil {
		log.Error("no pude aplicar che:locked", output.F{Cause: err})
		return ExitRetry
	}
	defer func() {
		if err := labels.Unlock(prRef); err != nil {
			log.Warn(fmt.Sprintf("no se pudo quitar che:locked de %s: %v — corré `che unlock %s`", prRef, err, prRef))
		}
	}()

	// Resolver dónde viven los labels che:* de máquina de estados. Si el
	// PR tiene issue linkeado con algún che:*, las transiciones van al
	// issue; si no, al PR mismo (compat).
	stateRes := pr.ResolveStateRef(prRef)
	stateRef := stateRes.Ref

	// Lock con heartbeat + TTL (PRD §6.d) — opt-in. Aplicado al stateRef
	// (issue raíz si linkeado, PR si no) — alineado con donde van las
	// transiciones.
	heartbeat := runguard.AcquireLock(stateRef, "validate-pr", log)
	defer runguard.ReleaseLock(heartbeat, log)
	if heartbeat == nil && lock.HeartbeatEnabled() {
		return ExitSemantic
	}
	auditTarget := pr.Number
	if stateRes.ResolvedToIssue {
		auditTarget = stateRes.IssueNumber
	}

	// Si la resolución cayó al issue, repetimos el guard v1 sobre los
	// labels del issue — `che migrate-labels-v2` puede haber dejado
	// algún issue mezclado/sin migrar. Si fall back al PR ya validamos
	// arriba con pr.PRLabelNames().
	if stateRes.ResolvedToIssue {
		if err := rejectV1Labels("issue", stateRes.IssueNumber, stateRes.Labels); err != nil {
			log.Error("gate v1 falló", output.F{Issue: stateRes.IssueNumber, Cause: err})
			return ExitSemantic
		}
	}

	// Transición de máquina de estados: che:state:execute → che:state:applying:validate_pr.
	// Si el target no tiene che:state:execute (humano corrió validate sobre un
	// PR no creado por execute, o execute no terminó OK) saltamos la
	// transición — solo aplicamos los labels validated:* al final.
	hasExecutedState := stateRes.HasLabel(pipelinelabels.StateExecute)
	var stateValidated bool
	if hasExecutedState {
		if stateRes.ResolvedToIssue {
			log.Step("transicionando issue a "+pipelinelabels.StateApplyingValidatePR, output.F{Issue: stateRes.IssueNumber})
		} else {
			log.Step("transicionando a "+pipelinelabels.StateApplyingValidatePR, output.F{PR: pr.Number})
		}
		if err := labels.Apply(stateRef, pipelinelabels.StateExecute, pipelinelabels.StateApplyingValidatePR); err != nil {
			log.Error("no pude transicionar a "+pipelinelabels.StateApplyingValidatePR, output.F{Cause: err})
			return ExitRetry
		}
		runguard.AuditAppend(auditTarget, "validate-pr", pipelinelabels.StateExecute, pipelinelabels.StateApplyingValidatePR, "", log)
		defer func() {
			if stateValidated {
				return
			}
			if err := labels.Apply(stateRef, pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExecute); err != nil {
				log.Warn(fmt.Sprintf("rollback %s → %s fallo: %v — revisá labels a mano", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExecute, err))
				return
			}
			runguard.AuditAppend(auditTarget, "validate-pr", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExecute, "rollback", log)
		}()
	}

	log.Step("descargando diff del PR", output.F{PR: pr.Number})
	diff, err := FetchDiff(prRef)
	if err != nil {
		log.Error("fetching diff failed", output.F{Cause: err})
		return ExitRetry
	}
	if strings.TrimSpace(diff) == "" {
		log.Error(fmt.Sprintf("diff del PR #%d está vacío — ¿está mergeado o no tiene cambios?", pr.Number))
		return ExitSemantic
	}

	log.Step("leyendo comments previos para calcular iter")
	comments, err := FetchPRComments(prRef)
	if err != nil {
		log.Error("fetching comments failed", output.F{Cause: err})
		return ExitRetry
	}
	iter := DetermineIter(comments)

	log.Info(fmt.Sprintf("corriendo %d validador(es) en paralelo", len(opts.Validators)), output.F{Iter: iter, PR: pr.Number})
	prompt := buildPRValidatorPrompt(pr, diff)
	results := runValidatorsParallel(prompt, opts.Validators, log)

	log.Step("posteando comments de validadores")
	for _, r := range results {
		body := renderValidatorComment(r, iter)
		if err := postPRComment(prRef, body); err != nil {
			log.Error(fmt.Sprintf("posting %s#%d comment failed", r.Validator.Agent, r.Validator.Instance),
				output.F{Validator: fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance), Cause: err})
			return ExitRetry
		}
		verdict := ""
		if r.Response != nil {
			verdict = r.Response.Verdict
		}
		log.Success("comment posteado",
			output.F{Validator: fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance), Verdict: verdict})
	}

	log.Step("posteando comment resumen")
	summary := renderSummaryComment(results, iter)
	if err := postPRComment(prRef, summary); err != nil {
		log.Error("posting summary comment failed", output.F{Cause: err})
		return ExitRetry
	}

	verdict := consolidateVerdict(results)
	if verdict != "" {
		target := verdictToLabel(verdict)
		log.Step("aplicando label al PR", output.F{Labels: []string{target}, PR: pr.Number})
		if err := applyValidatedLabel(prRef, pr, target); err != nil {
			log.Warn("warning: no pude aplicar label al PR",
				output.F{Labels: []string{target}, PR: pr.Number, Cause: err})
		} else {
			log.Success("verdict consolidado",
				output.F{PR: pr.Number, Verdict: verdict, Labels: []string{target}})
		}
	}

	// Cierre de la transición de máquina de estados: che:state:applying:validate_pr →
	// che:state:validate_pr. Solo aplica si arrancamos con che:state:execute. El
	// target es el mismo que usamos para abrir la transición (issue si
	// había closing issue con che:*, PR si no).
	switch {
	case hasExecutedState:
		if stateRes.ResolvedToIssue {
			log.Step("transicionando issue a "+pipelinelabels.StateValidatePR, output.F{Issue: stateRes.IssueNumber})
		} else {
			log.Step("transicionando a "+pipelinelabels.StateValidatePR, output.F{PR: pr.Number})
		}
		if err := labels.Apply(stateRef, pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateValidatePR); err != nil {
			log.Warn(fmt.Sprintf("no pude transicionar a %s: %v — revisá labels a mano", pipelinelabels.StateValidatePR, err))
		} else {
			stateValidated = true
			runguard.AuditAppend(auditTarget, "validate-pr", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateValidatePR, "", log)
		}
	case verdict != "" && !stateRes.ResolvedToIssue:
		// Adopt mode: validate es la puerta de entrada al state machine
		// para PRs sin che:* previo (v0.0.79 / commit 881c964). No hubo
		// transición desde che:state:execute, pero el flow cerró OK con
		// verdict, así que aplicamos che:state:validate_pr directo al PR para que
		// el dash lo mueva fuera de la columna de adopt.
		log.Step("adopt mode: aplicando "+pipelinelabels.StateValidatePR+" al PR", output.F{PR: pr.Number})
		if err := labels.Ensure(pipelinelabels.StateValidatePR); err != nil {
			log.Warn(fmt.Sprintf("no pude crear label %s en el repo: %v — revisá labels a mano", pipelinelabels.StateValidatePR, err))
		} else if err := labels.AddLabels(pr.Number, pipelinelabels.StateValidatePR); err != nil {
			log.Warn(fmt.Sprintf("no pude aplicar %s al PR: %v — revisá labels a mano", pipelinelabels.StateValidatePR, err))
		}
	case verdict != "" && stateRes.ResolvedToIssue:
		// PR linkeado a un issue con che:* pero NO en che:state:execute (ej.
		// che:state:idea / che:state:explore). No saltamos estados de la máquina por
		// nuestra cuenta: dejamos el verdict aplicado y warnea para que
		// el humano resuelva.
		log.Warn(fmt.Sprintf("issue #%d linkeado no estaba en %s; aplique %s manualmente si corresponde", stateRes.IssueNumber, pipelinelabels.StateExecute, pipelinelabels.StateValidatePR))
	}

	fmt.Fprintln(stdout, renderReport(results))
	fmt.Fprintf(stdout, "che ya dejó los comments en el PR %s\n", pr.URL)
	return ExitOK
}

// runPlan es el flow nuevo: valida el plan consolidado del body de un issue
// en che:plan, postea comments en el issue y aplica plan-validated:*. El
// preflight (auth + remote) ya corrió en Run.
//
// Gates:
//   - issue abierto
//   - issue tiene che:plan (corré `che explore` si no)
//   - body del issue tiene un plan consolidado parseable (no basta
//     HasConsolidatedHeader: Parse puede devolver ambigüedad irrecuperable
//     o sin sub-secciones; acá rechazamos con mensaje accionable).
//
// Transición: che:plan → che:validating → che:validated (con rollback si
// algo falla post-lock).
func runPlan(issueRef string, opts Opts, stdout io.Writer, log *output.Logger) ExitCode {
	log.Info("obteniendo issue desde GitHub")
	issue, err := FetchIssue(issueRef)
	if err != nil {
		log.Error("fetching issue failed", output.F{Cause: err})
		return ExitRetry
	}
	if issue.State != "OPEN" {
		log.Error(fmt.Sprintf("issue #%d is not OPEN (state=%s)", issue.Number, issue.State))
		return ExitSemantic
	}
	// Rechazar v1 antes de chequear la presencia del label v2 — el mensaje
	// es más útil ("corré migrate-labels-v2") que "no está en che:state:explore".
	if err := rejectV1Labels("issue", issue.Number, issue.LabelNames()); err != nil {
		log.Error("gate v1 falló", output.F{Issue: issue.Number, Cause: err})
		return ExitSemantic
	}
	if !issue.HasLabel(pipelinelabels.StateExplore) {
		log.Error(fmt.Sprintf("issue #%d no está en %s — corré `che explore %d` primero", issue.Number, pipelinelabels.StateExplore, issue.Number))
		return ExitSemantic
	}
	if issue.HasLabel(labels.CheLocked) {
		log.Error(fmt.Sprintf("issue #%d tiene che:locked — otro flow lo tiene agarrado, o quedó colgado. Si es lo segundo: `che unlock %d`", issue.Number, issue.Number))
		return ExitSemantic
	}

	log.Step("aplicando lock che:locked", output.F{Issue: issue.Number})
	if err := labels.Lock(issueRef); err != nil {
		log.Error("no pude aplicar che:locked", output.F{Cause: err})
		return ExitRetry
	}
	defer func() {
		if err := labels.Unlock(issueRef); err != nil {
			log.Warn(fmt.Sprintf("no se pudo quitar che:locked de %s: %v — corré `che unlock %s`", issueRef, err, issueRef))
		}
	}()

	// Lock con heartbeat + TTL (PRD §6.d) — opt-in.
	heartbeat := runguard.AcquireLock(issueRef, "validate-plan", log)
	defer runguard.ReleaseLock(heartbeat, log)
	if heartbeat == nil && lock.HeartbeatEnabled() {
		return ExitSemantic
	}

	// Transición: che:state:explore → che:state:applying:validate_pr. El defer revierte
	// si succeded queda en false al final (rollback a che:state:explore). LIFO: el
	// unlock corre después del rollback, garantizando que el lock cubre toda la ventana.
	log.Step("transicionando a "+pipelinelabels.StateApplyingValidatePR, output.F{Issue: issue.Number})
	if err := labels.Apply(issueRef, pipelinelabels.StateExplore, pipelinelabels.StateApplyingValidatePR); err != nil {
		log.Error("no pude transicionar a "+pipelinelabels.StateApplyingValidatePR, output.F{Cause: err})
		return ExitRetry
	}
	runguard.AuditAppend(issue.Number, "validate-plan", pipelinelabels.StateExplore, pipelinelabels.StateApplyingValidatePR, "", log)
	var stateValidated bool
	defer func() {
		if stateValidated {
			return
		}
		if err := labels.Apply(issueRef, pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExplore); err != nil {
			log.Warn(fmt.Sprintf("rollback %s → %s fallo: %v — revisá labels a mano", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExplore, err))
			return
		}
		runguard.AuditAppend(issue.Number, "validate-plan", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateExplore, "rollback", log)
	}()

	// Parseamos el plan consolidado del body. Dos modos de fallo semantic:
	//   - ErrAmbiguousPlan: body con múltiples headers "## Plan consolidado"
	//     (typically porque alguien editó a mano o corrieron explore dos
	//     veces). Acción del humano: limpiar el body.
	//   - Plan parseado pero sin header real ni sub-secciones: el issue tiene
	//     che:plan pero nunca se consolidó. Acción: correr explore.
	consolidated, parseErr := planpkg.Parse(issue.Body)
	if parseErr != nil {
		log.Error(fmt.Sprintf("no pude parsear el plan consolidado del issue #%d: %v", issue.Number, parseErr))
		return ExitSemantic
	}
	if !planpkg.HasConsolidatedHeader(issue.Body) {
		log.Error(fmt.Sprintf("issue #%d tiene che:plan pero no tiene plan consolidado en el body — corré `che explore %d`", issue.Number, issue.Number))
		return ExitSemantic
	}

	log.Step("leyendo comments previos para calcular iter")
	comments, err := FetchIssueComments(issueRef)
	if err != nil {
		log.Error("fetching comments failed", output.F{Cause: err})
		return ExitRetry
	}
	iter := DetermineIter(comments)

	log.Info(fmt.Sprintf("corriendo %d validador(es) en paralelo sobre el plan", len(opts.Validators)), output.F{Iter: iter, Issue: issue.Number})
	prompt := buildPlanValidatorPrompt(issue, consolidated)
	results := runValidatorsParallel(prompt, opts.Validators, log)

	log.Step("posteando comments de validadores")
	for _, r := range results {
		body := renderValidatorComment(r, iter)
		if err := postIssueComment(issueRef, body); err != nil {
			log.Error(fmt.Sprintf("posting %s#%d comment failed", r.Validator.Agent, r.Validator.Instance),
				output.F{Validator: fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance), Cause: err})
			return ExitRetry
		}
		verdict := ""
		if r.Response != nil {
			verdict = r.Response.Verdict
		}
		log.Success("comment posteado",
			output.F{Validator: fmt.Sprintf("%s#%d", r.Validator.Agent, r.Validator.Instance), Verdict: verdict})
	}

	log.Step("posteando comment resumen")
	summary := renderSummaryComment(results, iter)
	if err := postIssueComment(issueRef, summary); err != nil {
		log.Error("posting summary comment failed", output.F{Cause: err})
		return ExitRetry
	}

	if verdict := consolidateVerdict(results); verdict != "" {
		target := verdictToPlanLabel(verdict)
		log.Step("aplicando label al issue", output.F{Labels: []string{target}, Issue: issue.Number})
		if err := applyPlanValidatedLabel(issueRef, issue, target); err != nil {
			log.Warn("warning: no pude aplicar label al issue",
				output.F{Labels: []string{target}, Issue: issue.Number, Cause: err})
		} else {
			log.Success("verdict consolidado",
				output.F{Issue: issue.Number, Verdict: verdict, Labels: []string{target}})
		}
	}

	// Cierre de la transición: che:state:applying:validate_pr → che:state:validate_pr.
	log.Step("transicionando a "+pipelinelabels.StateValidatePR, output.F{Issue: issue.Number})
	if err := labels.Apply(issueRef, pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateValidatePR); err != nil {
		log.Warn(fmt.Sprintf("no pude transicionar a %s: %v — revisá labels a mano", pipelinelabels.StateValidatePR, err))
	} else {
		stateValidated = true
		runguard.AuditAppend(issue.Number, "validate-plan", pipelinelabels.StateApplyingValidatePR, pipelinelabels.StateValidatePR, "", log)
	}

	fmt.Fprintln(stdout, renderReport(results))
	fmt.Fprintf(stdout, "che ya dejó los comments en el issue %s\n", issue.URL)
	return ExitOK
}

// errInvalidRef marca errores de detectTarget/resolveRefNumber que vienen
// de un ref con formato irreparable (no número, no URL con /pull/ o
// /issues/, no owner/repo#N). Permite al caller distinguir "input del
// usuario malformado" (ExitSemantic) de "gh api falló" (ExitRetry) sin
// tener que parsear strings.
var errInvalidRef = errors.New("invalid ref")

// detectTarget decide si un ref apunta a un issue o un PR. Consulta el
// endpoint `gh api repos/:owner/:repo/issues/:n`, que devuelve el mismo
// shape para issues y PRs pero con un campo `pull_request` no-null cuando
// es un PR (documentado en la REST API de GitHub). Es el approach más
// confiable: no depende de errores como "not a pull request" que tienen
// wording sensible a idioma o versión de gh.
//
// Para soportar refs que no son números puros (URL, owner/repo#N), usamos
// `gh issue view <ref> --json number` primero para obtener el número
// canónico; eso nos da el número aunque el ref sea "https://.../pull/7"
// (gh normaliza: a un PR lo acepta también como issue en el REST view).
// Si eso falla caemos a detectar si es un número directo; si no, error.
func detectTarget(ref string) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return TargetUnknown, fmt.Errorf("%w: empty ref", errInvalidRef)
	}
	number, err := resolveRefNumber(ref)
	if err != nil {
		return TargetUnknown, err
	}
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return TargetUnknown, fmt.Errorf("gh api issues/%d: %s", number, strings.TrimSpace(string(ee.Stderr)))
		}
		return TargetUnknown, err
	}
	// Parseamos solo los campos que necesitamos. pull_request aparece como
	// objeto cuando el número corresponde a un PR; está ausente o null en
	// issues regulares. json.RawMessage nos deja distinguir null de objeto
	// sin depender del shape interno (que es documentado pero extenso).
	var probe struct {
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return TargetUnknown, fmt.Errorf("parse gh api output: %w", err)
	}
	if len(probe.PullRequest) > 0 && string(probe.PullRequest) != "null" {
		return TargetPR, nil
	}
	return TargetPlan, nil
}

// ResolveRefNumber expone resolveRefNumber al resto del módulo. Otros flows
// (iterate, close) lo necesitan para llamar a labels.AddLabels/RemoveLabel
// con el number en vez del ref crudo.
func ResolveRefNumber(ref string) (int, error) { return resolveRefNumber(ref) }

// resolveRefNumber devuelve el número del issue/PR que corresponde al ref.
// Si el ref es un número puro, lo devuelve tal cual (sin tocar red). Para
// URLs / owner/repo#N extraemos el número con parsing local — no llamamos
// a gh para evitar un round-trip extra cuando el usuario tipeó "7" o una
// URL perfectamente formada. Los refs que no matcheamos acá los dejamos
// caer al caller (detectTarget) con un error accionable.
func resolveRefNumber(ref string) (int, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return n, nil
	}
	if strings.Contains(ref, "#") {
		parts := strings.SplitN(ref, "#", 2)
		if len(parts) == 2 {
			if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
				return n, nil
			}
		}
	}
	// URL de GitHub: /pull/N o /issues/N.
	for _, segment := range []string{"/pull/", "/issues/"} {
		if i := strings.Index(ref, segment); i >= 0 {
			tail := ref[i+len(segment):]
			// cortar query / fragment / path extra
			for _, sep := range []string{"/", "?", "#"} {
				if j := strings.Index(tail, sep); j >= 0 {
					tail = tail[:j]
				}
			}
			if n, err := strconv.Atoi(tail); err == nil {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("%w: could not resolve number from ref %q", errInvalidRef, ref)
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
//
// Usa REST (`gh api .../issues/{n}/labels`) en lugar de `gh pr edit
// --add-label`: el segundo dispara GraphQL que requiere scope read:org en
// repos de orgs (`gh auth login` default no lo entrega).
func applyValidatedLabel(prRef string, pr *PullRequest, target string) error {
	if target == "" {
		return fmt.Errorf("empty target label")
	}
	if err := labels.Ensure(target); err != nil {
		return err
	}
	number, err := resolveRefNumber(prRef)
	if err != nil {
		return fmt.Errorf("apply validated label: %w", err)
	}
	for _, l := range labels.AllValidated {
		if l == target || !pr.HasLabel(l) {
			continue
		}
		if err := labels.RemoveLabel(number, l); err != nil {
			return err
		}
	}
	if pr.HasLabel(target) {
		return nil
	}
	return labels.AddLabels(number, target)
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
	raw, err := FetchOpenPullRequests()
	if err != nil {
		return nil, err
	}
	return filterValidatable(raw), nil
}

// FetchOpenPullRequests corre `gh pr list` sin filtrar y devuelve el raw
// de PullRequest. Expuesto para que otros flows (ej. close) armen sus
// propias listas con criterios de filter distintos sin duplicar el
// shell-out ni el shape del parse.
func FetchOpenPullRequests() ([]PullRequest, error) {
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
	return raw, nil
}

// ToCandidate proyecta un PullRequest al shape Candidate usado por la TUI.
// Expuesto para que otros paquetes armen slices de Candidate sin acceder
// al internals del struct.
func ToCandidate(p PullRequest) Candidate {
	related := make([]int, 0, len(p.ClosingIssuesReferences))
	for _, r := range p.ClosingIssuesReferences {
		related = append(related, r.Number)
	}
	return Candidate{
		Number:        p.Number,
		Title:         p.Title,
		URL:           p.URL,
		IsDraft:       p.IsDraft,
		Author:        p.Author.Login,
		RelatedIssues: related,
	}
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
		if p.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		res = append(res, ToCandidate(p))
	}
	return res
}

// ParseRef acepta varios formatos de ref a issue o PR y devuelve una forma
// que gh entiende:
//   - "7" → "7"
//   - "owner/repo#7" → "owner/repo#7" (gh lo soporta nativo)
//   - URL "https://github.com/owner/repo/pull/7" → tal cual (gh ok)
//   - URL "https://github.com/owner/repo/issues/42" → tal cual (gh ok)
//
// El único formato que rechazamos es el vacío o formas irreconocibles. Para
// el resto delegamos la validación a `gh pr view` / `gh issue view` (si el
// ref es inválido, gh devuelve error y lo propagamos con contexto).
//
// `che validate` lo usa para ambos targets (issue en che:plan o PR); los
// flows PR-exclusive (iterate, close) siguen llamándolo — si el usuario
// pasa un issue a esos, el preflight de `FetchPR` falla con un mensaje
// claro.
func ParseRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("ref is empty")
	}
	// "7" o número puro → ok.
	if _, err := strconv.Atoi(ref); err == nil {
		return ref, nil
	}
	// URL de GitHub con /pull/<n> o /issues/<n> → ok.
	if strings.HasPrefix(ref, "https://github.com/") &&
		(strings.Contains(ref, "/pull/") || strings.Contains(ref, "/issues/")) {
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
	return "", fmt.Errorf("unrecognized ref %q — accepted: '7', 'owner/repo#7', '/pull/N' URL, '/issues/N' URL", ref)
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
		"--json", "number,title,url,state,isDraft,author,headRefName,labels,closingIssuesReferences")
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
//
// El prompt es el mismo para todos los validadores de un run dado (cambia
// solo según el target: PR vs plan). Lo recibimos como string construido
// afuera para que el runner sea agnóstico de qué se valida.
func runValidatorsParallel(prompt string, validators []Validator, log *output.Logger) []validatorResult {
	results := make([]validatorResult, len(validators))
	var wg sync.WaitGroup
	for i, v := range validators {
		wg.Add(1)
		go func(i int, v Validator) {
			defer wg.Done()
			label := fmt.Sprintf("%s#%d", v.Agent, v.Instance)
			log.Step(label + ": consultando")
			resp, err := callValidator(v, prompt, log, label)
			results[i] = validatorResult{Validator: v, Response: resp, Err: err}
		}(i, v)
	}
	wg.Wait()
	return results
}

// callValidator invoca al binario del agente validador con el prompt dado y
// parsea la respuesta. El prompt ya viene construido desde el caller (build
// PR o build plan); el validador no distingue el target.
func callValidator(v Validator, prompt string, log *output.Logger, label string) (*Response, error) {
	out, err := runAgentCmd(v.Agent, prompt, log, label+": ")
	if err != nil {
		return nil, err
	}
	return parseResponse(out)
}

// runAgentCmd invoca al binario del agente con el prompt construido. Adapter
// sobre agent.Run que preserva los mensajes de error históricos y el hecho
// de que cada línea se emite como log.Step(prefix + line).
func runAgentCmd(a Agent, prompt string, log *output.Logger, progressPrefix string) (string, error) {
	res, err := agent.Run(a, prompt, agent.RunOpts{
		Timeout: AgentTimeout,
		Format:  agent.OutputText,
		OnLine: func(line string) {
			if log != nil {
				log.Step(progressPrefix + line)
			}
		},
		OnStderrLine: func(line string) {
			if log != nil {
				log.Step(progressPrefix + "stderr: " + line)
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

// buildPRValidatorPrompt arma el prompt del validador en modo PR: le da el
// título del PR y el diff, y le pide un JSON estructurado con el verdict y
// los findings. El contrato de respuesta (verdict/summary/findings con kind
// product/technical/documented) se comparte con buildPlanValidatorPrompt —
// la diferencia es qué se valida (código ya escrito vs plan por escribirse).
func buildPRValidatorPrompt(pr *PullRequest, diff string) string {
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

// ---- modo plan: issue fetch, comments, prompt, labels ----

// Issue modela el subset de `gh issue view --json ...` que usamos en el modo
// plan. Mismo shape minimalista que usa explore; no lo importamos de ahí
// para no crear dependencia cruzada entre flows.
type Issue struct {
	Number int          `json:"number"`
	Title  string       `json:"title"`
	Body   string       `json:"body"`
	URL    string       `json:"url"`
	State  string       `json:"state"`
	Labels []IssueLabel `json:"labels"`
}

// IssueLabel es el shape que gh devuelve para cada label del issue.
type IssueLabel struct {
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

// LabelNames proyecta i.Labels al slice de nombres. Útil para alimentar
// helpers de validación como rejectV1Labels y ValidateNoMixedLabels.
func (i *Issue) LabelNames() []string {
	if i == nil {
		return nil
	}
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// FetchIssue corre `gh issue view <ref> --json ...` para el modo plan. Trae
// un superset mínimo: number/title/body/labels/url/state. Los comments se
// fetchean aparte con FetchIssueComments porque es consistente con cómo se
// hace en modo PR (dos round-trips, más barato que pedir un JSON enorme).
func FetchIssue(ref string) (*Issue, error) {
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

// FetchIssueComments trae los comments del issue (para calcular iter). Usa
// una llamada separada a gh issue view con --json comments. El shape de
// comments es el mismo que para PRs (PRComment lo reusamos).
func FetchIssueComments(ref string) ([]PRComment, error) {
	cmd := exec.Command("gh", "issue", "view", ref, "--json", "comments")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue view comments: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var wrap struct {
		Comments []PRComment `json:"comments"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse gh issue view comments: %w", err)
	}
	return wrap.Comments, nil
}

// postIssueComment postea un comment en el issue via `gh issue comment <ref>
// --body-file`. Hermano de postPRComment — los mantenemos separados porque
// la abstracción "dispatcher por target" es más ruido que señal: son dos
// líneas de diferencia y el llamador siempre sabe qué target está.
func postIssueComment(issueRef, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-validate-issuec-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "issue", "comment", issueRef, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// verdictToPlanLabel mapea un verdict JSON (snake_case) al label plan-validated
// correspondiente (kebab-case). Hermano de verdictToLabel, pero con los labels
// del set AllPlanValidated (aplicado sobre issues, no PRs).
func verdictToPlanLabel(verdict string) string {
	switch verdict {
	case "approve":
		return labels.PlanValidatedApprove
	case "changes_requested":
		return labels.PlanValidatedChangesRequested
	case "needs_human":
		return labels.PlanValidatedNeedsHuman
	}
	return ""
}

// applyPlanValidatedLabel es al modo plan lo que applyValidatedLabel al modo
// PR: asegura que target exista, lo aplica al issue, y remueve los otros
// plan-validated:* presentes (son mutuamente excluyentes). Idempotente.
// Usa REST por la misma razón que applyValidatedLabel — uniformemente
// `repo` scope alcanza para issues y PRs.
func applyPlanValidatedLabel(issueRef string, issue *Issue, target string) error {
	if target == "" {
		return fmt.Errorf("empty target label")
	}
	if err := labels.Ensure(target); err != nil {
		return err
	}
	number, err := resolveRefNumber(issueRef)
	if err != nil {
		return fmt.Errorf("apply plan-validated label: %w", err)
	}
	for _, l := range labels.AllPlanValidated {
		if l == target || !issue.HasLabel(l) {
			continue
		}
		if err := labels.RemoveLabel(number, l); err != nil {
			return err
		}
	}
	if issue.HasLabel(target) {
		return nil
	}
	return labels.AddLabels(number, target)
}

// buildPlanValidatorPrompt arma el prompt del validador en modo plan. La
// diferencia con buildPRValidatorPrompt es qué se le pide analizar (un plan
// escrito, no código) y qué anti-patrones buscar (ambigüedad de producto,
// gaps técnicos del plan, criterios poco observables, effort sospechoso).
// El contrato de respuesta (verdict + findings con severity/area/kind) es
// idéntico — así el render de comments, el consolidador de verdict y el
// parser de respuestas se comparten tal cual.
//
// Si el plan parseado vino sin secciones (degradado) pasamos el body raw
// como input secundario para que el validador igual pueda opinar.
func buildPlanValidatorPrompt(issue *Issue, c *planpkg.ConsolidatedPlan) string {
	var sb strings.Builder
	sb.WriteString(`Sos un validador técnico senior. Otro agente produjo un PLAN DE IMPLEMENTACIÓN para un issue.
Tu tarea es revisar el plan ANTES de que se empiece a codear y marcar lo que está flojo — NO reescribir el plan.

Estamos en la etapa previa a ` + "`che execute`" + `: si aprobás, el plan va a alimentar directo a un agente que implementa.

Chequeá específicamente:
1. ¿El goal y el summary son consistentes con el título del issue?
2. ¿Los criterios de aceptación son observables y testeables? (Evitá "funciona bien".)
3. ¿Los pasos son concretos y accionables? ¿Cubren end-to-end el approach?
4. ¿El effort implícito (número y tamaño de pasos) es razonable para el approach?
5. ¿Faltan riesgos obvios que el plan no menciona?
6. ¿Hay asunciones del plan que no son correctas (e.g. dep que no existe, API distinta)?
7. ¿El approach elegido tiene caminos alternativos razonables que el plan descartó sin justificar?
8. ¿Hay ambigüedad de producto irreducible que el plan resolvió arbitrariamente y debería escalarse al humano?

IMPORTANTE — Clasificación de findings:

- kind="product": ambigüedad irreducible de dominio/producto (política, UX opinada, alcance).
  Puede ir con needs_human=true si requiere decisión del dueño.
- kind="technical": gap técnico del plan (paso faltante, criterio poco claro, riesgo no listado, asunción incorrecta).
  needs_human=false — es feedback para re-consolidar el plan.
- kind="documented": el plan ignoró algo que está en el body del issue / criterios iniciales / labels.
  needs_human=false — bug del consolidador, se resuelve re-leyendo.

Valores válidos:
- verdict: "approve" (plan listo para ejecutar), "changes_requested" (gaps técnicos a corregir antes de ejecutar), "needs_human" (ambigüedad de producto irreducible)
- severity: "blocker" | "major" | "minor"
- area: "code" | "tests" | "docs" | "security" | "other"
- kind: "product" | "technical" | "documented"

Reglas:
- Si el plan es sólido y accionable, verdict=approve y findings=[].
- needs_human=true requiere kind=product Y decisión del dueño del producto.
- No inventes faltantes — si el plan cubre algo aunque sea brevemente, no lo marques como gap.
- "where" puede referenciar una sección del plan ("steps[3]", "acceptance_criteria", "approach") — no hay paths de archivo todavía.

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "verdict": "approve|changes_requested|needs_human",
  "summary": "Tu opinión global en 1-2 oraciones",
  "findings": [
    {
      "severity": "major",
      "area": "code",
      "where": "steps[2] o approach o acceptance_criteria[1]",
      "issue": "qué está flojo o mal",
      "suggestion": "cómo arreglarlo antes de ejecutar",
      "needs_human": false,
      "kind": "technical"
    }
  ]
}

Issue #`)
	sb.WriteString(fmt.Sprint(issue.Number))
	sb.WriteString(` (título: "`)
	sb.WriteString(issue.Title)
	sb.WriteString(`")

PLAN CONSOLIDADO (parseado del body):
`)
	if c != nil {
		if strings.TrimSpace(c.Summary) != "" {
			sb.WriteString("Resumen: " + c.Summary + "\n")
		}
		if strings.TrimSpace(c.Goal) != "" {
			sb.WriteString("Goal: " + c.Goal + "\n")
		}
		if strings.TrimSpace(c.Approach) != "" {
			sb.WriteString("Approach: " + c.Approach + "\n")
		}
		if len(c.AcceptanceCriteria) > 0 {
			sb.WriteString("Acceptance criteria:\n")
			for i, crit := range c.AcceptanceCriteria {
				sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, crit))
			}
		}
		if len(c.Steps) > 0 {
			sb.WriteString("Steps:\n")
			for i, step := range c.Steps {
				sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, step))
			}
		}
		if len(c.RisksToMitigate) > 0 {
			sb.WriteString("Risks to mitigate:\n")
			for _, r := range c.RisksToMitigate {
				sb.WriteString(fmt.Sprintf("  - %s (likelihood=%s, impact=%s) — %s\n",
					r.Risk, r.Likelihood, r.Impact, r.Mitigation))
			}
		}
		if len(c.OutOfScope) > 0 {
			sb.WriteString("Out of scope:\n")
			for _, o := range c.OutOfScope {
				sb.WriteString("  - " + o + "\n")
			}
		}
	}
	// Fallback: si el plan parseado viene sin secciones (body con el header
	// pero sin sub-secciones reconocibles) incluimos el body raw para que
	// el validador pueda opinar sobre el texto real, no sobre vacío.
	planEmpty := c == nil ||
		(strings.TrimSpace(c.Goal) == "" && strings.TrimSpace(c.Approach) == "" &&
			len(c.AcceptanceCriteria) == 0 && len(c.Steps) == 0)
	if planEmpty {
		sb.WriteString("\n(El parser no extrajo secciones estructuradas; se adjunta el body raw como fallback.)\n")
	}
	sb.WriteString("\nBody raw del issue:\n<<<\n")
	sb.WriteString(issue.Body)
	sb.WriteString("\n>>>\n")
	return sb.String()
}

// ---- list candidates (para TUI PR #6) ----

// PlanCandidate es la vista mínima de un issue listo para validar como plan:
// abierto, con che:plan, sin plan-validated:approve. La TUI lo consume
// para poblar la lista "plans pending validation". Es un struct dedicado en
// vez de reusar explore.Candidate porque el set de labels relevantes y los
// filtros de exclusión son distintos (acá excluimos plan-validated:approve,
// allá filtramos por ct:plan sin che:*).
type PlanCandidate struct {
	Number int
	Title  string
	URL    string
}

// ListPlanCandidates devuelve los issues abiertos con che:plan que NO
// tienen plan-validated:approve — candidatos a validar como plan. Limita a
// 50 (la TUI se vuelve inmanejable con más, igual que ListOpenPRs).
//
// Excluye plan-validated:approve pero mantiene :changes-requested y
// :needs-human visibles: el humano puede re-validar tras editar el plan.
func ListPlanCandidates() ([]PlanCandidate, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--label", pipelinelabels.StateExplore,
		"--state", "open",
		"--json", "number,title,url,labels",
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
	return filterPlanCandidates(raw), nil
}

// filterPlanCandidates aplica la regla de exclusión de approve. Lo separamos
// en función testeable para no depender de `gh` en los unit tests.
func filterPlanCandidates(raw []Issue) []PlanCandidate {
	out := make([]PlanCandidate, 0, len(raw))
	for _, i := range raw {
		if i.HasLabel(labels.PlanValidatedApprove) {
			continue
		}
		if i.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		out = append(out, PlanCandidate{
			Number: i.Number,
			Title:  i.Title,
			URL:    i.URL,
		})
	}
	return out
}
