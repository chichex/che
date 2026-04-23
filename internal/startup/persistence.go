// Package startup implementa los chequeos secundarios que la TUI corre
// antes de mostrar el menú principal: labels viejos sin migrar, versión
// desactualizada y locks colgados.
//
// El paquete es "secundario" en el sentido de que jamás debe romper la
// TUI: cualquier fallo (gh sin red, sin auth, repo no inicializado) se
// degrada a "este check no triggerea" en silencio. La TUI sigue siendo
// usable aunque los chequeos exploten.
package startup

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// skipFileName es el nombre del archivo que persiste qué chequeos se
// skipean para siempre en este repo. Vive en `.git/` para quedar fuera
// del tracking sin necesidad de tocar `.gitignore`.
const skipFileName = "che-skip-checks"

// IsSkipped devuelve true si el chequeo `checkName` está marcado como
// "nunca para este repo" en el archivo `.git/che-skip-checks` del repo
// dado. `repoRoot` es el path absoluto del working dir del repo (donde
// vive `.git/`); si no existe `.git/` o el archivo, devuelve false sin
// error — no encontramos persistencia, asumimos que no está skipeado.
func IsSkipped(repoRoot, checkName string) bool {
	if repoRoot == "" || checkName == "" {
		return false
	}
	path := skipFilePath(repoRoot)
	f, err := os.Open(path)
	if err != nil {
		// Sin archivo (o sin permisos) → no skipeado. No queremos romper
		// la TUI por un check secundario.
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == checkName {
			return true
		}
	}
	return false
}

// MarkSkipped agrega `checkName` al archivo `.git/che-skip-checks` del
// repo. Idempotente: si el chequeo ya está, no duplica la línea. Si el
// archivo no existe, lo crea. Si `.git/` no existe (estamos fuera de un
// repo git) devuelve un error explícito — el caller debería no haber
// llegado hasta acá.
func MarkSkipped(repoRoot, checkName string) error {
	if repoRoot == "" {
		return errors.New("repoRoot vacío")
	}
	if checkName == "" {
		return errors.New("checkName vacío")
	}
	gitDir := filepath.Join(repoRoot, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return errors.New(".git no existe en " + repoRoot)
	}

	// Si ya está, no-op.
	if IsSkipped(repoRoot, checkName) {
		return nil
	}

	path := skipFilePath(repoRoot)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.WriteString(checkName + "\n"); err != nil {
		return err
	}
	return nil
}

// skipFilePath devuelve el path absoluto del archivo de persistencia
// (`<repoRoot>/.git/che-skip-checks`).
func skipFilePath(repoRoot string) string {
	return filepath.Join(repoRoot, ".git", skipFileName)
}

// HasGitDir devuelve true si `repoRoot` contiene un directorio `.git/`.
// Sirve como guard para los callers (ej. la TUI) que necesitan saber si
// estamos dentro de un repo git antes de correr chequeos. Tolerante:
// devuelve false en cualquier error de stat (sin permisos, etc.).
func HasGitDir(repoRoot string) bool {
	if repoRoot == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(repoRoot, ".git"))
	if err != nil {
		return false
	}
	return info.IsDir()
}
