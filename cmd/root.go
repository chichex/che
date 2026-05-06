package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "che",
	Short:   "che - CLI en revamp",
	Long:    `che — CLI en revamp. Subcomandos disponibles: dash, doctor, upgrade.`,
	Version: Version,
}

func init() {
	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
