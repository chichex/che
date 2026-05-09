package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/wizard"
)

// TestResolveInput_TextPassthrough cubre el caso text: payload = texto crudo.
func TestResolveInput_TextPassthrough(t *testing.T) {
	got, err := resolveInput(wizard.InputText, "hola mundo")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "hola mundo" {
		t.Errorf("got %q, want %q", got, "hola mundo")
	}
}

// TestResolveInput_NoneEmpty cubre el caso none: payload vacio sin error.
func TestResolveInput_NoneEmpty(t *testing.T) {
	got, err := resolveInput(wizard.InputNone, "")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty payload, got %q", got)
	}
}

// TestResolveFile_Happy lee un archivo chico y devuelve el contenido.
func TestResolveFile_Happy(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "in.txt")
	if err := os.WriteFile(path, []byte("contenido fake"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := resolveInput(wizard.InputFile, path)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "contenido fake" {
		t.Errorf("got %q, want %q", got, "contenido fake")
	}
}

// TestResolveFile_Missing surface el error inline cuando el path no existe.
func TestResolveFile_Missing(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "no-existe.txt")
	_, err := resolveInput(wizard.InputFile, path)
	if err == nil {
		t.Fatalf("expected err for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "no existe") {
		t.Errorf("expected 'no existe' in error, got: %v", err)
	}
}

// TestResolveFile_IsDir rechaza paths que apuntan a un dir.
func TestResolveFile_IsDir(t *testing.T) {
	tmp := t.TempDir()
	_, err := resolveInput(wizard.InputFile, tmp)
	if err == nil {
		t.Fatalf("expected err for dir path, got nil")
	}
	if !strings.Contains(err.Error(), "es un dir") {
		t.Errorf("expected 'es un dir' in error, got: %v", err)
	}
}

// TestResolveFile_TooBig respeta CHE_MAX_INPUT_SIZE.
func TestResolveFile_TooBig(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "big.txt")
	if err := os.WriteFile(path, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CHE_MAX_INPUT_SIZE", "4")
	_, err := resolveInput(wizard.InputFile, path)
	if err == nil {
		t.Fatalf("expected err for too-big file, got nil")
	}
	if !strings.Contains(err.Error(), "demasiado grande") {
		t.Errorf("expected 'demasiado grande' in error, got: %v", err)
	}
}

// TestResolveURL_Happy hace GET contra un httptest server y devuelve el body.
func TestResolveURL_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body fake del fetch"))
	}))
	defer srv.Close()
	got, err := resolveInput(wizard.InputURL, srv.URL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "body fake del fetch" {
		t.Errorf("got %q, want %q", got, "body fake del fetch")
	}
}

// TestResolveURL_Non2xx falla cuando el server responde 404.
func TestResolveURL_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := resolveInput(wizard.InputURL, srv.URL)
	if err == nil {
		t.Fatalf("expected err for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected '404' in error, got: %v", err)
	}
}

// TestResolveURL_BadScheme rechaza URLs sin http/https.
func TestResolveURL_BadScheme(t *testing.T) {
	_, err := resolveInput(wizard.InputURL, "file:///etc/passwd")
	if err == nil {
		t.Fatalf("expected err for non-http scheme")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("expected scheme-related error, got: %v", err)
	}
}

// TestResolveGH_BadFormat rechaza refs sin owner/repo#NNN antes de spawnear gh.
func TestResolveGH_BadFormat(t *testing.T) {
	_, err := resolveInput(wizard.InputPR, "no-tiene-formato")
	if err == nil {
		t.Fatalf("expected err for bad ref")
	}
	if !strings.Contains(err.Error(), "owner/repo#NNN") {
		t.Errorf("expected format hint in error, got: %v", err)
	}
}

// TestResolveGH_Happy reemplaza ghCommand con un script que emite stdout
// previsible. Sirve para validar que el args parsing y el captura de stdout
// funcionan sin depender del binario real.
func TestResolveGH_Happy(t *testing.T) {
	saved := ghCommand
	t.Cleanup(func() { ghCommand = saved })
	ghCommand = func(ctx context.Context, args ...string) *exec.Cmd {
		// Validamos que pasemos los argumentos esperados a gh.
		want := []string{"issue", "view", "--repo", "chichex/che", "1", "--json", "title,body,comments"}
		if len(args) != len(want) {
			t.Errorf("ghCommand args len = %d, want %d (got=%v)", len(args), len(want), args)
		}
		for i := range want {
			if i < len(args) && args[i] != want[i] {
				t.Errorf("ghCommand args[%d] = %q, want %q", i, args[i], want[i])
			}
		}
		// echo del payload simulado.
		return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '{"title":"fake","body":"hola"}'`)
	}
	got, err := resolveInput(wizard.InputIssue, "chichex/che#1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(got, `"body":"hola"`) {
		t.Errorf("expected gh JSON payload in stdout; got %q", got)
	}
}

// TestResolveGH_Failure surface el stderr de gh cuando el exit code != 0.
func TestResolveGH_Failure(t *testing.T) {
	saved := ghCommand
	t.Cleanup(func() { ghCommand = saved })
	ghCommand = func(ctx context.Context, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", `echo "issue not found" 1>&2; exit 1`)
	}
	_, err := resolveInput(wizard.InputPR, "chichex/che#9999")
	if err == nil {
		t.Fatalf("expected err for gh exit 1")
	}
	if !strings.Contains(err.Error(), "issue not found") {
		t.Errorf("expected gh stderr surfaced in error, got: %v", err)
	}
}
