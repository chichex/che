// Package dash — watcher.go: disk-polling watcher for a single (slug, runID).
//
// Polls manifest.yaml every 250ms via os.Stat (mtime+size) and step-NN.stdout.log
// files via size delta. Publishes events to the Bus.
//
// Lifecycle: started by Bus.Subscribe (first subscriber), stopped when the last
// subscriber unsubscribes OR when the run reaches a terminal status.
package dash

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chichex/che/internal/runner"
	"gopkg.in/yaml.v3"
)

const pollInterval = 250 * time.Millisecond

// isTerminalStatus returns true for run statuses that mean the run is over
// (and not just paused).
func isTerminalStatus(status string) bool {
	switch status {
	case runner.ManifestStatusDone,
		runner.ManifestStatusFailed,
		runner.ManifestStatusCancelled,
		runner.ManifestStatusInterrupted:
		return true
	}
	return false
}

// stdoutState tracks the last-seen size of a step's stdout log.
type stdoutState struct {
	size int64
	// nextOrdinal is the ordinal to assign to the next line emitted.
	nextOrdinal int
}

// watcher observes manifest.yaml and step-NN.stdout.log for a single run.
type watcher struct {
	runsDir string
	slug    string
	runID   string
	bus     *Bus

	stopCh chan struct{}
	once   sync.Once
}

func newWatcher(runsDir, slug, runID string, bus *Bus) *watcher {
	return &watcher{
		runsDir: runsDir,
		slug:    slug,
		runID:   runID,
		bus:     bus,
		stopCh:  make(chan struct{}),
	}
}

// stop signals the watcher to halt. Safe to call multiple times.
func (w *watcher) stop() {
	w.once.Do(func() { close(w.stopCh) })
}

// run is the main polling loop. Must be called in a goroutine.
func (w *watcher) run() {
	runDir := filepath.Join(w.runsDir, w.slug, w.runID)
	manifestPath := filepath.Join(runDir, "manifest.yaml")

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Manifest tracking state.
	var lastMtime time.Time
	var lastSize int64
	var lastManifest *runner.Manifest

	// Stdout tracking: map step index (0-based) → state.
	stdoutStates := make(map[int]*stdoutState)

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			// ── manifest check ────────────────────────────────────
			fi, err := os.Stat(manifestPath)
			if err != nil {
				continue
			}
			if fi.ModTime() == lastMtime && fi.Size() == lastSize {
				// No change — still check stdout for running step.
				if lastManifest != nil {
					w.checkStdout(runDir, lastManifest, stdoutStates)
				}
				continue
			}
			// Manifest changed.
			lastMtime = fi.ModTime()
			lastSize = fi.Size()

			data, err := os.ReadFile(manifestPath)
			if err != nil {
				continue
			}
			var m runner.Manifest
			if err := yaml.Unmarshal(data, &m); err != nil {
				continue
			}

			// Diff manifest vs last known.
			w.diffManifest(lastManifest, &m)
			lastManifest = &m

			// Check stdout for running steps.
			w.checkStdout(runDir, &m, stdoutStates)

			// If run transitioned to terminal, drain stdout then stop.
			if isTerminalStatus(m.Status) {
				// Final stdout drain.
				w.checkStdout(runDir, &m, stdoutStates)
				// Tell the bus we're done so it removes this watcher entry.
				w.bus.stopWatcher(w.slug, w.runID)
				return
			}
		}
	}
}

