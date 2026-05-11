// Package dash — runs_watcher.go: global watcher for ~/.che/runs/*/*.
//
// Polls every 250ms, scanning ~/.che/runs/<slug>/<runId>/manifest.yaml.
// Maintains snapshot per (slug, runId) → status. Emits:
//   - run:started when a new manifest with status=running appears
//   - run:finished when an existing run transitions to a terminal status
//
// Optimization: if a run's status is already terminal, skip stat until mtime changes.
package dash

import (
	"os"
	"path/filepath"
	"time"
)

// runSnapshot holds last-seen status and mtime for a single run manifest.
type runSnapshot struct {
	status    string
	mtime     time.Time
	startedAt string // ISO8601
}

// runKey identifies a unique (slug, runId) pair.
type runKey struct {
	slug  string
	runID string
}

// runsWatcher polls ~/.che/runs/ and emits global run events.
type runsWatcher struct {
	runsDir string
	bus     *Bus
	stopCh  chan struct{}
}

// newRunsWatcher creates a watcher for runsDir, publishing to bus.
func newRunsWatcher(runsDir string, bus *Bus) *runsWatcher {
	return &runsWatcher{
		runsDir: runsDir,
		bus:     bus,
		stopCh:  make(chan struct{}),
	}
}

// stop signals the watcher to halt. Safe to call multiple times.
func (w *runsWatcher) stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
}

// run is the main polling loop. Must be called in a goroutine.
func (w *runsWatcher) run() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	snapshot := make(map[runKey]runSnapshot)

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.tick(snapshot)
		}
	}
}

func (w *runsWatcher) tick(snapshot map[runKey]runSnapshot) {
	if w.runsDir == "" {
		return
	}

	// Enumerate slug dirs.
	slugEntries, err := os.ReadDir(w.runsDir)
	if err != nil {
		return
	}

	seen := make(map[runKey]bool)

	for _, slugEntry := range slugEntries {
		if !slugEntry.IsDir() {
			continue
		}
		slug := slugEntry.Name()
		slugDir := filepath.Join(w.runsDir, slug)

		runEntries, err := os.ReadDir(slugDir)
		if err != nil {
			continue
		}

		for _, runEntry := range runEntries {
			if !runEntry.IsDir() {
				continue
			}
			runID := runEntry.Name()
			key := runKey{slug: slug, runID: runID}
			seen[key] = true

			manifestPath := filepath.Join(slugDir, runID, "manifest.yaml")

			prev, existed := snapshot[key]

			// If terminal, only re-stat on mtime change (optimization).
			if existed && isTerminalStatus(prev.status) {
				fi, err := os.Stat(manifestPath)
				if err != nil {
					continue
				}
				if fi.ModTime().Equal(prev.mtime) {
					continue
				}
				// mtime changed on a terminal run — unusual but update snapshot silently.
				snapshot[key] = runSnapshot{status: prev.status, mtime: fi.ModTime(), startedAt: prev.startedAt}
				continue
			}

			// Read and parse manifest.
			fi, err := os.Stat(manifestPath)
			if err != nil {
				continue
			}
			if existed && fi.ModTime().Equal(prev.mtime) {
				continue
			}

			m, err := parseManifest(manifestPath)
			if err != nil {
				continue
			}

			startedAt := ""
			if !m.StartedAt.IsZero() {
				startedAt = m.StartedAt.Format(time.RFC3339)
			}

			newSnap := runSnapshot{
				status:    m.Status,
				mtime:     fi.ModTime(),
				startedAt: startedAt,
			}

			if !existed {
				// New run — emit run:started if status is running.
				snapshot[key] = newSnap
				if m.Status == "running" {
					w.bus.PublishGlobal(Event{
						Type: EventRunStarted,
						Payload: map[string]any{
							"slug":        slug,
							"run_id":      runID,
							"started_at":  startedAt,
							"input_kind":  m.InputKind,
							"input_value": m.InputValue,
						},
					})
				}
				continue
			}

			// Existing run — check for transition to terminal.
			snapshot[key] = newSnap
			if !isTerminalStatus(prev.status) && isTerminalStatus(m.Status) {
				finishedAt := ""
				if !m.FinishedAt.IsZero() {
					finishedAt = m.FinishedAt.Format(time.RFC3339)
				}
				w.bus.PublishGlobal(Event{
					Type: EventRunFinished,
					Payload: map[string]any{
						"slug":        slug,
						"run_id":      runID,
						"status":      m.Status,
						"started_at":  startedAt,
						"finished_at": finishedAt,
					},
				})
			}
		}
	}
	// Note: we don't emit events for deleted runs (run dirs are typically kept).
}
