// Package dash — bus.go: in-process pub/sub bus for per-run SSE events.
//
// Each (slug, runId) pair can have multiple subscribers. The bus is ref-counted:
// the first subscriber for a (slug, runId) starts a disk watcher; the last one
// to unsubscribe stops it.
//
// Global subscribers (SubscribeGlobal) receive all events published via
// PublishGlobal (pipeline:changed, run:started, run:finished). They are
// independent of per-run subscribers and are not ref-counted.
package dash

import (
	"sync"
)

// EventType is the SSE event type string.
type EventType string

const (
	EventRunStatus     EventType = "run:status"
	EventStepStart     EventType = "step:start"
	EventStepEnd       EventType = "step:end"
	EventStepStdout    EventType = "step:stdout"
	EventValidatorLoop EventType = "validator:loop"

	// Global event types (emitted by pipelines_watcher and runs_watcher).
	EventPipelineChanged EventType = "pipeline:changed"
	EventRunStarted      EventType = "run:started"
	EventRunFinished     EventType = "run:finished"
)

// Event is a single SSE event with a typed payload (JSON-marshallable map).
type Event struct {
	Type    EventType
	Payload map[string]any
}

// subKey identifies a unique (slug, runId) watcher.
type subKey struct {
	slug  string
	runID string
}

type subscriber struct {
	ch      chan Event
	cancel  func()
	removed bool // set to true when removeSub has processed this subscriber
}

// globalSubscriber is a subscriber for global events (not tied to a run).
type globalSubscriber struct {
	ch      chan Event
	removed bool
}

// Bus is a per-run pub/sub bus. Safe for concurrent use.
type Bus struct {
	mu         sync.Mutex
	subs       map[subKey][]*subscriber
	watchers   map[subKey]*watcher
	runsDir    string
	globalSubs []*globalSubscriber
}

// NewBus creates a new Bus serving runs from runsDir.
func NewBus(runsDir string) *Bus {
	return &Bus{
		subs:     make(map[subKey][]*subscriber),
		watchers: make(map[subKey]*watcher),
		runsDir:  runsDir,
	}
}

// SubscribeGlobal registers a new subscriber for global events
// (pipeline:changed, run:started, run:finished).
// Returns a receive-only channel and a cancel function.
// The cancel function MUST be called when done.
// Buffer size is 256; if the subscriber is too slow, it is dropped and closed.
func (b *Bus) SubscribeGlobal() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	gs := &globalSubscriber{ch: ch}

	b.mu.Lock()
	b.globalSubs = append(b.globalSubs, gs)
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeGlobalSub(gs)
	}
	return ch, cancel
}

// removeGlobalSub removes gs from globalSubs (must hold b.mu). Idempotent.
func (b *Bus) removeGlobalSub(gs *globalSubscriber) {
	if gs.removed {
		return
	}
	gs.removed = true
	newList := b.globalSubs[:0]
	for _, s := range b.globalSubs {
		if s != gs {
			newList = append(newList, s)
		}
	}
	b.globalSubs = newList
	close(gs.ch)
}

// PublishGlobal sends a global event to all global subscribers.
// Slow subscribers (full buffer) are dropped.
func (b *Bus) PublishGlobal(ev Event) {
	b.mu.Lock()
	var slow []*globalSubscriber
	for _, gs := range b.globalSubs {
		select {
		case gs.ch <- ev:
		default:
			slow = append(slow, gs)
		}
	}
	for _, gs := range slow {
		b.removeGlobalSub(gs)
	}
	b.mu.Unlock()
}

// Subscribe registers a new subscriber for (slug, runID).
// Returns a receive-only channel and a cancel function.
// The cancel function MUST be called when the subscriber is done.
// If this is the first subscriber for the key, a disk watcher is started.
// Buffer size is 256; if the subscriber is too slow, the channel is closed
// (slow-subscriber drop) and the subscriber is removed.
func (b *Bus) Subscribe(slug, runID string) (<-chan Event, func()) {
	key := subKey{slug: slug, runID: runID}
	ch := make(chan Event, 256)

	b.mu.Lock()
	sub := &subscriber{ch: ch}
	b.subs[key] = append(b.subs[key], sub)

	// Start watcher if this is the first subscriber.
	if _, ok := b.watchers[key]; !ok {
		w := newWatcher(b.runsDir, slug, runID, b)
		b.watchers[key] = w
		go w.run()
	}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeSub(key, sub)
	}
	sub.cancel = cancel
	return ch, cancel
}

// removeSub removes sub from the key's subscriber list (must hold b.mu).
// If the list becomes empty, the watcher for that key is stopped.
// Safe to call multiple times for the same subscriber (idempotent).
func (b *Bus) removeSub(key subKey, sub *subscriber) {
	if sub.removed {
		return
	}
	sub.removed = true

	list := b.subs[key]
	newList := list[:0]
	for _, s := range list {
		if s != sub {
			newList = append(newList, s)
		}
	}
	if len(newList) == 0 {
		delete(b.subs, key)
		if w, ok := b.watchers[key]; ok {
			w.stop()
			delete(b.watchers, key)
		}
	} else {
		b.subs[key] = newList
	}
	// Close the channel so any consumer blocked on receive unblocks.
	// Buffered items remain readable until drained by the consumer.
	close(sub.ch)
}

// Publish sends an event to all subscribers of (slug, runID).
// Slow subscribers whose buffer is full are closed and dropped.
func (b *Bus) Publish(slug, runID string, ev Event) {
	key := subKey{slug: slug, runID: runID}
	b.mu.Lock()
	list := b.subs[key]
	var slow []*subscriber
	for _, s := range list {
		select {
		case s.ch <- ev:
		default:
			slow = append(slow, s)
		}
	}
	for _, s := range slow {
		b.removeSub(key, s)
	}
	b.mu.Unlock()
}

// stopWatcher is called by the watcher itself when it detects a terminal
// run status, after draining remaining events. It removes the watcher from
// the map without stopping it again (it's already stopping).
func (b *Bus) stopWatcher(slug, runID string) {
	key := subKey{slug: slug, runID: runID}
	b.mu.Lock()
	delete(b.watchers, key)
	b.mu.Unlock()
}
