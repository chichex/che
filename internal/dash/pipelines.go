package dash

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/chichex/che/internal/wizard"
)

// pipelineJSON is the wire shape for GET /api/pipelines (list item).
type pipelineJSON struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
}

// pipelineDetailJSON is the wire shape for GET /api/pipelines/:slug.
type pipelineDetailJSON struct {
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	Steps       []stepJSON `json:"steps"`
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

// handleListPipelines returns an http.HandlerFunc for GET /api/pipelines.
// It reads all *.yaml pipelines from dir, skipping corrupt files.
// Returns JSON array (empty array if dir is empty or missing).
func handleListPipelines(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pairs := loadPipelinesFromDir(dir)
		list := make([]pipelineJSON, 0, len(pairs))
		for _, pair := range pairs {
			list = append(list, pipelineJSON{
				Slug:        pair.slug,
				Name:        pair.p.Name,
				Description: pair.p.Description,
				Status:      pipelineStatus(pair.p),
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
func handleGetPipeline(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.TrimPrefix(r.URL.Path, "/api/pipelines/")
		if slug == "" || strings.ContainsAny(slug, "/\\") {
			http.Error(w, `{"error":"pipeline not found"}`, http.StatusNotFound)
			return
		}

		if dir == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"pipeline not found"}`))
			return
		}

		path := filepath.Join(dir, slug+".yaml")
		p, err := wizard.Load(path)
		if err != nil {
			if os.IsNotExist(err) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"pipeline not found"}`))
				return
			}
			log.Printf("[dash] load %s: %v", path, err)
			http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			return
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detail)
	}
}
