package cmd

import (
	"fmt"
	"os"

	"github.com/chichex/che/internal/runner"
	"github.com/chichex/che/internal/tui"
	"github.com/chichex/che/internal/wizard"
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
		// Loop: las pantallas que devuelven el control "hacia atras" (p.ej.
		// esc desde el listado de skills) re-entran al menu principal. Las
		// que piden exit total (q/ctrl+c) cortan el loop.
		for {
			action, err := tui.Run()
			if err != nil {
				return err
			}
			switch action {
			case tui.ActionExit:
				return nil
			case tui.ActionSeeSkills:
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				exit, err := tui.RunSkills(cwd)
				if err != nil {
					return err
				}
				if exit {
					return nil
				}
			case tui.ActionCreatePipeline:
				exit, err := wizard.Run()
				if err != nil {
					return err
				}
				if exit {
					return nil
				}
			case tui.ActionAIGen:
				exit, err := tui.RunAIGen()
				if err != nil {
					return err
				}
				if exit {
					return nil
				}
			case tui.ActionMyPipelines:
				if err := runMyPipelines(); err != nil {
					return err
				}
			default:
				fmt.Fprintln(cmd.ErrOrStderr(), "unknown action:", action)
				os.Exit(1)
				return nil
			}
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

// runMyPipelines orquesta el lister H9/H10: levanta la pantalla, y segun
// la accion del usuario abre el wizard reanudando un draft o re-editando un
// ready, despues vuelve al lister hasta que el usuario hace esc/q. Asi el
// flujo "abrir → cancelar → ver lista actualizada" no necesita pasar por el
// menu principal cada vez.
func runMyPipelines() error {
	// H8 recovery: al boot del lister, reescribir a `interrupted` los
	// manifests con status:running y started_at > 1h. Best-effort — un
	// error en la recovery NO debe frenar el lister (el usuario sigue
	// pudiendo ver / borrar / re-correr pipelines).
	_ = runner.RecoverInterruptedRuns("")
	for {
		action, target, exitApp, err := wizard.RunList()
		if err != nil {
			return err
		}
		switch action {
		case wizard.ListActionExit:
			if exitApp {
				os.Exit(0)
			}
			return nil
		case wizard.ListActionNone:
			return nil
		case wizard.ListActionResume:
			exit, err := wizard.RunResume(target)
			if err != nil {
				return err
			}
			if exit {
				os.Exit(0)
			}
		case wizard.ListActionEditReady:
			exit, err := wizard.RunEditReady(target)
			if err != nil {
				return err
			}
			if exit {
				os.Exit(0)
			}
		case wizard.ListActionRun:
			// H1 del flow de runner: ejecutar un pipeline ready abre el
			// runner skeleton. Tras esc volvemos al lister; tras q salida
			// total. Errores de load/IsValid se imprimen y volvemos al
			// lister — H2+ hara el toast inline; H1 deja el surface al
			// caller para no inflar el contrato.
			exit, err := runner.Run(target)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			if exit {
				os.Exit(0)
			}
		default:
			return nil
		}
	}
}
