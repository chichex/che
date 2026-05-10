package dash

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/runner"
	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// ── test helpers ───────────────────────────────────────────────────────

// plantManifest writes a runner.Manifest to runsDir/<slug>/<runID>/manifest.yaml.
func plantManifest(t *testing.T, runsDir, slug, runID string, m runner.Manifest) {
	t.Helper()
	dir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), data, 0o600); err != nil {
		t.Fatalf("WriteFile manifest: %v", err)
	}
}

// plantCorruptManifest writes invalid YAML to simulate a corrupt manifest.
func plantCorruptManifest(t *testing.T, runsDir, slug, runID string) {
	t.Helper()
	dir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("{\nnot valid yaml: ["), 0o600); err != nil {
		t.Fatalf("WriteFile corrupt: %v", err)
	}
}

// plantStdoutLog writes a fake stdout log file for a given step idx (0-indexed).
func plantStdoutLog(t *testing.T, runsDir, slug, runID string, idx int, content string) {
	t.Helper()
	dir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fname := fmt.Sprintf("step-%02d.stdout.log", idx+1)
	if err := os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile stdout: %v", err)
	}
}

func callHandler(t *testing.T, h http.HandlerFunc, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// ── listing tests ──────────────────────────────────────────────────────

func TestListRunsEmpty(t *testing.T) {
	runsDir := t.TempDir()
	h := handleListRuns(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs", map[string]string{hdrSlug: "mypipe"})

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []runListItemJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rr.Body.String())
	}
	if len(list) != 0 {
		t.Fatalf("want empty list, got %d items", len(list))
	}
}

func TestListRunsOneRun(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	m := runner.Manifest{
		RunID:     "abc123",
		Pipeline:  "mypipe",
		StartedAt: now,
		Status:    "done",
	}
	plantManifest(t, runsDir, "mypipe", "abc123", m)

	h := handleListRuns(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs", map[string]string{hdrSlug: "mypipe"})

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []runListItemJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 run, got %d", len(list))
	}
	if list[0].ID != "abc123" {
		t.Errorf("want id=abc123, got %q", list[0].ID)
	}
	if list[0].Status != "done" {
		t.Errorf("want status=done, got %q", list[0].Status)
	}
}

func TestListRunsMultipleDescOrder(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().Truncate(time.Second)

	plantManifest(t, runsDir, "mypipe", "run-newer", runner.Manifest{
		RunID:     "run-newer",
		StartedAt: now,
		Status:    "done",
	})
	plantManifest(t, runsDir, "mypipe", "run-older", runner.Manifest{
		RunID:     "run-older",
		StartedAt: now.Add(-1 * time.Hour),
		Status:    "failed",
	})

	h := handleListRuns(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs", map[string]string{hdrSlug: "mypipe"})

	var list []runListItemJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 runs, got %d", len(list))
	}
	if list[0].ID != "run-newer" {
		t.Errorf("first run should be newer, got %q", list[0].ID)
	}
	if list[1].ID != "run-older" {
		t.Errorf("second run should be older, got %q", list[1].ID)
	}
}

func TestListRunsCorruptSkipped(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().Truncate(time.Second)

	plantManifest(t, runsDir, "mypipe", "good-run", runner.Manifest{
		RunID:     "good-run",
		StartedAt: now,
		Status:    "done",
	})
	plantCorruptManifest(t, runsDir, "mypipe", "bad-run")

	h := handleListRuns(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs", map[string]string{hdrSlug: "mypipe"})

	var list []runListItemJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 run (corrupt skipped), got %d", len(list))
	}
	if list[0].ID != "good-run" {
		t.Errorf("want good-run, got %q", list[0].ID)
	}
}

// ── detail tests ───────────────────────────────────────────────────────

func TestGetRunFound(t *testing.T) {
	runsDir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	m := runner.Manifest{
		RunID:     "xyz789",
		Pipeline:  "mypipe",
		StartedAt: now,
		Status:    "done",
		Steps: []runner.ManifestStep{
			{Idx: 0, Name: "plan", CLI: "claude", Kind: "prompt", Status: "done", ExitCode: 0},
			{
				Idx:    1,
				Name:   "review",
				CLI:    "gemini",
				Kind:   "prompt",
				Status: "done",
				Validator: &runner.ManifestValidator{
					CLI:          "gemini",
					LoopsRun:     2,
					MaxLoops:     3,
					FinalVerdict: "accepted",
					LastFeedback: "looks good",
				},
			},
		},
	}
	plantManifest(t, runsDir, "mypipe", "xyz789", m)

	h := handleGetRun(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs/xyz789", map[string]string{
		hdrSlug:  "mypipe",
		hdrRunID: "xyz789",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.ID != "xyz789" {
		t.Errorf("want id=xyz789, got %q", detail.ID)
	}
	if detail.Slug != "mypipe" {
		t.Errorf("want slug=mypipe, got %q", detail.Slug)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(detail.Steps))
	}
	if detail.Steps[0].Validator != nil {
		t.Errorf("step[0] should have nil validator")
	}
	if detail.Steps[1].Validator == nil {
		t.Fatalf("step[1] should have validator")
	}
	if detail.Steps[1].Validator.FinalVerdict != "accepted" {
		t.Errorf("want final_verdict=accepted, got %q", detail.Steps[1].Validator.FinalVerdict)
	}
}

