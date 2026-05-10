package dash

import (
	"bufio"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/runner"
	"gopkg.in/yaml.v3"
)

// TestSSE_Headers verifies that the SSE handler sets the correct headers.
func TestSSE_Headers(t *testing.T) {
	runsDir := t.TempDir()
	slug, runID := "pipe", "run-h"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifestForSSE(t, runDir, runner.Manifest{
		RunID:     runID,
		Pipeline:  slug,
		Status:    runner.ManifestStatusRunning,
		StartedAt: time.Now().UTC(),
	})

	bus := NewBus(runsDir)
	h := handleEvents(runsDir, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	req.Header.Set(hdrSlug, slug)
	req.Header.Set(hdrRunID, runID)

	rr := httptest.NewRecorder()
	h(rr, req)

	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type want text/event-stream, got %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control want no-cache, got %q", cc)
	}
	if ab := rr.Header().Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("X-Accel-Buffering want no, got %q", ab)
	}
}

// TestSSE_ReplaySnapshot verifies that connecting to the SSE endpoint for a
// run that already has some state emits a run:status replay event.
func TestSSE_ReplaySnapshot(t *testing.T) {
	runsDir := t.TempDir()
	slug, runID := "pipe", "run-r"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := runner.Manifest{
		RunID:     runID,
		Pipeline:  slug,
		Status:    runner.ManifestStatusRunning,
		StartedAt: time.Now().UTC(),
		Steps: []runner.ManifestStep{
			{Idx: 0, Name: "build", Status: "running", StartedAt: time.Now().UTC()},
		},
	}
	writeManifestForSSE(t, runDir, m)

	bus := NewBus(runsDir)
	h := handleEvents(runsDir, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	req.Header.Set(hdrSlug, slug)
	req.Header.Set(hdrRunID, runID)

	rr := httptest.NewRecorder()
	h(rr, req)

	body := rr.Body.String()

	if !strings.Contains(body, "event: run:status") {
		t.Errorf("expected run:status event in SSE output:\n%s", body)
	}
	if !strings.Contains(body, "running") {
		t.Errorf("expected 'running' status in SSE output:\n%s", body)
	}
}

// TestSSE_NotFound verifies that a missing run yields a comment and closes.
func TestSSE_NotFound(t *testing.T) {
	runsDir := t.TempDir()
	bus := NewBus(runsDir)
	h := handleEvents(runsDir, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	req.Header.Set(hdrSlug, "noexist")
	req.Header.Set(hdrRunID, "noexist")

	rr := httptest.NewRecorder()
	h(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, ": run not found") {
		t.Errorf("expected 'run not found' comment, got:\n%s", body)
	}
}

// TestSSE_TailEvent verifies that after replay, events published to the bus
// are forwarded to the SSE stream.
func TestSSE_TailEvent(t *testing.T) {
	runsDir := t.TempDir()
	slug, runID := "pipe", "run-t"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifestForSSE(t, runDir, runner.Manifest{
		RunID: runID, Pipeline: slug,
		Status: runner.ManifestStatusRunning, StartedAt: time.Now().UTC(),
	})

	bus := NewBus(runsDir)
	h := handleEvents(runsDir, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/events", nil).WithContext(ctx)
	req.Header.Set(hdrSlug, slug)
	req.Header.Set(hdrRunID, runID)

	// Publish an event after a short delay to simulate a tail event.
	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.Publish(slug, runID, Event{
			Type:    EventStepStdout,
			Payload: map[string]any{"idx": 0, "line": "hello tail", "ordinal": 0},
		})
	}()

	rr := httptest.NewRecorder()
	h(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "step:stdout") {
		t.Errorf("expected step:stdout in tail; got:\n%s", body)
	}
	if !strings.Contains(body, "hello tail") {
		t.Errorf("expected 'hello tail' line; got:\n%s", body)
	}
}

// ── helpers ───────────────────────────────────────────────────────────

func writeManifestForSSE(t *testing.T, runDir string, m runner.Manifest) {
	t.Helper()
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "manifest.yaml"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// scanSSEEventTypes extracts event type names from raw SSE body.
func scanSSEEventTypes(body string) []string {
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	return events
}

var _ = scanSSEEventTypes // avoid "declared but not used" if only used in comments
