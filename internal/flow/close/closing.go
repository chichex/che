// Package closing implementa flow 04 — tomar un PR abierto (draft o no),
// asegurarse de que esté mergeable (sin conflictos, CI verde, sin validator
// que haya pedido cambios), invocar al agente para arreglar lo que haga falta
// hasta MaxFixAttempts veces, mergearlo con merge commit y cerrar el issue
// asociado.
//
// El package se llama "closing" y no "close" para evitar el shadow de la
// función builtin `close()` de Go dentro del paquete. El subcomando sigue
// siendo `che close`.
//
// NOTA: este paquete comparte mucho plumbing con execute y validate
// (runAgent, worktree resolution, gh shell-out). No se extrae a
// `internal/flow/common/` todavía por coherencia con el resto del proyecto
// — la deuda ya está anotada en los otros paquetes.
package closing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/flow/execute"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/output"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // CI sigue rojo, conflictos no resueltos, gh/git falla
	ExitSemantic ExitCode = 3 // ref inválido, PR cerrado, verdict reject, sin merge permission
)

// MaxFixAttempts es el tope de iteraciones del loop fix→re-check. Cada
// iteración puede incluir: invocar al agente, commit+push, esperar CI.
// Cerrado con el usuario: 3 intentos totales.
const MaxFixAttempts = 3

// AgentBinary es el CLI que se invoca para resolver conflictos / fixes de CI.
// Hardcoded a claude (opus) — no es configurable por diseño.
const AgentBinary = "claude"

// AgentTimeout acota cada invocación individual del agente. Configurable con
// CHE_CLOSE_AGENT_TIMEOUT_SECS para entornos lentos. Default 60 min (igual
// que execute): arreglar conflictos + correr tests localmente puede tardar.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_CLOSE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Minute
}()

// CIPollTimeout es cuánto esperamos post-push a que CI termine (pass o fail)
// antes de dar el intento por perdido. Configurable con
// CHE_CLOSE_CI_POLL_TIMEOUT_SECS. Default 20 min: workflows típicos corren
// en <10, esto da margen para colas.
var CIPollTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_CLOSE_CI_POLL_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 20 * time.Minute
}()

// CIPollInterval es cuánto esperamos entre polls de CI. Configurable con
// CHE_CLOSE_CI_POLL_INTERVAL_SECS. Default 15s.
var CIPollInterval = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_CLOSE_CI_POLL_INTERVAL_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 15 * time.Second
}()

// Opts agrupa el writer de stdout (payload) y el logger estructurado
// (progress + errors). Si Out es nil, el flow corre silencioso.
type Opts struct {
	Stdout io.Writer
	Out    *output.Logger
	// KeepBranch omite el --delete-branch del merge y el cleanup del
	// worktree asociado. Default false: tras mergear, che close borra la
	// branch remota/local y remueve el worktree para dejar el repo limpio.
	KeepBranch bool
}

