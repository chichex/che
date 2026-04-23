package startup

import (
	"os"
	"path/filepath"
	"testing"
)

// makeRepo crea un tmpdir con un `.git/` adentro y devuelve el path.
// Útil para los tests que necesitan un "repo válido".
func makeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatalf("creando .git: %v", err)
	}
	return dir
}

func TestIsSkipped_ArchivoNoExiste(t *testing.T) {
	repo := makeRepo(t)
	if IsSkipped(repo, "version") {
		t.Errorf("archivo inexistente no debería marcar nada skipeado")
	}
}

func TestIsSkipped_ChequeoMarcado(t *testing.T) {
	repo := makeRepo(t)
	if err := MarkSkipped(repo, "version"); err != nil {
		t.Fatalf("MarkSkipped: %v", err)
	}
	if !IsSkipped(repo, "version") {
		t.Errorf("version debería aparecer skipeado")
	}
	if IsSkipped(repo, "locks") {
		t.Errorf("locks no debería estar skipeado todavía")
	}
}

func TestMarkSkipped_Idempotente(t *testing.T) {
	repo := makeRepo(t)
	if err := MarkSkipped(repo, "version"); err != nil {
		t.Fatalf("MarkSkipped 1: %v", err)
	}
	if err := MarkSkipped(repo, "version"); err != nil {
		t.Fatalf("MarkSkipped 2: %v", err)
	}
	// Lectura debe seguir devolviendo true (no se duplicó la línea de
	// forma que rompa el parser).
	if !IsSkipped(repo, "version") {
		t.Errorf("debería seguir skipeado tras dos MarkSkipped")
	}
	// Verificamos que el archivo solo tiene una línea relevante.
	data, err := os.ReadFile(filepath.Join(repo, ".git", skipFileName))
	if err != nil {
		t.Fatalf("leyendo archivo: %v", err)
	}
	count := 0
	for _, line := range splitLines(string(data)) {
		if line == "version" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("la línea 'version' debería aparecer 1 vez, got %d", count)
	}
}

func TestMarkSkipped_VariosChequeos(t *testing.T) {
	repo := makeRepo(t)
	if err := MarkSkipped(repo, "migrate-labels"); err != nil {
		t.Fatalf("mark migrate: %v", err)
	}
	if err := MarkSkipped(repo, "version"); err != nil {
		t.Fatalf("mark version: %v", err)
	}
	if !IsSkipped(repo, "migrate-labels") || !IsSkipped(repo, "version") {
		t.Errorf("ambos deberían estar skipeados")
	}
	if IsSkipped(repo, "locks") {
		t.Errorf("locks no debería estar skipeado")
	}
}

func TestMarkSkipped_SinGit(t *testing.T) {
	dir := t.TempDir() // sin `.git`
	err := MarkSkipped(dir, "version")
	if err == nil {
		t.Errorf("MarkSkipped sin .git debería devolver error")
	}
}

func TestIsSkipped_ToleraComentariosYBlancos(t *testing.T) {
	repo := makeRepo(t)
	path := filepath.Join(repo, ".git", skipFileName)
	body := "# comentario\n\nversion\n  \nlocks\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !IsSkipped(repo, "version") {
		t.Errorf("version debería estar skipeado")
	}
	if !IsSkipped(repo, "locks") {
		t.Errorf("locks debería estar skipeado")
	}
	if IsSkipped(repo, "comentario") {
		t.Errorf("comentario no debería contar como skip")
	}
}

func TestHasGitDir(t *testing.T) {
	repo := makeRepo(t)
	if !HasGitDir(repo) {
		t.Errorf("repo con .git debería devolver true")
	}
	if HasGitDir(t.TempDir()) {
		t.Errorf("dir sin .git debería devolver false")
	}
	if HasGitDir("") {
		t.Errorf("path vacío debería devolver false")
	}
}

// splitLines parte un string por '\n' sin importar trailing newline.
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
