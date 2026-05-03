package lock

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chichex/che/internal/pipelinelabels"
)

// fakeRepo es un mock minimalista de la API REST de labels. Mantiene una
// lista de labels en memoria y stubea Add/Del/List. Thread-safe porque la
// goroutine de heartbeat puede tocarlo en paralelo con la del test.
type fakeRepo struct {
	mu     sync.Mutex
	labels map[string]bool
}

func newFakeRepo(initial ...string) *fakeRepo {
	m := map[string]bool{}
	for _, l := range initial {
		m[l] = true
	}
	return &fakeRepo{labels: m}
}

func (f *fakeRepo) add(_ int, l string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[l] = true
	return nil
}

func (f *fakeRepo) del(_ int, l string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.labels, l)
	return nil
}

func (f *fakeRepo) list(_ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.labels))
	for l := range f.labels {
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeRepo) snapshot() []string {
	out, _ := f.list(0)
	return out
}

// fakeClock devuelve un time inyectable. Set lo mueve manualmente — los
// tests no usan time.Sleep ni timers reales.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// withInstantHeartbeat sustituye HeartbeatInterval y restaura al volver,
// para que los tests del heartbeat no esperen 60s reales.
func withInstantHeartbeat(t *testing.T) {
	t.Helper()
	prev := HeartbeatInterval
	HeartbeatInterval = 1 * time.Hour // efectivamente nunca dispara solo
	t.Cleanup(func() { HeartbeatInterval = prev })
}

// TestAcquire_Fresh: ref sin labels previos → Acquire aplica un che:lock:*
// y arranca el heartbeat. Release lo borra.
func TestAcquire_Fresh(t *testing.T) {
	withInstantHeartbeat(t)
	repo := newFakeRepo()
	clock := newFakeClock(time.Unix(1700000000, 0))

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire fresh: %v", err)
	}
	defer h.Release()

	got := repo.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 label, got %d (%v)", len(got), got)
	}
	if !strings.HasPrefix(got[0], pipelinelabels.PrefixLock) {
		t.Errorf("label %q no tiene prefijo %s", got[0], pipelinelabels.PrefixLock)
	}
	parsed, perr := pipelinelabels.Parse(got[0])
	if perr != nil {
		t.Fatalf("parse aplicado: %v", perr)
	}
	if parsed.PID != 12345 || parsed.Host != "test-host" {
		t.Errorf("identidad: pid=%d host=%q, want 12345/test-host", parsed.PID, parsed.Host)
	}
	if !parsed.Timestamp.Equal(clock.Now()) {
		t.Errorf("timestamp: got %v, want %v", parsed.Timestamp, clock.Now())
	}

	if err := h.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
	if got := repo.snapshot(); len(got) != 0 {
		t.Errorf("post-Release: %d labels remain (%v), want 0", len(got), got)
	}
}

// TestAcquire_AlreadyLocked: ref ya tiene un lock vivo (timestamp reciente)
// → Acquire devuelve ErrAlreadyLocked sin tocar el lock existente.
func TestAcquire_AlreadyLocked(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	existing := pipelinelabels.LockLabelAt(clock.Now().Add(-30*time.Second), 999, "other-host")
	repo := newFakeRepo(existing)

	_, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err == nil {
		t.Fatal("Acquire over live lock no devolvió error")
	}
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Errorf("got err %v, want ErrAlreadyLocked", err)
	}
	// No debe haber tocado el lock existente.
	got := repo.snapshot()
	if len(got) != 1 || got[0] != existing {
		t.Errorf("repo state cambió ante lock vivo: %v (want exactly [%s])", got, existing)
	}
}

