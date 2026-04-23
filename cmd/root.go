package cmd

import (
	"fmt"
	"os"

	"github.com/chichex/che/internal/tui"
	"github.com/spf13/cobra"
)

var Version = "dev"

// noChecks deshabilita los chequeos secundarios del arranque de la TUI
// (labels viejos / versión / locks colgados). Lo expone --no-checks
// en root para que el usuario pueda saltearlos manualmente cuando no
// quiere ruido o sabe que gh va a fallar (offline, sin auth, etc.).
var noChecks bool

var rootCmd = &cobra.Command{
	Use:     "che",
	Short:   "che - workflow estandarizado para trabajar con agentes de IA sobre issues de GitHub",
	Long:    `che estandariza el embudo idea → explore → execute → close para reducir el miss rate de agentes de IA, usando GitHub como único estado.`,
	Version: Version,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Sin args → abre la TUI interactiva. --no-checks deshabilita
		// los chequeos del arranque.
		return tui.Run(Version, !noChecks)
	},
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&noChecks, "no-checks", false,
		"deshabilita los chequeos del arranque de la TUI (labels viejos / versión / locks)")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
