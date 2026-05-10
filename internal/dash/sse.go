// Package dash — sse.go: SSE handler for per-run live events.
//
// GET /api/pipelines/:slug/runs/:runId/events
//
// Protocol:
//  1. Subscribe to bus FIRST (before reading disk snapshot).
//  2. Read manifest snapshot from disk.
//  3. Emit replay events: run:status, step:start/end, step:stdout (running step),
//     validator:loop (historic).
//  4. Drain any bus events that arrived during snapshot.
//  5. Tail: forward bus events until client disconnects or run is terminal.
//
// Heartbeat: `: heartbeat\n\n` comment every 15s when idle.
package dash

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/chichex/che/internal/runner"
)

const heartbeatInterval = 15 * time.Second

// handleEvents returns an http.HandlerFunc for SSE per-run events.
// runsDir is the base directory for run data (~/.che/runs).
// bus is the shared Bus singleton.
func handleEvents(runsDir string, bus *Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.Header.Get(hdrSlug)
		runID := r.Header.Get(hdrRunID)
		if slug == "" || runID == "" {
			http.Error(w, "missing slug or runId", http.StatusBadRequest)
			return
		}

		// SSE headers.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// 1. Subscribe FIRST — before reading disk, so we don't miss events
		//    that land during the snapshot window.
		ch, cancel := bus.Subscribe(slug, runID)
		defer cancel()

		// 2. Read manifest snapshot.
		runDir := filepath.Join(runsDir, slug, runID)
		manifestPath := filepath.Join(runDir, "manifest.yaml")
		m, err := parseManifest(manifestPath)
		if err != nil {
			// Run not found — emit a single error comment and close.
			fmt.Fprintf(w, ": run not found\n\n")
			flusher.Flush()
			return
		}

		// 3. Replay from snapshot.
		emitReplay(w, flusher, runDir, m)

		// 4. Drain any buffered bus events that arrived during snapshot.
	drainLoop:
		for {
			select {
			case ev, more := <-ch:
				if !more {
					return
				}
				writeSSEEvent(w, ev)
				flusher.Flush()
			default:
				break drainLoop
			}
		}

		// 5. Tail: forward events from bus until client disconnects or
		//    we see a terminal run:status.
		heartbeat := time.NewTicker(heartbeatInterval)
		defer heartbeat.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return

			case <-heartbeat.C:
				fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()

			case ev, more := <-ch:
				if !more {
					return
				}
				writeSSEEvent(w, ev)
				flusher.Flush()
				heartbeat.Reset(heartbeatInterval)

				// If we get a terminal run:status, close the stream.
				if ev.Type == EventRunStatus {
					if status, _ := ev.Payload["status"].(string); isTerminalStatus(status) {
						return
					}
				}
			}
		}
	}
}

// emitReplay emits the full historical state from the manifest and stdout files.
// This gives a reconnecting client the current picture immediately.
//
// Order:
//  1. run:status (current)
//  2. step:start / step:end for each completed or running step
//  3. step:stdout lines for the running step (or recently-done step with a log)
//  4. validator:loop for each step that has a validator with loops_run > 0
func emitReplay(w http.ResponseWriter, flusher http.Flusher, runDir string, m runner.Manifest) {
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. run:status
	writeSSEEvent(w, Event{Type: EventRunStatus, Payload: map[string]any{
		"status": m.Status,
	}})
	flusher.Flush()

	for _, s := range m.Steps {
		// 2a. step:start for running or terminal steps.
		switch s.Status {
		case "running", "done", "failed", "cancelled", "interrupted":
			startedAt := now
			if !s.StartedAt.IsZero() {
				startedAt = s.StartedAt.Format(time.RFC3339)
			}
			writeSSEEvent(w, Event{Type: EventStepStart, Payload: map[string]any{
				"idx":        s.Idx,
				"name":       s.Name,
				"started_at": startedAt,
			}})
			flusher.Flush()
		}

		// 2b. step:end for terminal steps.
		switch s.Status {
		case "done", "failed", "cancelled", "interrupted":
			p := map[string]any{
				"idx":       s.Idx,
				"status":    s.Status,
				"exit_code": s.ExitCode,
			}
			if !s.FinishedAt.IsZero() {
				p["finished_at"] = s.FinishedAt.Format(time.RFC3339)
			}
			if s.Error != "" {
				p["error"] = s.Error
			}
			writeSSEEvent(w, Event{Type: EventStepEnd, Payload: p})
			flusher.Flush()
		}

		// 3. step:stdout lines for running or recently-done steps.
		if s.Status == "running" || s.Status == "done" || s.Status == "failed" {
			lines, err := readStdoutLines(runDir, s.Idx)
			if err == nil && len(lines) > 0 {
				for ordinal, line := range lines {
					writeSSEEvent(w, Event{Type: EventStepStdout, Payload: map[string]any{
						"idx":     s.Idx,
						"line":    line,
						"ts":      now,
						"ordinal": ordinal,
					}})
				}
				flusher.Flush()
			}
		}

		// 4. validator:loop history.
		if s.Validator != nil && s.Validator.LoopsRun > 0 {
			for loop := 1; loop <= s.Validator.LoopsRun; loop++ {
				verdict := ""
				feedback := ""
				if loop == s.Validator.LoopsRun {
					verdict = s.Validator.FinalVerdict
					feedback = s.Validator.LastFeedback
				}
				writeSSEEvent(w, Event{Type: EventValidatorLoop, Payload: map[string]any{
					"idx":       s.Idx,
					"loop":      loop,
					"max_loops": s.Validator.MaxLoops,
					"verdict":   verdict,
					"feedback":  feedback,
				}})
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes a single SSE event to w.
// Format: "event: <type>\ndata: <json>\n\n"
func writeSSEEvent(w http.ResponseWriter, ev Event) {
	data, err := json.Marshal(ev.Payload)
	if err != nil {
		data = []byte("{}")
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
}
