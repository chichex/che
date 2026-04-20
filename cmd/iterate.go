package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/iterate"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/spf13/cobra"
)

var iterateCmd = &cobra.Command{
	Use:   "iterate <pr-ref>",
	Short: "aplica los findings de che validate con opus sobre un PR con validated:changes-requested",
	Long: `iterate toma un PR que tiene verdict validated:changes-requested y le
aplica los cambios que pidieron los validadores:

  1. Lee los comments del último run de che validate (todos los findings).
  2. Resuelve el worktree del PR (reusa ` + "`.worktrees/issue-<N>`" + ` si lo
     creó che execute, o crea uno nuevo sobre la head branch).
  3. Invoca a opus (claude) con los findings y el contexto del PR.
  4. Si opus dejó commits, los pushea, postea un comment flow=iterate y
     remueve el label validated:changes-requested para dejar el PR listo
     para una nueva validación.
  5. Si opus no produjo cambios reales, sale con exit 2 y deja todo como
     estaba — no miente sobre una iteración que no ocurrió.

iterate NO gatea por estado del PR (abierto/draft/etc) más allá de lo
mínimo; el humano pide iterar y che obedece. Tampoco requiere que el PR
tenga el label validated:changes-requested — si no hay findings de
validate en los comments, sale con exit 3 porque no hay nada que iterar.

Formatos aceptados para <pr-ref>:
  che iterate 7
  che iterate https://github.com/owner/repo/pull/7
  che iterate owner/repo#7

No hay flag --agent: iterate usa opus (claude) por diseño.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := validate.ParsePRRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid pr ref: " + err.Error() + "\n")
			os.Exit(int(iterate.ExitSemantic))
		}
		code := iterate.Run(args[0], iterate.Opts{
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
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
