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

// TestAcquire_RaceLossByTimestampTieBreak: en la re-list aparece otro
// lock vivo con timestamp MÁS VIEJO que el nuestro → perdemos por
// tie-break, retiramos nuestro lock y devolvemos ErrAlreadyLocked.
//
// Setup: el primer list devuelve vacío. El POST aplica el nuestro. El
// segundo list devuelve [nuestro, otro-mas-viejo]. Como el otro tiene
// timestamp MENOR (más viejo == llegó primero), el otro gana, nosotros
// retiramos.
func TestAcquire_RaceLossByTimestampTieBreak(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	// El otro lock tiene timestamp 30s más viejo (más temprano). Vamos a
	// inyectarlo entre el primer list y el segundo via list hook.
	otherLabel := pipelinelabels.LockLabelAt(clock.Now().Add(-30*time.Second), 999, "other-host")

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		// Re-list: agregamos el otro lock.
		out, _ := repo.list(n)
		out = append(out, otherLabel)
		return out, nil
	}

	_, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFn,
	})
	if err == nil {
		t.Fatal("Acquire ganó la race contra un lock más viejo (debería perder)")
	}
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Errorf("got err %v, want ErrAlreadyLocked", err)
	}
	// Nuestro lock debe haberse borrado (el repo no debe contener un
	// che:lock con nuestro pid). El otro NO está realmente en el repo (lo
	// inyectamos solo en el list hook), así que el repo debe quedar vacío.
	got := repo.snapshot()
	for _, l := range got {
		p, _ := pipelinelabels.Parse(l)
		if p.PID == 12345 {
			t.Errorf("nuestro lock %q quedó en el repo tras race lost", l)
		}
	}
}

// TestAcquire_RaceWinByTimestampTieBreak: en la re-list aparece otro
// lock vivo con timestamp MÁS NUEVO que el nuestro → ganamos por tie-break,
// borramos el lock del otro, conservamos el nuestro, retornamos OK.
func TestAcquire_RaceWinByTimestampTieBreak(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	// El otro lock tiene timestamp MÁS NUEVO (10s en el futuro relativo
	// al nuestro, simulando que llegó después).
	otherLabel := pipelinelabels.LockLabelAt(clock.Now().Add(10*time.Second), 999, "other-host")

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		out, _ := repo.list(n)
		out = append(out, otherLabel)
		return out, nil
	}
	// Inyectamos el "otro" en el repo para que el del() del race-won lo
	// pueda borrar de verdad.
	repoOtherInjected := false
	listFnWithSideEffect := func(n int) ([]string, error) {
		out, err := listFn(n)
		if !repoOtherInjected && listCalls == 2 {
			repo.add(n, otherLabel)
			repoOtherInjected = true
		}
		return out, err
	}

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFnWithSideEffect,
	})
	if err != nil {
		t.Fatalf("Acquire perdió contra un lock más nuevo (debería ganar): %v", err)
	}
	defer h.Release()

	// El otro lock debe haberse borrado, el nuestro debe quedar.
	got := repo.snapshot()
	hasOurs := false
	for _, l := range got {
		if l == otherLabel {
			t.Errorf("lock más nuevo %q no fue borrado tras race won", l)
		}
		p, _ := pipelinelabels.Parse(l)
		if p.PID == 12345 {
			hasOurs = true
		}
	}
	if !hasOurs {
		t.Errorf("nuestro lock no quedó aplicado tras race won; repo=%v", got)
	}
}

// TestAcquire_RaceTieBreakByPidHost: timestamps idénticos → desempate
// determinístico por string compare del segmento pid-host. El que tiene
// pid-host menor lexicográficamente gana.
//
// Caso: nuestro pid-host es "12345-test-host", el otro es "999-other-host".
// "12345..." < "999..." en orden lexicográfico (compara char-a-char,
// '1' < '9'), así que ganamos.
func TestAcquire_RaceTieBreakByPidHost(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	// Mismo timestamp que el nuestro → fuerza el desempate por pid-host.
	otherLabel := pipelinelabels.LockLabelAt(clock.Now(), 999, "other-host")

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		out, _ := repo.list(n)
		out = append(out, otherLabel)
		if listCalls == 2 {
			repo.add(n, otherLabel)
		}
		return out, nil
	}

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFn,
	})
	// "12345-test-host" < "999-other-host" → nosotros ganamos.
	if err != nil {
		t.Fatalf("Acquire perdió desempate por pid-host: %v\nour=12345-test-host other=999-other-host", err)
	}
	defer h.Release()

	got := repo.snapshot()
	for _, l := range got {
		if l == otherLabel {
			t.Errorf("lock perdedor por pid-host no fue borrado: %q", l)
		}
	}
}

