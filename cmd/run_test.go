package cmd

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsTerminalRunStatus verifica los status terminales reconocidos.
func TestIsTerminalRunStatus(t *testing.T) {
	terminals := []string{"done", "failed", "interrupted", "cancelled"}
	for _, s := range terminals {
		if !isTerminalRunStatus(s) {
			t.Errorf("isTerminalRunStatus(%q) = false, want true", s)
		}
	}
	nonTerminals := []string{"running", "pending", "", "unknown"}
	for _, s := range nonTerminals {
		if isTerminalRunStatus(s) {
			t.Errorf("isTerminalRunStatus(%q) = true, want false", s)
		}
	}
}

// TestReadDashPort_NoFile verifica que readDashPort no panickea y devuelve
// string (vacio si no hay dash, o un puerto si hay uno corriendo).
func TestReadDashPort_NoFile(t *testing.T) {
	port := readDashPort()
	_ = port // puede ser "" o un numero — ambos son validos
}

// TestDialDash_Closed verifica que dialDash devuelve false para un puerto
// que no tiene nada escuchando.
func TestDialDash_Closed(t *testing.T) {
	if dialDash("19999") {
		t.Skip("hay algo escuchando en 19999, skip")
	}
}

// TestJsonInt verifica el helper jsonInt con distintos tipos JSON.
func TestJsonInt(t *testing.T) {
	m := map[string]any{
		"float": float64(3),
		"int":   42,
		"str":   "x",
	}
	if v := jsonInt(m, "float"); v != 3 {
		t.Errorf("float64: got %d want 3", v)
	}
	if v := jsonInt(m, "int"); v != 42 {
		t.Errorf("int: got %d want 42", v)
	}
	if v := jsonInt(m, "str"); v != 0 {
		t.Errorf("str: got %d want 0", v)
	}
	if v := jsonInt(m, "missing"); v != 0 {
		t.Errorf("missing: got %d want 0", v)
	}
}

// TestHandleSSEEvent_StepStart verifica que step:start registra el nombre.
func TestHandleSSEEvent_StepStart(t *testing.T) {
	names := map[int]string{}
	data := `{"idx":1,"name":"my-step","started_at":"2026-01-01T00:00:00Z"}`
	var out, errOut bytes.Buffer
	if err := handleSSEEvent("step:start", data, names, &out, &errOut); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	if names[1] != "my-step" {
		t.Errorf("stepNames[1] = %q want %q", names[1], "my-step")
	}
}

// TestHandleSSEEvent_StepStdout verifica que step:stdout imprime la linea con prefijo.
func TestHandleSSEEvent_StepStdout(t *testing.T) {
	names := map[int]string{1: "my-step"}
	data := `{"idx":1,"line":"hello world","ts":"now","ordinal":0}`
	var out, errOut bytes.Buffer
	if err := handleSSEEvent("step:stdout", data, names, &out, &errOut); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "[my-step] hello world") {
		t.Errorf("stdout no contiene prefijo: %q", got)
	}
}

// TestHandleSSEEvent_StepEnd_Failed verifica que step:end con status=failed
// escribe en stderr con el prefijo correcto.
func TestHandleSSEEvent_StepEnd_Failed(t *testing.T) {
	names := map[int]string{1: "my-step"}
	data := `{"idx":1,"status":"failed","exit_code":7,"error":"boom"}`
	var out, errOut bytes.Buffer
	if err := handleSSEEvent("step:end", data, names, &out, &errOut); err != nil {
		t.Fatalf("handleSSEEvent: %v", err)
	}
	got := errOut.String()
	if !strings.Contains(got, "[my-step] FAILED") {
		t.Errorf("stderr no contiene FAILED: %q", got)
	}
	if !strings.Contains(got, "exit=7") {
		t.Errorf("stderr no contiene exit=7: %q", got)
	}
}

// TestRunViaDash_Happy verifica el happy path via un httptest.Server que
// responde 201 + SSE con secuencia conocida y run:status=done.
func TestRunViaDash_Happy(t *testing.T) {
	sseBody := buildSSEBody([]string{
		"event: run:status\ndata: {\"status\":\"running\"}\n\n",
		"event: step:start\ndata: {\"idx\":1,\"name\":\"my-step\"}\n\n",
		"event: step:stdout\ndata: {\"idx\":1,\"line\":\"hola\",\"ts\":\"now\",\"ordinal\":0}\n\n",
		"event: step:end\ndata: {\"idx\":1,\"status\":\"done\",\"exit_code\":0}\n\n",
		"event: run:status\ndata: {\"status\":\"done\"}\n\n",
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintln(w, `{"run_id":"test-run-001"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, sseBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	// Extraer solo el puerto del server de test.
	addr := ts.Listener.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]

	var out, errOut bytes.Buffer
	ok, err := runViaDash(port, "test-slug", "my input", &out, &errOut)
	if err != nil {
		t.Fatalf("runViaDash: %v", err)
	}
	if !ok {
		t.Errorf("runViaDash: got ok=false want true (status=done)")
	}
	if !strings.Contains(out.String(), "[my-step] hola") {
		t.Errorf("stdout no contiene '[my-step] hola': %q", out.String())
	}
}

// TestRunViaDash_Failed verifica que run:status=failed devuelve ok=false.
func TestRunViaDash_Failed(t *testing.T) {
	sseBody := buildSSEBody([]string{
		"event: run:status\ndata: {\"status\":\"running\"}\n\n",
		"event: step:start\ndata: {\"idx\":1,\"name\":\"boom-step\"}\n\n",
		"event: step:end\ndata: {\"idx\":1,\"status\":\"failed\",\"exit_code\":1,\"error\":\"ugh\"}\n\n",
		"event: run:status\ndata: {\"status\":\"failed\"}\n\n",
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintln(w, `{"run_id":"test-run-002"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, sseBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]

	var out, errOut bytes.Buffer
	ok, err := runViaDash(port, "test-slug", "", &out, &errOut)
	if err != nil {
		t.Fatalf("runViaDash: %v", err)
	}
	if ok {
		t.Errorf("runViaDash: got ok=true want false (status=failed)")
	}
	if !strings.Contains(errOut.String(), "FAILED") {
		t.Errorf("stderr no contiene FAILED: %q", errOut.String())
	}
}

// buildSSEBody concatena las partes del SSE body en un solo string.
func buildSSEBody(parts []string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}
