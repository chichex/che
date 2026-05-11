package dash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chichex/che/internal/runner"
	"gopkg.in/yaml.v3"
)

// writeManifestYAML writes a runner.Manifest as YAML to runDir/manifest.yaml.
func writeManifestYAML(t *testing.T, runDir string, m runner.Manifest) {
	t.Helper()
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "manifest.yaml"), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// TestWatcher_ManifestChange verifies that writing a manifest with a running
// step emits run:status and step:start events.
func TestWatcher_ManifestChange(t *testing.T) {
	runsDir := t.TempDir()
	slug := "pipe"
	runID := "run-1"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	b := NewBus(runsDir)
	ch, cancel := b.Subscribe(slug, runID)
	defer cancel()

	// Write initial manifest with a running step.
	m := runner.Manifest{
		RunID:     runID,
		Pipeline:  "pipe",
		Status:    runner.ManifestStatusRunning,
		StartedAt: time.Now().UTC(),
		Steps: []runner.ManifestStep{
			{Idx: 0, Name: "build", Status: "running", StartedAt: time.Now().UTC()},
		},
	}
	writeManifestYAML(t, runDir, m)

	// Collect events for up to 1 second.
	events := collectEvents(ch, 2, time.Second)

	types := eventTypes(events)
	if !containsType(types, EventRunStatus) {
		t.Errorf("want run:status in %v", types)
	}
	if !containsType(types, EventStepStart) {
		t.Errorf("want step:start in %v", types)
	}
}

// TestWatcher_StdoutAppend verifies that appending lines to a step stdout log
// emits step:stdout events with incrementing ordinals.
func TestWatcher_StdoutAppend(t *testing.T) {
	runsDir := t.TempDir()
	slug := "pipe"
	runID := "run-2"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write manifest first so watcher has context.
	m := runner.Manifest{
		RunID:     runID,
		Pipeline:  slug,
		Status:    runner.ManifestStatusRunning,
		StartedAt: time.Now().UTC(),
		Steps: []runner.ManifestStep{
			{Idx: 0, Name: "run", Status: "running", StartedAt: time.Now().UTC()},
		},
	}
	writeManifestYAML(t, runDir, m)

	b := NewBus(runsDir)
	ch, cancel := b.Subscribe(slug, runID)
	defer cancel()

	// Give watcher time to do its first tick and read the manifest.
	time.Sleep(400 * time.Millisecond)

	// Append lines to stdout log.
	logPath := filepath.Join(runDir, "step-01.stdout.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("hello world\n")
	f.WriteString("second line\n")
	f.Close()

	// Collect step:stdout events.
	events := collectEvents(ch, 5, time.Second)
	var stdoutEvents []Event
	for _, ev := range events {
		if ev.Type == EventStepStdout {
			stdoutEvents = append(stdoutEvents, ev)
		}
	}

	if len(stdoutEvents) < 2 {
		t.Fatalf("want at least 2 step:stdout events, got %d (all events: %v)", len(stdoutEvents), eventTypes(events))
	}

	// Check ordinals are incrementing.
	if ord, _ := stdoutEvents[0].Payload["ordinal"].(int); ord != 0 {
		t.Errorf("first event ordinal want 0, got %d", ord)
	}
	if ord, _ := stdoutEvents[1].Payload["ordinal"].(int); ord != 1 {
		t.Errorf("second event ordinal want 1, got %d", ord)
	}
}

// TestWatcher_TerminalHalt verifies that transitioning to a terminal status
// emits run:status terminal and stops the watcher.
func TestWatcher_TerminalHalt(t *testing.T) {
	runsDir := t.TempDir()
	slug := "pipe"
	runID := "run-3"
	runDir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Start with running manifest.
	m := runner.Manifest{
		RunID:     runID,
		Pipeline:  slug,
		Status:    runner.ManifestStatusRunning,
		StartedAt: time.Now().UTC(),
		Steps: []runner.ManifestStep{
			{Idx: 0, Name: "run", Status: "running", StartedAt: time.Now().UTC()},
		},
	}
	writeManifestYAML(t, runDir, m)

	b := NewBus(runsDir)
	ch, cancel := b.Subscribe(slug, runID)
	defer cancel()

	// Wait a tick so the watcher reads the initial manifest.
	time.Sleep(400 * time.Millisecond)

	// Update to terminal.
	fin := time.Now().UTC()
	m.Status = runner.ManifestStatusDone
	m.FinishedAt = fin
	m.Steps[0].Status = "done"
	m.Steps[0].FinishedAt = fin
	writeManifestYAML(t, runDir, m)

	// Collect events — look for run:status = done.
	events := collectEvents(ch, 10, 2*time.Second)
	var terminalSeen bool
	for _, ev := range events {
		if ev.Type == EventRunStatus {
			if status, _ := ev.Payload["status"].(string); status == "done" {
				terminalSeen = true
			}
		}
	}
	if !terminalSeen {
		t.Errorf("want run:status=done event; got %v", eventTypes(events))
	}

	// After terminal, watcher should have self-removed from bus.
	// Give it a moment to complete.
	time.Sleep(400 * time.Millisecond)
	b.mu.Lock()
	key := subKey{slug: slug, runID: runID}
	_, watcherExists := b.watchers[key]
	b.mu.Unlock()
	if watcherExists {
		t.Error("watcher should have been removed after terminal status")
	}
}

// ── helpers ───────────────────────────────────────────────────────────

func collectEvents(ch <-chan Event, max int, timeout time.Duration) []Event {
	var events []Event
	dl := time.After(timeout)
	for {
		select {
		case ev, more := <-ch:
			if !more {
				return events
			}
			events = append(events, ev)
			if len(events) >= max {
				return events
			}
		case <-dl:
			return events
		}
	}
}

func eventTypes(events []Event) []EventType {
	types := make([]EventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func containsType(types []EventType, t EventType) bool {
	for _, tt := range types {
		if tt == t {
			return true
		}
	}
	return false
}
