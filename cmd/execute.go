package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/execute"
	"github.com/spf13/cobra"
)

var (
	executeAgentFlag      string
	executeValidatorsFlag string
)

var executeCmd = &cobra.Command{
	Use:   "execute <issue-ref>",
	Short: "ejecuta un issue en status:plan: worktree aislado + PR draft + validadores disparados",
	Long: `execute toma la referencia a un issue ya explorado por 'che explore'
(en status:plan, con ct:plan), parsea la sección "## Plan consolidado" de
su body, abre un worktree aislado en .worktrees/issue-<N> sobre una branch
exec/<N>-<slug>, invoca al agente para aplicar el plan, commitea los
cambios, pushea la branch y abre/actualiza un PR draft contra main. Al
terminar dispara los validadores (sin esperar) y transiciona el issue a
status:executed + awaiting-human.

Formatos aceptados para <issue-ref>:
  che execute 42
  che execute https://github.com/owner/repo/issues/42
  che execute owner/repo#42

Agentes disponibles (--agent): opus (default, binario 'claude'), codex, gemini.
Validadores (--validators): 1-3 separados por coma, pueden repetir tipo
(ej: 'codex,gemini' o 'codex,codex,gemini'). Usar 'none' para no
dispararlos (útil para tests/CI).

Este subcomando es la ruta no-interactiva (scripting/CI). La TUI de che
(invocable con 'che' sin args) usa el mismo flow por dentro.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		agent, err := execute.ParseAgent(executeAgentFlag)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid --agent: " + err.Error() + "\n")
			os.Exit(int(execute.ExitSemantic))
		}
		validators, err := execute.ParseValidators(executeValidatorsFlag)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid --validators: " + err.Error() + "\n")
			os.Exit(int(execute.ExitSemantic))
		}
		code := execute.Run(args[0], execute.Opts{
			Stdout:     cmd.OutOrStdout(),
			Stderr:     cmd.ErrOrStderr(),
			Agent:      agent,
			Validators: validators,
		})
		if code != execute.ExitOK {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	executeCmd.Flags().StringVar(&executeAgentFlag, "agent", string(execute.DefaultAgent),
		"ejecutor a usar: opus | codex | gemini")
	executeCmd.Flags().StringVar(&executeValidatorsFlag, "validators", "none",
		"1-3 validadores separados por coma (opus|codex|gemini), o 'none' para no disparar")
	rootCmd.AddCommand(executeCmd)
}
