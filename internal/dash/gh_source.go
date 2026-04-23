// Package dash — GhSource: poller que lee issues/PRs del repo via `gh` CLI y
// construye el snapshot que el server expone al template.
//
// Diseño:
//   - NewGhSource valida que `gh` esté en PATH y autenticado. Falla temprano.
//   - Run() arranca un loop con primer poll inmediato + ticker periódico.
//     Respeta ctx.Done() para shutdown limpio.
//   - refresh() lanza `gh issue list` + `gh pr list` (paralelos por goroutines)
//     y combina los resultados en entities.
//   - Un mutex RW protege snap; Snapshot() toma RLock, refresh() toma WLock.
//   - En caso de error, NO limpiamos snap.Entities — dejamos los últimos datos
//     buenos y marcamos Stale=true para que el chip avise.
//
// Filtrado:
//   - Issues sin `ct:plan` se excluyen (son tickets externos que che no gestiona).
//   - PRs con closingIssuesReferences se fusionan con su issue (Kind=KindFused,
//     IssueNumber/IssueTitle del primero de la lista). El issue correspondiente
//     NO se emite como issue-only separado para evitar duplicación en el board.
//   - PRs sin closingIssuesReferences se omiten — no forman parte del flow de
//     che (che execute siempre abre PRs con `closes #N`).
package dash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/labels"
)

// defaultClosedCap es el cap default de issues closed que el poller trae al
// snapshot. Se elige 20 como compromiso: suficiente para mostrar continuidad
// histórica reciente sin inflar el board (la columna closed scrollea
// internamente). Configurable via GhSource.ClosedCap si en el futuro se
// expone como flag de `che dash`.
//
// TODO: confirmar — si el repo tiene mucho throughput puede que 20 quede
// corto; por ahora es lo más conservador para evitar payloads grandes.
const defaultClosedCap = 20

// GhSource es un poller que refleja el estado del repo via `gh`. Se refresca
// cada `interval`; entre refreshes Snapshot() devuelve el último corte bueno.
type GhSource struct {
	repoDir  string
	nwo      string // owner/name — informativo, no se usa en las queries
	interval time.Duration
	// ClosedCap es el límite de issues closed (con label che:closed) que el
	// poller trae al snapshot. Default defaultClosedCap. La columna "closed"
	// del board solo muestra los más recientes.
	ClosedCap int

	mu   sync.RWMutex
	snap Snapshot
}

// NewGhSource construye un poller y valida precondiciones (gh instalado,
// autenticado, repo accesible). No arranca el loop — el caller debe hacer
// `go src.Run(ctx)` después.
//
// repoDir puede estar vacío (usa cwd). interval debe ser > 0.
func NewGhSource(repoDir string, interval time.Duration) (*GhSource, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be > 0, got %s", interval)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh no disponible en PATH: %w (instalá gh o usá --mock)", err)
	}
	// `gh auth status` sale 0 si hay credenciales activas para algún host.
	// Corremos con cmd.Dir=repoDir para que respete un posible GH_HOST per-repo.
	authCmd := exec.Command("gh", "auth", "status")
	if repoDir != "" {
		authCmd.Dir = repoDir
	}
	if out, err := authCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gh no autenticado: %s (corré `gh auth login` o usá --mock)", strings.TrimSpace(string(out)))
	}
	// `gh repo view` resuelve el repo activo via el remote del working dir —
	// nos sirve como sanity check y como fuente del nwo para logs.
	viewCmd := exec.Command("gh", "repo", "view", "--json", "nameWithOwner")
	if repoDir != "" {
		viewCmd.Dir = repoDir
	}
	out, err := viewCmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh repo view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh repo view: %w", err)
	}
	var probe struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("parse gh repo view: %w", err)
	}
	if probe.NameWithOwner == "" {
		return nil, errors.New("gh repo view: nameWithOwner vacío")
	}
	return &GhSource{
		repoDir:   repoDir,
		nwo:       probe.NameWithOwner,
		interval:  interval,
		ClosedCap: defaultClosedCap,
	}, nil
}

// Snapshot devuelve el último snapshot conocido. Concurrency-safe.
func (g *GhSource) Snapshot() Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.snap
}

// Run corre el loop del poller hasta que ctx se cancele. El primer poll se
// dispara inmediatamente para que el dashboard tenga datos sin esperar un
// intervalo completo.
func (g *GhSource) Run(ctx context.Context) {
	// Primer poll inmediato. No cancelamos por el ctx antes de intentar —
	// si falla, se loggea y se sigue con el ticker.
	if err := g.refresh(ctx); err != nil {
		log.Printf("dash: initial poll failed: %v", err)
	}
	tick := time.NewTicker(g.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := g.refresh(ctx); err != nil {
				log.Printf("dash: poll failed: %v", err)
			}
		}
	}
}