// PullRequest modela el subset de `gh pr view --json ...` que usamos.
// Incluye los campos que validate.PullRequest no tiene (mergeable,
// mergeStateStatus) porque close los necesita para decidir si hay
// conflictos. Los comments se fetchan por separado.
type PullRequest struct {
	Number           int    `json:"number"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	HeadBranch       string `json:"headRefName"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Author           struct {
		Login string `json:"login"`
	} `json:"author"`
	ClosingIssuesReferences []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	} `json:"closingIssuesReferences"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// HasLabel devuelve true si el PR tiene el label.
func (p *PullRequest) HasLabel(name string) bool {
	for _, l := range p.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Check es el subset de un item de `gh pr checks --json ...` que usamos.
// Los estados posibles incluyen: SUCCESS, FAILURE, PENDING, IN_PROGRESS,
// SKIPPED, NEUTRAL, CANCELLED, TIMED_OUT, ACTION_REQUIRED.
//
// Los campos que pedimos a `gh pr checks --json` están acotados a los que
// el CLI de gh acepta: name, state, link, workflow, startedAt, completedAt,
// description, bucket, event. Campos como "completed" o "conclusion" NO
// son aceptados por gh (aunque aparecen en la API REST) — pedirlos falla
// con "Unknown JSON field". De los soportados sólo usamos los 4 primeros.
type Check struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Link     string `json:"link"`
	Workflow string `json:"workflow"`
}

// CIState es el estado agregado de todos los checks del PR.
type CIState int

const (
	// CINone significa que el PR no tiene checks configurados — para close
	// lo tratamos como verde (no bloquea).
	CINone CIState = iota
	CIGreen
	CIFailing
	CIPending
)

func (s CIState) String() string {
	switch s {
	case CINone:
		return "none"
	case CIGreen:
		return "green"
	case CIFailing:
		return "failing"
	case CIPending:
		return "pending"
	}
	return "unknown"
}

// aggregateCIState agrega el estado de una lista de checks:
//   - Sin checks → CINone (no bloquea).
//   - Hay al menos uno failing (FAILURE, CANCELLED, TIMED_OUT,
//     ACTION_REQUIRED) → CIFailing.
//   - Hay al menos uno pendiente y ninguno failing → CIPending.
//   - Todos SUCCESS/SKIPPED/NEUTRAL → CIGreen.
func aggregateCIState(checks []Check) CIState {
	if len(checks) == 0 {
		return CINone
	}
	hasPending := false
	for _, c := range checks {
		switch strings.ToUpper(c.State) {
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED":
			return CIFailing
		case "PENDING", "IN_PROGRESS", "QUEUED", "REQUESTED", "WAITING":
			hasPending = true
		}
	}
	if hasPending {
		return CIPending
	}
	return CIGreen
}

// failingChecks devuelve los checks que están en estado "failing" para
// construir el prompt del agente (solo los que requieren fix).
func failingChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		switch strings.ToUpper(c.State) {
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED":
			out = append(out, c)
		}
	}
	return out
}

// BlockingVerdict devuelve el label validated:* que bloquea el merge si
// está presente en el PR, o "" si ninguno bloquea. validated:approve NO
// bloquea; validated:changes-requested y validated:needs-human sí.
func BlockingVerdict(pr *PullRequest) string {
	for _, l := range pr.Labels {
		if l.Name == labels.ValidatedChangesRequested || l.Name == labels.ValidatedNeedsHuman {
			return l.Name
		}
	}
	return ""
}

// closeFromState devuelve el estado de origen de la máquina al momento
// del gate (che:executed o che:validated). Devuelve "" si el PR no tiene
// ninguno — el gate aborta. Prefiere che:validated cuando ambos están
// presentes (path normal post-validate).
func closeFromState(p *PullRequest) string {
	if p.HasLabel(labels.CheValidated) {
		return labels.CheValidated
	}
	if p.HasLabel(labels.CheExecuted) {
		return labels.CheExecuted
	}
	return ""
}

// hasConflicts devuelve true si el PR tiene conflictos con la base y
// necesita resolución manual/del agente antes de poder mergear.
// mergeStateStatus puede venir vacío en fetchs rápidos; mergeable es más
// confiable para este chequeo.
func hasConflicts(pr *PullRequest) bool {
	return strings.EqualFold(pr.Mergeable, "CONFLICTING") ||
		strings.EqualFold(pr.MergeStateStatus, "DIRTY")
}

// CloseablePRs agrupa los PRs abiertos del repo para la TUI de close en
// dos categorías: Ready (sin verdict bloqueante — el camino feliz) y
// Blocked (tienen validated:changes-requested o validated:needs-human).
// La TUI las renderiza como secciones separadas para que el usuario las
// distinga visualmente, pero puede elegir cualquiera — close no impone
// un gate, solo warnea.
type CloseablePRs struct {
	Ready   []validate.Candidate
	Blocked []validate.Candidate
}

// ListCloseable devuelve los PRs abiertos del repo, agrupados entre los
// que están listos para cerrar (aprobados o sin validar) y los que tienen
// un verdict bloqueante de che validate.
//
// Ambos grupos aparecen — che close no esconde ni rechaza los bloqueantes,
// el humano decide. La agrupación existe solo para UX.
func ListCloseable() (CloseablePRs, error) {
	raw, err := validate.FetchOpenPullRequests()
	if err != nil {
		return CloseablePRs{}, err
	}
	return groupCloseable(raw), nil
}

// groupCloseable separa el raw de PRs en ready/blocked según labels.
// Función pura, testeable sin shell-out.
func groupCloseable(raw []validate.PullRequest) CloseablePRs {
	out := CloseablePRs{}
	for _, p := range raw {
		if p.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado.
		}
		c := validate.ToCandidate(p)
		if hasBlockingLabel(p) {
			out.Blocked = append(out.Blocked, c)
		} else {
			out.Ready = append(out.Ready, c)
		}
	}
	return out
}

// hasBlockingLabel devuelve true si el PR tiene un validated:* que suele
// bloquear el merge. Se usa en la TUI para agrupar y en Run para warnear.
func hasBlockingLabel(p validate.PullRequest) bool {
	for _, l := range p.Labels {
		if l.Name == labels.ValidatedChangesRequested || l.Name == labels.ValidatedNeedsHuman {
			return true
		}
	}
	return false
}

// ---- Run ----

// Run ejecuta el flow completo sobre un PR. Decisiones:
//   - Preflight: repo git + gh auth.
//   - Fetch PR + gate (open, no validator reject).
//   - Si draft → ready al inicio.
//   - Loop MaxFixAttempts: detectar problemas; si hay conflictos o CI rojo
//     invocar opus en worktree (reuse .worktrees/issue-N si existe, o crear
//     uno sobre la head branch del PR), commit+push, poll CI.
//   - Merge con merge commit. Post-merge, delete de la branch remota vía
//     gh api (salvo --keep-branch). No pasamos --delete-branch a gh pr
//     merge: ese flag hace delete local también, que falla si la branch
//     está checkouteada en un worktree y arrastra el exit code aunque el
//     merge remoto haya ocurrido.
//   - Cerrar issue asociado + transición de labels a che:closed (vía che:closing).
//   - Cleanup del worktree (ver shouldCleanupWorktree):
//     · --keep-branch inhibe siempre, aunque el worktree sea propio del run.
//     · happy path (merge OK): limpia el worktree asociado, sea propio o
//     reusado/auto-detectado bajo .worktrees/.
//     · failure path: limpia solo si el worktree es propio del run, para no
//     borrar trabajo del usuario en worktrees reusados.
func Run(prRef string, opts Opts) ExitCode {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	log := opts.Out
	if log == nil {
		log = output.New(nil)
	}
	keepBranch := opts.KeepBranch

	prRef = strings.TrimSpace(prRef)
	if prRef == "" {
		log.Error("pr ref is empty")
		return ExitSemantic
	}
	if _, err := validate.ParseRef(prRef); err != nil {
		log.Error("pr ref invalido", output.F{Cause: err})
		return ExitSemantic
	}

	log.Info("chequeando repo git y auth de GitHub")
	repoRoot, err := repoToplevel()
	if err != nil {
		log.Error("git repo invalido", output.F{Cause: err})
		return ExitRetry
	}
	if err := precheckGitHubRemote(); err != nil {
		log.Error("github remote invalido", output.F{Cause: err})
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		log.Error("gh auth fallo", output.F{Cause: err})
		return ExitRetry
	}

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

	// Gate de máquina de estados: aceptamos che:executed o che:validated.
	// `from` se captura para que el lock + el rollback usen el mismo
	// origen (executed por path normal, validated post-validate).
	prFrom := closeFromState(pr)
	if prFrom == "" {
		log.Error(fmt.Sprintf("PR #%d no está en che:executed ni che:validated — corré `che execute` o `che validate` antes", pr.Number))
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

	// Transición <prFrom> → che:closing. Rollback en defer LIFO si
	// stateClosed queda en false.
	log.Step("transicionando a che:closing", output.F{PR: pr.Number})
	if err := labels.Apply(prRef, prFrom, labels.CheClosing); err != nil {
		log.Error("no pude transicionar a che:closing", output.F{Cause: err})
		return ExitRetry
	}
	var stateClosed bool
	defer func() {
		if stateClosed {
			return
		}
		if err := labels.Apply(prRef, labels.CheClosing, prFrom); err != nil {
			log.Warn(fmt.Sprintf("rollback che:closing → %s fallo: %v — revisá labels a mano", prFrom, err))
		}
	}()

	if blocking := BlockingVerdict(pr); blocking != "" {
		log.Warn(fmt.Sprintf("warning: PR #%d tiene verdict bloqueante — procedo igual porque así lo pediste", pr.Number),
			output.F{Labels: []string{blocking}})
	}

	if pr.IsDraft {
		log.Step(fmt.Sprintf("PR #%d está en draft — pasando a ready for review", pr.Number))
		if err := prReady(prRef); err != nil {
			log.Error("gh pr ready fallo", output.F{PR: pr.Number, Cause: err})
			return ExitRetry
		}
	}

	// Loop de fix. En cada iteración: detectar problemas, si hay → invocar
	// agente en worktree, commit+push, poll CI.
	var (
		wt           *execute.Worktree
		wtOwned      bool // true si lo creamos en este run (cleanup al final)
		mergedOK     bool // true tras retorno exitoso de mergePR
		lastProblems []string
	)

	// Cleanup del worktree. shouldCleanupWorktree decide si corresponde:
	//   - --keep-branch SIEMPRE inhibe el cleanup (aunque hayamos creado el
	//     worktree en este run). El usuario pidió explícitamente preservar.
	//   - happy path sin --keep-branch: limpia el worktree asociado.
	//   - failure path: limpia solo si lo creamos (no dejar residuo propio).
	defer func() {
		if !shouldCleanupWorktree(mergedOK, keepBranch, wtOwned) {
			return
		}
		if wt == nil {
			return
		}
		wtCtx, wtCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer wtCancel()
		cleanupWorktree(wtCtx, log, repoRoot, wt)
	}()

	issueNum := firstClosingIssue(pr)

	for attempt := 1; attempt <= MaxFixAttempts+1; attempt++ {
		// Refrescar estado del PR (mergeable puede venir UNKNOWN en la
		// primera vista; github lo computa lazy).
		log.Step("chequeando estado del PR", output.F{Attempt: attempt, Total: MaxFixAttempts + 1, PR: pr.Number})
		freshPR, err := FetchPR(prRef)
		if err != nil {
			log.Error("fetching PR failed", output.F{Cause: err})
			return ExitRetry
		}
		pr = freshPR

		checks, err := FetchChecks(prRef)
		if err != nil {
			log.Error("fetching checks failed", output.F{Cause: err})
			return ExitRetry
		}

		ci := aggregateCIState(checks)
		conflict := hasConflicts(pr)
		problems := problemsList(conflict, ci)
		lastProblems = problems

		if len(problems) == 0 {
			log.Success("PR mergeable y CI verde", output.F{PR: pr.Number})
			break
		}

		if attempt > MaxFixAttempts {
			log.Error(fmt.Sprintf("agotados %d intentos de fix; problemas restantes: %s",
				MaxFixAttempts, strings.Join(problems, ", ")),
				output.F{PR: pr.Number})
			return ExitRetry
		}

		log.Warn(fmt.Sprintf("problemas detectados: %s — intentando fix con opus", strings.Join(problems, ", ")),
			output.F{PR: pr.Number, Agent: "opus"})

		if wt == nil {
			w, owned, err := resolveWorktree(repoRoot, issueNum, pr.HeadBranch)
			if err != nil {
				log.Error("resolver worktree fallo", output.F{Cause: err})
				return ExitRetry
			}
			wt = w
			wtOwned = owned
		}

		prompt := buildFixPrompt(pr, conflict, failingChecks(checks))
		if err := runAgent(wt.Path, prompt, log); err != nil {
			log.Error("agente falló", output.F{Agent: "opus", Cause: err})
			return ExitRetry
		}

		log.Step("esperando que CI re-evalúe tras el push del agente")
		if err := waitCIStable(prRef, log); err != nil {
			// waitCIStable no retorna error fatal si vencía el timeout —
			// solo si gh explotó. En timeout seguimos al próximo intento.
			log.Warn(fmt.Sprintf("warning: %v", err))
		}
	}

	// Si vamos a borrar la branch tras mergear, ubicar el worktree asociado
	// ahora (si el fix loop no lo hizo ya). Necesitamos conocerlo para que
	// el defer limpie tras el retorno exitoso.
	//
	// Restringimos el auto-lookup a worktrees bajo `.worktrees/` del repo:
	// nunca tocamos el worktree principal (repoRoot) ni worktrees que el
	// usuario haya creado en otros paths — esos son suyos aunque tengan la
	// head branch del PR checkouteada.
	if wt == nil && !keepBranch {
		if p, _ := findWorktreePathByBranch(repoRoot, pr.HeadBranch); p != "" && isCheManagedWorktree(repoRoot, p) {
			wt = &execute.Worktree{Path: p, Branch: pr.HeadBranch}
			wtOwned = false
		}
	}

	// Snapshot del remote antes del merge para distinguir "lo borramos"
	// de "ya no estaba" (auto-delete de GitHub o borrado manual previo).
	preRemoteMissing, preRemoteKnown := false, false
	if !keepBranch {
		exists, known := remoteBranchExists(repoRoot, pr.HeadBranch)
		preRemoteMissing = known && !exists
		preRemoteKnown = known
	}

	log.Step(fmt.Sprintf("mergeando PR #%d con merge commit", pr.Number))
	if err := mergePR(prRef); err != nil {
		log.Error("gh pr merge fallo", output.F{PR: pr.Number, Cause: err})
		return ExitRetry
	}
	mergedOK = true

	// Delete remoto post-merge. Solo si el usuario no pidió --keep-branch y
	// la branch seguía en remote antes del merge. Si falla, warn y seguimos:
	// el merge ya ocurrió, no queremos devolver ExitRetry por un cleanup.
	remoteDeleteFailed := false
	if !keepBranch && !(preRemoteKnown && preRemoteMissing) {
		if err := deleteRemoteBranch(pr.HeadBranch); err != nil {
			log.Warn(fmt.Sprintf("warning: no pude borrar branch remota %s — borrala a mano con: git push origin --delete %s",
				pr.HeadBranch, pr.HeadBranch),
				output.F{Cause: err})
			remoteDeleteFailed = true
		}
	}

	fmt.Fprintln(stdout, branchOutcomeMessage(pr.HeadBranch, keepBranch, preRemoteKnown, preRemoteMissing, remoteDeleteFailed))

	// Cierre de la transición de máquina de estados: che:closing → che:closed.
	// El PR queda en estado terminal `che:closed`, consistente con el merge
	// remoto. Best-effort: si falla, warneamos pero el merge ya ocurrió.
	log.Step("transicionando PR a che:closed", output.F{PR: pr.Number})
	if err := labels.Apply(prRef, labels.CheClosing, labels.CheClosed); err != nil {
		log.Warn(fmt.Sprintf("no pude transicionar PR a che:closed: %v — revisá labels a mano", err))
	} else {
		stateClosed = true
	}

	// Cerrar issues asociados. Después del merge, los que tenían "closes #N"
	// en el body del PR quedan OPEN un tiempo hasta que github procesa el
	// auto-close; hacemos un close explícito para no depender de eso y para
	// aplicar la transición de labels atómicamente con el cierre.
	closedIssues := closeAssociatedIssues(pr, log)

	log.Success("merged PR", output.F{PR: pr.Number, URL: pr.URL})
	fmt.Fprintf(stdout, "Closed PR %s\n", pr.URL)
	if len(closedIssues) > 0 {
		fmt.Fprintf(stdout, "Cerrado(s) issue(s): %s\n", joinInts(closedIssues))
	}
	if len(lastProblems) > 0 {
		fmt.Fprintf(stdout, "(todo verde al final tras fix de: %s)\n", strings.Join(lastProblems, ", "))
	}
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

// problemsList traduce conflict + ci a strings legibles para logs y para
// construir el prompt del agente.
func problemsList(conflict bool, ci CIState) []string {
	var out []string
	if conflict {
		out = append(out, "conflicts")
	}
	switch ci {
	case CIFailing:
		out = append(out, "ci-failing")
	case CIPending:
		out = append(out, "ci-pending")
	}
	return out
}

// firstClosingIssue devuelve el número del primer issue referenciado por
// "Closes #N" en el PR, o 0 si no hay. Si hay varios, devolvemos el primero
// — el path de worktree usa ese N.
func firstClosingIssue(pr *PullRequest) int {
	for _, r := range pr.ClosingIssuesReferences {
		if r.Number > 0 {
			return r.Number
		}
	}
	return 0
}

// closeAssociatedIssues cierra TODOS los issues referenciados con "closes
// #N" en el PR (puede haber más de uno) que estén todavía OPEN, y les
// aplica la transición de labels a che:closed cuando corresponde. La
// transición intermedia es <from> → che:closing → che:closed, donde
// <from> puede ser che:executed o che:validated según el path. Best-
// effort: si los labels no están como esperamos, logueamos y seguimos.
// Devuelve la lista de números de issues que se cerraron efectivamente
// (para reporte en stdout).
func closeAssociatedIssues(pr *PullRequest, log *output.Logger) []int {
	var closed []int
	for _, ref := range pr.ClosingIssuesReferences {
		if ref.Number == 0 {
			continue
		}
		refStr := fmt.Sprintf("%d", ref.Number)
		// Si github ya lo cerró por el merge auto, el issueClose es
		// idempotente (devuelve error que ignoramos) pero la transición de
		// labels sí queremos aplicarla igual.
		log.Step("cerrando issue", output.F{Issue: ref.Number})
		if err := issueClose(refStr); err != nil {
			log.Warn(fmt.Sprintf("warning: no se pudo cerrar issue #%d", ref.Number),
				output.F{Issue: ref.Number, Cause: err})
		} else {
			closed = append(closed, ref.Number)
			log.Success("issue cerrado", output.F{Issue: ref.Number, Labels: []string{labels.CheClosed}})
		}
		// Transición de labels best-effort. Probamos primero che:executed y
		// si falla che:validated. Si el issue no tiene ninguno (ej.
		// referenciado pero no manejado por che) los dos fallan y warneamos.
		if err := labels.Apply(refStr, labels.CheExecuted, labels.CheClosing); err != nil {
			if err2 := labels.Apply(refStr, labels.CheValidated, labels.CheClosing); err2 != nil {
				log.Warn(fmt.Sprintf("warning: labels del issue #%d no transicionaron a che:closing (executed: %v · validated: %v)",
					ref.Number, err, err2),
					output.F{Issue: ref.Number})
				continue
			}
		}
		if err := labels.Apply(refStr, labels.CheClosing, labels.CheClosed); err != nil {
			log.Warn(fmt.Sprintf("warning: labels del issue #%d no transicionaron a che:closed", ref.Number),
				output.F{Issue: ref.Number, Cause: err})
		}
	}
	return closed
}

// joinInts formatea una lista de ints como "#1, #2, #3".
func joinInts(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, fmt.Sprintf("#%d", n))
	}
	return strings.Join(parts, ", ")
}

// ---- worktree resolution ----

// resolveWorktree devuelve un worktree usable para que el agente opere.
// Preferencia (en orden):
//  1. Si existe .worktrees/issue-<issueNum> y su branch coincide con la
//     head del PR → reusarlo (ideal: el que dejó execute).
//  2. Si no existe, crear uno con git worktree add sobre la head branch
//     del PR. El path sigue la convención .worktrees/issue-<issueNum>
//     si hay issueNum, o .worktrees/pr-<headBranch-slug> si no.
//
// owned=true cuando nosotros lo creamos (el defer del Run hace cleanup);
// owned=false cuando lo reusamos (no limpiamos un worktree que otro
// proceso/run dejó preparado).
func resolveWorktree(repoRoot string, issueNum int, headBranch string) (*execute.Worktree, bool, error) {
	if strings.TrimSpace(headBranch) == "" {
		return nil, false, fmt.Errorf("PR sin head branch — no puedo crear worktree")
	}

	// Antes que nada: si la branch ya está checkouteada en algún worktree
	// (ej. el que dejó execute en .worktrees/issue-<N>), reusamos ese path.
	// Git no permite dos worktrees en la misma branch — sin esta búsqueda
	// por-branch, close fallaría con "branch is already used by worktree at
	// ...".
	if p, err := findWorktreePathByBranch(repoRoot, headBranch); err != nil {
		return nil, false, err
	} else if p != "" {
		return &execute.Worktree{Path: p, Branch: headBranch}, false, nil
	}

	path := worktreePathFor(repoRoot, issueNum, headBranch)

	// Chequear si ya existe. Usamos `git worktree list --porcelain` y
	// buscamos el path; si la branch coincide, reusamos.
	existing, err := existingWorktreeBranch(repoRoot, path)
	if err != nil {
		return nil, false, err
	}
	if existing != "" {
		if existing != headBranch {
			return nil, false, fmt.Errorf("worktree %s ya existe en branch %q, pero el PR está en %q — resolvé manualmente con `git worktree remove %s`",
				path, existing, headBranch, path)
		}
		return &execute.Worktree{Path: path, Branch: headBranch}, false, nil
	}

	// No existe. Crear: fetch origin/<branch>, checkout en path.
	skipFetch := os.Getenv("CHE_CLOSE_SKIP_FETCH") == "1"
	if !skipFetch {
		if err := runGit(repoRoot, "fetch", "origin", headBranch); err != nil {
			return nil, false, fmt.Errorf("git fetch origin %s: %w — para tests locales sin red setear CHE_CLOSE_SKIP_FETCH=1", headBranch, err)
		}
	}

	// Si la branch existe localmente usamos `worktree add <path> <branch>`,
	// sino `worktree add <path> origin/<branch>` (detached → attach) no es
	// directo; lo más simple: crear la branch local tracking origin y
	// después worktree add.
	if ok, err := localBranchExists(repoRoot, headBranch); err != nil {
		return nil, false, err
	} else if !ok {
		// Crear branch local tracking origin/<branch>.
		if err := runGit(repoRoot, "branch", headBranch, "origin/"+headBranch); err != nil {
			// Puede fallar si origin/<branch> no existe (tests sin red).
			// Fallback: crear desde HEAD para no morir — el agente va a
			// resolver el estado igual.
			if err2 := runGit(repoRoot, "branch", headBranch); err2 != nil {
				return nil, false, fmt.Errorf("git branch %s: %v (fallback sin origin también falló: %v)", headBranch, err, err2)
			}
		}
	}

	if err := runGit(repoRoot, "worktree", "add", path, headBranch); err != nil {
		return nil, false, fmt.Errorf("git worktree add %s %s: %w", path, headBranch, err)
	}

	return &execute.Worktree{Path: path, Branch: headBranch}, true, nil
}

// worktreePathFor calcula la ruta del worktree a usar. Si hay issueNum
// preferimos .worktrees/issue-<N> (coherente con execute). Si no,
// .worktrees/pr-<branch-sanitized>.
func worktreePathFor(repoRoot string, issueNum int, headBranch string) string {
	if issueNum > 0 {
		return filepath.Join(repoRoot, ".worktrees", fmt.Sprintf("issue-%d", issueNum))
	}
	slug := sanitizeBranchSlug(headBranch)
	return filepath.Join(repoRoot, ".worktrees", "pr-"+slug)
}

// sanitizeBranchSlug convierte una branch name a slug apto para ser parte
// de un path, reemplazando "/" y caracteres raros por "-".
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
// ninguno. Dual de existingWorktreeBranch (busca por path); usado para
// reusar el worktree que dejó execute aunque esté en un path que close
// calcularía distinto.
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

// existingWorktreeBranch consulta `git worktree list --porcelain` y
// devuelve la branch del worktree en path si existe, "" si no.
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

// canonPath resuelve symlinks para comparar paths robustamente en macOS
// donde /var y /private/var se mezclan.
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

// localBranchExists devuelve true si existe refs/heads/<branch>.
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

// ---- agent invocation ----

// buildFixPrompt arma el prompt del agente según los problemas detectados.
// Incluye: contexto del PR (título, branch), tipo de problema (conflictos
// y/o CI), y las instrucciones para que resuelva end-to-end dentro del
// worktree (git ops + push).
func buildFixPrompt(pr *PullRequest, conflicts bool, failingChecks []Check) string {
	var sb strings.Builder
	sb.WriteString("Sos un ingeniero senior encargado de dejar este PR mergeable. ")
	sb.WriteString("Estás parado en el worktree de la branch ")
	sb.WriteString("`" + pr.HeadBranch + "` (el cwd ya está en el worktree). ")
	sb.WriteString("Tu objetivo es arreglar los problemas listados y pushear los commits; ")
	sb.WriteString("no abras PRs nuevos ni toques otras branches.\n\n")

	sb.WriteString(fmt.Sprintf("PR #%d — %s\n", pr.Number, pr.Title))
	sb.WriteString("URL: " + pr.URL + "\n\n")

	sb.WriteString("Problemas a resolver:\n")
	if conflicts {
		sb.WriteString("- **Conflictos con main**: el PR tiene conflictos. Hacé `git fetch origin main` y `git merge origin/main` (o rebase, si preferís) y resolvé los conflictos en los archivos afectados. Si los conflictos son triviales (imports, formato), resolvelos directo. Si tocan lógica, preservá el intent del PR sobre el de main.\n")
	}
	if len(failingChecks) > 0 {
		sb.WriteString("- **CI rojo**: los siguientes checks están fallando:\n")
		for _, c := range failingChecks {
			line := fmt.Sprintf("  - %s (state=%s", c.Name, c.State)
			if c.Workflow != "" {
				line += ", workflow=" + c.Workflow
			}
			if c.Link != "" {
				line += ", log=" + c.Link
			}
			line += ")\n"
			sb.WriteString(line)
		}
		sb.WriteString("  Usá `gh run view --log-failed <run-id>` o `gh pr checks " + fmt.Sprintf("%d", pr.Number) + "` para leer los logs, identificá la falla real (no parchees síntomas), arreglá el código o los tests y commiteá.\n")
	}
	sb.WriteString("\n")

	sb.WriteString("Workflow esperado (ejecutalo end-to-end):\n")
	sb.WriteString("1. Leé el estado del worktree con `git status` y `git log --oneline -5`.\n")
	sb.WriteString("2. Arreglá el problema en los archivos. NO toques nada fuera del scope del fix.\n")
	sb.WriteString("3. Verificá localmente si podés (ej. `go test ./...` si es Go) antes de commitear.\n")
	sb.WriteString("4. `git add -A && git commit -m \"fix: <descripción corta>\"`\n")
	sb.WriteString("5. `git push` (la branch ya tiene upstream configurado).\n")
	sb.WriteString("6. Reportá qué hiciste en 2-3 líneas al final.\n\n")

	sb.WriteString("Reglas:\n")
	sb.WriteString("- No cambies la base del PR ni abras PRs nuevos.\n")
	sb.WriteString("- No hagas force-push que pise commits ajenos — usá push normal (la branch es tuya en el worktree).\n")
	sb.WriteString("- Si el fix es imposible sin decisión humana (ambigüedad de producto, requiere secret, etc.), no commitees nada y explicá por qué. El harness va a detectar que no hubo push y abortar.\n")
	return sb.String()
}

