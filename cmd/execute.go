package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/chichex/che/internal/flow/execute"
	"github.com/chichex/che/internal/output"
	"github.com/spf13/cobra"
)

var executeAgentFlag string

var executeCmd = &cobra.Command{
	Use:   "execute <issue-ref>",
	Short: "ejecuta un issue en che:idea o che:plan: worktree aislado + PR draft contra main",
	Long: `execute toma la referencia a un issue (con ct:plan) en che:idea o
che:plan — explore es opcional. Si tiene plan consolidado en el body, lo
parsea (sección "## Plan consolidado"); si no, el agente improvisa desde
el body crudo. Abre un worktree aislado en .worktrees/issue-<N> sobre una
branch exec/<N>-<slug>, invoca al agente para aplicar el plan, commitea
los cambios, pushea la branch y abre/actualiza un PR draft contra main.
Al terminar, transiciona el issue a che:executed (pasando por che:executing
durante el run).

Gate: execute NO corre si el issue tiene plan-validated:changes-requested
o plan-validated:needs-human — esos labels los aplica 'che validate'
cuando el validador del plan pide cambios o escala al humano. Para
destrabar: resolvé a mano, o corré 'che validate' de nuevo para
re-validar. Con plan-validated:approve o sin ningún plan-validated:*,
execute pasa normal (la validación del plan es opt-in).

Formatos aceptados para <issue-ref>:
  che execute 42
  che execute https://github.com/owner/repo/issues/42
  che execute owner/repo#42

Agentes disponibles (--agent): opus (default, binario 'claude'), codex, gemini.

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
		// Signal handling propio: SIGINT/SIGTERM cancelan el ctx y execute
		// aplica cleanup síncrono (label rollback, worktree remove, branch
		// local). El exit code 130 distingue "cancelado por el usuario" del
		// exit 1/2/3 de error semántico o remediable.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		code := execute.Run(args[0], execute.Opts{
			Stdout: cmd.OutOrStdout(),
			Out:    output.New(output.NewWriterSink(cmd.ErrOrStderr())),
			Agent:  agent,
			Ctx:    ctx,
		})
		// Si la señal llegó antes de que Run marcara ExitCancelled
		// (p.ej. la TUI nunca entraría acá; este branch es para CLI y
		// coordina "cancelado a nivel proceso" → exit 130 aunque Run
		// haya retornado otro code por el orden del cleanup).
		if ctx.Err() != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(execute.ExitCancelled))
		}
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
	rootCmd.AddCommand(executeCmd)
}
