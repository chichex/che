package output_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chichex/che/internal/output"
)

func TestLogger_NilSinkIsSafe(t *testing.T) {
	log := output.New(nil)
	// No debe panicar ni fallar.
	log.Info("hola")
	log.Success("ok", output.F{Issue: 1})
	log.Error("boom", output.F{Cause: errors.New("x")})
}

func TestCapturingSink_CollectsEvents(t *testing.T) {
	sink := &output.CapturingSink{}
	log := output.New(sink)

	log.Info("chequeando repo")
	log.Success("creado", output.F{Issue: 47, URL: "https://example.test/issues/47"})
	log.Warn("best-effort fallo", output.F{Detail: "retry=3"})
	log.Error("boom", output.F{Cause: errors.New("db down")})

	evs := sink.Events()
	if got, want := len(evs), 4; got != want {
		t.Fatalf("events len = %d, want %d", got, want)
	}
	if evs[0].Level != output.LevelInfo {
		t.Errorf("ev[0].Level = %v, want LevelInfo", evs[0].Level)
	}
	if evs[1].Fields.Issue != 47 {
		t.Errorf("ev[1].Fields.Issue = %d, want 47", evs[1].Fields.Issue)
	}
	if evs[2].Level != output.LevelWarn {
		t.Errorf("ev[2].Level = %v, want LevelWarn", evs[2].Level)
	}
	if evs[3].Fields.Cause == nil || evs[3].Fields.Cause.Error() != "db down" {
		t.Errorf("ev[3].Fields.Cause = %v, want db down", evs[3].Fields.Cause)
	}
}

func TestCapturingSink_FindByMessage(t *testing.T) {
	sink := &output.CapturingSink{}
	log := output.New(sink)
	log.Info("procesando items")
	log.Success("creado")

	ev, ok := sink.FindByMessage("procesando")
	if !ok {
		t.Fatal("FindByMessage no encontro 'procesando'")
	}
	if ev.Level != output.LevelInfo {
		t.Errorf("nivel = %v, want Info", ev.Level)
	}
	if _, ok := sink.FindByMessage("no-existe"); ok {
		t.Error("FindByMessage devolvio true para un substring que no existe")
	}
}

func TestCapturingSink_CountByLevel(t *testing.T) {
	sink := &output.CapturingSink{}
	log := output.New(sink)
	log.Info("a")
	log.Info("b")
	log.Warn("c")

	if got := sink.CountByLevel(output.LevelInfo); got != 2 {
		t.Errorf("CountByLevel(Info) = %d, want 2", got)
	}
	if got := sink.CountByLevel(output.LevelWarn); got != 1 {
		t.Errorf("CountByLevel(Warn) = %d, want 1", got)
	}
}

func TestWriterSink_NoColorWritesPlainText(t *testing.T) {
	// bytes.Buffer no es *os.File → shouldColor devuelve false →
	// NewWriterSink desactiva ANSI. Esto es lo que vamos a asertar en e2e
	// tests (que corren con stderr redirigido a un pipe).
	var buf bytes.Buffer
	sink := output.NewWriterSink(&buf)
	log := output.New(sink)

	log.Success("creado", output.F{Issue: 47, Labels: []string{"type:feat", "ct:plan"}, URL: "https://example.test/47"})
	log.Warn("problema", output.F{PR: 7})
	log.Error("fallo", output.F{Cause: errors.New("timeout")})

	got := buf.String()
	// No debe haber ANSI escape codes.
	if strings.Contains(got, "\x1b[") {
		t.Errorf("output contiene ANSI escape codes en modo no-TTY:\n%s", got)
	}
	// Simbolos preservados.
	for _, want := range []string{"✓", "⚠", "✗"} {
		if !strings.Contains(got, want) {
			t.Errorf("output no contiene simbolo %q:\n%s", want, got)
		}
	}
	// Mensajes presentes sin prefijos verbales (los simbolos expresan severidad).
	if !strings.Contains(got, "⚠ problema") {
		t.Errorf("output no contiene '⚠ problema':\n%s", got)
	}
	if !strings.Contains(got, "✗ fallo") {
		t.Errorf("output no contiene '✗ fallo':\n%s", got)
	}
	// Fields estructurados presentes.
	if !strings.Contains(got, "#47") {
		t.Errorf("output no contiene '#47':\n%s", got)
	}
	if !strings.Contains(got, "[type:feat, ct:plan]") {
		t.Errorf("output no contiene labels con formato '[a, b]':\n%s", got)
	}
	if !strings.Contains(got, "https://example.test/47") {
		t.Errorf("output no contiene la URL:\n%s", got)
	}
	if !strings.Contains(got, "PR") || !strings.Contains(got, "#7") {
		t.Errorf("output no contiene 'PR #7':\n%s", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("output no contiene el Cause:\n%s", got)
	}
}

func TestWriterSink_OneLinePerEmit(t *testing.T) {
	var buf bytes.Buffer
	log := output.New(output.NewWriterSink(&buf))

	log.Info("a")
	log.Info("b")
	log.Info("c")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if got, want := len(lines), 3; got != want {
		t.Errorf("lines = %d, want %d:\n%s", got, want, buf.String())
	}
}

func TestWriterSink_ConcurrentSafe(t *testing.T) {
	// Simulamos el caso de execute: varias goroutines emiten en paralelo.
	// Sin mutex se entrelazarian a media linea. Con mutex, cada linea
	// queda entera.
	var buf bytes.Buffer
	log := output.New(output.NewWriterSink(&buf))

	const goroutines = 16
	const perGoroutine = 32

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				log.Info("paralelo", output.F{Issue: id*1000 + i})
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if got, want := len(lines), goroutines*perGoroutine; got != want {
		t.Fatalf("lines = %d, want %d (hay entrelazado?)", got, want)
	}
	// Cada linea debe empezar con el simbolo de info.
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "▸") {
			t.Errorf("linea %d no empieza con simbolo de info (entrelazado?): %q", i, ln)
			break
		}
	}
}

func TestNopSink_DoesNothing(t *testing.T) {
	// Probamos que no panica y que no afecta al logger.
	log := output.New(output.NopSink{})
	log.Info("x")
	log.Success("y")
	log.Error("z", output.F{Cause: errors.New("e")})
}

func TestRender_FunctionReturnsString(t *testing.T) {
	// Render() es util para la TUI que quiera pre-renderizar usando el
	// mismo formato del CLI.
	got := output.Render(output.Event{
		Level:   output.LevelSuccess,
		Message: "creado",
		Fields:  output.F{Issue: 42},
	})
	if !strings.Contains(got, "creado") {
		t.Errorf("Render no contiene el message: %q", got)
	}
	if !strings.Contains(got, "#42") {
		t.Errorf("Render no contiene el issue: %q", got)
	}
}
