// Package execute implements flow 03 — tomar un issue en che:idea o
// che:plan, armar un worktree aislado, invocar al agente para producir el
// diff, abrir/actualizar un PR draft contra main y transicionar el issue a
// che:executed. La lógica vive acá (pura, testeable) para que el subcomando
// `che execute` y la TUI compartan la misma implementación.
//
// execute NO dispara validadores: si el humano quiere validación
// automática del plan antes de ejecutar o del PR después de crearlo,
// corre `che validate` explícitamente. El gate de intervención humana
// son los labels plan-validated:* sobre el issue.
//
// La transición de estados captura el `from` (che:idea o che:plan) al
// momento del lock para poder hacer rollback al estado original si algo
// falla post-lock.
package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chichex/che/internal/agent"
	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/output"
	"github.com/chichex/che/internal/pipelinelabels"
	planpkg "github.com/chichex/che/internal/plan"
)

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK        ExitCode = 0
	ExitRetry     ExitCode = 2   // error remediable (red, gh/git falla, rollback aplicado)
	ExitSemantic  ExitCode = 3   // ref vacío, issue sin ct:plan, ya ejecutándose, agente inválido
	ExitCancelled ExitCode = 130 // SIGINT/SIGTERM recibido; cleanup local aplicado
)

// agentKillGrace es el tiempo que esperamos al agente después de mandarle
// SIGTERM a su process group antes de escalar a SIGKILL. Corto a propósito:
// queremos exit determinista aunque el agente ignore SIGTERM.
const agentKillGrace = 5 * time.Second

// Agent es un alias del enum centralizado en internal/agent. Re-exportado
// para que cmd/execute.go y la TUI sigan escribiendo `execute.Agent`.
type Agent = agent.Agent

const (
	AgentOpus   = agent.AgentOpus
	AgentCodex  = agent.AgentCodex
	AgentGemini = agent.AgentGemini
)

const DefaultAgent = agent.DefaultAgent

var ValidAgents = agent.ValidAgents

// ParseAgent delega en internal/agent.
func ParseAgent(s string) (Agent, error) { return agent.ParseAgent(s) }

// AgentTimeout para llamadas al CLI del agente. execute tiene un default
// mayor que explore porque generar diff + tool use es típicamente más largo
// que devolver un JSON de análisis. 60 min da margen para issues que tocan
// varios archivos + corren tests; con stream-json el operador ve si el
// agente se colgó mucho antes de que expire y puede cancelar con Ctrl+C.
var AgentTimeout = func() time.Duration {
	if s := strings.TrimSpace(os.Getenv("CHE_EXEC_AGENT_TIMEOUT_SECS")); s != "" {
		if n, err := time.ParseDuration(s + "s"); err == nil && n > 0 {
			return n
		}
	}
	return 60 * time.Minute
}()

