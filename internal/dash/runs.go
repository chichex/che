package dash

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chichex/che/internal/runner"
	"gopkg.in/yaml.v3"
)

// ── JSON wire shapes ───────────────────────────────────────────────────

// runListItemJSON is the wire shape for a single item in GET /api/pipelines/:slug/runs.
type runListItemJSON struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at,omitempty"`
	InputKind  string  `json:"input_kind,omitempty"`
	InputValue string  `json:"input_value,omitempty"`
}

// runDetailJSON is the wire shape for GET /api/pipelines/:slug/runs/:runId.
type runDetailJSON struct {
	ID         string         `json:"id"`
	Slug       string         `json:"slug"`
	Status     string         `json:"status"`
	StartedAt  string         `json:"started_at"`
	FinishedAt *string        `json:"finished_at,omitempty"`
	InputKind  string         `json:"input_kind,omitempty"`
	InputValue string         `json:"input_value,omitempty"`
	Steps      []runStepJSON  `json:"steps"`
}

// runStepJSON is the wire shape for a step in a run detail.
type runStepJSON struct {
	Idx        int                  `json:"idx"`
	Name       string               `json:"name"`
	CLI        string               `json:"cli,omitempty"`
	Kind       string               `json:"kind,omitempty"`
	Status     string               `json:"status"`
	ExitCode   int                  `json:"exit_code"`
	StartedAt  *string              `json:"started_at,omitempty"`
	FinishedAt *string              `json:"finished_at,omitempty"`
	Error      string               `json:"error,omitempty"`
	Validator  *runValidatorJSON    `json:"validator,omitempty"`
}

// runValidatorJSON is the wire shape for an optional step validator in run detail.
type runValidatorJSON struct {
	CLI          string `json:"cli,omitempty"`
	LoopsRun     int    `json:"loops_run"`
	MaxLoops     int    `json:"max_loops"`
	OnMaxLoops   string `json:"on_max_loops,omitempty"`
	FinalVerdict string `json:"final_verdict,omitempty"`
	LastFeedback string `json:"last_feedback,omitempty"`
}

// ── manifest loading ───────────────────────────────────────────────────

// loadManifest parses a manifest.yaml file at path into a runner.Manifest.
func loadManifest(path string) (runner.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runner.Manifest{}, err
	}
	var m runner.Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return runner.Manifest{}, err
	}
	return m, nil
}

// fmtTime returns a RFC3339 string pointer if t is non-zero, nil otherwise.
func fmtTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// manifestToRunDetail converts a runner.Manifest + slug to the wire detail shape.
func manifestToRunDetail(m runner.Manifest, slug string) runDetailJSON {
	steps := make([]runStepJSON, 0, len(m.Steps))
	for _, s := range m.Steps {
		sj := runStepJSON{
			Idx:        s.Idx,
			Name:       s.Name,
			CLI:        s.CLI,
			Kind:       s.Kind,
			Status:     s.Status,
			ExitCode:   s.ExitCode,
			StartedAt:  fmtTime(s.StartedAt),
			FinishedAt: fmtTime(s.FinishedAt),
			Error:      s.Error,
		}
		if s.Validator != nil {
			sj.Validator = &runValidatorJSON{
				CLI:          s.Validator.CLI,
				LoopsRun:     s.Validator.LoopsRun,
				MaxLoops:     s.Validator.MaxLoops,
				OnMaxLoops:   s.Validator.OnMaxLoops,
				FinalVerdict: s.Validator.FinalVerdict,
				LastFeedback: s.Validator.LastFeedback,
			}
		}
		steps = append(steps, sj)
	}

	runID := m.RunID
	if runID == "" {
		runID = filepath.Base(filepath.Dir(m.PipelinePath))
	}

	return runDetailJSON{
		ID:         runID,
		Slug:       slug,
		Status:     m.Status,
		StartedAt:  m.StartedAt.Format(time.RFC3339),
		FinishedAt: fmtTime(m.FinishedAt),
		InputKind:  m.InputKind,
		InputValue: m.InputValue,
		Steps:      steps,
	}
}

// ── listing ────────────────────────────────────────────────────────────

