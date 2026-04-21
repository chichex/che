package cmd

import (
	"os"

	closing "github.com/chichex/che/internal/flow/close"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/spf13/cobra"
)

var closeKeepBranch bool

var closeCmd = &cobra.Command{
	Use:   "close <pr-ref>",
	Short: "cierra un PR: lo pasa a ready si está draft, arregla conflictos/CI con opus, mergea y cierra el issue",
	Long: `close toma un PR abierto (draft o ready) y lo lleva hasta merge:

  1. Si está draft, lo pasa a ready-for-review.
  2. Chequea conflictos con main y estado de CI.
  3. Si hay problemas, invoca a opus (claude) en el worktree del PR
     (reusa ` + "`.worktrees/issue-<N>`" + ` si lo creó che execute, o crea uno
     nuevo sobre la branch del PR) para resolverlos: el agente hace
     commit+push y close espera a que CI re-evalúe.
  4. Repite hasta 3 intentos totales. Si persisten problemas, sale con
     exit 2 (retry) sin mergear.
  5. Si todo verde, mergea con merge commit (gh pr merge --merge) y borra
     la branch remota + local (--delete-branch). El worktree asociado se
     remueve para dejar el repo limpio. Usá --keep-branch para opt-out.
  6. Cierra los issues asociados vía "Closes #N" del PR y les aplica la
     transición de labels status:executed → status:closed.

close REFUSE mergear si el PR tiene label validated:changes-requested o
validated:needs-human (verdicts bloqueantes de che validate): exit 3.

Formatos aceptados para <pr-ref>:
  che close 7
  che close https://github.com/owner/repo/pull/7
  che close owner/repo#7

No hay flag --agent: close usa opus (claude) por diseño.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := validate.ParsePRRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid pr ref: " + err.Error() + "\n")
			os.Exit(int(closing.ExitSemantic))
		}
		code := closing.Run(args[0], closing.Opts{
			Stdout:     cmd.OutOrStdout(),
			Stderr:     cmd.ErrOrStderr(),
			KeepBranch: closeKeepBranch,
		})
		if code != closing.ExitOK {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	closeCmd.Flags().BoolVar(&closeKeepBranch, "keep-branch", false,
		"preservar la branch remota/local y el worktree tras el merge (default: eliminar)")
	rootCmd.AddCommand(closeCmd)
}