func TestGetRunNotFound(t *testing.T) {
	runsDir := t.TempDir()
	h := handleGetRun(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs/nonexistent", map[string]string{
		hdrSlug:  "mypipe",
		hdrRunID: "nonexistent",
	})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not found") {
		t.Errorf("want 'not found' in body, got %q", rr.Body.String())
	}
}

func TestGetRunCorrupt(t *testing.T) {
	runsDir := t.TempDir()
	plantCorruptManifest(t, runsDir, "mypipe", "bad-run")

	h := handleGetRun(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs/bad-run", map[string]string{
		hdrSlug:  "mypipe",
		hdrRunID: "bad-run",
	})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "corrupt") {
		t.Errorf("want 'corrupt' in body, got %q", rr.Body.String())
	}
}

// ── stdout tests ───────────────────────────────────────────────────────

func TestGetStdoutFound(t *testing.T) {
	runsDir := t.TempDir()
	content := "hello from step 1\n"
	plantStdoutLog(t, runsDir, "mypipe", "run1", 0, content)

	h := handleGetStepStdout(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs/run1/steps/0/stdout", map[string]string{
		hdrSlug:    "mypipe",
		hdrRunID:   "run1",
		hdrStepIdx: "0",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello from step 1") {
		t.Errorf("want content in body, got %q", rr.Body.String())
	}
}

func TestGetStdoutNotFound(t *testing.T) {
	runsDir := t.TempDir()
	h := handleGetStepStdout(runsDir)
	rr := callHandler(t, h, http.MethodGet, "/api/pipelines/mypipe/runs/run1/steps/0/stdout", map[string]string{
		hdrSlug:    "mypipe",
		hdrRunID:   "run1",
		hdrStepIdx: "0",
	})

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not found") {
		t.Errorf("want 'not found' in body, got %q", rr.Body.String())
	}
}

// ── dispatcher routing tests ───────────────────────────────────────────

func TestDispatcherRouting(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()

	// Plant a pipeline for spec 1 paths
	writeYAML(t, pipelinesDir, "my-pipe", wizard.Pipeline{
		Name:  "My Pipe",
		Steps: []wizard.Step{{Name: "step1", CLI: "claude", Kind: "prompt"}},
	})

	// Plant a run for spec 2 paths
	now := time.Now().Truncate(time.Second)
	plantManifest(t, runsDir, "my-pipe", "run1", runner.Manifest{
		RunID:     "run1",
		Pipeline:  "my-pipe",
		StartedAt: now,
		Status:    "done",
		Steps:     []runner.ManifestStep{{Idx: 0, Name: "step1", Status: "done"}},
	})

	// Plant stdout for step 0
	plantStdoutLog(t, runsDir, "my-pipe", "run1", 0, "stdout content")

	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir))

	cases := []struct {
		path           string
		wantStatus     int
		wantBodySubstr string
	}{
		// spec 1 paths
		{"/api/pipelines/my-pipe", 200, "my-pipe"},
		{"/api/pipelines/nonexistent", 404, "not found"},
		{"/api/pipelines/", 404, "not found"},
		// spec 2: list runs
		{"/api/pipelines/my-pipe/runs", 200, "run1"},
		// spec 2: get run detail
		{"/api/pipelines/my-pipe/runs/run1", 200, "run1"},
		{"/api/pipelines/my-pipe/runs/nonexistent", 404, "not found"},
		// spec 2: get stdout
		{"/api/pipelines/my-pipe/runs/run1/steps/0/stdout", 200, "stdout content"},
		{"/api/pipelines/my-pipe/runs/run1/steps/99/stdout", 404, "not found"},
		// bad paths → 404
		{"/api/pipelines/my-pipe/something", 404, "not found"},
		{"/api/pipelines/my-pipe/runs/run1/steps/0/stderr", 404, "not found"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rr := httptest.NewRecorder()
			dispatcher(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("want status %d, got %d (body: %s)", tc.wantStatus, rr.Code, rr.Body.String())
			}
			if tc.wantBodySubstr != "" && !strings.Contains(rr.Body.String(), tc.wantBodySubstr) {
				t.Errorf("want %q in body, got %q", tc.wantBodySubstr, rr.Body.String())
			}
		})
	}
}
