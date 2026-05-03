// Package iterate implementa flow "iterar" — tomar un PR con verdict
// validated:changes-requested de che validate, invocar a opus en el
// worktree para que aplique los findings pedidos, y dejar el PR listo
// para una nueva validación (commit+push + comment estructurado + remover
// el label bloqueante).
//
// Decisiones base:
//   - Opus hardcoded (no hay flag --agent) — mismo criterio que close.
//   - Lee TODOS los findings del último comment flow=validate (incluye
//     needs_human) y se los pasa a opus — el humano decidió iterar,
//     opus hace su mejor esfuerzo y si no puede arreglar un finding
//     product lo menciona en el comment final.
//   - Sacar el label validated:changes-requested SOLO si hubo commit+
//     push real. Si opus no tocó nada, el label queda para no engañar
//     al próximo validador.
//   - El comment que postea iterate lleva header flow=iterate con iter
//     propio (incrementa por cada run de iterate), paralelo al de
//     validate — así el próximo validate ve "iter=max(validate)+1".
//
// Comparte mucho plumbing con close y execute (runAgent, worktree,
// gh/git shell-outs). La deuda de extraer común ya está anotada en los
// otros paquetes del flow.
package iterate

import (
	"context"
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
	"github.com/chichex/che/internal/flow/execute"
	"github.com/chichex/che/internal/flow/runguard"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/output"
	"github.com/chichex/che/internal/pipelinelabels"
	planpkg "github.com/chichex/che/internal/plan"
)

// rejectV1Labels devuelve un error accionable si la lista contiene algún
// label v1 del modelo viejo. Wirea `ValidateNoMixedLabels` para detectar
// mezcla v1+v2 antes de gritar "no está en che:state:validate_pr".
//
// REMOVE IN PR6d junto con `labels.V1LegacyStates`/`ValidateNoMixedLabels`.
func rejectV1Labels(kind string, number int, current []string) error {
	if err := labels.ValidateNoMixedLabels(current); err != nil {
		return fmt.Errorf("%s #%d: %w", kind, number, err)
	}
	for _, v1 := range labels.V1LegacyStates() {
		for _, l := range current {
			if l == v1 {
				return fmt.Errorf("%s #%d tiene labels v1 (%s); este flow opera sobre el modelo v2 (`che:state:*`). Corré `che migrate-labels-v2` antes de iterar, o ajustá los labels a mano", kind, number, v1)
			}
		}
	}
	return nil
}

// ExitCode es el código de salida semántico.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // gh/git falla, agente falla, push falla
	ExitSemantic ExitCode = 3 // ref inválido, PR cerrado, no hay findings para iterar
)

// AgentBinary es el CLI de opus. Hardcoded.
const AgentBinary = "claude"

// AgentTimeout acota la invocación del agente. Configurable con
// CHE_ITERATE_AGENT_TIMEOUT_SECS. Default 60 min (igual que close).
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_ITERATE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Minute
}()

// Opts agrupa el writer de stdout (payload: "Iterated PR ...", "Done.")
// y el logger estructurado (progress + errors).
type Opts struct {
	Stdout io.Writer
	Out    *output.Logger
}

// ListIterable devuelve los PRs abiertos del repo que piden iteración,
// i.e. los que tienen label validated:changes-requested. validated:needs-
// human NO entra: esos requieren decisión humana, no ejecución de opus.
// (Decisión de producto cerrada — el usuario pidió específicamente
// "aquellos PRs que requieren cambios validated_changes-requested".)
func ListIterable() ([]validate.Candidate, error) {
	raw, err := validate.FetchOpenPullRequests()
	if err != nil {
		return nil, err
	}
	return filterIterable(raw), nil
}

// filterIterable aplica el criterio de inclusión. Pura, testeable.
// Requiere validated:changes-requested AND che:validated — el segundo
// label asegura que el PR pasó por validate (sino la transición de
// máquina de estados no aplicaría).
func filterIterable(raw []validate.PullRequest) []validate.Candidate {
	res := make([]validate.Candidate, 0, len(raw))
	for _, p := range raw {
		if !hasChangesRequested(p) {
			continue
		}
		if !p.HasLabel(pipelinelabels.StateValidatePR) {
			continue
		}
		if p.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		res = append(res, validate.ToCandidate(p))
	}
	return res
}

func hasChangesRequested(p validate.PullRequest) bool {
	for _, l := range p.Labels {
		if l.Name == labels.ValidatedChangesRequested {
			return true
		}
	}
	return false
}

// ---- Run ----

// Run ejecuta el flow iterate despachando por tipo de ref:
//   - ref → issue con plan-validated:changes-requested: modo plan. El agente
//     edita el plan consolidado del body, se postea un comment flow=iterate,
//     se remueve el label. NO usa worktree ni git.
//   - ref → PR con validated:changes-requested: modo PR (histórico). Worktree
//   - commits + push + comment + remove label.
//
// El preflight de GitHub (auth + remote github) corre antes de detectTarget
// para que errores de entorno den el mensaje accionable correcto. El preflight
// de git repo solo hace falta en modo PR (worktree) — runPlan no lo necesita.
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
	if _, err := validate.ParseRef(ref); err != nil {
		log.Error("ref invalido", output.F{Cause: err})
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
	target, err := validate.DetectTarget(ref)
	if err != nil {
		// resolveRefNumber falla solo si el formato del ref es irreparable.
		// El resto de los fallos vienen de `gh api` (red o 404) y son
		// potencialmente remediables.
		if errors.Is(err, validate.ErrInvalidRef) {
			log.Error("ref invalido", output.F{Cause: err})
			return ExitSemantic
		}
		log.Error("no se pudo determinar si es issue o PR", output.F{Cause: err})
		return ExitRetry
	}
	switch target {
	case validate.TargetPR:
		return runPR(ref, opts, stdout, log)
	case validate.TargetPlan:
		return runPlan(ref, opts, stdout, log)
	default:
		log.Error(fmt.Sprintf("tipo de ref desconocido: %v", target))
		return ExitRetry
	}
}

