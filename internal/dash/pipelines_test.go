package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// writeYAML is a test helper that writes a wizard.Pipeline to dir/<slug>.yaml.
func writeYAML(t *testing.T, dir, slug string, p wizard.Pipeline) {
	t.Helper()
	data, err := wizard.Marshal(p)
	if err != nil {
		t.Fatalf("marshal %s: %v", slug, err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), data, 0o600); err != nil {
		t.Fatalf("writeYAML %s: %v", slug, err)
	}
}

// writeCorrupt writes raw bytes to dir/<slug>.yaml to simulate a corrupt YAML.
func writeCorrupt(t *testing.T, dir, slug string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte("{\nnot valid yaml: ["), 0o600); err != nil {
		t.Fatalf("writeCorrupt %s: %v", slug, err)
	}
}

func getJSON(t *testing.T, handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

// ── list handler tests ─────────────────────────────────────────────────────

func TestListEmpty(t *testing.T) {
	dir := t.TempDir()
	rr := getJSON(t, handleListPipelines(dir, ""), "/api/pipelines")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty list, got %d items", len(list))
	}
}

func TestListOneReady(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "my-pipe", wizard.Pipeline{
		Name:        "My Pipe",
		Description: "test ready",
		Steps:       []wizard.Step{{Name: "step1", CLI: "claude", Kind: "prompt"}},
	})
	rr := getJSON(t, handleListPipelines(dir, ""), "/api/pipelines")
	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1, got %d", len(list))
	}
	if list[0].Status != "ready" {
		t.Errorf("want status=ready, got %q", list[0].Status)
	}
	if list[0].Slug != "my-pipe" {
		t.Errorf("want slug=my-pipe, got %q", list[0].Slug)
	}
}

func TestListOneDraft(t *testing.T) {
	dir := t.TempDir()
	draftStatus := &wizard.Status{Stage: wizard.StageInfo}
	writeYAML(t, dir, "draft-pipe", wizard.Pipeline{
		Name:   "Draft Pipe",
		Status: draftStatus,
	})
	rr := getJSON(t, handleListPipelines(dir, ""), "/api/pipelines")
	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1, got %d", len(list))
	}
	if list[0].Status != "draft" {
		t.Errorf("want status=draft, got %q", list[0].Status)
	}
}

func TestListMixed(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "ready-pipe", wizard.Pipeline{Name: "Ready"})
	writeYAML(t, dir, "draft-pipe", wizard.Pipeline{
		Name:   "Draft",
		Status: &wizard.Status{Stage: wizard.StageStep},
	})
	rr := getJSON(t, handleListPipelines(dir, ""), "/api/pipelines")
	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	statuses := map[string]string{}
	for _, item := range list {
		statuses[item.Slug] = item.Status
	}
	if statuses["ready-pipe"] != "ready" {
		t.Errorf("ready-pipe: want ready, got %q", statuses["ready-pipe"])
	}
	if statuses["draft-pipe"] != "draft" {
		t.Errorf("draft-pipe: want draft, got %q", statuses["draft-pipe"])
	}
}

func TestListCorruptExcluded(t *testing.T) {
	dir := t.TempDir()
	writeCorrupt(t, dir, "bad-pipe")
	writeYAML(t, dir, "good-pipe", wizard.Pipeline{Name: "Good"})

	rr := getJSON(t, handleListPipelines(dir, ""), "/api/pipelines")
	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 (corrupt excluded), got %d", len(list))
	}
	if list[0].Slug != "good-pipe" {
		t.Errorf("want good-pipe, got %q", list[0].Slug)
	}
}

// ── detail handler tests ───────────────────────────────────────────────────

func TestDetailMissingSlug(t *testing.T) {
	dir := t.TempDir()
	rr := getJSON(t, handleGetPipeline(dir), "/api/pipelines/nonexistent")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not found") {
		t.Errorf("want 'not found' in body, got %q", rr.Body.String())
	}
}