// ghLabel, ghIssue, ghPR, ghCloseRef, ghCheck son los shapes que devuelve gh
// con `--json ...` en las queries de issues/PRs. Mantenerlos acá (no exportados)
// evita que consumidores dependan de un formato que es propio del parser.
type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	State  string    `json:"state"`
	Labels []ghLabel `json:"labels"`
	// Body es el markdown del issue. Se expone en el tab "Issue" del drawer
	// para entidades fused (contexto histórico). Puede estar vacío.
	Body string `json:"body"`
}

type ghCloseRef struct {
	Number int `json:"number"`
}

type ghCheck struct {
	TypeName   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	State      string `json:"state"`      // StatusContext
	Conclusion string `json:"conclusion"` // CheckRun
	Status     string `json:"status"`     // CheckRun (COMPLETED / IN_PROGRESS / QUEUED)
}

type ghPR struct {
	Number                  int          `json:"number"`
	Title                   string       `json:"title"`
	State                   string       `json:"state"`
	HeadRefName             string       `json:"headRefName"`
	HeadRefOid              string       `json:"headRefOid"`
	Labels                  []ghLabel    `json:"labels"`
	ClosingIssuesReferences []ghCloseRef `json:"closingIssuesReferences"`
	StatusCheckRollup       []ghCheck    `json:"statusCheckRollup"`
}

// refresh es un poll único. Lanza cuatro queries en paralelo (issues open,
// issues closed con label che:closed, PRs open, PRs closed con cap) y
// combina los resultados; si cualquiera falla, marcamos el snapshot stale
// y salimos sin pisar los datos anteriores. Los closed PRs son necesarios
// para que la columna `closed` muestre cards fused (#issue → !PR) — sin
// ellos el merge issue↔PR no encuentra match y el closed cae como
// issue-only.
func (g *GhSource) refresh(ctx context.Context) error {
	type issuesRes struct {
		data []ghIssue
		err  error
	}
	type prsRes struct {
		data []ghPR
		err  error
	}
	issuesCh := make(chan issuesRes, 1)
	closedCh := make(chan issuesRes, 1)
	prsCh := make(chan prsRes, 1)
	closedPRsCh := make(chan prsRes, 1)

	go func() {
		data, err := g.fetchIssues(ctx)
		issuesCh <- issuesRes{data, err}
	}()
	go func() {
		data, err := g.fetchClosedIssues(ctx)
		closedCh <- issuesRes{data, err}
	}()
	go func() {
		data, err := g.fetchPRs(ctx)
		prsCh <- prsRes{data, err}
	}()
	go func() {
		data, err := g.fetchClosedPRs(ctx)
		closedPRsCh <- prsRes{data, err}
	}()

	ir := <-issuesCh
	cr := <-closedCh
	pr := <-prsCh
	cpr := <-closedPRsCh
	if ir.err != nil || cr.err != nil || pr.err != nil || cpr.err != nil {
		err := ir.err
		if err == nil {
			err = cr.err
		}
		if err == nil {
			err = pr.err
		}
		if err == nil {
			err = cpr.err
		}
		g.mu.Lock()
		g.snap.LastErr = err
		g.snap.Stale = len(g.snap.Entities) > 0
		g.mu.Unlock()
		return err
	}

	// Mezclar issues open + closed antes de combinar con PRs. combineEntities
	// no distingue open/closed — el filtro real ocurre via labels (los closed
	// que vienen del query ya tienen che:closed, así que caen en su columna).
	allIssues := make([]ghIssue, 0, len(ir.data)+len(cr.data))
	allIssues = append(allIssues, ir.data...)
	allIssues = append(allIssues, cr.data...)
	// Idem para PRs: mergeamos open + closed antes de combinar. Los closed
	// PRs mantienen closingIssuesReferences, así que se fusionan correctamente
	// con el issue cerrado correspondiente.
	allPRs := make([]ghPR, 0, len(pr.data)+len(cpr.data))
	allPRs = append(allPRs, pr.data...)
	allPRs = append(allPRs, cpr.data...)
	entities := combineEntities(allIssues, allPRs)
	g.mu.Lock()
	g.snap = Snapshot{
		Entities: entities,
		LastOK:   time.Now(),
		LastErr:  nil,
		Stale:    false,
		// NWO se resolvió en NewGhSource via `gh repo view`; lo propagamos
		// al snapshot para que el template arme links absolutos a github.com.
		NWO: g.nwo,
	}
	g.mu.Unlock()
	return nil
}