// runPR es el flow histórico: worktree + agente + commits + push + comment +
// remove label sobre un PR con validated:changes-requested. El preflight de
// GitHub (auth + remote) ya corrió en Run — acá hacemos solo el preflight de
// git repo (repoToplevel).
//
// Gates: PR open + head branch + NO che:locked + che:validated +
// validated:changes-requested. Transición de máquina: che:validated →
// che:executing (lock) → che:executed (éxito) ó che:validated (rollback).
// Los che:* viven en el issue linkeado cuando existe (consistente con
// execute.go); si no hay issue linkeado caemos al PR.
func runPR(prRef string, opts Opts, stdout io.Writer, log *output.Logger) ExitCode {
	log.Info("chequeando repo git")
	repoRoot, err := repoToplevel()
	if err != nil {
		log.Error("git repo invalido", output.F{Cause: err})
		return ExitRetry
	}

	log.Info("obteniendo PR desde GitHub")
	pr, err := validate.FetchPR(prRef)
	if err != nil {
		log.Error("fetching PR failed", output.F{Cause: err})
		return ExitRetry
	}
	if pr.State != "OPEN" {
		log.Error(fmt.Sprintf("PR #%d is not OPEN (state=%s)", pr.Number, pr.State))
		return ExitSemantic
	}
	if strings.TrimSpace(pr.HeadBranch) == "" {
		log.Error(fmt.Sprintf("PR #%d no tiene head branch (¿fork?) — iterate no soporta ese caso", pr.Number))
		return ExitSemantic
	}
	// Rechazar v1 en el PR antes de stateref (más rápido: no fetch del issue
	// linkeado si ya falla acá). Si stateref cae al issue, repetimos abajo.
	if err := rejectV1Labels("PR", pr.Number, pr.PRLabelNames()); err != nil {
		log.Error("gate v1 falló", output.F{PR: pr.Number, Cause: err})
		return ExitSemantic
	}

	// Resolver dónde viven los labels che:* de máquina de estados. Si el
	// PR tiene issue linkeado con algún che:*, los gates leen del issue y
	// las transiciones van al issue; si no, al PR mismo (compat).
	stateRes := pr.ResolveStateRef(prRef)
	stateRef := stateRes.Ref

	if stateRes.ResolvedToIssue {
		if err := rejectV1Labels("issue", stateRes.IssueNumber, stateRes.Labels); err != nil {
			log.Error("gate v1 falló", output.F{Issue: stateRes.IssueNumber, Cause: err})
			return ExitSemantic
		}
	}

	if !stateRes.HasLabel(pipelinelabels.StateValidatePR) {
		if stateRes.ResolvedToIssue {
			log.Error(fmt.Sprintf("issue #%d (linkeado al PR #%d) no está en %s — corré `che validate %d` primero", stateRes.IssueNumber, pr.Number, pipelinelabels.StateValidatePR, pr.Number))
		} else {
			log.Error(fmt.Sprintf("PR #%d no está en %s — corré `che validate %d` primero", pr.Number, pipelinelabels.StateValidatePR, pr.Number))
		}
		return ExitSemantic
	}
	if !pr.HasLabel(labels.ValidatedChangesRequested) {
		log.Error(fmt.Sprintf("PR #%d no tiene validated:changes-requested — nada que iterar", pr.Number))
		return ExitSemantic
	}
	if pr.HasLabel(labels.CheLocked) {
		log.Error(fmt.Sprintf("PR #%d tiene che:locked — otro flow lo tiene agarrado, o quedó colgado. Si es lo segundo: `che unlock %d`", pr.Number, pr.Number))
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

	// Lock con heartbeat + TTL (PRD §6.d) — opt-in. El target del lock es
	// el mismo `stateRef` que las transiciones (issue raíz si está
	// linkeado, PR si no) para no aplicar el lock en un lugar y las
	// transiciones en otro.
	heartbeat, lockResult := runguard.AcquireLock(stateRef, "iterate-pr", log)
	defer runguard.ReleaseLock(heartbeat, log)
	if lockResult == runguard.AcquireContended {
		return ExitSemantic
	}
	auditTarget := pr.Number
	if stateRes.ResolvedToIssue {
		auditTarget = stateRes.IssueNumber
	}

	// Transición che:state:validate_pr → che:state:applying:execute. Rollback en defer LIFO.
	// Target: issue si hay linkeado con che:*, PR si no.
	if stateRes.ResolvedToIssue {
		log.Step("transicionando issue a "+pipelinelabels.StateApplyingExecute, output.F{Issue: stateRes.IssueNumber})
	} else {
		log.Step("transicionando a "+pipelinelabels.StateApplyingExecute, output.F{PR: pr.Number})
	}
	if err := labels.Apply(stateRef, pipelinelabels.StateValidatePR, pipelinelabels.StateApplyingExecute); err != nil {
		log.Error("no pude transicionar a "+pipelinelabels.StateApplyingExecute, output.F{Cause: err})
		return ExitRetry
	}
	runguard.AuditAppend(auditTarget, "iterate-pr", pipelinelabels.StateValidatePR, pipelinelabels.StateApplyingExecute, "", log)
	var stateExecuted bool
	defer func() {
		if stateExecuted {
			return
		}
		if err := labels.Apply(stateRef, pipelinelabels.StateApplyingExecute, pipelinelabels.StateValidatePR); err != nil {
			log.Warn(fmt.Sprintf("rollback %s → %s fallo: %v — revisá labels a mano", pipelinelabels.StateApplyingExecute, pipelinelabels.StateValidatePR, err))
			return
		}
		runguard.AuditAppend(auditTarget, "iterate-pr", pipelinelabels.StateApplyingExecute, pipelinelabels.StateValidatePR, "rollback", log)
	}()

	log.Step("leyendo comments previos para buscar findings")
	comments, err := validate.FetchPRComments(prRef)
	if err != nil {
		log.Error("fetching comments failed", output.F{Cause: err})
		return ExitRetry
	}

	findingsBlocks := LatestValidateFindings(comments)
	if len(findingsBlocks) == 0 {
		log.Error(fmt.Sprintf("PR #%d no tiene findings de che validate — no hay nada que iterar. Corré `che validate %d` antes.", pr.Number, pr.Number))
		return ExitSemantic
	}

	iter := DetermineIterateIter(comments)
	log.Success("encontré findings del último run de validate", output.F{PR: pr.Number, Iter: iter, Detail: fmt.Sprintf("%d validators", len(findingsBlocks))})

	issueNum := firstClosingIssue(pr)

	log.Step(fmt.Sprintf("resolviendo worktree (issue=%d, branch=%s)", issueNum, pr.HeadBranch))
	wt, wtOwned, err := resolveWorktree(repoRoot, issueNum, pr.HeadBranch)
	if err != nil {
		log.Error("resolver worktree fallo", output.F{Cause: err})
		return ExitRetry
	}
	defer func() {
		if wt != nil && wtOwned {
			wtCtx, wtCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := wt.Cleanup(wtCtx, repoRoot, false); err != nil {
				log.Warn("cleanup local parcial — revisá git worktree list y git branch para limpiar a mano", output.F{Cause: err})
			}
			wtCancel()
		}
	}()

	prompt := BuildIteratePrompt(pr, iter, findingsBlocks)

	beforeHEAD, err := gitRevParse(wt.Path, "HEAD")
	if err != nil {
		log.Error("git rev-parse HEAD fallo", output.F{Cause: err})
		return ExitRetry
	}

	log.Step("invocando a opus para aplicar los cambios", output.F{Agent: "opus", Detail: wt.Path})
	if err := runAgent(wt.Path, prompt, log); err != nil {
		log.Error("opus falló", output.F{Agent: "opus", Cause: err})
		return ExitRetry
	}

	log.Step("chequeando si el worktree quedó con cambios")
	hasChanges, err := worktreeHasChanges(wt.Path)
	if err != nil {
		log.Error("git status fallo", output.F{Cause: err})
		return ExitRetry
	}

	pushed, newCommits, err := commitAndPushIfChanged(wt.Path, pr.HeadBranch, beforeHEAD, hasChanges, log)
	if err != nil {
		log.Error("commit/push fallo", output.F{Cause: err})
		return ExitRetry
	}

	if !pushed {
		log.Error(fmt.Sprintf("opus no produjo cambios commiteables para el PR #%d — revisá los findings a mano o reintentá", pr.Number))
		return ExitRetry
	}

	log.Step("posteando comment de iterate en el PR")
	body := RenderIterateComment(iter, newCommits)
	if err := postPRComment(prRef, body); err != nil {
		log.Warn("warning: no se pudo postear comment de iterate — sigo igual", output.F{Cause: err})
	}

	log.Step("removiendo label validated:changes-requested del PR")
	if err := removeLabel(prRef, labels.ValidatedChangesRequested); err != nil {
		log.Warn("warning: no se pudo remover label — removelo a mano", output.F{Cause: err})
	}

	// Cierre de la transición: che:state:applying:execute → che:state:execute. Target: el
	// mismo ref que usamos para abrir la transición (issue si corresponde).
	if stateRes.ResolvedToIssue {
		log.Step("transicionando issue a "+pipelinelabels.StateExecute, output.F{Issue: stateRes.IssueNumber})
	} else {
		log.Step("transicionando a "+pipelinelabels.StateExecute, output.F{PR: pr.Number})
	}
	if err := labels.Apply(stateRef, pipelinelabels.StateApplyingExecute, pipelinelabels.StateExecute); err != nil {
		log.Warn(fmt.Sprintf("no pude transicionar a %s: %v — revisá labels a mano", pipelinelabels.StateExecute, err))
	} else {
		stateExecuted = true
		runguard.AuditAppend(auditTarget, "iterate-pr", pipelinelabels.StateApplyingExecute, pipelinelabels.StateExecute, "", log)
	}

	log.Success("iterated PR", output.F{PR: pr.Number, URL: pr.URL, Detail: fmt.Sprintf("%d nuevos commits", len(newCommits))})
	fmt.Fprintf(stdout, "Iterated PR %s\n", pr.URL)
	fmt.Fprintf(stdout, "Nuevos commits: %d\n", len(newCommits))
	fmt.Fprintln(stdout, "Done. Re-corré `che validate` para obtener un verdict nuevo.")
	return ExitOK
}

// runPlan es el flow nuevo: edita el plan consolidado del body de un issue
// con plan-validated:changes-requested aplicando los findings de validate.
// NO usa worktree ni git — todo el trabajo es sobre el body del issue +
// comments via gh.
//
// Gates (exit semantic si alguno falla):
//   - issue abierto.
//   - issue tiene che:validated (pasó por validate).
//   - issue tiene plan-validated:changes-requested.
//   - issue NO tiene che:executing ni che:executed: si el execute ya
//     corrió, iterar el plan no tiene efecto sobre el PR asociado.
//   - hay findings de validate en comments del issue.
//   - el body del issue tiene un plan consolidado parseable.
//
// Transición: che:validated → che:planning (lock) → che:plan (éxito) ó
// che:validated (rollback).
func runPlan(issueRef string, opts Opts, stdout io.Writer, log *output.Logger) ExitCode {
	log.Info("obteniendo issue desde GitHub")
	issue, err := validate.FetchIssue(issueRef)
	if err != nil {
		log.Error("fetching issue failed", output.F{Cause: err})
		return ExitRetry
	}
	if issue.State != "OPEN" {
		log.Error(fmt.Sprintf("issue #%d is not OPEN (state=%s)", issue.Number, issue.State))
		return ExitSemantic
	}
	// Rechazar v1 antes de chequear v2 — error más útil que "no está en
	// che:state:validate_pr" cuando el repo no migró todavía.
	if err := rejectV1Labels("issue", issue.Number, issue.LabelNames()); err != nil {
		log.Error("gate v1 falló", output.F{Issue: issue.Number, Cause: err})
		return ExitSemantic
	}
	if !issue.HasLabel(pipelinelabels.StateValidatePR) {
		log.Error(fmt.Sprintf("issue #%d no está en %s — corré `che validate %d` primero", issue.Number, pipelinelabels.StateValidatePR, issue.Number))
		return ExitSemantic
	}
	if !issue.HasLabel(labels.PlanValidatedChangesRequested) {
		log.Error(fmt.Sprintf("issue #%d no tiene plan-validated:changes-requested — corré `che validate %d` primero", issue.Number, issue.Number))
		return ExitSemantic
	}
	// Execute ya corrió → iterar el plan no tiene efecto sobre el PR.
	if issue.HasLabel(pipelinelabels.StateApplyingExecute) || issue.HasLabel(pipelinelabels.StateExecute) {
		log.Error(fmt.Sprintf("issue #%d ya pasó por execute — iterar el plan no tiene efecto sobre el PR asociado. Iterar el PR directamente con `che iterate <pr>`.", issue.Number))
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
	heartbeat, lockResult := runguard.AcquireLock(issueRef, "iterate-plan", log)
	defer runguard.ReleaseLock(heartbeat, log)
	if lockResult == runguard.AcquireContended {
		return ExitSemantic
	}

	// Transición che:state:validate_pr → che:state:applying:explore. Rollback en defer LIFO.
	log.Step("transicionando a "+pipelinelabels.StateApplyingExplore, output.F{Issue: issue.Number})
	if err := labels.Apply(issueRef, pipelinelabels.StateValidatePR, pipelinelabels.StateApplyingExplore); err != nil {
		log.Error("no pude transicionar a "+pipelinelabels.StateApplyingExplore, output.F{Cause: err})
		return ExitRetry
	}
	runguard.AuditAppend(issue.Number, "iterate-plan", pipelinelabels.StateValidatePR, pipelinelabels.StateApplyingExplore, "", log)
	var statePlan bool
	defer func() {
		if statePlan {
			return
		}
		if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExplore, pipelinelabels.StateValidatePR); err != nil {
			log.Warn(fmt.Sprintf("rollback %s → %s fallo: %v — revisá labels a mano", pipelinelabels.StateApplyingExplore, pipelinelabels.StateValidatePR, err))
			return
		}
		runguard.AuditAppend(issue.Number, "iterate-plan", pipelinelabels.StateApplyingExplore, pipelinelabels.StateValidatePR, "rollback", log)
	}()

	currentPlan, parseErr := planpkg.Parse(issue.Body)
	if parseErr != nil {
		log.Error(fmt.Sprintf("no pude parsear el plan consolidado del issue #%d: %v", issue.Number, parseErr))
		return ExitSemantic
	}
	if !planpkg.HasConsolidatedHeader(issue.Body) {
		log.Error(fmt.Sprintf("issue #%d no tiene plan consolidado en el body — corré `che explore %d`", issue.Number, issue.Number))
		return ExitSemantic
	}

	log.Step("leyendo comments del issue para buscar findings de validate")
	comments, err := validate.FetchIssueComments(issueRef)
	if err != nil {
		log.Error("fetching comments failed", output.F{Cause: err})
		return ExitRetry
	}
	findingsBlocks := LatestValidateFindings(comments)
	if len(findingsBlocks) == 0 {
		log.Error(fmt.Sprintf("no encontré findings de che validate en el issue #%d — corré `che validate %d` primero", issue.Number, issue.Number))
		return ExitSemantic
	}

	iter := DetermineIterateIter(comments)
	log.Success("encontré findings del último run de validate", output.F{Issue: issue.Number, Iter: iter, Detail: fmt.Sprintf("%d validators", len(findingsBlocks))})

	originalBody := extractOriginalBody(issue.Body)
	prompt := BuildPlanIteratePrompt(issue, originalBody, currentPlan, findingsBlocks, iter)

	log.Step("invocando a opus para reescribir el plan", output.F{Agent: "opus"})
	out, err := runPlanAgent(prompt, log)
	if err != nil {
		log.Error("opus falló", output.F{Agent: "opus", Cause: err})
		return ExitRetry
	}

	newPlan, err := parseIteratedPlan(out)
	if err != nil {
		log.Error("opus devolvió un plan inválido", output.F{Agent: "opus", Cause: err})
		return ExitRetry
	}

	// Si el plan resultante es idéntico al actual (misma JSON canónica),
	// consideramos que opus no tuvo qué aplicar — exit retry con mensaje
	// accionable para que el humano revise a mano.
	if plansEqual(currentPlan, newPlan) {
		log.Error(fmt.Sprintf("opus no produjo cambios al plan del issue #%d — revisá los findings a mano o reintentá", issue.Number))
		return ExitRetry
	}

	newBody := planpkg.Render(newPlan, originalBody)

	log.Step("editando body del issue con el plan iterado")
	if err := editIssueBody(issueRef, newBody); err != nil {
		log.Error("gh issue edit --body-file fallo", output.F{Cause: err})
		return ExitRetry
	}

	log.Step("posteando comment de iterate en el issue")
	body := RenderIteratePlanComment(iter, len(findingsBlocks))
	if err := postIssueComment(issueRef, body); err != nil {
		log.Warn("warning: no se pudo postear comment de iterate — sigo igual", output.F{Cause: err})
	}

	log.Step("removiendo label plan-validated:changes-requested del issue")
	if err := removeIssueLabel(issueRef, labels.PlanValidatedChangesRequested); err != nil {
		log.Warn("warning: no se pudo remover label — removelo a mano", output.F{Cause: err})
	}

	// Cierre de la transición: che:state:applying:explore → che:state:explore.
	log.Step("transicionando a "+pipelinelabels.StateExplore, output.F{Issue: issue.Number})
	if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExplore, pipelinelabels.StateExplore); err != nil {
		log.Warn(fmt.Sprintf("no pude transicionar a %s: %v — revisá labels a mano", pipelinelabels.StateExplore, err))
	} else {
		statePlan = true
		runguard.AuditAppend(issue.Number, "iterate-plan", pipelinelabels.StateApplyingExplore, pipelinelabels.StateExplore, "", log)
	}

	log.Success("iterated plan", output.F{Issue: issue.Number, URL: issue.URL})
	fmt.Fprintf(stdout, "Iterated plan %s\n", issue.URL)
	fmt.Fprintf(stdout, "Re-validá con `che validate %d`.\n", issue.Number)
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

// extractOriginalBody devuelve la sección "## Idea original" (sin el header)
// del body del issue, o el body entero si no hay esa sección. Es lo que
// plan.Render espera como segundo argumento cuando queremos reemplazar el
// plan consolidado sin duplicar el texto original. Best-effort: si el body
// no matchea el shape esperado, devolvemos issue.Body tal cual y que
// plan.Render anide — no es ideal pero es mejor que borrar contexto.
func extractOriginalBody(body string) string {
	const marker = "## Idea original"
	i := strings.Index(body, marker)
	if i < 0 {
		return body
	}
	rest := body[i+len(marker):]
	// saltar el header tail ("(de `che idea`)") hasta el primer \n\n
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		return strings.TrimLeft(rest[j+2:], "\n")
	}
	// fallback: sin doble newline, cortamos después del primer \n
	if j := strings.Index(rest, "\n"); j >= 0 {
		return strings.TrimLeft(rest[j+1:], "\n")
	}
	return rest
}