func TestDetailFound(t *testing.T) {
	dir := t.TempDir()
	val := &wizard.Validator{CLI: "gemini", Kind: "prompt", Content: "check it"}
	writeYAML(t, dir, "my-pipe", wizard.Pipeline{
		Name:        "My Pipe",
		Description: "desc",
		Steps: []wizard.Step{
			{Name: "step1", CLI: "claude", Kind: "prompt", Content: "do stuff", Validator: val},
			{Name: "step2", CLI: "codex", Kind: "prompt", Input: "previous_output"},
		},
	})

	rr := getJSON(t, handleGetPipeline(dir), "/api/pipelines/my-pipe")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var detail pipelineDetailJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Slug != "my-pipe" {
		t.Errorf("slug: want my-pipe, got %q", detail.Slug)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(detail.Steps))
	}
	if detail.Steps[0].Validator == nil {
		t.Errorf("step[0] should have validator")
	}
	if detail.Steps[1].Validator != nil {
		t.Errorf("step[1] should have no validator")
	}
}

// ── last_run field tests ───────────────────────────────────────────────────

// writeRunManifest creates a minimal manifest.yaml under runsDir/slug/runID/.
func writeRunManifest(t *testing.T, runsDir, slug, runID, status string, startedAt time.Time) {
	t.Helper()
	dir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "run_id: " + runID + "\npipeline: " + slug + "\nstatus: " + status + "\nstarted_at: " + startedAt.UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("writeRunManifest %s/%s: %v", slug, runID, err)
	}
}

// TestListPipelines_LastRunPresent verifies that /api/pipelines includes
// last_run={id, status, started_at} when the pipeline has a run.
func TestListPipelines_LastRunPresent(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()

	writeYAML(t, pipelinesDir, "my-pipe", wizard.Pipeline{
		Name:  "My Pipe",
		Steps: []wizard.Step{{Name: "step1", CLI: "claude", Kind: "prompt"}},
	})

	startedAt := time.Now().UTC().Truncate(time.Second)
	writeRunManifest(t, runsDir, "my-pipe", "run-abc", "done", startedAt)

	// Invalidate cache to avoid stale TTL entries from other tests.
	lastRunCache.mu.Lock()
	lastRunCache.entries = make(map[string]lastRunCacheEntry)
	lastRunCache.mu.Unlock()

	rr := getJSON(t, handleListPipelines(pipelinesDir, runsDir), "/api/pipelines")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 pipeline, got %d", len(list))
	}
	p := list[0]
	if p.LastRun == nil {
		t.Fatal("want last_run present, got nil")
	}
	if p.LastRun.ID != "run-abc" {
		t.Errorf("last_run.id: want run-abc, got %q", p.LastRun.ID)
	}
	if p.LastRun.Status != "done" {
		t.Errorf("last_run.status: want done, got %q", p.LastRun.Status)
	}
	if p.LastRun.StartedAt == "" {
		t.Errorf("last_run.started_at: want non-empty, got empty")
	}
}

// TestListPipelines_LastRunOmitted verifies that last_run is omitted (omitempty)
// when there are no runs for the pipeline.
func TestListPipelines_LastRunOmitted(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()

	writeYAML(t, pipelinesDir, "no-runs-pipe", wizard.Pipeline{
		Name:  "No Runs",
		Steps: []wizard.Step{{Name: "s1", CLI: "claude", Kind: "prompt"}},
	})

	// Invalidate cache.
	lastRunCache.mu.Lock()
	lastRunCache.entries = make(map[string]lastRunCacheEntry)
	lastRunCache.mu.Unlock()

	rr := getJSON(t, handleListPipelines(pipelinesDir, runsDir), "/api/pipelines")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	var list []pipelineJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 pipeline, got %d", len(list))
	}
	if list[0].LastRun != nil {
		t.Errorf("want last_run omitted, got %+v", list[0].LastRun)
	}

	// Also verify JSON does not contain "last_run" key at all.
	rawJSON := rr.Body.String()
	if strings.Contains(rawJSON, "last_run") {
		t.Errorf("last_run should be omitted from JSON, got: %s", rawJSON)
	}
}