// runAgent invoca al CLI de claude (opus) con el prompt, corriendo con cwd
// en el worktree para que los tool use (Edit, Bash) afecten esa copia del
// repo. Usa stream-json + formatOpusLine para que los eventos aparezcan en
// el progress. Mismo patrón que execute.runAgent.
func runAgent(cwd, prompt string, log *output.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), AgentTimeout)
	defer cancel()

	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	cmd := exec.CommandContext(ctx, AgentBinary, args...)
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
		return fmt.Errorf("starting %s: %w", AgentBinary, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var stderrBuf strings.Builder
	go streamPipe(&wg, stdoutPipe, nil, log, "opus: ", formatOpusLine)
	go streamPipe(&wg, stderrPipe, &stderrBuf, log, "opus stderr: ", nil)
	wg.Wait()

	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		if se := strings.TrimSpace(stderrBuf.String()); se != "" {
			return fmt.Errorf("opus timed out after %s; stderr: %s", AgentTimeout, se)
		}
		return fmt.Errorf("opus timed out after %s (subí CHE_CLOSE_AGENT_TIMEOUT_SECS)", AgentTimeout)
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return fmt.Errorf("opus exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderrBuf.String()))
		}
		return waitErr
	}
	return nil
}

// streamPipe lee un pipe y reenvía las líneas al progress. Si format no
// es nil, cada línea se pasa por él. Igual patrón que execute.
func streamPipe(wg *sync.WaitGroup, r io.Reader, acc *strings.Builder, log *output.Logger, prefix string, format func(string) (string, bool)) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if acc != nil {
			acc.WriteString(line + "\n")
		}
		out := line
		if format != nil {
			msg, ok := format(line)
			if !ok {
				continue
			}
			out = msg
		}
		if strings.TrimSpace(out) != "" && log != nil {
			log.Step(prefix + out)
		}
	}
}