// runPlanAgent invoca a opus (claude) en modo text (no stream-json) sobre el
// prompt dado. Modo text porque el output es un JSON chico y sincrónico —
// no necesitamos ver tool_use en vivo (opus no edita archivos en este modo,
// solo devuelve el plan nuevo).
func runPlanAgent(prompt string, log *output.Logger) (string, error) {
	res, err := agent.Run(agent.AgentOpus, prompt, agent.RunOpts{
		Timeout: AgentTimeout,
		Format:  agent.OutputText,
		OnLine: func(line string) {
			if log != nil {
				log.Step("opus: " + line)
			}
		},
		OnStderrLine: func(line string) {
			if log != nil {
				log.Step("opus stderr: " + line)
			}
		},
	})
	if errors.Is(err, agent.ErrTimeout) {
		if se := strings.TrimSpace(res.Stderr); se != "" {
			return res.Stdout, fmt.Errorf("opus timed out after %s; stderr: %s", AgentTimeout, se)
		}
		return res.Stdout, fmt.Errorf("opus timed out after %s (subí CHE_ITERATE_AGENT_TIMEOUT_SECS)", AgentTimeout)
	}
	var ee *agent.ExitError
	if errors.As(err, &ee) {
		return res.Stdout, fmt.Errorf("opus exit %d: %s", ee.ExitCode, ee.Stderr)
	}
	if err != nil {
		return res.Stdout, err
	}
	return res.Stdout, nil
}

