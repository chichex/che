package dash

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chichex/che/internal/runner"
	"github.com/chichex/che/internal/wizard"
)

// ── mocks ────────────────────────────────────────────────────────────────

// recordingStarter es un runStarter de tests: registra cada Start y devuelve
// un runID determinista. Sirve para los happy-paths (verificamos que el
// handler llamo al starter con los argumentos esperados). El callback
// onDone se guarda para los tests que quieren simular "Execute() termino"
// invocandolo manualmente; los demas lo ignoran y el lock queda retenido
// durante la duracion del test (semantica original).
type recordingStarter struct {
	mu        sync.Mutex
	calls     []struct{ target, input string }
	dones     []func()
	nextRunID string
	err       error
}

func (s *recordingStarter) Start(target, input string, onDone func()) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, struct{ target, input string }{target, input})
	s.dones = append(s.dones, onDone)
	if s.err != nil {
		return "", s.err
	}
	return s.nextRunID, nil
}

// noopStarter es un starter que solo devuelve un runID fijo. Lo usamos en
// rutas donde el resultado del Start no importa (ej. dispatcher routing).
type noopStarter struct{}

func (s *noopStarter) Start(_, _ string, _ func()) (string, error) { return "noop-run", nil }

// ── helpers ──────────────────────────────────────────────────────────────

// postRun arma un POST contra el dispatcher con el body JSON dado y devuelve
// el recorder. Cuando body es nil, no se envia Content-Type ni payload —
// emula el `fetch POST` sin body que la UI dispara para pipelines input=none.
func postRun(t *testing.T, dispatcher http.HandlerFunc, slug string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/pipelines/"+slug+"/runs", reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	dispatcher(rr, req)
	return rr
}

func decodeCreateRunResponse(t *testing.T, rr *httptest.ResponseRecorder) createRunResponse {
	t.Helper()
	var resp createRunResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v body=%s", err, rr.Body.String())
	}
	return resp
}

func decodeErrorBody(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body: %v body=%s", err, rr.Body.String())
	}
	return body["error"]
}

// ── tests ────────────────────────────────────────────────────────────────

// TestCreateRun_HappyPathNoInput cubre el AC #1: POST a un pipeline con
// input=none responde 201 + runID + url, sin pasar body. El handler tiene
// que llamar al starter con target derivado del slug (path en disco) e
// input string vacio.
func TestCreateRun_HappyPathNoInput(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "no-input-pipe", wizard.Pipeline{
		Name:  "no-input-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	starter := &recordingStarter{nextRunID: "RUN-001"}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "no-input-pipe", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeCreateRunResponse(t, rr)
	if resp.RunID != "RUN-001" {
		t.Errorf("run_id: got %q want %q", resp.RunID, "RUN-001")
	}
	wantURL := "/api/pipelines/no-input-pipe/runs/RUN-001"
	if resp.URL != wantURL {
		t.Errorf("url: got %q want %q", resp.URL, wantURL)
	}
	if len(starter.calls) != 1 {
		t.Fatalf("starter.calls: got %d want 1", len(starter.calls))
	}
	if !strings.HasSuffix(starter.calls[0].target, "no-input-pipe.yaml") {
		t.Errorf("starter.target: got %q want suffix no-input-pipe.yaml", starter.calls[0].target)
	}
	if starter.calls[0].input != "" {
		t.Errorf("starter.input: got %q want empty", starter.calls[0].input)
	}
}

// TestCreateRun_HappyPathWithInput cubre el AC #2: POST con body
// {"input":"foo"} propaga el input al starter.
func TestCreateRun_HappyPathWithInput(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "text-pipe", wizard.Pipeline{
		Name:  "text-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputText}},
	})
	starter := &recordingStarter{nextRunID: "RUN-text"}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "text-pipe", map[string]string{"input": "foo"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(starter.calls) != 1 || starter.calls[0].input != "foo" {
		t.Errorf("starter.input: got %+v want input=foo", starter.calls)
	}
}

// TestCreateRun_BuiltinSlug verifica que el handler resuelve slugs que
// existen solo como builtin (sin .yaml en disco). target esperado:
// "builtin:che-funnel". che-funnel declara input=text en su primer step
// (segun internal/wizard/embedded/che-funnel.yaml), asi que mandamos
// {"input": "..."} para satisfacer la validacion.
func TestCreateRun_BuiltinSlug(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	starter := &recordingStarter{nextRunID: "RUN-builtin"}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "che-funnel", map[string]string{"input": "una idea"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(starter.calls) != 1 || starter.calls[0].target != "builtin:che-funnel" {
		t.Errorf("starter.target: got %+v want builtin:che-funnel", starter.calls)
	}
}

// TestCreateRun_InputRequired cubre AC #3: POST a un pipeline con
// input.kind != none sin body responde 400 con "input requerido".
func TestCreateRun_InputRequired(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "text-pipe", wizard.Pipeline{
		Name:  "text-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputText}},
	})
	starter := &recordingStarter{}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "text-pipe", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if msg := decodeErrorBody(t, rr); msg != "input requerido" {
		t.Errorf("error: got %q want 'input requerido'", msg)
	}
	if len(starter.calls) != 0 {
		t.Errorf("starter no debio invocarse; calls=%+v", starter.calls)
	}
}

// TestCreateRun_NotFound cubre AC #4: slug que no existe ni en disco ni en
// builtins responde 404.
func TestCreateRun_NotFound(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	starter := &recordingStarter{}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "no-existe", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	if msg := decodeErrorBody(t, rr); msg != "pipeline not found" {
		t.Errorf("error: got %q want 'pipeline not found'", msg)
	}
}

// TestCreateRun_ConflictLockHeldByMemory verifica que el lock en memoria
// rechaza un segundo POST mientras el primero todavia esta vivo.
func TestCreateRun_ConflictLockHeldByMemory(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "lock-pipe", wizard.Pipeline{
		Name:  "lock-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	starter := &recordingStarter{nextRunID: "RUN-lock"}
	lock := newRunLock()
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, lock)

	// Primer POST OK.
	rr1 := postRun(t, dispatcher, "lock-pipe", nil)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("primer POST: want 201, got %d", rr1.Code)
	}

	// Segundo POST con lock activo → 409.
	rr2 := postRun(t, dispatcher, "lock-pipe", nil)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("segundo POST: want 409, got %d body=%s", rr2.Code, rr2.Body.String())
	}
	if msg := decodeErrorBody(t, rr2); msg != "pipeline ya en curso" {
		t.Errorf("error: got %q want 'pipeline ya en curso'", msg)
	}
}

