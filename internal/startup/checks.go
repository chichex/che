package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Nombres canónicos de los chequeos. Se usan tanto como ID para
// IsSkipped/MarkSkipped como key para identificar resultados.
const (
	CheckMigrateLabels = "migrate-labels"
	CheckVersion       = "version"
	CheckLocks         = "locks"
)

// oldStatusLabels enumera los labels del modelo viejo `status:*` que
// `che migrate-labels` renombra a `che:*`. Si encontramos cualquiera
// de estos en el repo, el check de labels triggerea.
//
// Importante: estos strings viven duplicados con cmd/migrate_labels.go
// a propósito — son input de migración, no constantes runtime, y el
// paquete startup no debería depender del paquete cmd (ciclo de
// imports y acoplamiento innecesario).
var oldStatusLabels = []string{
	"status:idea",
	"status:plan",
	"status:executing",
	"status:executed",
	"status:closed",
}

// staleLockThreshold es el umbral por encima del cual consideramos que
// un che:locked está "colgado". Una hora cubre con margen el peor caso
// real (ejecutar un agente largo) sin warnar por flows en curso.
const staleLockThreshold = time.Hour

// LockedItem describe un issue/PR lockeado que el check de locks
// devuelve. Reutiliza el mismo shape que internal/labels.LockedRef pero
// duplicado acá para no acoplar paquetes (el paquete startup no debería
// depender de labels y viceversa).
type LockedItem struct {
	Number    int
	Title     string
	IsPR      bool
	UpdatedAt time.Time
}

// Result describe el resultado de UN chequeo. Triggered=true significa
// "encontramos algo que mostrar al usuario"; en false el banner ignora
// este resultado completamente. Si Err != nil el check falló (ej. gh
// sin auth) y la TUI también lo ignora — los chequeos son secundarios.
type Result struct {
	Name      string
	Triggered bool
	Err       error

	// Campos específicos por tipo de chequeo. Solo se leen cuando
	// Triggered==true. Mantenemos el shape plano (sin interface{}) para
	// que el render de la TUI no tenga que hacer type asserts.

	// migrate-labels:
	OldLabels []string

	// version:
	CurrentVersion string
	LatestVersion  string

	// locks:
	Locks []LockedItem
}

// Runner es la abstracción inyectable para correr `gh`. En producción
// usa exec.CommandContext; los tests inyectan una implementación que
// devuelve outputs scripted sin tocar la red ni el filesystem.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner es la implementación default que ejecuta procesos reales
// con exec.CommandContext. Respeta el ctx para timeouts.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return out, err
	}
	return out, nil
}

// Options agrupa lo que necesita RunChecks: el repo donde estamos
// (para IsSkipped, y como guard de "estoy en un repo git"), la versión
// actual del binario, el runner inyectable, y un timeout total. Con
// Runner=nil usa el runner default (exec real).
type Options struct {
	RepoRoot       string
	CurrentVersion string
	Runner         Runner
	Timeout        time.Duration
}