// listRuns returns all runs for a slug under runsDir, ordered desc by started_at.
// Corrupt manifests are logged and skipped.
func listRuns(runsDir, slug string) []runListItemJSON {
	if runsDir == "" || slug == "" {
		return []runListItemJSON{}
	}
	slugDir := filepath.Join(runsDir, slug)
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[dash] readdir %s: %v", slugDir, err)
		}
		return []runListItemJSON{}
	}

	type runWithTime struct {
		item      runListItemJSON
		startedAt time.Time
	}

	var runs []runWithTime
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		mPath := filepath.Join(slugDir, ent.Name(), "manifest.yaml")
		m, err := loadManifest(mPath)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[dash] manifest corrupt: %s: %v", mPath, err)
			}
			continue
		}
		runID := m.RunID
		if runID == "" {
			runID = ent.Name()
		}
		item := runListItemJSON{
			ID:         runID,
			Status:     m.Status,
			StartedAt:  m.StartedAt.Format(time.RFC3339),
			FinishedAt: fmtTime(m.FinishedAt),
			InputKind:  m.InputKind,
			InputValue: m.InputValue,
		}
		runs = append(runs, runWithTime{item: item, startedAt: m.StartedAt})
	}

	// Sort desc by started_at; tie-break by ID desc for stability.
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].startedAt.Equal(runs[j].startedAt) {
			return runs[i].startedAt.After(runs[j].startedAt)
		}
		return runs[i].item.ID > runs[j].item.ID
	})

	result := make([]runListItemJSON, len(runs))
	for i, r := range runs {
		result[i] = r.item
	}
	return result
}

// ── handlers ──────────────────────────────────────────────────────────

// handleListRuns returns an http.HandlerFunc for GET /api/pipelines/:slug/runs.
// It reads all runs from runsDir/<slug>/, skipping corrupt manifests.
// Returns JSON array (empty if no runs or dir missing).
func handleListRuns(runsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// slug is extracted by the dispatcher from the URL path
		slug := extractSlug(r)
		list := listRuns(runsDir, slug)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}
}

// handleGetRun returns an http.HandlerFunc for GET /api/pipelines/:slug/runs/:runId.
// It parses the manifest.yaml and returns the full run detail.
// Returns 404 if not found, 500 if corrupt.
func handleGetRun(runsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug, runID := extractSlugAndRunID(r)
		if slug == "" || runID == "" {
			writeJSONError(w, http.StatusNotFound, "run not found")
			return
		}

		mPath := filepath.Join(runsDir, slug, runID, "manifest.yaml")
		m, err := loadManifest(mPath)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSONError(w, http.StatusNotFound, "run not found")
				return
			}
			log.Printf("[dash] manifest corrupt: %s: %v", mPath, err)
			writeJSONError(w, http.StatusInternalServerError, "manifest corrupt")
			return
		}

		detail := manifestToRunDetail(m, slug)
		// Ensure RunID reflects the directory name if manifest didn't set it
		if detail.ID == "" || detail.ID == slug {
			detail.ID = runID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detail)
	}
}

// handleGetStepStdout returns an http.HandlerFunc for
// GET /api/pipelines/:slug/runs/:runId/steps/:idx/stdout.
// Serves the step-NN.stdout.log file (NN = 1-indexed, 2-padded).
// Returns 404 JSON if missing.
func handleGetStepStdout(runsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug, runID, stepIdx := extractSlugRunIDAndStepIdx(r)
		if slug == "" || runID == "" || stepIdx < 0 {
			writeJSONError(w, http.StatusNotFound, "stdout not found")
			return
		}

		// NN is 1-indexed, 2-padded
		nn := fmt.Sprintf("%02d", stepIdx+1)
		logPath := filepath.Join(runsDir, slug, runID, "step-"+nn+".stdout.log")

		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			writeJSONError(w, http.StatusNotFound, "stdout not found")
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFile(w, r, logPath)
	}
}

// ── URL extraction helpers ─────────────────────────────────────────────

// These helpers extract path parameters from the URL. The dispatcher sets
// them via query params for simplicity, since http.ServeMux doesn't support
// path params. Instead, the dispatcher injects slug/runId into r.URL.Path
// context by routing to closures that already have the segments.
//
// Actually: the dispatcher in dash.go calls these handlers with the full
// request, so we store the extracted values in a custom header set by the
// dispatcher before calling the handler.

const (
	hdrSlug   = "X-Dash-Slug"
	hdrRunID  = "X-Dash-RunID"
	hdrStepIdx = "X-Dash-StepIdx"
)

