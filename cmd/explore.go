package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/explore"
	"github.com/chichex/che/internal/output"
	"github.com/spf13/cobra"
)

var exploreAgentFlag string

var exploreCmd = &cobra.Command{
	Use:   "explore <issue-ref>",
	Short: "explora un issue del backlog: preguntas, riesgos, caminos posibles y plan consolidado",
	Long: `explore toma la referencia a un issue creado por 'che idea' (con label
ct:plan), lee su body, profundiza con el agente elegido y produce dos
artefactos:
  1. Un comentario en el issue con el análisis (paths, riesgos,
     preguntas abiertas, decisiones técnicas tomadas).
  2. Un "plan consolidado" que se escribe en el body del issue — es lo
     que después lee 'che execute' al arrancar.

Cuando termina, el issue transiciona de status:idea a status:plan.
explore NO dispara validadores ni pausa el flow para input humano: la
validación automática vive en 'che validate' como paso opt-in antes de
ejecutar.

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
			Out:    output.New(output.NewWriterSink(cmd.ErrOrStderr())),
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