// RunChecks ejecuta los 3 chequeos en paralelo, respeta el timeout
// total (los que no completan a tiempo se marcan como no-triggered y
// se siguen), y devuelve los resultados en orden canónico
// (migrate-labels, version, locks). Skipea silenciosamente los
// chequeos marcados como "nunca para este repo" en el persistence
// file. Si no hay `.git/`, devuelve nil sin correr nada.
func RunChecks(ctx context.Context, opts Options) []Result {
	if !HasGitDir(opts.RepoRoot) {
		return nil
	}
	runner := opts.Runner
	if runner == nil {
		runner = execRunner{}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type job struct {
		name string
		fn   func(context.Context, Runner) Result
	}
	jobs := []job{
		{CheckMigrateLabels, checkMigrateLabels},
		{CheckVersion, func(ctx context.Context, r Runner) Result {
			return checkVersion(ctx, r, opts.CurrentVersion)
		}},
		{CheckLocks, checkLocks},
	}

	results := make([]Result, len(jobs))
	done := make(chan struct {
		idx int
		res Result
	}, len(jobs))

	for i, j := range jobs {
		i, j := i, j
		// Skip persisted: marcamos como no-triggered sin correr la
		// función. El array preserva el orden por índice.
		if IsSkipped(opts.RepoRoot, j.name) {
			done <- struct {
				idx int
				res Result
			}{idx: i, res: Result{Name: j.name}}
			continue
		}
		go func() {
			res := Result{Name: j.name}
			defer func() {
				// Cualquier panic en un check secundario no debe romper
				// la TUI: capturamos y marcamos como error silencioso.
				if r := recover(); r != nil {
					res = Result{Name: j.name, Err: fmt.Errorf("panic: %v", r)}
				}
				done <- struct {
					idx int
					res Result
				}{idx: i, res: res}
			}()
			res = j.fn(ctx, runner)
		}()
	}

	// Esperar a todos o al timeout. Si el timeout se dispara, los
	// goroutines siguen corriendo en background — bonito pero no
	// crítico: exec.CommandContext mata el subproceso cuando el ctx
	// se cancela, así que no hay leak de procesos. La goroutine en
	// sí puede completar tarde, pero como el array ya quedó con su
	// zero value (Result{}, no triggered), su escritura tardía no
	// afecta a la UI (que ya leyó el snapshot).
	for i := 0; i < len(jobs); i++ {
		select {
		case r := <-done:
			results[r.idx] = r.res
		case <-ctx.Done():
			// Timeout: devolvemos lo que tengamos (los slots no
			// completos quedan como Result{} = no triggered).
			return results
		}
	}
	return results
}

// ---- Implementaciones de cada chequeo ----

// checkMigrateLabels lista los labels del repo con `gh label list
// --search "status:"` y filtra exact-match contra los 5 status:* del
// modelo viejo. Si encuentra ≥1, triggerea.
func checkMigrateLabels(ctx context.Context, runner Runner) Result {
	res := Result{Name: CheckMigrateLabels}
	out, err := runner.Run(ctx, "gh", "label", "list",
		"--search", "status:",
		"--json", "name",
		"--limit", "100",
	)
	if err != nil {
		res.Err = err
		return res
	}
	var list []struct {
		Name string `json:"name"`
	}
	if jerr := json.Unmarshal(out, &list); jerr != nil {
		res.Err = jerr
		return res
	}
	old := map[string]bool{}
	for _, n := range oldStatusLabels {
		old[n] = true
	}
	var found []string
	seen := map[string]bool{}
	for _, l := range list {
		if old[l.Name] && !seen[l.Name] {
			found = append(found, l.Name)
			seen[l.Name] = true
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		res.Triggered = true
		res.OldLabels = found
	}
	return res
}

// checkVersion compara la versión del binario contra la última release
// publicada en GitHub. Skipea si CurrentVersion=="dev" (build local).
// Compara como strings tras normalizar el prefijo "v" para tolerar
// tags con o sin él.
func checkVersion(ctx context.Context, runner Runner, current string) Result {
	res := Result{Name: CheckVersion, CurrentVersion: current}
	if current == "" || current == "dev" {
		// Build local: nunca triggereamos.
		return res
	}
	out, err := runner.Run(ctx, "gh", "release", "view",
		"--repo", "chichex/che",
		"--json", "tagName",
		"--jq", ".tagName",
	)
	if err != nil {
		res.Err = err
		return res
	}
	latest := strings.TrimSpace(string(out))
	res.LatestVersion = latest
	if latest == "" {
		// Respuesta vacía: no triggereamos.
		return res
	}
	if normalizeVersion(current) != normalizeVersion(latest) {
		res.Triggered = true
	}
	return res
}

// normalizeVersion saca el prefijo "v" para comparar tags. Duplicado a
// propósito desde cmd/upgrade.go — no queremos que el paquete startup
// dependa de cmd.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// checkLocks lista issues/PRs con label che:locked en el repo actual y
// filtra los que llevan más de staleLockThreshold sin actualizarse. Si
// encuentra ≥1, triggerea con la lista para mostrar.
//
// Acota la búsqueda al repo actual via `repo:owner/name` query (gh
// search issues no acepta --repo). Si no podemos resolver el repo
// (ej. fuera de un repo git, o gh no auth), degrada silenciosamente
// y reporta error sin triggerear.
func checkLocks(ctx context.Context, runner Runner) Result {
	res := Result{Name: CheckLocks}
	nwo, err := runner.Run(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		res.Err = err
		return res
	}
	repo := strings.TrimSpace(string(nwo))
	if repo == "" {
		res.Err = errors.New("nameWithOwner vacío")
		return res
	}
	out, err := runner.Run(ctx, "gh", "search", "issues",
		"repo:"+repo,
		"label:che:locked",
		"state:open",
		"--json", "number,title,updatedAt,isPullRequest",
		"--limit", "100",
	)
	if err != nil {
		res.Err = err
		return res
	}
	var raw []struct {
		Number        int    `json:"number"`
		Title         string `json:"title"`
		UpdatedAt     string `json:"updatedAt"`
		IsPullRequest bool   `json:"isPullRequest"`
	}
	if jerr := json.Unmarshal(out, &raw); jerr != nil {
		res.Err = jerr
		return res
	}
	now := time.Now()
	var stale []LockedItem
	for _, r := range raw {
		t, perr := time.Parse(time.RFC3339, r.UpdatedAt)
		if perr != nil {
			// Fecha rota: la ignoramos. No queremos romper el check.
			continue
		}
		if now.Sub(t) <= staleLockThreshold {
			continue
		}
		stale = append(stale, LockedItem{
			Number:    r.Number,
			Title:     r.Title,
			IsPR:      r.IsPullRequest,
			UpdatedAt: t,
		})
	}
	if len(stale) > 0 {
		res.Triggered = true
		res.Locks = stale
	}
	return res
}

// AnyTriggered devuelve true si al menos un Result en results está
// triggered. Helper para el caller (la TUI) — evita filtrar en la mano.
func AnyTriggered(results []Result) bool {
	for _, r := range results {
		if r.Triggered {
			return true
		}
	}
	return false
}