// formatOpusLine traduce una línea del stream-json de claude a un mensaje
// corto. Lógica simplificada vs execute.formatOpusLine — close no necesita
// describir todas las tool use en detalle, solo tener feedback visible.
func formatOpusLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return line, true
	}
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content []struct {
				Type  string                 `json:"type"`
				Name  string                 `json:"name"`
				Input map[string]interface{} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return line, true
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return "sesión lista, arrancando…", true
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" {
				return describeOpusTool(c.Name, c.Input), true
			}
		}
	case "result":
		if ev.Subtype == "success" {
			return "agente terminó OK", true
		}
		if ev.Subtype != "" {
			return "agente terminó (" + ev.Subtype + ")", true
		}
	}
	return "", false
}

// describeOpusTool acompaña el nombre de la tool con un detalle
// (path/command/pattern) para que los logs del stream-json sean útiles.
// Copia del helper de execute/iterate.
func describeOpusTool(name string, input map[string]interface{}) string {
	detail := ""
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		if v, ok := input["file_path"].(string); ok {
			detail = v
		}
	case "Bash":
		if v, ok := input["command"].(string); ok {
			detail = truncateCmd(v, 80)
		}
	case "Glob", "Grep":
		if v, ok := input["pattern"].(string); ok {
			detail = v
		}
	case "Task":
		if v, ok := input["description"].(string); ok {
			detail = v
		}
	case "WebFetch":
		if v, ok := input["url"].(string); ok {
			detail = v
		}
	}
	if detail == "" {
		return "tool: " + name
	}
	return "tool: " + name + " " + detail
}

