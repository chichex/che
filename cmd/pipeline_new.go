package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineNewForce bool

var pipelineNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "materializa un pipeline a disco a partir del built-in",
	Long: `new escribe ` + "`.che/pipelines/<name>.json`" + ` con el contenido del
built-in ` + "`pipeline.Default()`" + ` (PRD §7.b). Es la forma rápida de
arrancar un pipeline custom: tomás el shape canónico, lo guardás bajo un
nombre nuevo y lo editás.

Por seguridad ` + "`new`" + ` falla si el archivo ya existe. Para sobrescribir
pasá --force.`,
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
		return runPipelineNew(cmd.OutOrStdout(), mgr, args[0], pipelineNewForce)
	},
}

func init() {
	pipelineNewCmd.Flags().BoolVar(&pipelineNewForce, "force", false,
		"sobrescribe el archivo si ya existe")
	pipelineCmd.AddCommand(pipelineNewCmd)
}

// runPipelineNew valida el nombre, materializa el built-in y reporta.
func runPipelineNew(out io.Writer, mgr *pipeline.Manager, name string, force bool) error {
	if name == "" {
		return fmt.Errorf("pipeline name no puede ser vacío")
	}
	dest := filepath.Join(mgr.PipelinesDir(), name+".json")
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s ya existe — pasá --force para sobrescribir", dest)
		}
	}
	if err := savePipelineFile(dest, pipeline.Default()); err != nil {
		return fmt.Errorf("escribir %s: %w", dest, err)
	}
	fmt.Fprintf(out, "creado %s\n", dest)
	return nil
}
