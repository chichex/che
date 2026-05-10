// Package dash — pipelines_watcher.go: global watcher for ~/.che/pipelines/.
//
// Polls every 250ms via os.ReadDir + os.Stat. Maintains a snapshot of
// slug → {mtime, status}. On diff emits pipeline:changed via bus.PublishGlobal.
package dash

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// pipelineSnapshot holds the last-seen mtime and status for a pipeline YAML.
type pipelineSnapshot struct {
	mtime  time.Time
	status string
}

// pipelinesWatcher polls ~/.che/pipelines/ and emits pipeline:changed events.
type pipelinesWatcher struct {
	dir    string
	bus    *Bus
	stopCh chan struct{}
}

// newPipelinesWatcher creates a watcher for dir, publishing to bus.
func newPipelinesWatcher(dir string, bus *Bus) *pipelinesWatcher {
	return &pipelinesWatcher{
		dir:    dir,
		bus:    bus,
		stopCh: make(chan struct{}),
	}
}

// stop signals the watcher to halt. Safe to call multiple times (idempotent via select).
func (w *pipelinesWatcher) stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
}

// run is the main polling loop. Must be called in a goroutine.
func (w *pipelinesWatcher) run() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	snapshot := make(map[string]pipelineSnapshot) // slug → snapshot

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.tick(snapshot)
		}
	}
}

func (w *pipelinesWatcher) tick(snapshot map[string]pipelineSnapshot) {
	if w.dir == "" {
		return
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}

	seen := make(map[string]bool, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		slug := strings.TrimSuffix(name, ".yaml")
		seen[slug] = true

		path := filepath.Join(w.dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}

		prev, existed := snapshot[slug]
		if existed && fi.ModTime().Equal(prev.mtime) {
			// No change.
			continue
		}

		// Parse status minimally.
		status := parsePipelineStatus(path)

		if !existed {
			// New pipeline.
			snapshot[slug] = pipelineSnapshot{mtime: fi.ModTime(), status: status}
			w.bus.PublishGlobal(Event{
				Type: EventPipelineChanged,
				Payload: map[string]any{
					"slug":   slug,
					"status": status,
				},
			})
			continue
		}

		// mtime changed — update snapshot regardless, emit if status flipped.
		snapshot[slug] = pipelineSnapshot{mtime: fi.ModTime(), status: status}
		if status != prev.status {
			w.bus.PublishGlobal(Event{
				Type: EventPipelineChanged,
				Payload: map[string]any{
					"slug":   slug,
					"status": status,
				},
			})
		}
	}

	// Detect deletes: slugs in snapshot but not in seen.
	for slug := range snapshot {
		if !seen[slug] {
			delete(snapshot, slug)
			w.bus.PublishGlobal(Event{
				Type: EventPipelineChanged,
				Payload: map[string]any{
					"slug":    slug,
					"deleted": true,
				},
			})
		}
	}
}

// parsePipelineStatus reads the pipeline YAML and returns "ready" or "draft".
// On parse error, returns "ready" as a safe default.
func parsePipelineStatus(path string) string {
	p, err := wizard.Load(path)
	if err != nil {
		return "ready"
	}
	if p.Status != nil {
		return "draft"
	}
	return "ready"
}