// TestCreateRun_ConflictRecentManifest cubre el caso "TUI corriendo en otra
// pestana": hay un manifest del slug en disco con status=running y
// started_at < 60s atras. El handler responde 409 ANTES de tomar el lock.
func TestCreateRun_ConflictRecentManifest(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "race-pipe", wizard.Pipeline{
		Name:  "race-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	// Manifest del run "ajeno" (TUI), iniciado hace 5s.
	now := time.Now().Add(-5 * time.Second)
	plantManifest(t, runsDir, "race-pipe", "tui-run", runner.Manifest{
		RunID:     "tui-run",
		Pipeline:  "race-pipe",
		StartedAt: now,
		Status:    runner.ManifestStatusRunning,
	})
	starter := &recordingStarter{}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "race-pipe", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(starter.calls) != 0 {
		t.Errorf("starter no debio invocarse; calls=%+v", starter.calls)
	}
}

// TestCreateRun_OldRunningManifestDoesNotBlock verifica que un manifest con
// status=running pero started_at > 60s atras NO bloquea el run nuevo —
// asumimos que es un huerfano (probable interrupcion previa).
func TestCreateRun_OldRunningManifestDoesNotBlock(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "stale-pipe", wizard.Pipeline{
		Name:  "stale-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	old := time.Now().Add(-10 * time.Minute)
	plantManifest(t, runsDir, "stale-pipe", "old-run", runner.Manifest{
		RunID: "old-run", Pipeline: "stale-pipe", StartedAt: old, Status: runner.ManifestStatusRunning,
	})
	starter := &recordingStarter{nextRunID: "RUN-fresh"}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	rr := postRun(t, dispatcher, "stale-pipe", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestCreateRun_StarterErrorReleasesLock cubre el error path: si el starter
// devuelve error, el handler responde 500 y el lock queda libre para
// reintentar.
func TestCreateRun_StarterErrorReleasesLock(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "boom-pipe", wizard.Pipeline{
		Name:  "boom-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	starter := &recordingStarter{err: errors.New("simulated")}
	lock := newRunLock()
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, lock)

	rr1 := postRun(t, dispatcher, "boom-pipe", nil)
	if rr1.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", rr1.Code, rr1.Body.String())
	}

	// El lock debe estar libre — segundo POST consigue arrancar (con un
	// starter feliz, intercambiamos el err por nil).
	starter.err = nil
	starter.nextRunID = "RUN-retry"
	rr2 := postRun(t, dispatcher, "boom-pipe", nil)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("retry: want 201, got %d body=%s", rr2.Code, rr2.Body.String())
	}
}

// TestCreateRun_LockReleasedOnExecuteDone verifica el AC del fix: una vez
// que Execute() termina (el starter invoca onDone), el lock por-slug queda
// libre y un POST posterior puede arrancar un run nuevo. Sin esto, el
// dash quedaba "atascado" en 409 hasta reiniciar el server.
func TestCreateRun_LockReleasedOnExecuteDone(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "loop-pipe", wizard.Pipeline{
		Name:  "loop-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	starter := &recordingStarter{nextRunID: "RUN-A"}
	lock := newRunLock()
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, lock)

	// Primer run arranca OK.
	rr1 := postRun(t, dispatcher, "loop-pipe", nil)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first run: want 201, got %d body=%s", rr1.Code, rr1.Body.String())
	}

	// Simula "Execute() termino" invocando el onDone que el handler le paso al starter.
	starter.mu.Lock()
	if len(starter.dones) != 1 || starter.dones[0] == nil {
		starter.mu.Unlock()
		t.Fatalf("expected starter to receive onDone callback, got dones=%v", starter.dones)
	}
	done := starter.dones[0]
	starter.mu.Unlock()
	done()

	// Lock liberado → segundo POST arranca, no devuelve 409.
	starter.nextRunID = "RUN-B"
	rr2 := postRun(t, dispatcher, "loop-pipe", nil)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second run after onDone: want 201, got %d body=%s", rr2.Code, rr2.Body.String())
	}
}

// TestCreateRun_InvalidJSON cubre el 400 cuando el body no parsea como JSON.
func TestCreateRun_InvalidJSON(t *testing.T) {
	pipelinesDir := t.TempDir()
	runsDir := t.TempDir()
	writeYAML(t, pipelinesDir, "any-pipe", wizard.Pipeline{
		Name:  "any-pipe",
		Steps: []wizard.Step{{Name: "single", CLI: "claude", Kind: "prompt", Input: wizard.InputNone}},
	})
	starter := &recordingStarter{}
	dispatcher := dispatchPipelinesPrefix(pipelinesDir, runsDir, NewBus(runsDir), starter, newRunLock())

	req := httptest.NewRequest(http.MethodPost, "/api/pipelines/any-pipe/runs", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	dispatcher(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}
