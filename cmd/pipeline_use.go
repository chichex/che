package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "marca un pipeline como default para el repo",
	Long: `use escribe ` + "`.che/pipelines.config.json`" + ` con el campo
` + "`default: <name>`" + ` (PRD §7.b). Después de correr ` + "`use`" + `, los flows
que no pasen --pipeline van a resolverse a este nombre.

Validaciones:
  - <name> debe existir en ` + "`.che/pipelines/`" + ` (excepto "default" que
    matchea el built-in implícito; ver ` + "`che pipeline new default`" + ` si
    querés materializarlo).
  - El pipeline tiene que parsear y validar — no permite settear un
    default roto.

` + "`use`" + ` es idempotente: si el config ya marca <name> como default,
no hace IO y devuelve OK.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		root, err := repoRootForPipeline()
		if err != nil {
			return err
		}
		mgr, err := pipeline.NewManager(root)
		if err != nil {
			return fmt.Errorf("init pipeline manager: %s", formatLoadError(err))
		}
		return runPipelineUse(cmd.OutOrStdout(), mgr, args[0])
	},
}

func init() {
	pipelineCmd.AddCommand(pipelineUseCmd)
}

// runPipelineUse aplica la jerarquía:
//  1. valida que el pipeline existe + parsea (excepto "default" implícito)
//  2. si el config ya marca <name> default → no-op + log
//  3. sino → escribe `.che/pipelines.config.json` con version + default
func runPipelineUse(out io.Writer, mgr *pipeline.Manager, name string) error {
	if name == "" {
		return fmt.Errorf("pipeline name no puede ser vacío")
	}

	// Caso especial: "default" sin archivo on-disk == built-in implícito.
	// Mantener el config con ese nombre es válido — Resolve() cae al
	// built-in cuando el archivo no existe.
	if _, ok := mgr.Path(name); !ok && name != "default" {
		return fmt.Errorf("pipeline %q no existe en %s — corré `che pipeline list` o `che pipeline new %s`",
			name, mgr.PipelinesDir(), name)
	}

	// Validar que el pipeline (si está on-disk) parsea + valida. No
	// queremos permitir `use` apuntando a un .json roto.
	if _, ok := mgr.Path(name); ok {
		if _, err := mgr.Get(name); err != nil {
			return fmt.Errorf("pipeline %q no es válido: %s", name, formatLoadError(err))
		}
	}

	if mgr.Config.Default == name {
		fmt.Fprintf(out, "default ya es %q (no-op)\n", name)
		return nil
	}

	cfg := pipeline.Config{
		Version: pipeline.ConfigVersion,
		Default: name,
	}
	if err := writeConfigFile(mgr.ConfigPath(), cfg); err != nil {
		return fmt.Errorf("escribir %s: %w", mgr.ConfigPath(), err)
	}
	fmt.Fprintf(out, "default = %q (escrito en %s)\n", name, mgr.ConfigPath())
	return nil
}

// writeConfigFile serializa cfg a JSON indentado y lo escribe en path.
// Crea el dir padre si falta — `.che/` puede no existir todavía cuando
// el repo nunca ejecutó che.
func writeConfigFile(path string, cfg pipeline.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
