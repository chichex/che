package cmd

import (
	"fmt"
	"os"

	"github.com/chichex/che/internal/tui"
	"github.com/spf13/cobra"
)

var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "che",
	Short:   "che - CLI en revamp",
	Long:    `che — CLI en revamp. Sin args abre el menu TUI; subcomandos disponibles: doctor, upgrade.`,
	Version: Version,
	RunE: func(cmd *cobra.Command, args []string) error {
		// SilenceUsage: el menu no es un error de invocacion. Si algo sale
		// mal, devolvemos el error directo sin imprimir el help completo.
		cmd.SilenceUsage = true
		action, err := tui.Run()
		if err != nil {
			return err
		}
		switch action {
		case tui.ActionExit:
			return nil
		default:
			fmt.Fprintln(cmd.ErrOrStderr(), "not implemented")
			os.Exit(1)
			return nil
		}
	},
}

func init() {
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	// Execute() ya imprime el error y hace os.Exit(1). Sin esto, cobra
	// tambien imprime "Error: ..." y queda duplicado.
	rootCmd.SilenceErrors = true
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
