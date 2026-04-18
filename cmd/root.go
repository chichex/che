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
	Short:   "che - workflow estandarizado para trabajar con agentes de IA sobre issues de GitHub",
	Long:    `che estandariza el embudo idea → explore → execute → close para reducir el miss rate de agentes de IA, usando GitHub como único estado.`,
	Version: Version,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Sin args → abre la TUI interactiva.
		return tui.Run(Version)
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
