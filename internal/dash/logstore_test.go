package dash

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestLogStore_AppendAndSnapshot chequea el happy path: al hacer Append N
// veces, Snapshot devuelve las N líneas en orden.
func TestLogStore_AppendAndSnapshot(t *testing.T) {
	s := NewLogStore()
	for i := 0; i < 5; i++ {
		s.Append(42, LogLine{Time: time.Now(), Stream: "stdout", Text: fmt.Sprintf("line %d", i)})
	}
	got := s.Snapshot(42)
	if len(got) != 5 {
		t.Fatalf("snapshot len: got %d want 5", len(got))
	}
	for i, ln := range got {
		want := fmt.Sprintf("line %d", i)
		if ln.Text != want {
			t.Errorf("snapshot[%d].Text: got %q want %q", i, ln.Text, want)
		}
	}
}

// TestLogStore_RingEviction chequea que al pasarse de la capacidad, las
// líneas más viejas se dropean y Snapshot sigue devolviendo cap en orden.
func TestLogStore_RingEviction(t *testing.T) {
	s := NewLogStoreSize(3)
	for i := 0; i < 5; i++ {
		s.Append(1, LogLine{Text: fmt.Sprintf("l%d", i)})
	}
	got := s.Snapshot(1)
	if len(got) != 3 {
		t.Fatalf("snapshot len: got %d want 3", len(got))
	}
	// Las últimas 3: l2, l3, l4.
	want := []string{"l2", "l3", "l4"}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("snapshot[%d].Text: got %q want %q", i, got[i].Text, w)
		}
	}
}

// TestLogStore_Subscribe_FutureLines chequea que Subscribe recibe solo las
// líneas apendeadas DESPUÉS de la suscripción. La historia se pide aparte.
func TestLogStore_Subscribe_FutureLines(t *testing.T) {
	s := NewLogStore()
	s.Append(1, LogLine{Text: "before"})

	ch, cancel := s.Subscribe(1)
	defer cancel()

	s.Append(1, LogLine{Text: "after-1"})
	s.Append(1, LogLine{Text: "after-2"})

	got := []string{}
	timeout := time.After(500 * time.Millisecond)
	for len(got) < 2 {
		select {
		case ln, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed prematurely")
			}
			got = append(got, ln.Text)
		case <-timeout:
			t.Fatalf("timeout esperando líneas; got %v want [after-1 after-2]", got)
		}
	}
	if got[0] != "after-1" || got[1] != "after-2" {
		t.Errorf("got %v want [after-1 after-2]", got)
	}
}

// TestLogStore_Subscribe_CancelIdempotent chequea que llamar cancel dos
// veces no panikea y libera el canal. Segundo Append no bloquea aunque el
// canal esté cerrado (drop silencioso por el select default).
func TestLogStore_Subscribe_CancelIdempotent(t *testing.T) {
	s := NewLogStore()
	ch, cancel := s.Subscribe(1)
	cancel()
	cancel() // idempotente, no panikea
	// Append después del cancel no hace nada observable — el canal no
	// recibe (ya está cerrado y fue removido de subs). Seguimos vivos.
	s.Append(1, LogLine{Text: "after-cancel"})
	// Drain: el canal debería estar cerrado.
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("channel debería estar cerrado después de cancel")
		}
	case <-time.After(100 * time.Millisecond):
		// Canal cerrado también satisface este branch; pero un canal
		// cerrado recibe el zero value inmediatamente, así que no
		// deberíamos quedarnos acá.
		t.Errorf("timeout esperando close en canal cancelado")
	}
}

// TestLogStore_CloseRun_ClosesSubscribers chequea que CloseRun cierra los
// canales de los subscribers vivos (un range sobre el canal sale del loop).
func TestLogStore_CloseRun_ClosesSubscribers(t *testing.T) {
	s := NewLogStore()
	ch, cancel := s.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	s.Append(1, LogLine{Text: "hello"})
	s.CloseRun(1)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("CloseRun no cerró el canal del subscriber; range del reader sigue bloqueado")
	}

	// Idempotencia.
	s.CloseRun(1)
}

