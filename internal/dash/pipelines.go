package dash

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
type pipelineJSON struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Status      string          `json:"status"`
	LastRun     *lastRunSummary `json:"last_run,omitempty"`
	Builtin     bool            `json:"builtin,omitempty"`
}

// pipelineDetailJSON is the wire shape for GET /api/pipelines/:slug.
type pipelineDetailJSON struct {
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	Steps       []stepJSON `json:"steps"`
	Builtin     bool       `json:"builtin,omitempty"`
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

// loadPipelinesFromDir reads all *.yaml files from dir, parses them with
// wizard.Load, and returns a slice of (slug, pipeline) pairs. Parse errors
// are logged to stderr with prefix [dash] and skipped.
func loadPipelinesFromDir(dir string) []struct {
	slug string
	p    wizard.Pipeline
} {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[dash] readdir %s: %v", dir, err)
		}
		return nil
	}
	var result []struct {
		slug string
		p    wizard.Pipeline
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		slug := strings.TrimSuffix(name, ".yaml")
		path := filepath.Join(dir, name)
		p, err := wizard.Load(path)
		if err != nil {
			log.Printf("[dash] load %s: %v", path, err)
			continue
		}
		result = append(result, struct {
			slug string
			p    wizard.Pipeline
		}{slug: slug, p: p})
	}
	return result
}

// pipelinePair is an enriched (slug, pipeline, builtin) tuple used internally.
type pipelinePair struct {
	slug    string
	p       wizard.Pipeline
	builtin bool
}

// mergeBuiltinsAndDisk combines builtin pipelines with pipelines loaded from
// dir. On-disk entries win on slug collision (consistent with copy-on-edit).
// If wizard.Builtins() fails it is logged with prefix [dash] and execution
// continues with on-disk only. The result is sorted stably by slug.
func mergeBuiltinsAndDisk(dir string) []pipelinePair {
	// Load on-disk pipelines.
	diskEntries := loadPipelinesFromDir(dir)
	diskBySlug := make(map[string]struct{}, len(diskEntries))
	for _, e := range diskEntries {
		diskBySlug[e.slug] = struct{}{}
	}

	// Load builtins.
	builtins, err := wizard.Builtins()
	if err != nil {
		log.Printf("[dash] wizard.Builtins(): %v", err)
		builtins = nil
	}

	// Start with builtins that are not overridden on disk.
	result := make([]pipelinePair, 0, len(builtins)+len(diskEntries))
	for _, b := range builtins {
		if _, overridden := diskBySlug[b.Slug]; overridden {
			continue
		}
		result = append(result, pipelinePair{slug: b.Slug, p: b.Pipeline, builtin: true})
	}
	// Append on-disk entries (not marked as builtin even if slug matches).
	for _, e := range diskEntries {
		result = append(result, pipelinePair{slug: e.slug, p: e.p, builtin: false})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].slug < result[j].slug
	})
	return result
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
// It merges builtin pipelines with on-disk ones (on-disk-wins), skipping
// corrupt files. Returns JSON array (never null).
// runsDir is used to populate last_run for each pipeline.
func handleListPipelines(dir, runsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pairs := mergeBuiltinsAndDisk(dir)
		list := make([]pipelineJSON, 0, len(pairs))
		for _, pair := range pairs {
			list = append(list, pipelineJSON{
				Slug:        pair.slug,
				Name:        pair.p.Name,
				Description: pair.p.Description,
				Status:      pipelineStatus(pair.p),
				LastRun:     lookupLastRun(runsDir, pair.slug),
				Builtin:     pair.builtin,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

// handleGetPipeline returns an http.HandlerFunc for GET /api/pipelines/:slug.
// It extracts the slug from the URL path, loads the matching YAML, and
// returns the full pipeline detail including steps. Returns 404 JSON if
// the slug does not exist, 500 on systemic errors.
//
// Deprecated: prefer calling getPipelineDetail directly from the dispatcher.
// This function is kept for backward compatibility with tests.
func handleGetPipeline(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/api/pipelines/")
		slug = strings.TrimSuffix(slug, "/")
		if slug == "" || strings.ContainsAny(slug, "/\\") {
			http.Error(w, `{"error":"pipeline not found"}`, http.StatusNotFound)
			return
		}
		getPipelineDetail(dir, slug, w, r)
	}
}

// getPipelineDetail is the helper that loads a pipeline by slug from dir and
// writes the JSON detail response. When the on-disk YAML does not exist it
// falls back to wizard.BuiltinBySlug. Used by both handleGetPipeline and the
// dispatcher in dispatchPipelinesPrefix.
func getPipelineDetail(dir, slug string, w http.ResponseWriter, r *http.Request) {
	var (
		p         wizard.Pipeline
		foundDisk bool
		isBuiltin bool
	)

	// Try loading from disk first.
	if dir != "" {
		path := filepath.Join(dir, slug+".yaml")
		loaded, err := wizard.Load(path)
		if err == nil {
			p = loaded
			foundDisk = true
		} else if os.IsNotExist(err) {
			// Fall through to builtin lookup.
		} else {
			log.Printf("[dash] load %s: %v", path, err)
			http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			return
		}
	}

	// If not found on disk, try builtins.
	if !foundDisk {
		b, err := wizard.BuiltinBySlug(slug)
		if err != nil {
			log.Printf("[dash] wizard.BuiltinBySlug(%s): %v", slug, err)
			http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			return
		}
		if b == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"pipeline not found"}`))
			return
		}
		p = b.Pipeline
		isBuiltin = true
	}

	steps := make([]stepJSON, 0, len(p.Steps))
	for _, s := range p.Steps {
		steps = append(steps, toStepJSON(s))
	}

	detail := pipelineDetailJSON{
		Slug:        slug,
		Name:        p.Name,
		Description: p.Description,
		Status:      pipelineStatus(p),
		Steps:       steps,
		Builtin:     isBuiltin,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(detail)
}