// TestAcquire_RaceTieBreakByPidHost_Lose: caso simétrico — desempate por
// pid-host donde nosotros perdemos (nuestro pid-host es lexicográficamente
// MAYOR). Con pid="999" vs other pid="100" + mismo host, "100-..." gana.
func TestAcquire_RaceTieBreakByPidHost_Lose(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	// Mismo host + mismo timestamp + pid menor → el otro gana.
	otherLabel := pipelinelabels.LockLabelAt(clock.Now(), 100, "test-host")

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		out, _ := repo.list(n)
		out = append(out, otherLabel)
		return out, nil
	}

	_, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         999,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFn,
	})
	if err == nil {
		t.Fatal("Acquire ganó desempate por pid-host (debería perder con pid mayor)")
	}
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Errorf("got err %v, want ErrAlreadyLocked", err)
	}
}

// TestAcquire_RaceWithUnparsableOther: el otro lock está malformado
// (timestamp inválido) → tratado como broken, nosotros ganamos y lo
// borramos.
func TestAcquire_RaceWithUnparsableOther(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	// Label con prefijo che:lock: pero formato inválido (no parsea).
	brokenLabel := pipelinelabels.PrefixLock + "not-a-timestamp:bad"

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		out, _ := repo.list(n)
		out = append(out, brokenLabel)
		if listCalls == 2 {
			repo.add(n, brokenLabel)
		}
		return out, nil
	}

	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFn,
	})
	if err != nil {
		t.Fatalf("Acquire falló contra broken lock (debería ganar): %v", err)
	}
	defer h.Release()

	got := repo.snapshot()
	for _, l := range got {
		if l == brokenLabel {
			t.Errorf("broken lock %q no fue borrado", l)
		}
	}
}

// TestAcquire_PostCheckListError: la segunda llamada a listLabels falla
// → Acquire devuelve ErrPostCheckFailed envuelto, NO asume OK silencioso.
// El handle devuelto lleva el lock aplicado (para que el caller pueda
// Release si decide abortar).
func TestAcquire_PostCheckListError(t *testing.T) {
	withInstantHeartbeat(t)
	clock := newFakeClock(time.Unix(1700000000, 0))
	repo := newFakeRepo()

	listCalls := 0
	listFn := func(n int) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return repo.list(n)
		}
		return nil, fmt.Errorf("simulated list failure on post-check")
	}

	var warns []string
	h, err := Acquire("42", Options{
		Now:         clock.Now,
		PID:         12345,
		Host:        "test-host",
		EnsureLabel: func(string) error { return nil },
		AddLabel:    repo.add,
		DelLabel:    repo.del,
		ListLabels:  listFn,
		LogErrf: func(format string, args ...any) {
			warns = append(warns, fmt.Sprintf(format, args...))
		},
	})
	if err == nil {
		t.Fatal("Acquire NO debería asumir OK silencioso si la post-check list falla")
	}
	if !errors.Is(err, ErrPostCheckFailed) {
		t.Errorf("got err %v, want wrapped ErrPostCheckFailed", err)
	}
	if h == nil {
		t.Fatal("Acquire debería devolver Handle parcial para que el caller pueda Release")
	}
	// El lock debe estar aplicado (el handle lo registra).
	if h.CurrentLabel() == "" {
		t.Errorf("Handle.CurrentLabel vacío tras post-check failure; want lock aplicado")
	}
	// Debe haber warneado al logger.
	if len(warns) == 0 {
		t.Errorf("post-check failure no warneó al logger del caller")
	}
	foundPostCheckWarn := false
	for _, w := range warns {
		if strings.Contains(w, "post-check") {
			foundPostCheckWarn = true
			break
		}
	}
	if !foundPostCheckWarn {
		t.Errorf("warns no mencionan post-check: %v", warns)
	}
	// Y Release debe ser idempotente / no-panic sobre este handle parcial.
	if relErr := h.Release(); relErr != nil {
		t.Errorf("Release del handle parcial falló: %v", relErr)
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