func truncateCmd(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// ---- gh / git shellouts ----

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

// FetchPR corre `gh pr view <ref> --json ...` con los campos que close
// necesita, incluyendo mergeable y mergeStateStatus.
func FetchPR(ref string) (*PullRequest, error) {
	cmd := exec.Command("gh", "pr", "view", ref,
		"--json", "number,title,url,state,isDraft,author,headRefName,mergeable,mergeStateStatus,closingIssuesReferences,labels")
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

// FetchChecks corre `gh pr checks <ref> --json ...` y parsea los checks
// del PR. El output puede ser un array JSON (nuevas versiones de gh) o un
// objeto con key "checks" — parseamos ambas.
func FetchChecks(ref string) ([]Check, error) {
	cmd := exec.Command("gh", "pr", "checks", ref,
		"--json", "name,state,link,workflow")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(ee.Stderr))
			switch {
			case ee.ExitCode() == 8 && stderr == "":
				// `gh pr checks` sale con exit=8 cuando hay checks que
				// fallan — el stdout igual viene bien formado. Tratamos 8
				// como OK y dejamos que aggregateCIState detecte el failure.
			case strings.Contains(stderr, "no checks reported"):
				// PR sin checks configurados o con CI bloqueado por
				// conflicts: gh sale con exit=1 y tira "no checks reported
				// on the '<branch>' branch". Semánticamente es CINone —
				// no hay nada que parsear, devolvemos slice vacío.
				return nil, nil
			default:
				return nil, fmt.Errorf("gh pr checks: %s", stderr)
			}
		}
	}
	out = trimBOM(out)
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	// Intentar array JSON directo primero.
	var arr []Check
	if err := json.Unmarshal(out, &arr); err == nil {
		return arr, nil
	}
	// Fallback: objeto con key "checks".
	var wrap struct {
		Checks []Check `json:"checks"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse gh pr checks output: %w", err)
	}
	return wrap.Checks, nil
}

// trimBOM quita un BOM UTF-8 al inicio del output si lo hay.
func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf {
		return b[3:]
	}
	return b
}

// prReady pasa el PR de draft a ready-for-review con `gh pr ready`.
func prReady(ref string) error {
	cmd := exec.Command("gh", "pr", "ready", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr ready: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// mergePR mergea el PR con merge commit (--merge). No usamos --auto porque
// ya chequeamos CI antes; --auto suma latencia innecesaria.
//
// No pasamos --delete-branch: gh hace el merge remoto + delete de branch en
// un solo paso, pero el delete local falla (exit != 0) cuando la branch
// está checkouteada en un worktree, aunque el merge remoto haya ocurrido.
// Separamos las dos operaciones: mergePR hace solo el merge, y post-merge
// llamamos a deleteRemoteBranch + cleanupWorktree para el resto.
func mergePR(ref string) error {
	args := mergePRArgs(ref)
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// mergePRArgs construye los args de gh pr merge. Extraído para testabilidad.
func mergePRArgs(ref string) []string {
	return []string{"pr", "merge", ref, "--merge"}
}

// deleteRemoteBranch borra la branch remota vía `gh api`. Lo hacemos
// nosotros en vez de pasar --delete-branch a gh pr merge porque ese flag
// hace merge + delete local + delete remoto de manera atómica, y el delete
// local falla si la branch está checkouteada en un worktree (che execute
// deja un worktree por PR) — arrastrando el exit code aunque el merge haya
// ocurrido.
//
// Usamos gh api y no `git push origin --delete` porque el resto del flow
// está en gh (precheck, auth, pr view, pr merge) — mantener una sola capa
// de creds simplifica el diagnóstico cuando falla.
//
// Idempotente: si la branch ya no existe remotamente (GitHub auto-delete
// le ganó, o alguien la borró antes), devolvemos nil.
func deleteRemoteBranch(branch string) error {
	path := "repos/{owner}/{repo}/git/refs/heads/" + branch
	cmd := exec.Command("gh", "api", "-X", "DELETE", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	stderr := strings.TrimSpace(string(out))
	if strings.Contains(stderr, "Reference does not exist") ||
		strings.Contains(stderr, "Not Found") {
		return nil
	}
	return fmt.Errorf("gh api: %s", stderr)
}

// issueClose cierra un issue (gh issue close). Idempotente: si ya está
// cerrado, gh devuelve error pero lo ignoramos upstream.
func issueClose(ref string) error {
	cmd := exec.Command("gh", "issue", "close", ref)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue close: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// runGit corre `git -C repoRoot args...` y devuelve error con output si
// falla. Igual patrón que execute.runGit.
func runGit(repoRoot string, args ...string) error {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// cleanupWorktree remueve el worktree asociado al PR mergeado y borra la
// branch local residual. Todo error acá es post-merge: emitimos warning a
// stderr y seguimos — el merge ya ocurrió.
//
// Contrato: solo toca worktrees administrados por che (bajo
// `<repoRoot>/.worktrees/`). Si el path es el worktree principal o un
// worktree creado por el usuario fuera de ese prefijo, NO lo tocamos —
// avisamos por stderr para que el humano decida. Hacerlo gratis sería un
// side effect no anunciado del `che close`.
func cleanupWorktree(ctx context.Context, log *output.Logger, repoRoot string, wt *execute.Worktree) {
	if wt == nil {
		return
	}
	if samePath(repoRoot, wt.Path) {
		log.Warn(fmt.Sprintf("warning: la branch del PR estaba checkouteada en el worktree principal (%s) — che close no modifica el cwd del usuario. La branch remota fue borrada post-merge; la local queda hasta que la elimines con git branch -D %s", wt.Path, wt.Branch))
		return
	}
	if !isCheManagedWorktree(repoRoot, wt.Path) {
		log.Warn(fmt.Sprintf("warning: la branch del PR estaba checkouteada en un worktree fuera de .worktrees/ (%s) — che close no toca worktrees que no creó. Limpialo a mano con git worktree remove %s", wt.Path, wt.Path))
		return
	}
	if err := wt.Cleanup(ctx, repoRoot, false); err != nil {
		log.Warn("cleanup local parcial — revisá git worktree list y git branch para limpiar a mano", output.F{Cause: err})
	}
}

// isCheManagedWorktree devuelve true si path está bajo
// `<repoRoot>/.worktrees/` (el directorio que usamos para los worktrees que
// che crea). Comparamos canonicalizando symlinks (macOS /var vs /private/var).
func isCheManagedWorktree(repoRoot, path string) bool {
	root, err := canonPath(filepath.Join(repoRoot, ".worktrees"))
	if err != nil {
		return false
	}
	target, err := canonPath(path)
	if err != nil {
		return false
	}
	if target == root {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// shouldCleanupWorktree decide si el defer del Run debe invocar el cleanup.
// Contratos (en orden de precedencia):
//   - --keep-branch: nunca limpia, aunque el worktree lo hayamos creado.
//     El usuario pidió explícitamente preservar.
//   - happy path (mergedOK=true) sin --keep-branch: limpia.
//   - failure path (mergedOK=false): limpia solo si wtOwned, para no dejar
//     residuo propio. Los worktrees reusados no se tocan.
func shouldCleanupWorktree(mergedOK, keepBranch, wtOwned bool) bool {
	if keepBranch {
		return false
	}
	return mergedOK || wtOwned
}

// branchOutcomeMessage formatea la línea de stdout post-merge sobre el destino
// de la branch remota. Encapsula los casos para poder testearlos sin
// stubbear ls-remote ni el delete remoto:
//   - keepBranch: el usuario pidió preservar.
//   - preRemoteKnown && preRemoteMissing: la branch ya no estaba antes del
//     merge (auto-delete previo o borrado manual). No intentamos borrar.
//   - remoteDeleteFailed: el merge OK pero el delete remoto posterior falló
//     — reportamos para que el humano la borre a mano.
//   - default: el delete remoto borró la branch.
func branchOutcomeMessage(branch string, keepBranch, preRemoteKnown, preRemoteMissing, remoteDeleteFailed bool) string {
	if keepBranch {
		return fmt.Sprintf("Keeping branch %s (--keep-branch)", branch)
	}
	if preRemoteKnown && preRemoteMissing {
		return fmt.Sprintf("Branch %s already removed", branch)
	}
	if remoteDeleteFailed {
		return fmt.Sprintf("Branch %s kept on remote (delete failed)", branch)
	}
	return fmt.Sprintf("Deleted branch %s", branch)
}

// samePath compara dos paths canonicalizándolos (resuelve symlinks para
// evitar false negatives entre /var y /private/var en macOS).
func samePath(a, b string) bool {
	ca, _ := canonPath(a)
	cb, _ := canonPath(b)
	return ca == cb
}

// remoteBranchExists consulta si refs/heads/<branch> existe en origin.
// Devuelve (exists, known): known=false si no pudimos determinarlo (red,
// auth, timeout) — el caller debe interpretarlo como "no sabemos".
//
// Usado pre-merge para distinguir "lo borró che" de "ya estaba borrado"
// (auto-delete de GitHub). Best-effort: 5s de timeout con
// GIT_TERMINAL_PROMPT=0 para no colgarse esperando credenciales.
func remoteBranchExists(repoRoot, branch string) (exists bool, known bool) {
	if os.Getenv("CHE_CLOSE_SKIP_REMOTE_CHECK") == "1" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "ls-remote", "--heads", "origin", branch)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		return false, false
	}
	return len(strings.TrimSpace(string(out))) > 0, true
}

// waitCIStable pollea `gh pr checks` hasta que el CI deje de estar pending
// o venza CIPollTimeout. No retorna error de timeout — solo de gh roto.
// El Run evalúa el CI final en la siguiente iteración del loop.
func waitCIStable(ref string, log *output.Logger) error {
	deadline := time.Now().Add(CIPollTimeout)
	for {
		checks, err := FetchChecks(ref)
		if err != nil {
			return err
		}
		ci := aggregateCIState(checks)
		if ci != CIPending {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("CI sigue pending tras %s — próximo intento va a re-chequear", CIPollTimeout)
		}
		log.Step(fmt.Sprintf("CI corriendo (%d checks) — próximo poll en %s", len(checks), CIPollInterval))
		time.Sleep(CIPollInterval)
	}
}
