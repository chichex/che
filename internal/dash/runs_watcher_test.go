package dash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSimpleManifest writes a minimal manifest.yaml for a run.
func writeSimpleManifest(t *testing.T, runsDir, slug, runID, status string) {
	t.Helper()
	dir := filepath.Join(runsDir, slug, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "run_id: " + runID + "\npipeline: " + slug + "\nstatus: " + status + "\nstarted_at: " + time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("writeSimpleManifest %s/%s: %v", slug, runID, err)
	}
}

// TestRunsWatcher_RunStarted verifies that a new manifest with status=running
// emits a run:started event.
func TestRunsWatcher_RunStarted(t *testing.T) {
	runsDir := t.TempDir()
	bus := NewBus(runsDir)
	gCh, gCancel := bus.SubscribeGlobal()
	defer gCancel()

	w := newRunsWatcher(runsDir, bus)
	snap := make(map[runKey]runSnapshot)

	// No runs yet.
	w.tick(snap)
	select {
	case ev := <-gCh:
		t.Fatalf("expected no event on empty dir, got %+v", ev)
	case <-time.After(20 * time.Millisecond):
	}

	// Create a running manifest.
	writeSimpleManifest(t, runsDir, "my-pipeline", "abc123", "running")
	w.tick(snap)

	select {
	case ev := <-gCh:
		if ev.Type != EventRunStarted {
			t.Fatalf("want run:started, got %s", ev.Type)
		}
		if ev.Payload["slug"] != "my-pipeline" {
			t.Fatalf("want slug=my-pipeline, got %v", ev.Payload["slug"])
		}
		if ev.Payload["run_id"] != "abc123" {
			t.Fatalf("want run_id=abc123, got %v", ev.Payload["run_id"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for run:started")
	}
}

// TestRunsWatcher_RunFinished verifies that a manifest that transitions to a
// terminal status emits a run:finished event.
func TestRunsWatcher_RunFinished(t *testing.T) {
	runsDir := t.TempDir()
	bus := NewBus(runsDir)
	gCh, gCancel := bus.SubscribeGlobal()
	defer gCancel()

	w := newRunsWatcher(runsDir, bus)
	snap := make(map[runKey]runSnapshot)

	// Create running manifest.
	writeSimpleManifest(t, runsDir, "pipe", "run1", "running")
	w.tick(snap)
	<-gCh // drain run:started

	// Overwrite with terminal status.
	time.Sleep(5 * time.Millisecond)
	writeSimpleManifest(t, runsDir, "pipe", "run1", "done")
	w.tick(snap)

	select {
	case ev := <-gCh:
		if ev.Type != EventRunFinished {
			t.Fatalf("want run:finished, got %s", ev.Type)
		}
		if ev.Payload["status"] != "done" {
			t.Fatalf("want status=done, got %v", ev.Payload["status"])
		}
		if ev.Payload["run_id"] != "run1" {
			t.Fatalf("want run_id=run1, got %v", ev.Payload["run_id"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for run:finished")
	}
}
