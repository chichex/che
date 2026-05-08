package wizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// pipelinesDirName es el subdir de HOME donde viven los pipelines (drafts
// y ready). Permisos 0700 — info personal del usuario.
const pipelinesDirName = ".che/pipelines"

// PipelinesDir devuelve el directorio absoluto donde se guardan los
// pipelines del usuario. Si home == "", usa $HOME / os.UserHomeDir.
func PipelinesDir(home string) (string, error) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("wizard: home dir: %w", err)
		}
		home = h
	}
	return filepath.Join(home, pipelinesDirName), nil
}

// PathFor devuelve el path completo (incluida extension .yaml) del archivo
// del pipeline cuyo slug es slug, asumiendo HOME = home.
func PathFor(home, slug string) (string, error) {
	dir, err := PipelinesDir(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, slug+".yaml"), nil
}

// EnsureDir crea ~/.che/pipelines si no existe (con permisos 0700). No es
// error si ya existe.
func EnsureDir(home string) (string, error) {
	dir, err := PipelinesDir(home)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("wizard: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// Save serializa el pipeline al path indicado de forma atomica: escribe
// a <path>.tmp y hace os.Rename. Permisos del archivo final: 0600.
//
// Si el dir padre no existe, lo crea con 0700. Si el rename falla, el
// archivo .tmp se intenta borrar en best-effort.
func Save(path string, p Pipeline) error {
	if path == "" {
		return errors.New("wizard: save with empty path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("wizard: mkdir %s: %w", dir, err)
	}

	data, err := Marshal(p)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("wizard: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("wizard: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Load lee y parsea un pipeline del path indicado. Errores os.IsNotExist
// se propagan como estan para que el caller los distinga.
func Load(path string) (Pipeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pipeline{}, err
	}
	return Unmarshal(data)
}

// Delete borra el archivo del pipeline. No es error si no existe (el
// caller no necesita chequear antes — discard sobre un draft que aun no
// llego a S1 setname es valido).
func Delete(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wizard: remove %s: %w", path, err)
	}
	return nil
}

// Exists devuelve true si el archivo del path existe (regular file).
// Errores distintos a os.IsNotExist se propagan.
func Exists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
