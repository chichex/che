package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineListCmd = &cobra.Command{
	Use:   "list",
	Short: "lista los pipelines del repo + el default activo",
	Long: `list escanea ` + "`.che/pipelines/`" + ` y muestra los pipelines on-disk en
formato tabular (NAME, DEFAULT, PATH). La columna DEFAULT marca con
"*" el pipeline declarado en ` + "`.che/pipelines.config.json`" + `.

El built-in ` + "`default`" + ` (PRD §7.b fallback) NO se lista: es un fallback
implícito, no un archivo. Para materializarlo a disco usá
` + "`che pipeline new default`" + `.

Si el repo no tiene ningún pipeline on-disk y tampoco un default
declarado, ` + "`list`" + ` imprime un mensaje informativo en stderr y devuelve
exit 0 (no es un error — es un repo limpio).`,
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
		return runPipelineList(cmd.OutOrStdout(), cmd.ErrOrStderr(), mgr)
	},
}

func init() {
	pipelineCmd.AddCommand(pipelineListCmd)
}

// runPipelineList renderiza la tabla. Separado del RunE para poder
// testear sin spawnear `git rev-parse` ni tocar disco real (los tests
// arman un Manager con un tmpdir y le pasan ese a la función).
func runPipelineList(out, errOut io.Writer, mgr *pipeline.Manager) error {
	names := mgr.List()
	if len(names) == 0 {
		fmt.Fprintf(errOut, "no hay pipelines en %s\n", mgr.PipelinesDir())
		fmt.Fprintln(errOut, "tip: `che pipeline new default` materializa el built-in como template editable.")
		return nil
	}

	def := mgr.Config.Default
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEFAULT\tPATH")
	for _, n := range names {
		marker := ""
		if n == def {
			marker = "*"
		}
		path, _ := mgr.Path(n)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", n, marker, path)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Si el config declara un default que no existe on-disk, avisar:
	// es un estado inconsistente que el usuario probablemente quiere
	// arreglar (typo, archivo borrado).
	if def != "" {
		if _, ok := mgr.Path(def); !ok {
			fmt.Fprintf(errOut, "warn: default %q declarado en %s no existe en %s\n",
				def, mgr.ConfigPath(), mgr.PipelinesDir())
		}
	}
	return nil
}