// TestAcquire_StaleEvicted: ref tiene un lock viejo (>TTL) → Acquire lo
// borra y aplica el suyo.
func TestAcquire_StaleEvicted(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	// Lock de hace 10 minutos — más del TTL default (5 min).
	stale := pipelinelabels.LockLabelAt(clock.Now().Add(-10*time.Minute), 999, "other-host")
	repo := newFakeRepo(stale)

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire over stale: %v", err)
	}
	defer h.Release()

	got := repo.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 label (the new one), got %d (%v)", len(got), got)
	}
	if got[0] == stale {
		t.Errorf("stale lock no fue evicted: %v", got)
	}
	// El nuevo es nuestro, no el stale.
	parsed, _ := pipelinelabels.Parse(got[0])
	if parsed.PID != 12345 {
		t.Errorf("pid del nuevo = %d, want 12345", parsed.PID)
	}
}

// TestHeartbeat_RefreshesTimestamp: tick avanza el timestamp del lock.
// Forzamos el clock a avanzar y llamamos HeartbeatNow para evitar timers
// reales.
func TestHeartbeat_RefreshesTimestamp(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()

	first := h.CurrentLabel()
	clock.Advance(2 * time.Minute)
	h.HeartbeatNow()
	second := h.CurrentLabel()

	if first == second {
		t.Errorf("CurrentLabel no cambió tras heartbeat: %s", first)
	}
	got := repo.snapshot()
	if len(got) != 1 {
		t.Fatalf("post-heartbeat: %d labels (%v), want 1", len(got), got)
	}
	if got[0] != second {
		t.Errorf("repo no refleja nuevo label: got %v want [%s]", got, second)
	}
	pNew, _ := pipelinelabels.Parse(second)
	pOld, _ := pipelinelabels.Parse(first)
	if !pNew.Timestamp.After(pOld.Timestamp) {
		t.Errorf("timestamp no avanzó: old %v new %v", pOld.Timestamp, pNew.Timestamp)
	}
}

// TestRelease_Idempotent: dos Releases no fallan ni tocan el repo de más.
func TestRelease_Idempotent(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Errorf("primera Release: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Errorf("segunda Release: %v", err)
	}
	if got := repo.snapshot(); len(got) != 0 {
		t.Errorf("post-doble-Release: %d labels (%v), want 0", len(got), got)
	}
}

// TestAcquire_StaleAtExactTTLBoundary: lock con edad == TTL es STALE
// (la condición es "edad < TTL = vivo"). Cubre el corner case del límite.
func TestAcquire_StaleAtExactTTLBoundary(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	// Edad exactamente igual al TTL.
	border := pipelinelabels.LockLabelAt(clock.Now().Add(-TTL), 999, "other-host")
	repo := newFakeRepo(border)

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire en boundary: %v", err)
	}
	defer h.Release()
}

// TestAcquire_RefFormats: número crudo, owner/repo#N, URL — todos parsean.
func TestAcquire_RefFormats(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"42", 42},
		{"#42", 42},
		{"acme/demo#7", 7},
		{"https://github.com/acme/demo/issues/9", 9},
		{"https://github.com/acme/demo/pull/10", 10},
	}
	for _, c := range cases {
		got, err := parseRefNumber(c.in)
		if err != nil {
			t.Errorf("parseRefNumber(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseRefNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestHeartbeat_KeepsCurrentOnAddFailure: si add falla durante el tick,
// CurrentLabel se revierte al viejo (no se queda con un label que no está
// aplicado en GitHub).
func TestHeartbeat_KeepsCurrentOnAddFailure(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	addFails := false
	add := func(n int, l string) error {
		if addFails {
			return fmt.Errorf("simulated network blip")
		}
		return repo.add(n, l)
	}

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    add,
		DelLabel:    repo.del,
		ListLabels:  repo.list,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()

	originalLabel := h.CurrentLabel()
	clock.Advance(2 * time.Minute)
	addFails = true
	h.HeartbeatNow()

	if got := h.CurrentLabel(); got != originalLabel {
		t.Errorf("tras add fallido, CurrentLabel = %q, want preservar %q", got, originalLabel)
	}
}
