// Package dash — sse_global.go: SSE handler for global events.
//
// GET /api/events
//
// Emits: pipeline:changed, run:started, run:finished
// Heartbeat: `: heartbeat\n\n` every 15s when idle.
// No snapshot/replay — frontend fetches current state on load.
package dash

import (
	"fmt"
	"net/http"
	"time"
)

// handleGlobalEvents returns an http.HandlerFunc for GET /api/events.
// Subscribes to bus global channel and streams events to the client.
func handleGlobalEvents(bus *Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SSE headers — same pattern as per-run handler (sse.go).
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch, cancel := bus.SubscribeGlobal()
		defer cancel()

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
			}
		}
	}
}