// parseIteratedPlan extrae el ConsolidatedPlan del stdout del agente,
// tolerando code fences y texto alrededor (mismo algoritmo que validate.
// parseResponse). Valida campos requeridos (summary/goal/approach no
// vacíos, al menos 1 step y 1 acceptance criterion) antes de devolver.
func parseIteratedPlan(raw string) (*planpkg.ConsolidatedPlan, error) {
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
	var p planpkg.ConsolidatedPlan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("invalid JSON from opus: %w", err)
	}
	if strings.TrimSpace(p.Summary) == "" {
		return nil, fmt.Errorf("summary is empty")
	}
	if strings.TrimSpace(p.Goal) == "" {
		return nil, fmt.Errorf("goal is empty")
	}
	if strings.TrimSpace(p.Approach) == "" {
		return nil, fmt.Errorf("approach is empty")
	}
	if len(p.AcceptanceCriteria) == 0 {
		return nil, fmt.Errorf("acceptance_criteria is empty")
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("steps is empty")
	}
	return &p, nil
}

// plansEqual compara dos planes por JSON canónico. json.Marshal con el shape
// de ConsolidatedPlan es determinista (tags fijos, orden de fields por
// struct): si los bytes coinciden, los planes son funcionalmente iguales.
func plansEqual(a, b *planpkg.ConsolidatedPlan) bool {
	if a == nil || b == nil {
		return a == b
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// BuildPlanIteratePrompt arma el prompt para opus en modo plan. Le damos:
//   - rol (ingeniero senior reescribiendo plan pre-implementación),
//   - título + body original del issue,
//   - plan consolidado actual serializado como JSON pretty (para que opus
//     entienda el shape exacto que tiene que devolver),
//   - bodies completos de los findings tal como los posteó validate,
//   - reglas sobre kind=product (no resolver unilateralmente, mencionar
//     en summary) y delta mínimo,
//   - shape de output esperado (ConsolidatedPlan JSON, sin fences).
func BuildPlanIteratePrompt(issue *validate.Issue, originalBody string, currentPlan *planpkg.ConsolidatedPlan, findings []string, iter int) string {
	var sb strings.Builder
	sb.WriteString("Sos un ingeniero senior reescribiendo un plan de implementación ANTES de implementar. ")
	sb.WriteString("Otro agente propuso un plan y validadores marcaron gaps; tu tarea es aplicar los fixes al plan — ")
	sb.WriteString("NO implementar código, NO tocar archivos, solo devolver el plan nuevo como JSON.\n\n")

	sb.WriteString(fmt.Sprintf("Issue #%d — %s\n", issue.Number, issue.Title))
	sb.WriteString("URL: " + issue.URL + "\n")
	sb.WriteString(fmt.Sprintf("Iter de iterate: %d\n\n", iter))

	sb.WriteString("## Body original del issue (criterios iniciales, contexto, idea)\n\n")
	sb.WriteString("<<<\n")
	sb.WriteString(originalBody)
	if !strings.HasSuffix(originalBody, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString(">>>\n\n")

	sb.WriteString("## Plan consolidado actual (lo que hay que iterar)\n\n")
	sb.WriteString("```json\n")
	if js, err := json.MarshalIndent(currentPlan, "", "  "); err == nil {
		sb.Write(js)
	} else {
		sb.WriteString("{}")
	}
	sb.WriteString("\n```\n\n")

	sb.WriteString("## Findings de los validadores (markdown del último run de che validate)\n\n")
	for i, body := range findings {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Reglas\n")
	sb.WriteString("- Priorizá findings con severity blocker → major → minor.\n")
	sb.WriteString("- Findings con `kind=product` y `needs_human=true`: NO los resuelvas unilateralmente. Mencionalos en el summary del plan nuevo como decisión pendiente que el humano tiene que zanjar.\n")
	sb.WriteString("- Findings con `kind=technical` o `kind=documented`: aplicalos al plan (editá steps, approach, acceptance_criteria, risks, etc).\n")
	sb.WriteString("- Hacé el delta mínimo: no reescribas secciones que los findings no tocan.\n")
	sb.WriteString("- El shape de output debe ser IDÉNTICO al del plan actual (mismos fields, mismos tipos).\n\n")

	sb.WriteString("## Output esperado\n")
	sb.WriteString("Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:\n\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"summary\": \"...\",\n")
	sb.WriteString("  \"goal\": \"...\",\n")
	sb.WriteString("  \"acceptance_criteria\": [\"...\"],\n")
	sb.WriteString("  \"approach\": \"...\",\n")
	sb.WriteString("  \"steps\": [\"...\"],\n")
	sb.WriteString("  \"risks_to_mitigate\": [{\"risk\": \"...\", \"likelihood\": \"low|medium|high\", \"impact\": \"low|medium|high\", \"mitigation\": \"...\"}],\n")
	sb.WriteString("  \"out_of_scope\": [\"...\"]\n")
	sb.WriteString("}\n")
	return sb.String()
}

// RenderIteratePlanComment arma el body del comment flow=iterate que se
// postea en el issue después de editar el body. Incluye header HTML
// estructurado para que validate/iterate futuros calculen el iter correcto.
func RenderIteratePlanComment(iter int, numValidators int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=iterate iter=%d agent=opus instance=1 role=executor -->\n", iter))
	sb.WriteString(fmt.Sprintf("## [che · iterate · plan · iter:%d]\n\n", iter))
	sb.WriteString(fmt.Sprintf("Plan iterado aplicando findings de %d validador(es). Ver el body del issue para el plan actualizado.\n\n", numValidators))
	sb.WriteString("El label `plan-validated:changes-requested` fue removido. Re-validá con `che validate` para obtener un verdict nuevo.\n")
	return sb.String()
}

// editIssueBody reemplaza el body del issue via `gh issue edit --body-file`.
// Usa un archivo temporal para evitar problemas con argv largos.
func editIssueBody(ref, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-iterate-body-*")
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

// postIssueComment postea un comment en el issue via `gh issue comment`.
func postIssueComment(issueRef, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-iterate-issuec-*")
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

// removeIssueLabel saca un label del issue vía REST (`gh api -X DELETE
// repos/.../issues/{n}/labels/{name}`). Antes usaba `gh issue edit
// --remove-label`, que dispara GraphQL y falla en repos de orgs sin scope
// read:org. REST solo necesita `repo`.
func removeIssueLabel(issueRef, name string) error {
	number, err := validate.ResolveRefNumber(issueRef)
	if err != nil {
		return fmt.Errorf("remove issue label %s: %w", name, err)
	}
	return labels.RemoveLabel(number, name)
}

// ListIterablePlanCandidates devuelve los issues abiertos del repo con
// che:validated + plan-validated:changes-requested — candidatos a iteración
// de plan. La TUI lo consume para poblar la lista "plans to iterate".
// Reusa validate.PlanCandidate como shape (number/title/url) porque es
// exactamente lo mismo que la TUI necesita para validate sobre plan, solo
// que filtrado por un label distinto.
func ListIterablePlanCandidates() ([]validate.PlanCandidate, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--label", pipelinelabels.StateValidatePR,
		"--label", labels.PlanValidatedChangesRequested,
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
	var raw []validate.Issue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	return filterIterablePlanCandidates(raw), nil
}

// filterIterablePlanCandidates proyecta la respuesta cruda de gh a shape
// PlanCandidate. No aplica exclusiones extra: ya filtramos por label en el
// comando gh. Lo separamos en helper pura para testear sin depender de gh.
func filterIterablePlanCandidates(raw []validate.Issue) []validate.PlanCandidate {
	out := make([]validate.PlanCandidate, 0, len(raw))
	for _, i := range raw {
		if i.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		out = append(out, validate.PlanCandidate{
			Number: i.Number,
			Title:  i.Title,
			URL:    i.URL,
		})
	}
	return out
}

// firstClosingIssue devuelve el primer issue referenciado por "Closes #N",
// o 0 si no hay. Igual criterio que close: el path del worktree usa ese N.
func firstClosingIssue(pr *validate.PullRequest) int {
	for _, r := range pr.ClosingIssuesReferences {
		if r.Number > 0 {
			return r.Number
		}
	}
	return 0
}

// ---- comments parsing ----

// LatestValidateFindings devuelve los bodies (markdown completo) de los
// comments de validadores (flow=validate role=validator) de la última
// iter. Si la última iter tiene N validators, devuelve los N bodies.
// Si no hay ningún comment de validate, slice vacío.
func LatestValidateFindings(comments []validate.PRComment) []string {
	type item struct {
		iter int
		body string
	}
	var validators []item
	max := 0
	for _, c := range comments {
		h := validate.ParseCommentHeader(c.Body)
		if h.Flow != "validate" || h.Role != "validator" {
			continue
		}
		if h.Iter > max {
			max = h.Iter
		}
		validators = append(validators, item{iter: h.Iter, body: c.Body})
	}
	if max == 0 {
		return nil
	}
	var out []string
	for _, v := range validators {
		if v.iter == max {
			out = append(out, v.body)
		}
	}
	return out
}

// DetermineIterateIter devuelve el próximo iter number para un run de
// iterate (max(flow=iterate) + 1). El iter de iterate es independiente
// del de validate — iterate=3 no implica validate=3.
func DetermineIterateIter(comments []validate.PRComment) int {
	max := 0
	for _, c := range comments {
		h := validate.ParseCommentHeader(c.Body)
		if h.Flow != "iterate" {
			continue
		}
		if h.Iter > max {
			max = h.Iter
		}
	}
	return max + 1
}

// ---- prompt builder ----

// BuildIteratePrompt arma el prompt para opus. Incluye: contexto del PR,
// los findings completos de los validators (markdown tal cual como los
// posteó che validate), y las instrucciones de workflow git.
func BuildIteratePrompt(pr *validate.PullRequest, iter int, findings []string) string {
	var sb strings.Builder
	sb.WriteString("Sos un ingeniero senior. Un validador de este PR pidió cambios. ")
	sb.WriteString("Estás parado en el worktree de la branch `")
	sb.WriteString(pr.HeadBranch)
	sb.WriteString("` (el cwd ya está en el worktree). ")
	sb.WriteString("Tu tarea: leer los findings de los validadores y aplicarlos end-to-end ")
	sb.WriteString("(edits + commits + push).\n\n")

	sb.WriteString(fmt.Sprintf("PR #%d — %s\n", pr.Number, pr.Title))
	sb.WriteString("URL: " + pr.URL + "\n")
	sb.WriteString(fmt.Sprintf("Iter de iterate: %d\n\n", iter))

	sb.WriteString("## Findings de los validadores (markdown del último run de che validate)\n\n")
	for i, body := range findings {
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Workflow esperado\n")
	sb.WriteString("1. Leé el estado del worktree (`git status`, `git log --oneline -5`).\n")
	sb.WriteString("2. Para cada finding aplicable (severity blocker/major primero), aplicá el fix. ")
	sb.WriteString("Si un finding tiene `kind=product` y `needs_human=true`, generalmente NO lo podés resolver vos — mencionalo al final pero no lo forces.\n")
	sb.WriteString("3. Hacé commits atómicos con mensajes descriptivos (`fix: <qué arreglaste>` o `test: <qué cubriste>`). Un commit por finding si tiene sentido; agrupá si son relacionados.\n")
	sb.WriteString("4. Verificá localmente si podés (tests/build) antes de pushear.\n")
	sb.WriteString("5. `git push` al terminar.\n")
	sb.WriteString("6. Como última cosa, imprimí en tu respuesta un resumen en bullets: qué arreglaste, qué no pudiste, y por qué.\n\n")

	sb.WriteString("## Reglas\n")
	sb.WriteString("- No abras PRs nuevos ni toques otras branches.\n")
	sb.WriteString("- No hagas force-push — usá push normal (la branch ya tiene upstream).\n")
	sb.WriteString("- Si un finding es irrelevante o duplicado, ignoralo y mencionalo en el resumen.\n")
	sb.WriteString("- Si no podés arreglar NADA (todo es product/needs_human), no commitees — el harness va a detectar que no hubo push y abortar con error claro.\n")
	return sb.String()
}

// ---- comment rendering ----

// RenderIterateComment arma el body del comment que postea iterate en el
// PR después de pushear los fixes. Incluye header HTML estructurado para
// que validate/iterate futuros calculen el iter correcto.
func RenderIterateComment(iter int, commitSubjects []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<!-- claude-cli: flow=iterate iter=%d agent=opus instance=1 role=executor -->\n", iter))
	sb.WriteString(fmt.Sprintf("## [che · iterate · opus#1 · iter:%d]\n\n", iter))
	if len(commitSubjects) == 0 {
		sb.WriteString("_Opus no dejó commits nuevos (no debería llegar acá — el flow aborta si no hubo push)._\n")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("Opus aplicó los findings del último run de che validate. %d commit(s) nuevo(s) pusheado(s):\n\n",
		len(commitSubjects)))
	for _, s := range commitSubjects {
		sb.WriteString("- " + s + "\n")
	}
	sb.WriteString("\nEl label `validated:changes-requested` fue removido. Podés re-correr `che validate` para un verdict nuevo.\n")
	return sb.String()
}

// ---- agent invocation (delega en internal/agent) ----

// runAgent invoca a opus sobre el worktree (cwd=cwd) en modo stream-json
// para que cada tool_use aparezca en los logs vía formatOpusLine.
//
// Iterate NO usa process group isolation (a diferencia de execute): cancel
// por timeout mata el PID directo. Históricamente el flow se usa en entornos
// donde opus no forkea herramientas problemáticas (los commits/push los hace
// iterate después, no opus), así que no hace falta. Si en el futuro esto
// cambia, se setea KillGrace=N*time.Second y listo.
func runAgent(cwd, prompt string, log *output.Logger) error {
	res, err := agent.Run(agent.AgentOpus, prompt, agent.RunOpts{
		Dir:     cwd,
		Timeout: AgentTimeout,
		Format:  agent.OutputStreamJSON,
		OnLine: func(line string) {
			if log != nil {
				log.Step("opus: " + line)
			}
		},
		OnStderrLine: func(line string) {
			if log != nil {
				log.Step("opus stderr: " + line)
			}
		},
		StreamFormatter: formatOpusLine,
	})
	if errors.Is(err, agent.ErrTimeout) {
		if se := strings.TrimSpace(res.Stderr); se != "" {
			return fmt.Errorf("opus timed out after %s; stderr: %s", AgentTimeout, se)
		}
		return fmt.Errorf("opus timed out after %s (subí CHE_ITERATE_AGENT_TIMEOUT_SECS)", AgentTimeout)
	}
	var ee *agent.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("opus exit %d: %s", ee.ExitCode, ee.Stderr)
	}
	return err
}

// formatOpusLine es un thin wrapper sobre agent.FormatOpusLine con el prefijo
// histórico que usa iterate ("tool: "). Los tests de este paquete lo
// ejercitan directamente con asserts sobre strings exactos.
func formatOpusLine(line string) (string, bool) {
	return agent.FormatOpusLine(line, "tool: ")
}

// ---- worktree resolution (copy-adapted de close) ----

func resolveWorktree(repoRoot string, issueNum int, headBranch string) (*execute.Worktree, bool, error) {
	if strings.TrimSpace(headBranch) == "" {
		return nil, false, fmt.Errorf("PR sin head branch — no puedo crear worktree")
	}

	// Antes que nada: si la branch ya está checkouteada en algún worktree
	// (ej. el que dejó execute en .worktrees/issue-<N>), reusamos ese path
	// — git no permite dos worktrees en la misma branch, así que intentar
	// crear uno nuevo fallaría. Busqueda por branch (no por path) porque
	// el path de execute puede no coincidir con el que calcula iterate.
	if p, err := findWorktreePathByBranch(repoRoot, headBranch); err != nil {
		return nil, false, err
	} else if p != "" {
		return &execute.Worktree{Path: p, Branch: headBranch}, false, nil
	}

	path := worktreePathFor(repoRoot, issueNum, headBranch)

	existing, err := existingWorktreeBranch(repoRoot, path)
	if err != nil {
		return nil, false, err
	}
	if existing != "" {
		if existing != headBranch {
			return nil, false, fmt.Errorf("worktree %s existe en branch %q, esperaba %q — resolvelo con `git worktree remove %s`",
				path, existing, headBranch, path)
		}
		return &execute.Worktree{Path: path, Branch: headBranch}, false, nil
	}

	skipFetch := os.Getenv("CHE_ITERATE_SKIP_FETCH") == "1"
	if !skipFetch {
		if err := runGit(repoRoot, "fetch", "origin", headBranch); err != nil {
			return nil, false, fmt.Errorf("git fetch origin %s: %w — para tests locales sin red setear CHE_ITERATE_SKIP_FETCH=1", headBranch, err)
		}
	}

	if ok, err := localBranchExists(repoRoot, headBranch); err != nil {
		return nil, false, err
	} else if !ok {
		if err := runGit(repoRoot, "branch", headBranch, "origin/"+headBranch); err != nil {
			if err2 := runGit(repoRoot, "branch", headBranch); err2 != nil {
				return nil, false, fmt.Errorf("git branch %s: %v (fallback falló: %v)", headBranch, err, err2)
			}
		}
	}

	if err := runGit(repoRoot, "worktree", "add", path, headBranch); err != nil {
		return nil, false, fmt.Errorf("git worktree add %s %s: %w", path, headBranch, err)
	}
	return &execute.Worktree{Path: path, Branch: headBranch}, true, nil
}

func worktreePathFor(repoRoot string, issueNum int, headBranch string) string {
	if issueNum > 0 {
		return filepath.Join(repoRoot, ".worktrees", fmt.Sprintf("issue-%d", issueNum))
	}
	slug := sanitizeBranchSlug(headBranch)
	return filepath.Join(repoRoot, ".worktrees", "pr-"+slug)
}

func sanitizeBranchSlug(branch string) string {
	branch = strings.ReplaceAll(branch, "/", "-")
	branch = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, branch)
	if branch == "" {
		return "pr"
	}
	return branch
}

// findWorktreePathByBranch parsea `git worktree list --porcelain` y
// devuelve el path del worktree que tiene branch checkouteada, o "" si
// ninguno. Es el dual de existingWorktreeBranch (que busca por path).
// Se usa para reusar el worktree que creó execute aunque esté en un path
// que iterate/close calcularían distinto.
func findWorktreePathByBranch(repoRoot, branch string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git worktree list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	var curPath, curBranch string
	flush := func() string {
		if curPath != "" && curBranch == branch {
			return curPath
		}
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if p := flush(); p != "" {
				return p, nil
			}
			curPath, curBranch = "", ""
			continue
		}
		if strings.HasPrefix(trimmed, "worktree ") {
			curPath = strings.TrimPrefix(trimmed, "worktree ")
		} else if strings.HasPrefix(trimmed, "branch ") {
			curBranch = strings.TrimPrefix(trimmed, "branch refs/heads/")
		}
	}
	if p := flush(); p != "" {
		return p, nil
	}
	return "", nil
}

func existingWorktreeBranch(repoRoot, path string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git worktree list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	target, err := canonPath(path)
	if err != nil {
		return "", err
	}
	var curPath, curBranch string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if curPath != "" {
				if cp, _ := canonPath(curPath); cp == target {
					return curBranch, nil
				}
			}
			curPath, curBranch = "", ""
			continue
		}
		if strings.HasPrefix(trimmed, "worktree ") {
			curPath = strings.TrimPrefix(trimmed, "worktree ")
		} else if strings.HasPrefix(trimmed, "branch ") {
			curBranch = strings.TrimPrefix(trimmed, "branch refs/heads/")
		}
	}
	if curPath != "" {
		if cp, _ := canonPath(curPath); cp == target {
			return curBranch, nil
		}
	}
	return "", nil
}

func canonPath(p string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	}
	dir, base := filepath.Split(p)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolved, base), nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p), nil
	}
	return abs, nil
}