// TestLogStore_MultipleSubscribers chequea que varios subscribers del mismo
// id reciben las líneas en paralelo.
func TestLogStore_MultipleSubscribers(t *testing.T) {
	s := NewLogStore()
	ch1, cancel1 := s.Subscribe(1)
	ch2, cancel2 := s.Subscribe(1)
	defer cancel1()
	defer cancel2()

	s.Append(1, LogLine{Text: "broadcast"})

	for _, ch := range []<-chan LogLine{ch1, ch2} {
		select {
		case ln := <-ch:
			if ln.Text != "broadcast" {
				t.Errorf("got %q want broadcast", ln.Text)
			}
		case <-time.After(200 * time.Millisecond):
			t.Errorf("subscriber no recibió la línea")
		}
	}
}

// TestLogStore_SubscribeAfterClose chequea que Subscribe sobre un run ya
// cerrado devuelve un canal cerrado (no deadlockea).
func TestLogStore_SubscribeAfterClose(t *testing.T) {
	s := NewLogStore()
	s.Append(1, LogLine{Text: "first"})
	s.CloseRun(1)

	ch, cancel := s.Subscribe(1)
	defer cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("canal debería estar cerrado (run ya terminó)")
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("Subscribe después de CloseRun debería devolver canal cerrado (no bloquear)")
	}

	// Snapshot sigue funcionando tras close.
	snap := s.Snapshot(1)
	if len(snap) != 1 || snap[0].Text != "first" {
		t.Errorf("snapshot post-close: got %+v want [first]", snap)
	}
}

// TestLogStore_Exists verifica Exists/Closed.
func TestLogStore_Exists(t *testing.T) {
	s := NewLogStore()
	if s.Exists(1) {
		t.Errorf("Exists(1) debería ser false antes del primer Append")
	}
	if s.Closed(1) {
		t.Errorf("Closed(1) debería ser false antes del primer Append")
	}
	s.Append(1, LogLine{Text: "hi"})
	if !s.Exists(1) {
		t.Errorf("Exists(1) debería ser true post-Append")
	}
	if s.Closed(1) {
		t.Errorf("Closed(1) debería ser false sin CloseRun")
	}
	s.CloseRun(1)
	if !s.Exists(1) {
		t.Errorf("Exists(1) debería seguir true post-CloseRun (historia persiste)")
	}
	if !s.Closed(1) {
		t.Errorf("Closed(1) debería ser true post-CloseRun")
	}
}

// TestLogStore_ConcurrentAppendSubscribeCancel stress-tea el race detector:
// múltiples goroutines apendean, se suscriben y cancelan en paralelo.
func TestLogStore_ConcurrentAppendSubscribeCancel(t *testing.T) {
	s := NewLogStore()
	var wg sync.WaitGroup

	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Append(1, LogLine{Text: fmt.Sprintf("w%d", i)})
		}
	}()

	// Subscribers que entran y salen.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ch, cancel := s.Subscribe(1)
				// Drenar un par y salir.
				drained := 0
				for drained < 3 {
					select {
					case _, ok := <-ch:
						if !ok {
							drained = 3
						} else {
							drained++
						}
					case <-time.After(5 * time.Millisecond):
						drained = 3
					}
				}
				cancel()
			}
		}()
	}

	wg.Wait()

	// Después de todo, seguimos pudiendo Append sin panic.
	s.Append(1, LogLine{Text: "final"})
	snap := s.Snapshot(1)
	if len(snap) == 0 {
		t.Errorf("snapshot vacío después de stress; esperaba al menos 1")
	}
}

// TestLogStore_ResetRun reinicia historia y cierra subs vivos.
func TestLogStore_ResetRun(t *testing.T) {
	s := NewLogStore()
	s.Append(1, LogLine{Text: "run1-a"})
	s.Append(1, LogLine{Text: "run1-b"})
	ch, cancel := s.Subscribe(1)
	defer cancel()

	// Reset cierra el subscriber vivo y limpia la historia.
	s.ResetRun(1)

	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("canal debería estar cerrado post-ResetRun")
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("ResetRun debería cerrar subscribers vivos")
	}

	if got := s.Snapshot(1); len(got) != 0 {
		t.Errorf("snapshot post-reset: got %d want 0", len(got))
	}

	// Nuevo run: append y snapshot ven solo lo nuevo.
	s.Append(1, LogLine{Text: "run2-a"})
	snap := s.Snapshot(1)
	if len(snap) != 1 || snap[0].Text != "run2-a" {
		t.Errorf("snapshot post-reset+append: got %+v want [run2-a]", snap)
	}
}
