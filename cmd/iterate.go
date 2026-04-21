package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/iterate"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/output"
	"github.com/spf13/cobra"
)

var iterateCmd = &cobra.Command{
	Use:   "iterate <ref>",
	Short: "aplica findings de che validate sobre un plan (issue) o PR",
	Long: `iterate aplica los findings del último run de che validate al ref dado.
Detecta si <ref> es un issue o un PR y despacha en consecuencia:

Modo plan (issue con plan-validated:changes-requested):
  1. Lee los comments del último run de che validate en el issue.
  2. Invoca a opus para reescribir el plan consolidado aplicando los findings.
  3. Reemplaza el body del issue con el plan iterado (sin tocar git ni worktree).
  4. Postea un comment flow=iterate y remueve plan-validated:changes-requested.

Modo PR (PR con validated:changes-requested):
  1. Lee los comments del último run de che validate en el PR.
  2. Resuelve el worktree del PR (reusa .worktrees/issue-<N> si existe).
  3. Invoca a opus con los findings y el contexto del PR.
  4. Si opus dejó commits, los pushea, postea flow=iterate y remueve el label.
  5. Si no produjo cambios reales, sale con exit 2 y deja todo como estaba.

Ejemplos:
  che iterate 42    # issue con plan-validated:changes-requested → edita plan consolidado
  che iterate 7     # PR con validated:changes-requested → commits en la branch

Formatos aceptados para <ref>:
  che iterate 7
  che iterate https://github.com/owner/repo/pull/7
  che iterate owner/repo#7

No hay flag --agent: iterate usa opus (claude) por diseño.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := validate.ParseRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid ref: " + err.Error() + "\n")
			os.Exit(int(iterate.ExitSemantic))
		}
		code := iterate.Run(args[0], iterate.Opts{
			Stdout: cmd.OutOrStdout(),
			Out:    output.New(output.NewWriterSink(cmd.ErrOrStderr())),
		})
		if code != iterate.ExitOK {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(iterateCmd)
}