func localBranchExists(repoRoot, branch string) (bool, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---- git/gh helpers ----

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

func runGit(repoRoot string, args ...string) error {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// worktreeHasChanges chequea si el worktree tiene unstaged/staged changes
// no-commiteadas. Usado para detectar que opus dejó cambios sin commitear.
func worktreeHasChanges(wtPath string) (bool, error) {
	out, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return false, fmt.Errorf("git status: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// commitAndPushIfChanged detecta si opus produjo cambios reales usando
// como invariante el SHA del HEAD antes/después de la invocación. Es
// robusto vs:
//   - Opus pushea por su cuenta durante su trabajo (el prompt se lo pide):
//     origin/<branch> avanza pero before != after, así que seguimos
//     detectando los commits correctamente.
//   - Opus hace `git commit --amend` + `push --force-with-lease`:
//     before != after (distinto SHA aunque parent igual), commits
//     visibles.
//   - Opus deja cambios sin commitear: hacemos auto-commit y volvemos a
//     snapshotear HEAD.
//
// El push final es idempotente: si opus ya pusheó, `git push origin <branch>`
// es no-op.
func commitAndPushIfChanged(wtPath, branch, beforeHEAD string, hasDirty bool, log *output.Logger) (bool, []string, error) {
	if hasDirty {
		log.Step("commiteando cambios no-commiteados que dejó opus")
		if err := runGitIn(wtPath, "add", "-A"); err != nil {
			return false, nil, err
		}
		if err := runGitIn(wtPath, "commit", "-m", "fix: apply validator findings (auto-commit by che iterate)"); err != nil {
			return false, nil, err
		}
	}

	afterHEAD, err := gitRevParse(wtPath, "HEAD")
	if err != nil {
		return false, nil, err
	}
	if afterHEAD == beforeHEAD {
		return false, nil, nil
	}

	subjects, err := commitSubjectsBetween(wtPath, beforeHEAD, afterHEAD)
	if err != nil {
		return false, nil, err
	}

	log.Step(fmt.Sprintf("pusheando a origin/%s (idempotente si opus ya pusheó)", branch))
	if err := runGitIn(wtPath, "push", "origin", branch); err != nil {
		return false, nil, err
	}

	if len(subjects) == 0 {
		// HEAD cambió pero no hay commits entre before..after — caso raro
		// (¿amend sin cambios? ¿rebase?). Reportamos al menos el SHA.
		subjects = []string{fmt.Sprintf("HEAD actualizado a %s", afterHEAD[:min(8, len(afterHEAD))])}
	}
	return true, subjects, nil
}

// gitRevParse devuelve el SHA de la ref en un worktree (no trimmed para
// comparación exacta).
func gitRevParse(wtPath, ref string) (string, error) {
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", ref).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git rev-parse %s: %s", ref, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

// commitSubjectsBetween devuelve los subjects de los commits entre
// before..after (oldest → newest). Si before no es ancestro de after
// (ej. tras un amend en un HEAD sin más commits), git log puede devolver
// vacío; el caller maneja ese caso.
func commitSubjectsBetween(wtPath, before, after string) ([]string, error) {
	cmd := exec.Command("git", "-C", wtPath, "log", "--reverse", "--format=%s", before+".."+after)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("git log: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// postPRComment postea un comment en el PR via gh pr comment.
func postPRComment(prRef, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-iterate-prc-*")
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

// removeLabel saca un label del PR vía REST (`gh api -X DELETE
// repos/.../issues/{n}/labels/{name}` — los PRs son issues en REST). Antes
// usaba `gh pr edit --remove-label`, que dispara GraphQL y requiere scope
// read:org en repos de orgs.
func removeLabel(prRef, name string) error {
	number, err := validate.ResolveRefNumber(prRef)
	if err != nil {
		return fmt.Errorf("remove pr label %s: %w", name, err)
	}
	return labels.RemoveLabel(number, name)
}
