package dash

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGlobalSSE_Headers verifies that GET /api/events sets SSE headers.
func TestGlobalSSE_Headers(t *testing.T) {
	bus := NewBus(t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	h := handleGlobalEvents(bus)
	h(rr, req)

	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: want text/event-stream, got %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: want no-cache, got %q", cc)
	}
}

// TestGlobalSSE_EventForwarded verifies that an event published to the global
// bus is forwarded to the SSE stream.
func TestGlobalSSE_EventForwarded(t *testing.T) {
	bus := NewBus(t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	// Publish a global event after a brief delay so the handler can subscribe first.
	go func() {
		time.Sleep(30 * time.Millisecond)
		bus.PublishGlobal(Event{
			Type:    EventRunStarted,
			Payload: map[string]any{"slug": "test-pipe", "run_id": "r1"},
		})
	}()

	h := handleGlobalEvents(bus)
	h(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "run:started") {
		t.Errorf("expected run:started event in SSE output:\n%s", body)
	}
	if !strings.Contains(body, "test-pipe") {
		t.Errorf("expected 'test-pipe' slug in SSE output:\n%s", body)
	}
}

// TestGlobalSSE_Heartbeat verifies that the handler emits ": heartbeat" comments
// when idle. Uses a shortened heartbeatInterval so the test doesn't take 15s.
func TestGlobalSSE_Heartbeat(t *testing.T) {
	orig := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = orig })

	bus := NewBus(t.TempDir())

	// Give enough time for at least one heartbeat tick.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	h := handleGlobalEvents(bus)
	h(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, ": heartbeat") {
		t.Errorf("expected heartbeat comment in SSE output:\n%s", body)
	}
}
