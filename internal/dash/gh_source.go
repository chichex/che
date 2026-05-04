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
//   - PRs sin closingIssuesReferences entran como Kind=KindPR, Status="adopt"
//     (columna "adopt" opt-in) para que el humano pueda adoptarlos con
//     validate/close.
//   - PRs con closingIssuesReferences apuntando a un issue sin ningún label
//     che:* también caen a Status="adopt" (Kind=KindFused, hay issue pero no
//     está trackeado).
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
	"github.com/chichex/che/internal/pipelinelabels"
)

// v2StatusByLabel mapea cada label terminal/applying del modelo v2
// (`che:state:*` y `che:state:applying:*`) al string canónico de Status que
// el resto del paquete dash espera (idea, planning, plan, executing,
// executed, validating, validated, closing, closed). Es la fuente de verdad
// del parser en applyLabels — agregar un step nuevo al pipeline declarativo
// implica agregar acá su entrada (terminal + applying si corresponde).
//
// Decisión: aunque v1 colapsaba "validate sobre plan" y "validate sobre PR"
// en `che:validating`/`che:validated`, v2 solo tiene un step `validate_pr`.
// Mapeamos `che:state:validate_pr` → `validated` y
// `che:state:applying:validate_pr` → `validating` para preservar las
// columnas históricas del kanban (loop.go/preflight.go usan esos strings).
var v2StatusByLabel = map[string]string{
	pipelinelabels.StateIdea:               "idea",
	pipelinelabels.StateApplyingExplore:    "planning",
	pipelinelabels.StateExplore:            "plan",
	pipelinelabels.StateApplyingExecute:    "executing",
	pipelinelabels.StateExecute:            "executed",
	pipelinelabels.StateApplyingValidatePR: "validating",
	pipelinelabels.StateValidatePR:         "validated",
	pipelinelabels.StateApplyingClose:      "closing",
	pipelinelabels.StateClose:              "closed",
}

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

	// bump es un canal 1-buffered para forzar un refresh entre ticks (ver
	// Bump). Capacidad 1 → múltiples Bump() consecutivos se coalescen a
	// uno solo; nunca bloquea al caller. El loop lo drena en Run.
	bump chan struct{}

	// refreshFn es el helper que Run dispara en cada tick/bump. Default
	// g.refresh; se sobreescribe en tests para ejercer la lógica del select
	// (canal bump + ticker + minGap) sin spawnear procesos `gh` reales.
	refreshFn func(context.Context) error

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
		// cap 1: un solo bump pendiente alcanza; llamadas extra se dropean.
		bump: make(chan struct{}, 1),
	}, nil
}

// Compile-time check: GhSource implementa Source + Bumper. MockSource solo
// implementa Source (sus datos son estáticos, no necesita bump).
var (
	_ Source = (*GhSource)(nil)
	_ Bumper = (*GhSource)(nil)
)

// Snapshot devuelve el último snapshot conocido. Concurrency-safe.
func (g *GhSource) Snapshot() Snapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.snap
}

// Bump pide un refresh ASAP — implementa la interface Bumper. Se llama
// desde el server cuando hay una transición local (ej: termina un subproceso
// che, o el handler POST /action acaba de disparar uno) para que el board
// refleje el cambio de label sin esperar al próximo tick del ticker.
//
// El canal tiene capacidad 1 → si ya hay un bump pendiente, el segundo se
// dropea silenciosamente. Es una coalescencia deliberada: no queremos 10
// refreshes en fila por 10 clicks seguidos.
func (g *GhSource) Bump() {
	select {
	case g.bump <- struct{}{}:
	default:
	}
}

// bumpMinGap es el intervalo mínimo entre dos refreshes disparados por el
// loop de Run. Protege contra tormentas de refreshes si un Bump cae justo
// después de un tick del ticker (o viceversa). 2s es arbitrario — suficiente
// para dejar que el subproceso `che` escriba su label y el próximo refresh
// lo vea, sin congestionar `gh` con calls redundantes.
const bumpMinGap = 2 * time.Second