// diffManifest compares old vs new manifest and publishes step:start / step:end
// events for any newly started or finished steps. Also emits run:status if
// the top-level status changed.
func (w *watcher) diffManifest(old, new *runner.Manifest) {
	// Run status change.
	oldStatus := ""
	if old != nil {
		oldStatus = old.Status
	}
	if new.Status != oldStatus {
		w.publish(EventRunStatus, map[string]any{
			"status": new.Status,
		})
	}

	if old == nil {
		// First read: emit step:start for already-running steps and
		// step:end for finished ones. This is the "cold start" case.
		for _, s := range new.Steps {
			switch s.Status {
			case "running":
				payload := map[string]any{
					"idx":        s.Idx,
					"name":       s.Name,
					"started_at": s.StartedAt.Format(time.RFC3339),
				}
				w.publish(EventStepStart, payload)
			case "done", "failed", "cancelled", "interrupted":
				payload := map[string]any{
					"idx":         s.Idx,
					"status":      s.Status,
					"exit_code":   s.ExitCode,
					"finished_at": s.FinishedAt.Format(time.RFC3339),
				}
				if s.Error != "" {
					payload["error"] = s.Error
				}
				w.publish(EventStepEnd, payload)
			}
		}
		return
	}

	// Build old-step map by idx.
	oldSteps := make(map[int]runner.ManifestStep, len(old.Steps))
	for _, s := range old.Steps {
		oldSteps[s.Idx] = s
	}

	for _, ns := range new.Steps {
		os, hadOld := oldSteps[ns.Idx]

		// step:start — transitioned to running.
		if ns.Status == "running" && (!hadOld || os.Status != "running") {
			payload := map[string]any{
				"idx":        ns.Idx,
				"name":       ns.Name,
				"started_at": ns.StartedAt.Format(time.RFC3339),
			}
			w.publish(EventStepStart, payload)
		}

		// step:end — transitioned to a terminal step status.
		terminalStepStatus := func(st string) bool {
			return st == "done" || st == "failed" || st == "cancelled" || st == "interrupted"
		}
		if terminalStepStatus(ns.Status) && (!hadOld || !terminalStepStatus(os.Status)) {
			payload := map[string]any{
				"idx":         ns.Idx,
				"status":      ns.Status,
				"exit_code":   ns.ExitCode,
				"finished_at": ns.FinishedAt.Format(time.RFC3339),
			}
			if ns.Error != "" {
				payload["error"] = ns.Error
			}
			w.publish(EventStepEnd, payload)
		}

		// validator:loop — loops_run increased.
		if ns.Validator != nil {
			var oldLoops int
			if hadOld && os.Validator != nil {
				oldLoops = os.Validator.LoopsRun
			}
			if ns.Validator.LoopsRun > oldLoops {
				payload := map[string]any{
					"idx":       ns.Idx,
					"loop":      ns.Validator.LoopsRun,
					"max_loops": ns.Validator.MaxLoops,
					"verdict":   ns.Validator.FinalVerdict,
					"feedback":  ns.Validator.LastFeedback,
				}
				w.publish(EventValidatorLoop, payload)
			}
		}
	}
}

// checkStdout reads stdout log files for running (or recently-finished) steps
// and publishes step:stdout events for any new lines since last check.
func (w *watcher) checkStdout(runDir string, m *runner.Manifest, states map[int]*stdoutState) {
	for _, s := range m.Steps {
		// Only emit stdout for running steps (tail in progress) or pending/recently
		// done steps that have a log file (in case we just missed the transition).
		if s.Status == "pending" {
			continue
		}
		nn := fmt.Sprintf("%02d", s.Idx+1) // 1-indexed, 2-padded
		logPath := filepath.Join(runDir, "step-"+nn+".stdout.log")

		fi, err := os.Stat(logPath)
		if err != nil {
			continue // file doesn't exist yet
		}

		st, ok := states[s.Idx]
		if !ok {
			st = &stdoutState{}
			states[s.Idx] = st
		}

		if fi.Size() <= st.size {
			continue // no new bytes
		}

		// Open and seek to last position.
		f, err := os.Open(logPath)
		if err != nil {
			continue
		}
		if st.size > 0 {
			if _, err := f.Seek(st.size, 0); err != nil {
				f.Close()
				continue
			}
		}

		scanner := bufio.NewScanner(f)
		ts := time.Now().UTC().Format(time.RFC3339)
		for scanner.Scan() {
			line := scanner.Text()
			// Skip empty lines at EOF (partial last line).
			if line == "" {
				continue
			}
			w.publish(EventStepStdout, map[string]any{
				"idx":     s.Idx,
				"line":    line,
				"ts":      ts,
				"ordinal": st.nextOrdinal,
			})
			st.nextOrdinal++
		}
		f.Close()

		// Update tracked size.
		newFi, err := os.Stat(logPath)
		if err == nil {
			st.size = newFi.Size()
		}
	}
}

// publish sends an event to the bus for this (slug, runID).
func (w *watcher) publish(typ EventType, payload map[string]any) {
	w.bus.Publish(w.slug, w.runID, Event{Type: typ, Payload: payload})
}

// stdoutLogPath returns the path of a step's stdout log (1-indexed, 2-padded).
func stdoutLogPath(runDir string, stepIdx int) string {
	nn := fmt.Sprintf("%02d", stepIdx+1)
	return filepath.Join(runDir, "step-"+nn+".stdout.log")
}

// readStdoutLines reads ALL lines from a step's stdout log file and returns
// them. Used by the SSE handler for the replay snapshot.
func readStdoutLines(runDir string, stepIdx int) ([]string, error) {
	path := stdoutLogPath(runDir, stepIdx)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines, scanner.Err()
}

// parseManifest reads and parses the manifest.yaml at the given path.
func parseManifest(path string) (runner.Manifest, error) {
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

