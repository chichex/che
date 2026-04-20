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
// CHE_CLOSE_AGENT_TIMEOUT_SECS para entornos lentos. Default 30 min (igual
// que execute): arreglar conflictos + correr tests localmente puede tardar.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_CLOSE_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 30 * time.Minute
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

// Opts agrupa los writers y la callback de progreso.
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string)
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

// hasConflicts devuelve true si el PR tiene conflictos con la base y
// necesita resolución manual/del agente antes de poder mergear.
// mergeStateStatus puede venir vacío en fetchs rápidos; mergeable es más
// confiable para este chequeo.
func hasConflicts(pr *PullRequest) bool {
	return strings.EqualFold(pr.Mergeable, "CONFLICTING") ||
		strings.EqualFold(pr.MergeStateStatus, "DIRTY")
}

// ---- Run ----

// Run ejecuta el flow completo sobre un PR. Decisiones:
//   - Preflight: repo git + gh auth.
//   - Fetch PR + gate (open, no validator reject).
//   - Si draft → ready al inicio.
//   - Loop MaxFixAttempts: detectar problemas; si hay conflictos o CI rojo
//     invocar opus en worktree (reuse .worktrees/issue-N si existe, o crear
//     uno sobre la head branch del PR), commit+push, poll CI.
//   - Merge con merge commit.
//   - Cerrar issue asociado + transición de labels a status:closed.
//   - Cleanup del worktree solo si fue creado por este run (reusado: dejar).
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
	if _, err := validate.ParsePRRef(prRef); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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

	if blocking := BlockingVerdict(pr); blocking != "" {
		fmt.Fprintf(stderr, "error: PR #%d tiene label %s — close no mergea PRs con changes-requested/needs-human. Resolvé los findings y re-validá antes.\n",
			pr.Number, blocking)
		return ExitSemantic
	}

	if pr.IsDraft {
		progress(fmt.Sprintf("PR #%d está en draft — pasándolo a ready for review…", pr.Number))
		if err := prReady(prRef); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
	}

	// Loop de fix. En cada iteración: detectar problemas, si hay → invocar
	// agente en worktree, commit+push, poll CI.
	var (
		wt           *execute.Worktree
		wtOwned      bool // true si lo creamos en este run (cleanup al final)
		lastProblems []string
	)

	defer func() {
		if wt != nil && wtOwned {
			_ = wt.Cleanup(repoRoot, false)
		}
	}()

	issueNum := firstClosingIssue(pr)

	for attempt := 1; attempt <= MaxFixAttempts+1; attempt++ {
		// Refrescar estado del PR (mergeable puede venir UNKNOWN en la
		// primera vista; github lo computa lazy).
		progress(fmt.Sprintf("chequeando estado del PR (intento %d/%d)…", attempt, MaxFixAttempts+1))
		freshPR, err := FetchPR(prRef)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
		pr = freshPR

		checks, err := FetchChecks(prRef)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}

		ci := aggregateCIState(checks)
		conflict := hasConflicts(pr)
		problems := problemsList(conflict, ci)
		lastProblems = problems

		if len(problems) == 0 {
			progress("PR mergeable y CI verde — procediendo al merge")
			break
		}

		if attempt > MaxFixAttempts {
			fmt.Fprintf(stderr, "error: agotados %d intentos de fix; problemas restantes: %s\n",
				MaxFixAttempts, strings.Join(problems, ", "))
			return ExitRetry
		}

		progress(fmt.Sprintf("problemas detectados: %s — intentando fix con opus", strings.Join(problems, ", ")))

		if wt == nil {
			w, owned, err := resolveWorktree(repoRoot, issueNum, pr.HeadBranch)
			if err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return ExitRetry
			}
			wt = w
			wtOwned = owned
		}

		prompt := buildFixPrompt(pr, conflict, failingChecks(checks))
		if err := runAgent(wt.Path, prompt, progress); err != nil {
			fmt.Fprintf(stderr, "error: agente falló: %v\n", err)
			return ExitRetry
		}

		progress("esperando que CI re-evalúe tras el push del agente…")
		if err := waitCIStable(prRef, progress); err != nil {
			// waitCIStable no retorna error fatal si vencía el timeout —
			// solo si gh explotó. En timeout seguimos al próximo intento.
			fmt.Fprintf(stderr, "warning: %v\n", err)
		}
	}

	progress(fmt.Sprintf("mergeando PR #%d con merge commit…", pr.Number))
	if err := mergePR(prRef); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	// Cerrar issues asociados. Después del merge, los que tenían "closes #N"
	// en el body del PR quedan OPEN un tiempo hasta que github procesa el
	// auto-close; hacemos un close explícito para no depender de eso y para
	// aplicar la transición de labels atómicamente con el cierre.
	closedIssues := closeAssociatedIssues(pr, stderr, progress)

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
// aplica la transición de labels a status:closed cuando corresponde
// (status:executed → status:closed). Errores no fatales (log + seguir).
// Devuelve la lista de números de issues que se cerraron efectivamente
// (para reporte en stdout).
func closeAssociatedIssues(pr *PullRequest, stderr io.Writer, progress func(string)) []int {
	var closed []int
	for _, ref := range pr.ClosingIssuesReferences {
		if ref.Number == 0 {
			continue
		}
		refStr := fmt.Sprintf("%d", ref.Number)
		// Si github ya lo cerró por el merge auto, el issueClose es
		// idempotente (devuelve error que ignoramos) pero la transición de
		// labels sí queremos aplicarla igual.
		progress(fmt.Sprintf("cerrando issue #%d…", ref.Number))
		if err := issueClose(refStr); err != nil {
			fmt.Fprintf(stderr, "warning: no se pudo cerrar issue #%d: %v\n", ref.Number, err)
		} else {
			closed = append(closed, ref.Number)
		}
		// Transición de labels. Best-effort: si el issue no estaba en
		// status:executed, la transición falla — lo logueamos y seguimos.
		if err := labels.Apply(refStr, labels.StatusExecuted, labels.StatusClosed); err != nil {
			fmt.Fprintf(stderr, "warning: labels del issue #%d no transicionaron a status:closed: %v\n",
				ref.Number, err)
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
func runAgent(cwd, prompt string, progress func(string)) error {
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
	go streamPipe(&wg, stdoutPipe, nil, progress, "opus: ", formatOpusLine)
	go streamPipe(&wg, stderrPipe, &stderrBuf, progress, "opus stderr: ", nil)
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
func streamPipe(wg *sync.WaitGroup, r io.Reader, acc *strings.Builder, progress func(string), prefix string, format func(string) (string, bool)) {
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
		if strings.TrimSpace(out) != "" && progress != nil {
			progress(prefix + out)
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
				Type string `json:"type"`
				Name string `json:"name"`
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
				return "tool: " + c.Name, true
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
			// `gh pr checks` sale con exit=8 cuando hay checks que fallan
			// — el stdout igual viene bien formado. Tratamos 8 como OK y
			// dejamos que aggregateCIState detecte el failure.
			if ee.ExitCode() == 8 && len(ee.Stderr) == 0 {
				// continuamos con lo que haya en stdout del cmd
			} else {
				return nil, fmt.Errorf("gh pr checks: %s", strings.TrimSpace(string(ee.Stderr)))
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
func mergePR(ref string) error {
	cmd := exec.Command("gh", "pr", "merge", ref, "--merge")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge: %s", strings.TrimSpace(string(out)))
	}
	return nil
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

// waitCIStable pollea `gh pr checks` hasta que el CI deje de estar pending
// o venza CIPollTimeout. No retorna error de timeout — solo de gh roto.
// El Run evalúa el CI final en la siguiente iteración del loop.
func waitCIStable(ref string, progress func(string)) error {
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
		progress(fmt.Sprintf("CI corriendo (%d checks)… próximo poll en %s", len(checks), CIPollInterval))
		time.Sleep(CIPollInterval)
	}
}