// Run corre el loop del poller hasta que ctx se cancele. El primer poll se
// dispara inmediatamente para que el dashboard tenga datos sin esperar un
// intervalo completo. Además del ticker baseline, el loop escucha el canal
// `bump` para poder refrescar ASAP después de una acción local (ver Bump).
func (g *GhSource) Run(ctx context.Context) {
	refresh := g.refreshFn
	if refresh == nil {
		refresh = g.refresh
	}

	// lastRefresh trackea el último refresh exitoso (o intentado) para
	// aplicar el minGap del canal bump. Se setea acá (no en el field del
	// struct) porque solo el loop lo toca y no hace falta sincronizar.
	var lastRefresh time.Time

	doRefresh := func(src string) {
		if !lastRefresh.IsZero() && time.Since(lastRefresh) < bumpMinGap {
			// Dentro del minGap — el refresh previo es suficientemente
			// reciente como para que el cambio de label ya esté en el
			// snapshot (o llegue en el próximo tick). Evita hammering.
			return
		}
		lastRefresh = time.Now()
		if err := refresh(ctx); err != nil {
			log.Printf("dash: %s poll failed: %v", src, err)
		}
	}

	// Primer poll inmediato. Setea lastRefresh para que un Bump() que llegue
	// en los próximos bumpMinGap no re-dispare (la data ya está fresca).
	lastRefresh = time.Now()
	if err := refresh(ctx); err != nil {
		log.Printf("dash: initial poll failed: %v", err)
	}

	tick := time.NewTicker(g.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			doRefresh("tick")
		case <-g.bump:
			doRefresh("bump")
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
	// CreatedAt alimenta Entity.CreatedAt para la priorización del auto-loop
	// (más viejo primero). gh devuelve RFC3339, que encoding/json mapea a
	// time.Time nativamente.
	CreatedAt time.Time `json:"createdAt"`
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
	// CreatedAt alimenta Entity.CreatedAt. Para entidades fused tomamos la
	// más reciente entre issue.createdAt y pr.createdAt (ver combineEntities).
	CreatedAt time.Time `json:"createdAt"`
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
		"--json", "number,title,labels,state,body,createdAt",
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

// fetchClosedIssues corre `gh issue list --state closed` filtrando por
// label de cierre — v2 (`che:state:close`) o v1 (`che:closed` legacy) — con
// el cap del poller. Permite que la columna closed muestre los últimos N
// issues completados sin traer todo el histórico del repo (en un proyecto
// activo puede ser miles).
//
// Combina ambas familias en un solo slice deduplicado por número. Si el
// repo nunca usó ninguno de los dos labels (recién migrado y nada cerrado
// todavía) gh devuelve [] y este path es no-op.
//
// REMOVE IN PR6d: cuando todos los repos usen v2, el label v1 deja de
// existir y queda solo la primera query.
func (g *GhSource) fetchClosedIssues(ctx context.Context) ([]ghIssue, error) {
	limit := g.ClosedCap
	if limit <= 0 {
		limit = defaultClosedCap
	}
	fetch := func(label string) ([]ghIssue, error) {
		cmd := exec.CommandContext(ctx, "gh", "issue", "list",
			"--state", "closed",
			"--label", label,
			"--limit", fmt.Sprintf("%d", limit),
			"--json", "number,title,labels,state,body,createdAt",
		)
		if g.repoDir != "" {
			cmd.Dir = g.repoDir
		}
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("gh issue list (closed, %s): %s", label, strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, fmt.Errorf("gh issue list (closed, %s): %w", label, err)
		}
		return parseIssues(out)
	}
	v2Issues, err := fetch(pipelinelabels.StateClose)
	if err != nil {
		return nil, err
	}
	// v1 legacy (`che:closed`) — REMOVE IN PR6d cuando ya no haya repos
	// sin migrar. El string vive literal acá porque post-PR6c la constante
	// del paquete labels ya no existe; este es el último consumidor (junto
	// con migrate-labels-v2 y los guards rejectV1Labels).
	v1Issues, err := fetch("che:closed")
	if err != nil {
		return nil, err
	}
	// Dedup por número (un issue migrado a medias podría tener ambos).
	seen := make(map[int]struct{}, len(v2Issues)+len(v1Issues))
	out := make([]ghIssue, 0, len(v2Issues)+len(v1Issues))
	for _, i := range v2Issues {
		if _, ok := seen[i.Number]; ok {
			continue
		}
		seen[i.Number] = struct{}{}
		out = append(out, i)
	}
	for _, i := range v1Issues {
		if _, ok := seen[i.Number]; ok {
			continue
		}
		seen[i.Number] = struct{}{}
		out = append(out, i)
	}
	return out, nil
}

// fetchPRs corre `gh pr list` y parsea el JSON.
func (g *GhSource) fetchPRs(ctx context.Context) ([]ghPR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--state", "open",
		"--limit", "100",
		"--json", "number,title,labels,state,headRefName,headRefOid,closingIssuesReferences,statusCheckRollup,createdAt",
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
		"--json", "number,title,labels,state,headRefName,headRefOid,closingIssuesReferences,statusCheckRollup,createdAt",
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
//     El issue linkeado queda "consumido" y no se emite separado. Si el
//     issue NO tiene ningún label che:* → Status="adopt" (Kind sigue siendo
//     Fused, hay issue linkeado; solo no está trackeado por che).
//  2. PR sin closingIssuesReferences → Kind=KindPR, Status="adopt". La
//     columna "adopt" es opt-in (toggle del header del dash); con el toggle
//     OFF estos no se ven.
//  3. Issue sin ct:plan → skip (no es gestionado por che).
//  4. Issue restante (no consumido, con ct:plan) → Kind=Issue.
//  5. Issue (KindIssue) sin ningún label `che:*` → skip. No cae al default
//     "idea" del board porque enmascara issues mal tageados como si fueran
//     ideas legítimas. El humano los ve con `gh issue list` si necesita
//     hacer triage. (Para fused / PR huérfanos usamos adopt en vez de skip.)
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
			// PR huérfano (sin close-keyword). Post-stateref v0.0.61 el PR
			// puede tener che:* directo (validate/iterate/close PR-mode caen
			// al PR cuando no hay issue con che:*). Si applyLabels detecta
			// uno, respetamos ese estado: el dash debe mostrar el PR en su
			// columna real, no en adopt.
			//
			// Si después de applyLabels el Status sigue vacío → adopt
			// genuino (PR sin che:* en ningún lado). Para esos solo
			// emitimos los OPEN: closed/merged orphans sin che:* no son
			// adoptables — no hay nada que validar ni cerrar, ya están
			// resueltos. Caían acá porque fetchClosedPRs los traía para
			// fusionar con issues closed; los descartamos silenciosamente.
			e := Entity{
				Kind:      KindPR,
				PRNumber:  p.Number,
				PRTitle:   p.Title,
				Branch:    p.HeadRefName,
				SHA:       shortSHA(p.HeadRefOid),
				CreatedAt: p.CreatedAt,
			}
			applyLabels(&e, p.Labels)
			if e.Status == "" {
				if p.State != "OPEN" {
					continue
				}
				e.Status = "adopt"
			}
			e.ChecksOK, e.ChecksPending, e.ChecksFail = countChecks(p.StatusCheckRollup)
			out = append(out, e)
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
		var issueCreatedAt time.Time
		if i, ok := issueByNumber[issueNum]; ok {
			issueCreatedAt = i.CreatedAt
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
			// Para fused, la fecha efectiva es max(issue, PR): si iteraste el
			// PR hace poco, la entity "envejece" menos y el loop la posterga
			// respecto a otras más viejas.
			CreatedAt: laterOf(issueCreatedAt, p.CreatedAt),
		}
		// Labels del issue alimentan Type/Size/Status/PlanVerdict.
		applyLabels(&e, issueLabels)
		// Labels del PR alimentan PRVerdict + Locked (override/merge).
		applyLabels(&e, p.Labels)
		if e.Status == "" {
			// Adopt mode: PR con close-keyword pero el issue linkeado no
			// tiene ningún label che:*. Hay issue → mantenemos KindFused
			// (drawer puede mostrar el body/ref del issue), pero el status
			// es "adopt" para que caiga en esa columna.
			//
			// Filtro: solo si el PR está OPEN. Un PR closed/merged sin
			// che:* en el issue ya está resuelto; mostrarlo en adopt
			// confunde (el usuario reportó "no me debería mostrar
			// untracked cosas que ya están cerradas", abril 2026). Si
			// querían trackear el cierre con `che close`, ya es tarde —
			// el PR está cerrado. Se filtra silenciosamente.
			if p.State != "OPEN" {
				continue
			}
			e.Status = "adopt"
		}
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
			CreatedAt:   i.CreatedAt,
		}
		applyLabels(&e, i.Labels)
		if e.Status == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// laterOf devuelve la más reciente entre dos time.Time. Un zero se trata como
// "sin dato" — gana el otro. Si ambos son zero, devuelve zero.
func laterOf(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.After(b) {
		return a
	}
	return b
}

// applyLabels rellena los campos de Entity derivados de labels: Type, Size,
// Status, PlanVerdict, PRVerdict, Locked. Es aditivo — se puede llamar dos
// veces (issue + PR) y los labels más recientes ganan.
//
// Status soporta DOS modelos durante la transición v1→v2:
//   - v2 (canónico post-PR6c): `che:state:<step>` (terminal) y
//     `che:state:applying:<step>` (lock óptimista). Los strings de Status
//     del kanban (idea/planning/plan/executing/executed/validating/
//     validated/closing/closed) se preservan via v2StatusByLabel.
//   - v1 (legacy): `che:idea` / `che:plan` / etc. Para que repos no migrados
//     no se queden sin Status en el dash. Cuando todos los repos corran
//     `che migrate-labels-v2` esta rama queda como dead code (REMOVE IN
//     PR6d).
//
// `che:locked` prende e.Locked, no pisa Status. plan-validated:* / validated:*
// son verdicts de los validadores (no son estados).
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
		case strings.HasPrefix(name, pipelinelabels.PrefixState):
			// v2: `che:state:<step>` o `che:state:applying:<step>`.
			// Mapeamos via v2StatusByLabel; si el label no está en el
			// mapa (por ej. step nuevo no registrado), preservamos
			// best-effort el sufijo TrimPrefix.
			if parsed, err := pipelinelabels.Parse(name); err == nil {
				e.StateStep = parsed.Step
				e.StateApplying = parsed.Kind == pipelinelabels.KindApplying
			}
			if s, ok := v2StatusByLabel[name]; ok {
				e.Status = s
			} else if strings.HasPrefix(name, pipelinelabels.PrefixApplying) {
				e.Status = strings.TrimPrefix(name, pipelinelabels.PrefixApplying)
			} else {
				e.Status = strings.TrimPrefix(name, pipelinelabels.PrefixState)
			}
		case strings.HasPrefix(name, "che:"):
			// v1 legacy: che:idea / che:planning / che:plan / che:executing /
			// che:executed / che:validating / che:validated / che:closing /
			// che:closed. che:locked y che:state:* se interceptan arriba.
			// REMOVE IN PR6d cuando ya no haya repos sin migrar.
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
