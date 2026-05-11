package dash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePipelineYAML writes a minimal pipeline YAML to dir/<slug>.yaml.
// If withStatus is true, writes a Status block to make it "draft".
func writePipelineYAML(t *testing.T, dir, slug string, withStatus bool) {
	t.Helper()
	content := "name: " + slug + "\n"
	if withStatus {
		content += "status:\n  stage: info\n"
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("writePipelineYAML %s: %v", slug, err)
	}
}

// TestPipelinesWatcher_NewPipeline verifies that a new YAML file triggers
// a pipeline:changed event.
func TestPipelinesWatcher_NewPipeline(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus("/tmp")
	gCh, gCancel := bus.SubscribeGlobal()
	defer gCancel()

	w := newPipelinesWatcher(dir, bus)
	snap := make(map[string]pipelineSnapshot)

	// No files yet — tick should produce nothing.
	w.tick(snap)
	select {
	case ev := <-gCh:
		t.Fatalf("expected no event on empty dir, got %+v", ev)
	case <-time.After(20 * time.Millisecond):
	}

	// Write a ready pipeline.
	writePipelineYAML(t, dir, "my-pipe", false)
	w.tick(snap)

	select {
	case ev := <-gCh:
		if ev.Type != EventPipelineChanged {
			t.Fatalf("want pipeline:changed, got %s", ev.Type)
		}
		if ev.Payload["slug"] != "my-pipe" {
			t.Fatalf("want slug=my-pipe, got %v", ev.Payload["slug"])
		}
		if ev.Payload["status"] != "ready" {
			t.Fatalf("want status=ready, got %v", ev.Payload["status"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for pipeline:changed event")
	}
}

// TestPipelinesWatcher_StatusFlip verifies that a status change (ready→draft)
// triggers a pipeline:changed event.
func TestPipelinesWatcher_StatusFlip(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus("/tmp")
	gCh, gCancel := bus.SubscribeGlobal()
	defer gCancel()

	w := newPipelinesWatcher(dir, bus)
	snap := make(map[string]pipelineSnapshot)

	// Write ready pipeline and tick to populate snapshot.
	writePipelineYAML(t, dir, "pipe", false)
	// Small sleep to ensure mtime differs on next write.
	time.Sleep(5 * time.Millisecond)
	w.tick(snap)
	// Drain the "new pipeline" event.
	<-gCh

	// Flip to draft.
	time.Sleep(5 * time.Millisecond)
	writePipelineYAML(t, dir, "pipe", true)
	w.tick(snap)

	select {
	case ev := <-gCh:
		if ev.Type != EventPipelineChanged {
			t.Fatalf("want pipeline:changed, got %s", ev.Type)
		}
		if ev.Payload["status"] != "draft" {
			t.Fatalf("want status=draft, got %v", ev.Payload["status"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for status flip event")
	}
}

// TestPipelinesWatcher_Delete verifies that removing a YAML file triggers
// a pipeline:changed{deleted:true} event.
func TestPipelinesWatcher_Delete(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus("/tmp")
	gCh, gCancel := bus.SubscribeGlobal()
	defer gCancel()

	w := newPipelinesWatcher(dir, bus)
	snap := make(map[string]pipelineSnapshot)

	writePipelineYAML(t, dir, "gone", false)
	w.tick(snap)
	<-gCh // drain new event

	// Delete the file.
	os.Remove(filepath.Join(dir, "gone.yaml"))
	w.tick(snap)

	select {
	case ev := <-gCh:
		if ev.Type != EventPipelineChanged {
			t.Fatalf("want pipeline:changed, got %s", ev.Type)
		}
		if ev.Payload["deleted"] != true {
			t.Fatalf("want deleted=true, got %v", ev.Payload["deleted"])
		}
		if ev.Payload["slug"] != "gone" {
			t.Fatalf("want slug=gone, got %v", ev.Payload["slug"])
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for delete event")
	}
}
