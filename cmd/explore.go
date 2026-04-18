package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/explore"
	"github.com/spf13/cobra"
)

var exploreAgentFlag string

var exploreCmd = &cobra.Command{
	Use:   "explore <issue-ref>",
	Short: "explora un issue del backlog: preguntas, riesgos, caminos posibles",
	Long: `explore toma la referencia a un issue creado por 'che idea' (con label
ct:plan), lee su body, y profundiza con el agente elegido para devolver
preguntas abiertas, riesgos y caminos de implementación posibles. El
análisis se postea como comentario en el issue y el label transiciona a
status:plan.

Formatos aceptados para <issue-ref>:
  che explore 42
  che explore https://github.com/owner/repo/issues/42
  che explore owner/repo#42

Agentes disponibles (--agent): opus (default, binario 'claude'), codex, gemini.

Este subcomando es la ruta no-interactiva (scripting/CI). La TUI de che
(invocable con 'che' sin args) usa el mismo flow por dentro.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agent, err := explore.ParseAgent(exploreAgentFlag)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid --agent: " + err.Error() + "\n")
			os.Exit(int(explore.ExitSemantic))
		}
		code := explore.Run(args[0], explore.Opts{
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
			Agent:  agent,
		})
		if code != explore.ExitOK {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	exploreCmd.Flags().StringVar(&exploreAgentFlag, "agent", string(explore.DefaultAgent),
		"ejecutor a usar: opus | codex | gemini")
	rootCmd.AddCommand(exploreCmd)
}
