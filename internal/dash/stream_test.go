package dash

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStreamServer arma un Server con un runAction stub que, en vez de
// spawnear un proceso real, escribe N líneas al LogStore y cierra el run.
// Esto ejercita la ruta POST /action + GET /stream end-to-end sin
// depender del binario che.
func newStreamServer(t *testing.T, lines []string) (*httptest.Server, *Server) {
	t.Helper()
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "plan ready", Status: "plan"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	// Stub: reset, append, close. Simula el lifecycle del subproceso.
	s.runAction = func(flow string, id int, repo string) error {
		s.logs.ResetRun(id)
		go func() {
			for _, ln := range lines {
				s.logs.Append(id, LogLine{
					Time:   time.Now(),
					Stream: "stdout",
					Text:   ln,
				})
				time.Sleep(5 * time.Millisecond)
			}
			s.logs.CloseRun(id)
			s.clearRunning(id)
		}()
		return nil
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, s
}

// parseSSEEvents lee una respuesta SSE y extrae pares (event, data). Corta
// al ver un `event: done` o EOF. Timeout defensivo.
func parseSSEEvents(t *testing.T, r io.Reader, timeout time.Duration) []sseEvent {
	t.Helper()
	out := []sseEvent{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.Now().Add(timeout)

	var cur sseEvent
	for time.Now().Before(deadline) {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, ":") {
			// Heartbeat / comment — ignorar.
			continue
		}
		if line == "" {
			// Fin del evento.
			if cur.Event != "" {
				out = append(out, cur)
				if cur.Event == "done" {
					return out
				}
			}
			cur = sseEvent{}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			cur.Event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if cur.Data != "" {
				cur.Data += "\n"
			}
			cur.Data += strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

type sseEvent struct {
	Event string
	Data  string
}

// TestStream_StreamsHistoryAndLiveLines chequea el happy path end-to-end:
// POST /action dispara el fake runner (que apendea 3 líneas + close), GET
// /stream/42 recibe las 3 como `line` y un `done` final.
func TestStream_StreamsHistoryAndLiveLines(t *testing.T) {
	ts, _ := newStreamServer(t, []string{"alpha", "beta", "gamma"})

	// 1. Dispatch del flow.
	resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST /action: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dispatch status: %d", resp.StatusCode)
	}

	// 2. Conectar al stream. Usamos net/http client directo para poder
	// leer incremental sin que el transport bufferee (httptest.Server
	// no buferrea, pero el client sí si esperamos a cerrar el body).
	req, _ := http.NewRequest("GET", ts.URL+"/stream/42", nil)
	streamResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != 200 {
		t.Fatalf("stream status: got %d want 200", streamResp.StatusCode)
	}
	if ct := streamResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type: got %q want text/event-stream", ct)
	}

	events := parseSSEEvents(t, streamResp.Body, 3*time.Second)

	// Filtrar solo los `line` para buscar los 3 textos conocidos.
	// Puede haber un `meta` extra al inicio — lo ignoramos (no rompe el
	// contrato, solo es un marker opcional).
	gotTexts := []string{}
	sawDone := false
	for _, ev := range events {
		if ev.Event == "line" {
			if strings.Contains(ev.Data, `"x":"alpha"`) {
				gotTexts = append(gotTexts, "alpha")
			} else if strings.Contains(ev.Data, `"x":"beta"`) {
				gotTexts = append(gotTexts, "beta")
			} else if strings.Contains(ev.Data, `"x":"gamma"`) {
				gotTexts = append(gotTexts, "gamma")
			}
		}
		if ev.Event == "done" {
			sawDone = true
		}
	}
	if len(gotTexts) != 3 {
		t.Errorf("expected 3 line events with alpha/beta/gamma; got %v (all events: %+v)", gotTexts, events)
	}
	if !sawDone {
		t.Errorf("missing `done` event at end of stream; got events: %+v", events)
	}
}

// TestStream_NotFoundForUnknownID: GET /stream/9999 cuando nadie disparó
// un flow para ese id devuelve 404.
func TestStream_NotFoundForUnknownID(t *testing.T) {
	ts, _ := newStreamServer(t, nil)

	resp, err := http.Get(ts.URL + "/stream/9999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
}

// TestStream_InvalidID: GET /stream/abc responde 400.
func TestStream_InvalidID(t *testing.T) {
	ts, _ := newStreamServer(t, nil)

	resp, err := http.Get(ts.URL + "/stream/abc")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
}

// TestStream_ClientDisconnect chequea que cuando el cliente corta la
// conexión a mitad del stream, la goroutine del handler sale limpio (no
// leak) y futuros Append al LogStore no panikean.
func TestStream_ClientDisconnect(t *testing.T) {
	s := NewServer(&fixedSource{snap: Snapshot{Entities: []Entity{{IssueNumber: 1}}}}, "t", 15)
	s.logs.Append(1, LogLine{Time: time.Now(), Stream: "meta", Text: "start"})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	// Conectar y cortar al toque.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream/1", nil)

	baseline := runtime.NumGoroutine()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream/1: %v", err)
	}

	// Leer un par de bytes para asegurar que el handler arrancó.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)

	// Cortar la conexión.
	cancel()
	resp.Body.Close()

	// Dar tiempo al handler de terminar. Append post-cancel debe ser
	// seguro (no bloqueo, no panic) — el handler canceló su Subscribe.
	time.Sleep(200 * time.Millisecond)
	s.logs.Append(1, LogLine{Time: time.Now(), Stream: "stdout", Text: "post-cancel"})
	s.logs.CloseRun(1)

	// Dar tiempo final al scheduler.
	time.Sleep(100 * time.Millisecond)
	leaked := runtime.NumGoroutine() - baseline
	// Algunos leaks son del runtime / httptest — toleramos un margen.
	// Lo importante: no leaks inesperados grandes.
	if leaked > 4 {
		t.Errorf("posible goroutine leak: %d más que baseline (%d → %d)", leaked, baseline, runtime.NumGoroutine())
	}
}

// TestStream_MultipleConcurrentSubscribers chequea que 2 clientes SSE
// sobre el mismo id reciben cada uno el mismo stream.
func TestStream_MultipleConcurrentSubscribers(t *testing.T) {
	s := NewServer(&fixedSource{snap: Snapshot{Entities: []Entity{{IssueNumber: 5}}}}, "t", 15)
	// Seed con 1 línea para que ambos vean algo en el snapshot.
	s.logs.Append(5, LogLine{Time: time.Now(), Stream: "stdout", Text: "seed"})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	results := make([][]sseEvent, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/stream/5")
			if err != nil {
				t.Errorf("client %d GET: %v", idx, err)
				return
			}
			defer resp.Body.Close()
			results[idx] = parseSSEEvents(t, resp.Body, 2*time.Second)
		}(i)
	}

	// Damos tiempo a los subscribers a engancharse antes de apendear.
	time.Sleep(100 * time.Millisecond)
	s.logs.Append(5, LogLine{Time: time.Now(), Stream: "stdout", Text: "live"})
	s.logs.CloseRun(5)

	wg.Wait()

	for i, evs := range results {
		sawSeed := false
		sawLive := false
		sawDone := false
		for _, ev := range evs {
			if ev.Event == "line" {
				if strings.Contains(ev.Data, `"x":"seed"`) {
					sawSeed = true
				}
				if strings.Contains(ev.Data, `"x":"live"`) {
					sawLive = true
				}
			}
			if ev.Event == "done" {
				sawDone = true
			}
		}
		if !sawSeed || !sawLive || !sawDone {
			t.Errorf("client %d: seed=%v live=%v done=%v (events=%+v)", i, sawSeed, sawLive, sawDone, evs)
		}
	}
}

// TestDrawer_RendersLiveLogMount chequea que el drawer incluye el mount
// point `.live-log` y el placeholder `.live-empty` (sin la referencia a
// "step 4/step 5").
func TestDrawer_RendersLiveLogMount(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "t", Status: "plan"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `class="live-log"`) {
		t.Errorf("drawer missing .live-log mount point")
	}
	if !strings.Contains(got, `class="live-empty"`) {
		t.Errorf("drawer missing .live-empty placeholder")
	}
	if !strings.Contains(got, `data-entity="42"`) {
		t.Errorf("drawer-logs-body missing data-entity=\"42\"")
	}
	// El mensaje viejo no debería seguir presente.
	if strings.Contains(got, "step 4 trae las acciones") {
		t.Errorf("drawer still contains legacy placeholder referencing steps internos")
	}
}