func extractSlug(r *http.Request) string {
	return r.Header.Get(hdrSlug)
}

func extractSlugAndRunID(r *http.Request) (slug, runID string) {
	return r.Header.Get(hdrSlug), r.Header.Get(hdrRunID)
}

func extractSlugRunIDAndStepIdx(r *http.Request) (slug, runID string, stepIdx int) {
	slug = r.Header.Get(hdrSlug)
	runID = r.Header.Get(hdrRunID)
	idxStr := r.Header.Get(hdrStepIdx)
	if idxStr == "" {
		return slug, runID, -1
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return slug, runID, -1
	}
	return slug, runID, idx
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}

// ── dispatcher ────────────────────────────────────────────────────────

// dispatchPipelinesPrefix is the handler for /api/pipelines/ that dispatches
// to sub-handlers based on path segments.
//
// Supported routes:
//   /api/pipelines/<slug>                                  → pipeline detail (spec 1)
//   /api/pipelines/<slug>/runs                             → list runs
//   /api/pipelines/<slug>/runs/<runId>                     → get run
//   /api/pipelines/<slug>/runs/<runId>/events              → SSE per-run stream (spec 3)
//   /api/pipelines/<slug>/runs/<runId>/steps/<idx>/stdout  → get step stdout
//
// Any other pattern returns 404.
func dispatchPipelinesPrefix(pipelinesDir, runsDir string, bus *Bus) http.HandlerFunc {
	listRunsH := handleListRuns(runsDir)
	getRunH := handleGetRun(runsDir)
	getStdoutH := handleGetStepStdout(runsDir)
	getEventsH := handleEvents(runsDir, bus)

	return func(w http.ResponseWriter, r *http.Request) {
		// Strip the /api/pipelines/ prefix and split into segments.
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/pipelines/")
		trimmed = strings.Trim(trimmed, "/")
		if trimmed == "" {
			writeJSONError(w, http.StatusNotFound, "pipeline not found")
			return
		}
		segs := strings.Split(trimmed, "/")

		switch len(segs) {
		case 1:
			// /api/pipelines/<slug>
			slug := segs[0]
			if slug == "" {
				writeJSONError(w, http.StatusNotFound, "pipeline not found")
				return
			}
			getPipelineDetail(pipelinesDir, slug, w, r)

		case 2:
			// /api/pipelines/<slug>/runs
			slug, sub := segs[0], segs[1]
			if sub != "runs" {
				writeJSONError(w, http.StatusNotFound, "not found")
				return
			}
			r2 := requestWithHeaders(r, map[string]string{hdrSlug: slug})
			listRunsH(w, r2)

		case 3:
			// /api/pipelines/<slug>/runs/<runId>
			slug, sub, runID := segs[0], segs[1], segs[2]
			if sub != "runs" {
				writeJSONError(w, http.StatusNotFound, "not found")
				return
			}
			r2 := requestWithHeaders(r, map[string]string{hdrSlug: slug, hdrRunID: runID})
			getRunH(w, r2)

		case 4:
			// /api/pipelines/<slug>/runs/<runId>/events
			slug, sub1, runID, sub2 := segs[0], segs[1], segs[2], segs[3]
			if sub1 != "runs" || sub2 != "events" {
				writeJSONError(w, http.StatusNotFound, "not found")
				return
			}
			r2 := requestWithHeaders(r, map[string]string{hdrSlug: slug, hdrRunID: runID})
			getEventsH(w, r2)

		case 6:
			// /api/pipelines/<slug>/runs/<runId>/steps/<idx>/stdout
			slug, sub1, runID, sub2, idxStr, sub3 := segs[0], segs[1], segs[2], segs[3], segs[4], segs[5]
			if sub1 != "runs" || sub2 != "steps" || sub3 != "stdout" {
				writeJSONError(w, http.StatusNotFound, "not found")
				return
			}
			r2 := requestWithHeaders(r, map[string]string{hdrSlug: slug, hdrRunID: runID, hdrStepIdx: idxStr})
			getStdoutH(w, r2)

		default:
			writeJSONError(w, http.StatusNotFound, "not found")
		}
	}
}

// requestWithHeaders returns a shallow copy of r with additional headers set.
func requestWithHeaders(r *http.Request, headers map[string]string) *http.Request {
	r2 := r.Clone(r.Context())
	for k, v := range headers {
		r2.Header.Set(k, v)
	}
	return r2
}
