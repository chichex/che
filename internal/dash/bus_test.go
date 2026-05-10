package dash

import (
	"testing"
	"time"
)

// TestBus_SubscribePublish verifies a single subscriber receives a published event.
func TestBus_SubscribePublish(t *testing.T) {
	b := NewBus("/tmp")
	ch, cancel := b.Subscribe("sl", "r1")
	defer cancel()

	ev := Event{Type: EventRunStatus, Payload: map[string]any{"status": "running"}}
	b.Publish("sl", "r1", ev)

	select {
	case got := <-ch:
		if got.Type != EventRunStatus {
			t.Fatalf("want %s, got %s", EventRunStatus, got.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

// TestBus_Fanout verifies two subscribers both receive the same event.
func TestBus_Fanout(t *testing.T) {
	b := NewBus("/tmp")
	ch1, cancel1 := b.Subscribe("sl", "r1")
	ch2, cancel2 := b.Subscribe("sl", "r1")
	defer cancel1()
	defer cancel2()

	ev := Event{Type: EventStepStart, Payload: map[string]any{"idx": 0}}
	b.Publish("sl", "r1", ev)

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Type != EventStepStart {
				t.Fatalf("want %s, got %s", EventStepStart, got.Type)
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for event on subscriber")
		}
	}
}

// TestBus_SlowDrop verifies that a slow subscriber (full buffer) is dropped
// and the channel is closed when Publish overflows it.
func TestBus_SlowDrop(t *testing.T) {
	b := NewBus("/tmp")
	ch, cancel := b.Subscribe("sl", "r2")
	defer cancel()

	ev := Event{Type: EventStepStdout, Payload: map[string]any{"line": "x"}}
	// Fill the buffer + 1 extra to trigger the drop path.
	for i := 0; i < 257; i++ {
		b.Publish("sl", "r2", ev)
	}

	// The channel should eventually be closed (drained by removeSub).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, more := <-ch:
			if !more {
				return // channel closed = drop happened
			}
		case <-deadline:
			t.Fatal("timeout: slow subscriber not dropped")
		}
	}
}

// TestBus_UnsubscribeStopsWatcher verifies that canceling the sole subscriber
// removes the watcher entry from the bus.
func TestBus_UnsubscribeStopsWatcher(t *testing.T) {
	b := NewBus(t.TempDir())
	_, cancel := b.Subscribe("sl", "r3")

	b.mu.Lock()
	key := subKey{slug: "sl", runID: "r3"}
	if _, ok := b.watchers[key]; !ok {
		b.mu.Unlock()
		t.Fatal("watcher should exist after subscribe")
	}
	b.mu.Unlock()

	cancel()
	// Give the goroutine a moment.
	time.Sleep(20 * time.Millisecond)

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.watchers[key]; ok {
		t.Fatal("watcher should be removed after sole subscriber cancels")
	}
}