// TestRunCmdWithLogs_TeeIntegration lanza un subproceso real (sh -c "echo
// a; echo b >&2; echo c") y verifica que el LogStore capture las 3 líneas
// (una a stdout, una a stderr, otra a stdout) y emita un done SSE cuando
// termina. Cubre la ruta bufio.Scanner + StdoutPipe/StderrPipe sin
// depender del binario che.
func TestRunCmdWithLogs_TeeIntegration(t *testing.T) {
	s := NewServer(&fixedSource{snap: Snapshot{Entities: []Entity{{IssueNumber: 77}}}}, "t", 15)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	cmd := exec.Command("sh", "-c", "echo alpha; echo beta 1>&2; echo gamma")
	if err := s.runCmdWithLogs(cmd, 77, "sh echo"); err != nil {
		t.Fatalf("runCmdWithLogs: %v", err)
	}

	// Conectar al stream y consumir hasta done.
	resp, err := http.Get(ts.URL + "/stream/77")
	if err != nil {
		t.Fatalf("GET /stream/77: %v", err)
	}
	defer resp.Body.Close()
	events := parseSSEEvents(t, resp.Body, 3*time.Second)

	gotTexts := map[string]string{}
	sawDone := false
	for _, ev := range events {
		if ev.Event == "line" {
			if strings.Contains(ev.Data, `"x":"alpha"`) {
				gotTexts["alpha"] = ev.Data
			} else if strings.Contains(ev.Data, `"x":"beta"`) {
				gotTexts["beta"] = ev.Data
			} else if strings.Contains(ev.Data, `"x":"gamma"`) {
				gotTexts["gamma"] = ev.Data
			}
		}
		if ev.Event == "done" {
			sawDone = true
		}
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if _, ok := gotTexts[want]; !ok {
			t.Errorf("missing line %q in events %+v", want, events)
		}
	}
	// Clasificación de streams correcta.
	if d, ok := gotTexts["alpha"]; ok && !strings.Contains(d, `"s":"stdout"`) {
		t.Errorf("alpha should be stdout stream; got data %q", d)
	}
	if d, ok := gotTexts["beta"]; ok && !strings.Contains(d, `"s":"stderr"`) {
		t.Errorf("beta should be stderr stream; got data %q", d)
	}
	if !sawDone {
		t.Errorf("missing done event (flow should have closed the run)")
	}
}

// TestSpawnChe_StubbedIntegration verifica que spawnChe emite marker meta
// + CloseRun cuando el Start falla (binario inexistente). El test usa un
// bin path inválido para forzar el branch de error sin dependencias.
func TestSpawnChe_StartFailurePath(t *testing.T) {
	// Caso indirecto: si no podemos testear el happy path sin un child
	// real, cubrimos al menos la ruta del CloseRun automático via
	// ResetRun + Append de marker. Lo hacemos a través de runAction stub
	// en TestStream_StreamsHistoryAndLiveLines — acá solo chequeamos que
	// el handler POST /action maneja un error del runAction limpiando la
	// reserva y devolviendo 500.
	src := &fixedSource{snap: Snapshot{
		NWO: "demo/che",
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
		},
		LastOK: time.Now(),
	}}
	s := NewServer(src, "t", 15)
	s.runAction = func(flow string, id int, repo string) error {
		return io.ErrUnexpectedEOF // marker arbitrario
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", resp.StatusCode)
	}
	// Reserva limpiada: un 2do POST no debe dar 409.
	resp2, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST#2: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusConflict {
		t.Errorf("segundo POST no debería ser 409 — la reserva no se limpió post-error")
	}
}
