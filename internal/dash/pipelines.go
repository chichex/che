package dash

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/pipelines"
	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// lastRunSummary holds the minimal info for the most recent run of a pipeline.
type lastRunSummary struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
}

// lastRunCache is a simple in-memory cache with TTL 1s for lookupLastRun.
var lastRunCache struct {
	mu      sync.Mutex
	entries map[string]lastRunCacheEntry
}

type lastRunCacheEntry struct {
	result  *lastRunSummary
	fetchedAt time.Time
}

func init() {
	lastRunCache.entries = make(map[string]lastRunCacheEntry)
}

// pipelineJSON is the wire shape for GET /api/pipelines (list item).
// Scope expone el origen ("project" | "global" | "builtin") para que el
// frontend pueda renderizar el badge correspondiente. Builtin se mantiene
// por back-compat con clientes que ya lo leen — es derivable de Scope.
type pipelineJSON struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Status      string          `json:"status"`
	LastRun     *lastRunSummary `json:"last_run,omitempty"`
	Builtin     bool            `json:"builtin,omitempty"`
	Scope       string          `json:"scope,omitempty"`
}

// pipelineDetailJSON is the wire shape for GET /api/pipelines/:slug.
type pipelineDetailJSON struct {
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	Steps       []stepJSON `json:"steps"`
	Builtin     bool       `json:"builtin,omitempty"`
	Scope       string     `json:"scope,omitempty"`
}

// stepJSON is the wire shape for a pipeline step.
type stepJSON struct {
	Name      string         `json:"name"`
	CLI       string         `json:"cli,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Content   string         `json:"content,omitempty"`
	Input     string         `json:"input,omitempty"`
	Validator *validatorJSON `json:"validator,omitempty"`
}

// validatorJSON is the wire shape for an optional step validator.
type validatorJSON struct {
	CLI     string `json:"cli"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// pipelineStatus returns "ready" if pipeline has no Status block, "draft" otherwise.
func pipelineStatus(p wizard.Pipeline) string {
	if p.Status == nil {
		return "ready"
	}
	return "draft"
}

// toStepJSON converts a wizard.Step to the wire shape, omitting Validator if nil.
func toStepJSON(s wizard.Step) stepJSON {
	sj := stepJSON{
		Name:    s.Name,
		CLI:     s.CLI,
		Kind:    s.Kind,
		Content: s.Content,
		Input:   s.Input,
	}
	if s.Validator != nil {
		sj.Validator = &validatorJSON{
			CLI:     s.Validator.CLI,
			Kind:    s.Validator.Kind,
			Content: s.Validator.Content,
		}
	}
	return sj
}

// listAll devuelve los pipelines visibles desde cwd+pipelinesDir en el
// mismo orden que pipelines.List (project → global → builtin, ordenado
// por slug). cwd vacio desactiva el scope project; pipelinesDir vacio
// desactiva el scope global.
func listAll(cwd, pipelinesDir string) []pipelines.Resolved {
	out, err := pipelines.ListInDirs(cwd, pipelinesDir)
	if err != nil {
		log.Printf("[dash] pipelines.ListInDirs: %v", err)
		return nil
	}
	return out
}

// lookupLastRun reads the most-recent run manifest for slug from runsDir.
// Results are cached with a 1s TTL.
// Returns nil if no runs exist.
func lookupLastRun(runsDir, slug string) *lastRunSummary {
	if runsDir == "" {
		return nil
	}

	cacheKey := runsDir + "/" + slug

	lastRunCache.mu.Lock()
	if entry, ok := lastRunCache.entries[cacheKey]; ok && time.Since(entry.fetchedAt) < time.Second {
		result := entry.result
		lastRunCache.mu.Unlock()
		return result
	}
	lastRunCache.mu.Unlock()

	result := fetchLastRun(runsDir, slug)

	lastRunCache.mu.Lock()
	lastRunCache.entries[cacheKey] = lastRunCacheEntry{result: result, fetchedAt: time.Now()}
	lastRunCache.mu.Unlock()

	return result
}

// fetchLastRun performs the actual disk read to find the most recent run.
func fetchLastRun(runsDir, slug string) *lastRunSummary {
	slugDir := filepath.Join(runsDir, slug)
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		return nil
	}

	var (
		bestID        string
		bestStatus    string
		bestStartedAt time.Time
	)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		manifestPath := filepath.Join(slugDir, runID, "manifest.yaml")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		// Minimal parse: only need status and started_at.
		var m struct {
			Status    string    `yaml:"status"`
			StartedAt time.Time `yaml:"started_at"`
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.StartedAt.IsZero() {
			continue
		}
		if bestID == "" || m.StartedAt.After(bestStartedAt) {
			bestID = runID
			bestStatus = m.Status
			bestStartedAt = m.StartedAt
		}
	}

	if bestID == "" {
		return nil
	}
	return &lastRunSummary{
		ID:        bestID,
		Status:    bestStatus,
		StartedAt: bestStartedAt.Format(time.RFC3339),
	}
}