// fetchIssues corre `gh issue list` y parsea el JSON.
func (g *GhSource) fetchIssues(ctx context.Context) ([]ghIssue, error) {
	cmd := exec.CommandContext(ctx, "gh", "issue", "list",
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,labels,state,body",
	)
	if g.repoDir != "" {
		cmd.Dir = g.repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	return parseIssues(out)
}

// fetchClosedIssues corre `gh issue list --state closed --label che:closed`
// con el cap del poller. Permite que la columna closed muestre los últimos
// N issues completados sin traer todo el histórico del repo (que en un
// proyecto activo puede ser miles).
//
// Si el repo no usa el label che:closed (proyecto recién migrado), gh
// devuelve [] y este path es no-op.
func (g *GhSource) fetchClosedIssues(ctx context.Context) ([]ghIssue, error) {
	limit := g.ClosedCap
	if limit <= 0 {
		limit = defaultClosedCap
	}
	cmd := exec.CommandContext(ctx, "gh", "issue", "list",
		"--state", "closed",
		"--label", labels.CheClosed,
		"--limit", fmt.Sprintf("%d", limit),
		"--json", "number,title,labels,state,body",
	)
	if g.repoDir != "" {
		cmd.Dir = g.repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh issue list (closed): %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh issue list (closed): %w", err)
	}
	return parseIssues(out)
}

// fetchPRs corre `gh pr list` y parsea el JSON.
func (g *GhSource) fetchPRs(ctx context.Context) ([]ghPR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,labels,state,headRefName,headRefOid,closingIssuesReferences,statusCheckRollup",
	)
	if g.repoDir != "" {
		cmd.Dir = g.repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return parsePRs(out)
}

// fetchClosedPRs corre `gh pr list --state closed` con el cap del poller.
// Necesario para que la columna `closed` muestre cards fused (#issue → !PR)
// en vez de issue-only: el merge issue↔PR de combineEntities requiere que
// el PR esté en el snapshot, y los PRs mergeados no aparecen en
// `--state open`. `--state closed` incluye tanto mergeados como cerrados
// sin merge (ambos casos válidos para fusión: el closingIssuesReferences
// del PR sigue apuntando al issue cerrado).
func (g *GhSource) fetchClosedPRs(ctx context.Context) ([]ghPR, error) {
	limit := g.ClosedCap
	if limit <= 0 {
		limit = defaultClosedCap
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--state", "closed",
		"--limit", fmt.Sprintf("%d", limit),
		"--json", "number,title,labels,state,headRefName,headRefOid,closingIssuesReferences,statusCheckRollup",
	)
	if g.repoDir != "" {
		cmd.Dir = g.repoDir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr list (closed): %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr list (closed): %w", err)
	}
	return parsePRs(out)
}

// parseIssues unmarshals el output de `gh issue list --json ...`.
func parseIssues(data []byte) ([]ghIssue, error) {
	var out []ghIssue
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	return out, nil
}

// parsePRs unmarshals el output de `gh pr list --json ...`.
func parsePRs(data []byte) ([]ghPR, error) {
	var out []ghPR
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	return out, nil
}

// combineEntities fusiona issues + PRs en un slice de Entity.
//
// Reglas:
//  1. Cada PR con closingIssuesReferences no vacío → Kind=Fused, IssueNumber
//     del primer close-ref, IssueTitle resuelto de la lista de issues si está.
//     El issue linkeado queda "consumido" y no se emite separado.
//  2. PR sin closingIssuesReferences → log + skip (che execute siempre abre
//     PRs con `closes #N`; uno huérfano no es parte del flow).
//  3. Issue sin ct:plan → skip (no es gestionado por che).
//  4. Issue restante (no consumido, con ct:plan) → Kind=Issue.
func combineEntities(issues []ghIssue, prs []ghPR) []Entity {
	issueByNumber := make(map[int]ghIssue, len(issues))
	for _, i := range issues {
		issueByNumber[i.Number] = i
	}
	consumed := make(map[int]bool, len(prs))

	out := make([]Entity, 0, len(issues)+len(prs))

	// Primero los PRs fusionados — el orden del board dentro de cada columna
	// queda determinado por groupByColumn (que preserva orden de aparición).
	for _, p := range prs {
		if len(p.ClosingIssuesReferences) == 0 {
			log.Printf("dash: pr #%d sin issue linkeado, omitido", p.Number)
			continue
		}
		issueNum := p.ClosingIssuesReferences[0].Number
		consumed[issueNum] = true
		var issueTitle string
		var issueLabels []ghLabel
		var issueBody string
		if i, ok := issueByNumber[issueNum]; ok {
			issueTitle = i.Title
			issueLabels = i.Labels
			issueBody = i.Body
		}
		e := Entity{
			Kind:        KindFused,
			IssueNumber: issueNum,
			IssueTitle:  issueTitle,
			IssueBody:   issueBody,
			PRNumber:    p.Number,
			PRTitle:     p.Title,
			Branch:      p.HeadRefName,
			SHA:         shortSHA(p.HeadRefOid),
		}
		// Labels del issue alimentan Type/Size/Status/PlanVerdict.
		applyLabels(&e, issueLabels)
		// Labels del PR alimentan PRVerdict + Locked (override/merge).
		applyLabels(&e, p.Labels)
		// Checks del PR.
		e.ChecksOK, e.ChecksPending, e.ChecksFail = countChecks(p.StatusCheckRollup)
		out = append(out, e)
	}

	// Después los issues-only con ct:plan que no fueron consumidos por un PR.
	for _, i := range issues {
		if consumed[i.Number] {
			continue
		}
		if !hasLabel(i.Labels, labels.CtPlan) {
			continue
		}
		e := Entity{
			Kind:        KindIssue,
			IssueNumber: i.Number,
			IssueTitle:  i.Title,
			IssueBody:   i.Body,
		}
		applyLabels(&e, i.Labels)
		out = append(out, e)
	}
	return out
}

// applyLabels rellena los campos de Entity derivados de labels: Type, Size,
// Status, PlanVerdict, PRVerdict, Locked. Es aditivo — se puede llamar dos
// veces (issue + PR) y los labels más recientes ganan.
//
// Status se deriva del prefijo `che:` (post-PR1/PR2): che:idea → "idea",
// che:planning → "planning", etc. El label `che:locked` es la única
// excepción — es un marker, no un estado, y prende e.Locked en vez de
// pisar Status. plan-validated:* / validated:* son los verdicts de los
// validadores (no son estados).
func applyLabels(e *Entity, ls []ghLabel) {
	for _, l := range ls {
		name := l.Name
		switch {
		case name == labels.CheLocked:
			e.Locked = true
		case strings.HasPrefix(name, "type:"):
			e.Type = strings.TrimPrefix(name, "type:")
		case strings.HasPrefix(name, "size:"):
			e.Size = strings.TrimPrefix(name, "size:")
		case strings.HasPrefix(name, "plan-validated:"):
			// Chequear plan-validated antes de validated: (más específico
			// gana — sino el case validated: matchearía a plan-validated:X).
			e.PlanVerdict = strings.TrimPrefix(name, "plan-validated:")
		case strings.HasPrefix(name, "validated:"):
			e.PRVerdict = strings.TrimPrefix(name, "validated:")
		case strings.HasPrefix(name, "che:"):
			// che:idea / che:planning / che:plan / che:executing /
			// che:executed / che:validating / che:validated / che:closing /
			// che:closed → sufijo va a Status. che:locked se intercepta
			// arriba (no llega acá).
			e.Status = strings.TrimPrefix(name, "che:")
		}
	}
}

// hasLabel devuelve true si ls contiene un label con ese nombre exacto.
func hasLabel(ls []ghLabel, name string) bool {
	for _, l := range ls {
		if l.Name == name {
			return true
		}
	}
	return false
}

// countChecks cuenta CheckRun + StatusContext en tres buckets: ok, pending,
// fail. Dos shapes distintos:
//   - CheckRun usa `conclusion` (SUCCESS/FAILURE/...) + `status` (COMPLETED/
//     QUEUED/IN_PROGRESS). Un CheckRun con status != COMPLETED todavía no
//     tiene conclusion final → cuenta como pending.
//   - StatusContext usa `state` (SUCCESS/PENDING/FAILURE/ERROR).
//
// Map de estados a buckets:
//
//	ok:      SUCCESS
//	pending: PENDING, QUEUED, IN_PROGRESS, NEUTRAL, "" (no reportado)
//	fail:    FAILURE, TIMED_OUT, CANCELLED, STARTUP_FAILURE, ACTION_REQUIRED, ERROR
func countChecks(checks []ghCheck) (ok, pending, fail int) {
	for _, c := range checks {
		val := c.Conclusion
		if val == "" {
			val = c.State
		}
		// Si status es IN_PROGRESS/QUEUED, el CheckRun todavía no tiene
		// conclusion — tratamos como pending sin importar el state.
		if c.Status != "" && c.Status != "COMPLETED" {
			pending++
			continue
		}
		switch val {
		case "SUCCESS":
			ok++
		case "PENDING", "QUEUED", "IN_PROGRESS", "NEUTRAL", "":
			pending++
		case "FAILURE", "TIMED_OUT", "CANCELLED", "STARTUP_FAILURE", "ACTION_REQUIRED", "ERROR":
			fail++
		default:
			// Defensa: un state/conclusion desconocido lo contamos como pending
			// en vez de perderlo.
			pending++
		}
	}
	return
}

// shortSHA trunca el OID a 7 chars (convención git). Vacío si input vacío.
func shortSHA(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}
