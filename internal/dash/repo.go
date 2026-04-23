package dash

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ValidateRepo confirma que dir es un repo git válido (i.e. git rev-parse
// --git-dir no falla). Devuelve un error claro si no lo es.
//
// Si dir está vacío, valida el working directory actual del proceso.
func ValidateRepo(dir string) error {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		target := dir
		if target == "" {
			target = "."
		}
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("%s is not a git repository: %s", target, detail)
	}
	return nil
}

// RepoName devuelve un nombre corto para mostrar en la UI. Usa el toplevel
// del repo si está disponible; si falla, cae al basename de dir; si dir está
// vacío, devuelve "repo" como último recurso.
func RepoName(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.Output(); err == nil {
		top := strings.TrimSpace(string(out))
		if top != "" {
			return filepath.Base(top)
		}
	}
	if dir != "" {
		return filepath.Base(dir)
	}
	return "repo"
}