// handleListPipelines returns an http.HandlerFunc for GET /api/pipelines.
// It merges pipelines de scope project (cwd-local), global (dir) y
// builtins via pipelines.ListInDirs — el orden de override es project
// → global → builtin. Returns JSON array (never null). runsDir is used
// to populate last_run. cwd="" desactiva scope project — los tests
// invocan asi (sin cwd) y conservan el comportamiento previo.
func handleListPipelines(cwd, dir, runsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all := listAll(cwd, dir)
		list := make([]pipelineJSON, 0, len(all))
		for _, r := range all {
			list = append(list, pipelineJSON{
				Slug:        r.Slug,
				Name:        r.Pipeline.Name,
				Description: r.Pipeline.Description,
				Status:      pipelineStatus(r.Pipeline),
				LastRun:     lookupLastRun(runsDir, r.Slug),
				Builtin:     r.Scope == pipelines.ScopeBuiltin,
				Scope:       r.Scope.String(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

// handleGetPipeline returns an http.HandlerFunc for GET /api/pipelines/:slug.
// It extracts the slug from the URL path, resolves project → global →
// builtin via pipelines.ResolveInDirs, y devuelve el full pipeline
// detail. Returns 404 JSON si el slug no existe, 500 on systemic
// errors. cwd="" desactiva scope project (path usado por tests).
//
// Deprecated: prefer calling getPipelineDetail directly from the dispatcher.
// This function is kept for backward compatibility with tests.
func handleGetPipeline(cwd, dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/api/pipelines/")
		slug = strings.TrimSuffix(slug, "/")
		if slug == "" || strings.ContainsAny(slug, "/\\") {
			http.Error(w, `{"error":"pipeline not found"}`, http.StatusNotFound)
			return
		}
		getPipelineDetail(cwd, dir, slug, w, r)
	}
}

// getPipelineDetail resuelve un pipeline por slug usando
// pipelines.ResolveInDirs (project → global → builtin) y escribe la
// respuesta JSON. Errores distintos a "no encontrado" devuelven 500
// con prefijo [dash].
func getPipelineDetail(cwd, dir, slug string, w http.ResponseWriter, _ *http.Request) {
	res, found, err := pipelines.ResolveInDirs(cwd, dir, slug)
	if err != nil {
		log.Printf("[dash] resolve %s: %v", slug, err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	if !found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"pipeline not found"}`))
		return
	}

	steps := make([]stepJSON, 0, len(res.Pipeline.Steps))
	for _, s := range res.Pipeline.Steps {
		steps = append(steps, toStepJSON(s))
	}

	detail := pipelineDetailJSON{
		Slug:        res.Slug,
		Name:        res.Pipeline.Name,
		Description: res.Pipeline.Description,
		Status:      pipelineStatus(res.Pipeline),
		Steps:       steps,
		Builtin:     res.Scope == pipelines.ScopeBuiltin,
		Scope:       res.Scope.String(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}