// Opts agrupa el writer de stdout (payload: "Executed ...", "PR: ..."),
// el logger estructurado (progress + errors) y el agente ejecutor.
//
// NO hay campo Validators: execute deja de disparar validadores
// automáticamente; el humano los corre con `che validate` si los quiere.
type Opts struct {
	Stdout io.Writer
	Out    *output.Logger
	Agent  Agent
	// Ctx permite al caller proveer un context cancelable (típicamente
	// signal.NotifyContext para SIGINT/SIGTERM). Si es nil, Run instala su
	// propio handler de señales. La TUI lo usa para compartir un context con
	// cancel explícito desde el key handler de Ctrl+C sin depender de un
	// signal real.
	Ctx context.Context
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

// ConsolidatedPlan es un type alias al shape canónico en internal/plan. Los
// campos que execute usa son un subset (no consume RisksToMitigate) pero el
// shape es el mismo para evitar duplicación y drift con explore.
type ConsolidatedPlan = planpkg.ConsolidatedPlan

// isPlanEmpty devuelve true si el plan no tiene NINGÚN contenido procesable.
// Honra el contrato de planpkg.Parse: el fallback con Summary=body (issue
// legacy sin header consolidado) sigue siendo procesable, así que no lo
// bloqueamos. Solo abortamos cuando el body está totalmente vacío y el
// fallback dejó todos los campos en cero — ahí sí no hay nada que mandar al
// agente.
func isPlanEmpty(p *ConsolidatedPlan) bool {
	if p == nil {
		return true
	}
	return p.Summary == "" && p.Goal == "" && len(p.Steps) == 0 && len(p.AcceptanceCriteria) == 0
}

// Sentinel errors devueltos por preparePlan para que Run mapee cada caso a
// un mensaje accionable con el número de issue. No se wrapean ErrAmbiguousPlan
// ni errores de Parse — esos se propagan directamente.
var (
	errPlanEmpty    = errors.New("plan empty: body sin contenido procesable")
	errPlanDegraded = errors.New("plan degraded: header consolidado presente pero sub-secciones vacías")
)

// preparePlan parsea el body del issue y valida que el resultado sea
// procesable por el ejecutor. Reglas:
//
//   - Cualquier error de planpkg.Parse (incluyendo ErrAmbiguousPlan) se
//     propaga tal cual — Run lo mapea a su mensaje correspondiente.
//   - Body completamente vacío → errPlanEmpty. No hay nada que mandar al
//     agente.
//   - Body con header "## Plan consolidado" real pero sin Goal/Steps/AC
//     parseables → errPlanDegraded. El fallback de Parse deja Summary=body,
//     pero ese body tiene forma de plan estructurado que no se consolidó
//     bien; mandar ese texto al ejecutor produce un run degradado sin las
//     guías reales. Preferimos cortar y pedir re-consolidación.
//   - Body sin header (issue legacy) con Summary=body es válido: sigue
//     siendo procesable aunque menos guiado.
func preparePlan(body string) (*ConsolidatedPlan, error) {
	p, err := planpkg.Parse(body)
	if err != nil {
		return nil, err
	}
	if isPlanEmpty(p) {
		return nil, errPlanEmpty
	}
	if planpkg.HasConsolidatedHeader(body) &&
		p.Goal == "" && len(p.Steps) == 0 && len(p.AcceptanceCriteria) == 0 {
		return nil, errPlanDegraded
	}
	return p, nil
}

// Run ejecuta el flow completo sobre un issue. Decisiones claves:
//   - Preflight: repo git + gh auth + gh pr list (scope check).
//   - Transition <from> → che:executing (lock), donde <from> es che:idea
//     o che:plan según el estado actual del issue.
//   - Crear worktree .worktrees/issue-N sobre branch exec/N-<slug>.
//   - Invocar agente, commit en el worktree, push.
//   - Crear o actualizar PR draft contra main con Closes #<n>.
//   - Fire-and-forget validadores sobre el diff del PR.
//   - Transition che:executing → che:executed.
//   - Comentario al issue con link al PR.
//   - Rollback: si algo falla después del lock, revertir a <from> y
//     limpiar worktree.
func Run(issueRef string, opts Opts) ExitCode {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	log := opts.Out
	if log == nil {
		log = output.New(nil)
	}
	// stderr + progress adapters: permiten reusar helpers legacy sin
	// tocar sus firmas. Cada línea al stderr se emite como log.Warn
	// (en execute, los fmt.Fprintf(stderr, ...) son mayoría warnings
	// best-effort + errors; usar Warn evita ruido cuando el flow continúa).
	stderr := log.AsWriter(output.LevelWarn)
	progress := func(s string) { log.Step(s) }
	_ = stderr // puede no usarse si todos los call-sites ya usan log directo

	// Instalar signal handling si el caller no pasó un context. cmd/execute.go
	// instala el suyo (para mapear SIGINT → exit 130 explícito); la TUI pasa
	// un context propio con cancel programático desde el key handler. Si
	// nadie pasó nada, hacemos el default seguro acá.
	ctx := opts.Ctx
	var stopSignals context.CancelFunc
	if ctx == nil {
		var parent context.Context
		parent, stopSignals = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		ctx = parent
		defer stopSignals()
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
	repoRoot, err := repoToplevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGitHubRemote(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckPRScopes(ctx); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	baseBranch, err := DetectBaseBranch(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	// Si ya entró una señal mientras corrían los prechecks, abortamos sin
	// tocar nada (no hay lock todavía).
	if ctx.Err() != nil {
		return ExitCancelled
	}

	progress("obteniendo issue desde GitHub…")
	issue, err := fetchIssue(ctx, issueRef)
	if err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: fetching issue: %v\n", err)
		return ExitRetry
	}

	// Adopt mode (v0.0.79): si el issue entró sin ct:plan ni ningún che:*,
	// lo "adoptamos" como entry point del state machine inyectando ct:plan +
	// che:idea antes del gate. Paralelo a explore.reclassifyIssue, pero sin
	// el LLM call: execute no necesita type/size para correr, y preparePlan
	// tolera body sin header consolidado (lo trata como Summary). Así el
	// botón execute del dash en adopt mode no falla con "no está en
	// che:idea/plan/validated".
	if err := seedAdoptLabels(issueRef, issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	if err := gate(issue); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitSemantic
	}

	// from = che:idea | che:plan. Lo capturamos acá para que el lock + el
	// rollback usen el mismo estado de origen, robusto frente a issues que
	// el humano arranca directo desde idea (sin pasar por explore).
	from := fromState(issue)

	plan, err := preparePlan(issue.Body)
	if err != nil {
		switch {
		case errors.Is(err, planpkg.ErrAmbiguousPlan):
			// Ambigüedad (>1 header "## Plan consolidado"): no podemos elegir.
			fmt.Fprintf(stderr, "error: issue #%d tiene múltiples '## Plan consolidado' en el body — editalo para dejar uno solo y reintentá\n", issue.Number)
		case errors.Is(err, errPlanEmpty):
			fmt.Fprintf(stderr, "error: issue #%d tiene el body vacío — agregá descripción o corré `che explore %d`\n", issue.Number, issue.Number)
		case errors.Is(err, errPlanDegraded):
			// Header consolidado presente pero sub-secciones vacías: mejor
			// re-consolidar que mandar un plan degradado al ejecutor.
			fmt.Fprintf(stderr, "error: issue #%d tiene '## Plan consolidado' pero las sub-secciones (Goal/Pasos/Criterios) no parsean — reconsolidalo con `che explore %d`\n", issue.Number, issue.Number)
		default:
			fmt.Fprintf(stderr, "error: parsing plan for issue #%d: %v\n", issue.Number, err)
		}
		return ExitSemantic
	}

	// Mutex vía label: aplicamos che:locked ANTES de la transición de status.
	// Si el Lock falla, no tocamos nada más (el status sigue en plan, el ref
	// queda limpio). Si el Lock pasa y después algo falla, el defer de Unlock
	// (stackeado inmediatamente abajo) saca el label en LIFO tras el
	// cleanupLocal — así el label se remueve siempre, sea éxito o rollback.
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

	// Transition <from> → applying:execute. Desde acá se lockea el issue
	// (también vía che:* — redundante con che:locked pero preservado porque
	// el listado de candidatos y el gate ya dependen de la máquina de
	// estados).
	progress("transicionando issue a " + pipelinelabels.StateApplyingExecute + "…")
	if err := labels.Apply(issueRef, from, pipelinelabels.StateApplyingExecute); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	var (
		succeeded       bool
		wt              *Worktree
		prCreated       bool // si true, no tocamos el rollback remoto; se documenta al user
		executedApplied bool // si true, el label ya transitó a executed — no lo revertimos
		cleanupDone     bool
	)
	// cleanupLocal limpia el estado local tras una falla o señal. Idempotente
	// (cleanupDone) y usa context.Background() para que una señal durante el
	// cleanup no aborte el cleanup en sí.
	//
	// Label handling (depende del punto de la falla):
	//   - executedApplied=true          → no tocamos el label (quedó en executed).
	//   - !executedApplied && prCreated → transicionamos a executed: el PR
	//     remoto ya existe, así que ese es el estado consistente. Volver al
	//     `from` dejaría el issue "libre" con un PR vivo apuntando a él, que
	//     es peor que dejarlo en executed para retry manual.
	//   - !executedApplied && !prCreated → rollback normal al `from`
	//     capturado (che:idea o che:plan), ownership-aware (re-fetch y
	//     chequeamos que seguimos teniendo el lock).
	//
	// Después del label, en orden fijo para que un segundo Ctrl+C vea
	// progreso: 2) git worktree remove --force, 3) git branch -D. Los
	// errores de esos pasos se propagan para loguearlos — best-effort pero
	// NO silencioso.
	cleanupLocal := func(cause string) {
		if cleanupDone {
			return
		}
		cleanupDone = true

		// Señal: avisamos al user qué estamos haciendo para que el bloqueo
		// síncrono del cleanup no parezca un cuelgue.
		if cause != "" {
			switch {
			case executedApplied:
				fmt.Fprintf(stderr, "%s — limpiando localmente (worktree, branch; label queda en %s)…\n",
					cause, pipelinelabels.StateExecute)
			case prCreated:
				fmt.Fprintf(stderr, "%s — limpiando localmente (label → %s por PR vivo, worktree, branch)…\n",
					cause, pipelinelabels.StateExecute)
			default:
				fmt.Fprintf(stderr, "%s — limpiando localmente (label → %s, worktree, branch)…\n", cause, from)
			}
		}

		// 1) Label handling. Caso tres-ramas (ver docstring).
		if !executedApplied {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rollbackCancel()
			if prCreated {
				// PR ya creado: dejar el issue en execute para preservar
				// consistencia con el estado remoto. Best-effort; si falla,
				// warneamos.
				if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExecute, pipelinelabels.StateExecute); err != nil {
					fmt.Fprintf(stderr, "warning: no se pudo transicionar a %s tras señal post-PR: %v — revisá labels a mano\n",
						pipelinelabels.StateExecute, err)
				}
			} else {
				// Sin PR todavía: rollback al `from`, pero solo si seguimos
				// siendo el owner del lock (otra corrida podría haberlo
				// tomado si el worktree se quedó colgado).
				current, fetchErr := fetchIssue(rollbackCtx, issueRef)
				if fetchErr != nil {
					fmt.Fprintf(stderr, "warning: rollback no aplicado: no se pudo re-fetch el issue (%v) — revisá labels a mano\n", fetchErr)
				} else if !current.HasLabel(pipelinelabels.StateApplyingExecute) {
					fmt.Fprintf(stderr, "rollback abortado: el issue ya no está en %s (owner=otro)\n",
						pipelinelabels.StateApplyingExecute)
				} else {
					if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExecute, from); err != nil {
						fmt.Fprintf(stderr, "warning: rollback failed: %v — revisá labels del issue a mano\n", err)
					}
				}
			}
		}

		// 2) Worktree + branch local. Acotado con timeout y propagamos los
		//    errores para loguearlos — si queda basura local, el usuario
		//    tiene que saberlo para limpiarla a mano.
		if wt != nil {
			wtCtx, wtCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := wt.Cleanup(wtCtx, repoRoot, false); err != nil {
				fmt.Fprintf(stderr, "warning: cleanup local parcial: %v — revisá `git worktree list` y `git branch` para limpiar a mano\n", err)
			}
			wtCancel()
		}

		// 3) Si el PR remoto ya quedó creado, avisamos al usuario que la
		//    branch remota + PR draft quedaron colgados para retry manual.
		if prCreated {
			fmt.Fprintln(stderr, "nota: la branch remota y el PR draft quedan intactos (best-effort); cerralos o reanudá con otro `che execute`")
		}
	}
	defer func() {
		if succeeded {
			return
		}
		reason := ""
		if ctx.Err() != nil {
			reason = "señal recibida"
		}
		cleanupLocal(reason)
	}()

	slug := Slugify(issue.Title)
	progress(fmt.Sprintf("creando worktree .worktrees/issue-%d (branch exec/%d-%s)…", issue.Number, issue.Number, slug))
	wt, err = CreateWorktree(WorktreeOpts{
		RepoRoot:   repoRoot,
		IssueNum:   issue.Number,
		Slug:       slug,
		BaseBranch: baseBranch,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	// Antes de invocar al agente, chequeamos si ya hay un PR abierto para
	// esta branch — si lo hay, estamos en modo "re-ejecutar" (idempotente)
	// y vamos a actualizarlo después de pushear los nuevos commits.
	existingPR, err := findOpenPRForBranch(ctx, wt.Branch, baseBranch)
	if err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress(fmt.Sprintf("invocando a %s en el worktree…", agent))
	if err := runAgent(ctx, agent, wt.Path, buildPrompt(issue, plan), progress); err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("chequeando cambios en el worktree…")
	hasChanges, err := worktreeHasChanges(ctx, wt.Path)
	if err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if !hasChanges {
		// Sin cambios, no importa si hay PR existente o no: no tenemos
		// nada que commitear ni que refrescar. NO transicionamos a
		// executed (eso engañaría al operador). Dejamos
		// que el defer revierta executing → plan así el issue queda
		// disponible para otro intento. Mensaje diferenciado según haya
		// PR previo o no.
		if existingPR != "" {
			fmt.Fprintf(stderr, "error: no se generaron cambios en este run; PR no actualizado (%s)\n", existingPR)
		} else {
			fmt.Fprintln(stderr, "error: no se generaron cambios en este run; no hay PR previo")
		}
		return ExitRetry
	}

	progress("armando commit en el worktree…")
	if err := commitAll(ctx, wt.Path, fmt.Sprintf("feat(#%d): %s\n\nCloses #%d", issue.Number, issue.Title, issue.Number)); err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("pusheando branch " + wt.Branch + "…")
	if err := pushBranch(ctx, wt.Path, wt.Branch); err != nil {
		if ctx.Err() != nil {
			return ExitCancelled
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	var prURL string
	if existingPR != "" {
		progress("actualizando PR existente " + existingPR + "…")
		prURL = existingPR
		prCreated = true
	} else {
		progress(fmt.Sprintf("creando PR draft contra %s…", baseBranch))
		prURL, err = createDraftPR(ctx, wt.Path, wt.Branch, issue, baseBranch)
		if err != nil {
			if ctx.Err() != nil {
				return ExitCancelled
			}
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitRetry
		}
		prCreated = true
	}

	// Transición a execute (terminal). Sin validadores automáticos; el label
	// refleja "ejecución terminada". Orden importante: una señal entre
	// `prCreated=true` y `executedApplied=true` dejaría al cleanup en modo
	// "PR vivo + label en applying:execute", que forzaría un label fix-up en
	// el defer. Transicionar ya mismo encoge la ventana a mínimo y deja al
	// issue en el estado consistente con el PR remoto.
	progress("transicionando issue a " + pipelinelabels.StateExecute + "…")
	if err := labels.Apply(issueRef, pipelinelabels.StateApplyingExecute, pipelinelabels.StateExecute); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	executedApplied = true

	progress("posteando comment en el issue con link al PR…")
	if err := commentIssue(ctx, issueRef, renderIssueComment(prURL)); err != nil {
		// No es fatal — ya hicimos todo el trabajo. Lo logueamos y seguimos.
		fmt.Fprintf(stderr, "warning: no se pudo comentar el issue: %v\n", err)
	}

	succeeded = true

	fmt.Fprintf(stdout, "Executed %s\n", issue.URL)
	fmt.Fprintf(stdout, "PR: %s\n", prURL)
	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

// ListCandidates devuelve los issues abiertos con ct:plan + che:plan.
// Estos son los candidatos típicos a ejecutar (post-explore). execute
// también acepta che:idea (skipping explore) pero esos casos se invocan
// por número directo, no via el listado de la TUI — mantener el filtro
// simple. El gate de plan-validated:* se chequea más tarde en gate() — el
// listado los muestra igual para que el humano vea los que necesitan
// intervención.
func ListCandidates() ([]Candidate, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--label", labels.CtPlan,
		"--label", pipelinelabels.StateExplore,
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
		if i.HasLabel(labels.CheLocked) {
			continue // otro flow lo tiene agarrado; no lo mostramos.
		}
		out2 = append(out2, Candidate{Number: i.Number, Title: i.Title})
	}
	return out2, nil
}

// ---- prechecks ----

func repoToplevel(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git repo: not inside a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

func precheckGitHubRemote(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "git", "remote", "get-url", "origin").Output()
	if err != nil {
		return fmt.Errorf("github remote: no origin configured")
	}
	url := strings.TrimSpace(string(out))
	if !strings.Contains(url, "github.com") {
		return fmt.Errorf("github remote: origin is not github.com: %s", url)
	}
	return nil
}

func precheckGhAuth(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "gh", "auth", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh auth: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// precheckPRScopes verifica que el token de gh tenga un scope que permita
// crear PRs: 'repo' (privados + públicos) o 'public_repo' (sólo públicos).
// El chequeo se hace parseando `gh auth status -t`, que imprime el token y
// los scopes concedidos en stderr. Antes se usaba `gh pr list --limit 1`,
// que sólo requiere scope read — un token read-only pasaba el precheck y
// fallaba tarde en `gh pr create`, después de gastar tokens LLM y pushear.
func precheckPRScopes(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "gh", "auth", "status", "-t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh auth status: %s — probá `gh auth login`", strings.TrimSpace(string(out)))
	}
	if !hasRepoScope(string(out)) {
		return fmt.Errorf("el token de gh no tiene scope 'repo' o 'public_repo'; ejecutá `gh auth refresh -s repo` y reintentá")
	}
	return nil
}

// hasRepoScope busca 'repo' o 'public_repo' en la lista de scopes que
// imprime `gh auth status -t`. La línea típica es:
//
//   - Token scopes: 'gist', 'read:org', 'repo', 'workflow'
//
// Matcheamos con word boundaries para no confundir 'repo' con 'repo:status'
// (que es un scope menos privilegiado).
func hasRepoScope(out string) bool {
	// Normalizamos: buscamos líneas que contengan "Token scopes:" o
	// "scopes:" (el formato exacto puede variar entre versiones de gh).
	for _, line := range strings.Split(out, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "scopes") {
			continue
		}
		// Parseamos los scopes individuales separados por coma.
		// Cada scope viene como 'nombre' (con comillas). Limpiamos las
		// comillas y chequeamos igualdad exacta.
		for _, raw := range strings.Split(line, ",") {
			tok := strings.TrimSpace(raw)
			tok = strings.Trim(tok, "'\"")
			// quedarnos con el pedacito después del último ":" o espacio
			if idx := strings.LastIndexAny(tok, ": "); idx >= 0 {
				tok = strings.Trim(tok[idx+1:], "'\"")
			}
			if tok == "repo" || tok == "public_repo" {
				return true
			}
		}
	}
	return false
}

// ---- issue fetch / gate ----

func fetchIssue(ctx context.Context, ref string) (*Issue, error) {
	cmd := exec.CommandContext(ctx, "gh", "issue", "view", ref,
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
//   - Tiene che:idea, che:plan O che:validated (los 3 son puntos válidos
//     de entrada al executor: el humano puede saltar explore y ejecutar
//     directo desde idea; puede ejecutar el plan sin validarlo; o puede
//     ejecutar un plan ya validado post-approve).
//   - NO tiene labels más avanzados (executing/executed/validating/
//     closing/closed).
//   - NO tiene plan-validated:changes-requested / :needs-human (si un
//     validate dejó feedback de cambios o escaló a humano, iterar/resolver
//     antes; no forzamos execute sobre un plan que se sabe no aprobado).
//
// plan-validated:approve, ausencia de cualquier plan-validated:*, o un
// issue en che:validated con verdict approve = green light. El humano
// decide si correr `che validate` antes de ejecutar; si no lo hace,
// execute pasa sin verdict y confía en el plan consolidado que dejó
// explore.
func gate(i *Issue) error {
	if i.State != "OPEN" {
		return fmt.Errorf("issue #%d is closed", i.Number)
	}
	if !i.HasLabel(labels.CtPlan) {
		return fmt.Errorf("issue #%d is missing label ct:plan (not created by `che idea`?)", i.Number)
	}
	if i.HasLabel(pipelinelabels.StateApplyingExecute) {
		return fmt.Errorf("issue #%d ya está en %s — otro run en curso o quedó colgado; quitá el label a mano si es lo segundo", i.Number, pipelinelabels.StateApplyingExecute)
	}
	// "Más avanzado" post validate_pr: estados donde execute no tiene
	// sentido porque ya hay PR abierto / mergeado / issue cerrado. NO
	// incluimos StateValidatePR: ese es un punto de entrada válido al flow
	// post-validate plan (issue con plan-validated:approve).
	for _, beyond := range []string{
		pipelinelabels.StateExecute,
		pipelinelabels.StateApplyingValidatePR,
		pipelinelabels.StateApplyingClose,
		pipelinelabels.StateClose,
	} {
		if i.HasLabel(beyond) {
			return fmt.Errorf("issue #%d ya avanzó en el pipeline (%s presente) — execute no aplica", i.Number, beyond)
		}
	}
	if i.HasLabel(labels.CheLocked) {
		return fmt.Errorf("issue #%d tiene che:locked — otro flow lo tiene agarrado, o quedó colgado. Si es lo segundo: `che unlock %d`", i.Number, i.Number)
	}
	if i.HasLabel(labels.PlanValidatedChangesRequested) {
		return fmt.Errorf("issue #%d tiene plan-validated:changes-requested — corré `che iterate %d` primero, o re-validá", i.Number, i.Number)
	}
	if i.HasLabel(labels.PlanValidatedNeedsHuman) {
		return fmt.Errorf("issue #%d tiene plan-validated:needs-human — resolvé a mano antes de ejecutar", i.Number)
	}
	if !i.HasLabel(pipelinelabels.StateIdea) && !i.HasLabel(pipelinelabels.StateExplore) && !i.HasLabel(pipelinelabels.StateValidatePR) {
		return fmt.Errorf("issue #%d no está en %s, %s ni %s — corré `che explore %d` o `che validate %d` según el flow",
			i.Number, pipelinelabels.StateIdea, pipelinelabels.StateExplore, pipelinelabels.StateValidatePR, i.Number, i.Number)
	}
	return nil
}

// seedAdoptLabels detecta issues sin ct:plan ni che:* (entrada por adopt
// mode del dash) y les inyecta ct:plan + che:idea para que el flow normal
// pueda correr (gate exige ct:plan + che:idea/plan/validated). Idempotente:
// si el issue ya tiene cualquier che:* o ct:plan, no toca nada — preserva
// el estado existente. Actualiza issue.Labels in-place para que el resto
// del flow vea los labels nuevos sin re-fetchear.
func seedAdoptLabels(ref string, issue *Issue) error {
	if issue.HasLabel(labels.CtPlan) {
		return nil
	}
	hasCheState := false
	for _, l := range issue.Labels {
		if strings.HasPrefix(l.Name, "che:") && l.Name != labels.CheLocked {
			hasCheState = true
			break
		}
	}
	toAdd := []string{labels.CtPlan}
	if !hasCheState {
		toAdd = append(toAdd, pipelinelabels.StateIdea)
	}
	for _, l := range toAdd {
		if err := labels.Ensure(l); err != nil {
			return fmt.Errorf("seeding adopt label %s: %w", l, err)
		}
	}
	number, err := labels.RefNumber(ref)
	if err != nil {
		return fmt.Errorf("seed adopt %s: %w", ref, err)
	}
	if err := labels.AddLabels(number, toAdd...); err != nil {
		return fmt.Errorf("seed adopt: %w", err)
	}
	for _, l := range toAdd {
		issue.Labels = append(issue.Labels, Label{Name: l})
	}
	return nil
}

// fromState devuelve el estado de origen del issue al momento del gate
// (idea, explore o validate_pr — modelo v2). Prioridad en caso de
// ambigüedad (issues con más de un label che:state:* — no debería pasar
// post-transición, pero defensa): validate_pr > explore > idea. El más
// avanzado gana para que el rollback restaure el estado más útil y la
// transición de lock parta de una clave válida en validTransitions. Los 3
// tienen entry en la máquina: idea → applying:execute, explore →
// applying:execute, validate_pr → applying:execute; y los 3 rollbacks
// inversos.
func fromState(i *Issue) string {
	if i.HasLabel(pipelinelabels.StateValidatePR) {
		return pipelinelabels.StateValidatePR
	}
	if i.HasLabel(pipelinelabels.StateExplore) {
		return pipelinelabels.StateExplore
	}
	return pipelinelabels.StateIdea
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
// directorio. El parent ctx se compone con el timeout del agente; cualquier
// cancelación (timeout o señal) mata el process group entero (Setpgid + kill
// al -pgid) para evitar subprocesos zombies — claude/codex pueden fork-ar
// tool-use processes (bash, ripgrep, etc.) que si solo matamos al PID
// directo siguen corriendo sueltos. El plumbing concreto vive en
// internal/agent.Run; acá solo traducimos errores al formato que esperan los
// tests (mensajes con "timed out after", "exit N:", "cancelado por señal").
func runAgent(parent context.Context, a Agent, cwd, prompt string, progress func(string)) error {
	var stdoutFormat func(string) (string, bool)
	if a == AgentOpus {
		stdoutFormat = formatOpusLine
	}
	res, err := agent.Run(a, prompt, agent.RunOpts{
		Ctx:       parent,
		Dir:       cwd,
		Timeout:   AgentTimeout,
		Format:    agent.OutputStreamJSON,
		KillGrace: agentKillGrace,
		OnLine: func(line string) {
			if progress != nil {
				progress(string(a) + ": " + line)
			}
		},
		OnStderrLine: func(line string) {
			if progress != nil {
				progress(string(a) + " stderr: " + line)
			}
		},
		StreamFormatter: stdoutFormat,
	})
	// Cancel por señal del parent: no es "error del agente", es cancelación
	// del usuario. Mensaje distintivo que el caller traduce a ExitCancelled
	// tras verificar parent.Err().
	if errors.Is(err, agent.ErrCancelled) {
		return fmt.Errorf("%s: cancelado por señal", a)
	}
	if errors.Is(err, agent.ErrTimeout) {
		// Incluimos el stderr acumulado cuando lo hay: un timeout puede
		// venir acompañado de pistas (auth expirado, prompt rechazado,
		// warnings del CLI) que el usuario necesita ver para distinguir
		// "el agente trabajó 15 min y no terminó" vs "el agente se colgó
		// en el segundo 1 porque algo está mal".
		if se := strings.TrimSpace(res.Stderr); se != "" {
			return fmt.Errorf("%s timed out after %s; stderr: %s", a, AgentTimeout, se)
		}
		return fmt.Errorf("%s timed out after %s (sin stderr — subí CHE_EXEC_AGENT_TIMEOUT_SECS si el agente necesita más tiempo)", a, AgentTimeout)
	}
	var ee *agent.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("%s exit %d: %s", a, ee.ExitCode, ee.Stderr)
	}
	return err
}

// formatOpusLine es un thin wrapper sobre agent.FormatOpusLine con el prefijo
// de tool que históricamente usa execute (sin prefijo, ej "Edit foo.go"). El
// test de este paquete lo ejercita directamente.
func formatOpusLine(line string) (string, bool) {
	return agent.FormatOpusLine(line, "")
}

// ---- git ops sobre el worktree ----

// worktreeHasChanges devuelve true si `git status --porcelain` devuelve
// líneas (es decir, hay archivos modificados/nuevos).
func worktreeHasChanges(ctx context.Context, wtPath string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", wtPath, "status", "--porcelain")
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
func commitAll(ctx context.Context, wtPath, message string) error {
	if err := runGitIn(ctx, wtPath, "add", "-A"); err != nil {
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
	if err := runGitIn(ctx, wtPath, "commit", "-F", msgFile); err != nil {
		return err
	}
	return nil
}

// pushBranch empuja la branch a origin. Usa --force-with-lease para
// re-ejecuciones idempotentes (caso de actualizar un PR existente) sin
// pisar cambios ajenos.
func pushBranch(ctx context.Context, wtPath, branch string) error {
	if err := runGitIn(ctx, wtPath, "push", "--force-with-lease", "--set-upstream", "origin", branch); err != nil {
		return err
	}
	return nil
}

func runGitIn(ctx context.Context, dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// ---- PR ops ----

// findOpenPRForBranch busca un PR abierto (contra baseBranch) cuyo head-branch
// sea el dado. Devuelve la URL si lo encuentra, "" si no. Si hay más de uno,
// devuelve error accionable: el caso es suficientemente raro como para
// frenar en vez de agarrar uno silenciosamente. Filtrar por --base evita
// falsos positivos si la branch tiene un PR abierto contra otra base.
func findOpenPRForBranch(ctx context.Context, branch, baseBranch string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--head", branch,
		"--base", baseBranch,
		"--state", "open",
		"--json", "url,number")
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
	if len(prs) > 1 {
		urls := make([]string, 0, len(prs))
		for _, p := range prs {
			urls = append(urls, p.URL)
		}
		return "", fmt.Errorf("múltiples PRs abiertos encontrados para head-branch %s (PRs: %v), resolver manualmente", branch, urls)
	}
	return prs[0].URL, nil
}

// createDraftPR crea un PR draft contra baseBranch con Closes #<n> en el body.
func createDraftPR(ctx context.Context, wtPath, branch string, issue *Issue, baseBranch string) (string, error) {
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

	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--draft",
		"--base", baseBranch,
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

// ---- issue comment al final ----

// renderIssueComment arma el body del comment que postea execute al issue
// cuando termina OK. El humano decide si correr `che validate` sobre el PR
// antes de mergear — execute no se mete en esa decisión.
func renderIssueComment(prURL string) string {
	var sb strings.Builder
	sb.WriteString("<!-- claude-cli: flow=execute role=pr-link -->\n")
	sb.WriteString("## Ejecución completada\n\n")
	sb.WriteString("Se abrió un PR draft con los cambios:\n\n")
	sb.WriteString("- PR: " + prURL + "\n\n")
	sb.WriteString("El issue quedó en `che:executed`. Revisá el PR + CI; si querés validación automática antes de mergear, corré `che validate <pr>`.\n")
	return sb.String()
}

func commentIssue(ctx context.Context, ref, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-exec-ic-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "gh", "issue", "comment", ref, "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh issue comment: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
