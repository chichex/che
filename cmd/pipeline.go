package cmd

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

// pipelineCmd es el padre de los subcomandos `che pipeline *`. No tiene
// RunE: si el usuario tipea `che pipeline` sin argumentos, cobra muestra
// el help con la lista de subcomandos disponibles.
//
// Los subcomandos viven en archivos hermanos (`pipeline_list.go`,
// `pipeline_show.go`, etc.) y se enganchan via `pipelineCmd.AddCommand`
// en sus respectivos init().
var pipelineCmd = &cobra.Command{
	Use:   "pipeline",
	Short: "gestiona los pipelines del repo (list, show, use, new, clone, validate, simulate, init-labels, migrate-labels, reset)",
	Long: `pipeline agrupa los subcomandos para inspeccionar y operar sobre los
pipelines configurables de che (PRD §7).

Layout on-disk:
  .che/pipelines/<name>.json    archivos de pipeline
  .che/pipelines.config.json    declara qué pipeline es default

Subcomandos:
  list       lista los pipelines del repo + el default activo
  show       imprime un pipeline por nombre (JSON o resumen)
  use        marca un pipeline como default
  new        materializa un pipeline desde el built-in (template)
  clone      copia un pipeline aplicando sustituciones de strings
  validate   chequea sintaxis + referencias a agentes (registry)
  simulate   dry-run: muestra cómo se resolvería una corrida sin invocar LLM
  init-labels     crea labels derivados del pipeline activo
  migrate-labels  migra labels v1 a labels derivados del pipeline
  reset           limpia locks/applying huérfanos de una entity`,
}

func init() {
	rootCmd.AddCommand(pipelineCmd)
}

// repoRootForPipeline resuelve el repo root activo via `git rev-parse
// --show-toplevel`. Si falla (no hay git, cwd fuera de repo), devuelve
// un error con un hint útil.
//
// Centralizado acá para que los 7 subcomandos compartan la misma
// detección — si en un PR siguiente pasamos a `--repo-root` flag global,
// solo cambia esta función.
func repoRootForPipeline() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("no se pudo detectar el repo root (asegurate de estar dentro de un repo git): %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", errors.New("git rev-parse --show-toplevel devolvió vacío")
	}
	return root, nil
}

// formatLoadError devuelve la representación humana de un error de
// carga del paquete pipeline. Si es *pipeline.LoadError respeta los
// metadatos (path:line:column field); sino, fallback al .Error() crudo.
//
// Mantenerlo en un helper evita repetir el `errors.As` en cada
// subcomando que muestra errores de loader/validator.
func formatLoadError(err error) string {
	if err == nil {
		return ""
	}
	var le *pipeline.LoadError
	if errors.As(err, &le) {
		return le.Error()
	}
	return err.Error()
}
