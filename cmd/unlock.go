package cmd

import (
	"fmt"
	"os"

	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
	"github.com/spf13/cobra"
)

var unlockCmd = &cobra.Command{
	Use:   "unlock <ref>",
	Short: "quita el label che:locked de un issue o PR (escape hatch si un flow quedó colgado)",
	Long: `unlock saca el label che:locked de un issue o PR. Es el escape hatch cuando
un flow de che murió sucio (crash, SIGKILL) y dejó el ref marcado como
ocupado — mientras el label esté aplicado, ninguna otra corrida de che
puede tomarlo.

Idempotente: si el ref no tiene el label, no falla.

Formatos aceptados para <ref>:
  che unlock 42
  che unlock https://github.com/owner/repo/issues/42
  che unlock https://github.com/owner/repo/pull/7
  che unlock owner/repo#42`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ref, err := validate.ParseRef(args[0])
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			fmt.Fprintln(os.Stderr, "error: invalid ref:", err)
			os.Exit(3)
		}
		if err := labels.Unlock(ref); err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Unlocked %s\n", ref)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(unlockCmd)
}
