package cmd

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/chichex/che/internal/dash"
	"github.com/spf13/cobra"
)

var (
	dashPort   int
	dashRepo   string
	dashNoOpen bool
	dashPoll   int
	dashMock   bool
)

var dashCmd = &cobra.Command{
	Use:   "dash",
	Short: "arranca un dashboard web local con Kanban por status",
	Long: `dash levanta un HTTP server local en http://localhost:<port> que sirve
un dashboard Kanban por status (backlog, exploring, plan, executing,
validating, approved) + drawer derecho con metadata + logs.

Step 3: el board se puebla con issues/PRs reales del repo via ` + "`gh`" + `
cada --poll segundos. Con --mock se usan fixtures hardcoded (útil para demo
o para correr sin gh). Las acciones del drawer siguen inertes (step 4).

Flags:
  --port    puerto local (default 7777)
  --repo    path al repo git (default: cwd)
  --no-open no abrir el browser automáticamente
  --poll    segundos entre polls a gh (default 15)
  --mock    usar datos mock en vez de gh (default false)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		// Cualquier fallo aquí es un runtime error (repo inválido, puerto
		// ocupado, gh no auth, etc.), no un problema de flags — no tiene
		// sentido imprimir el Usage de cobra.
		cmd.SilenceUsage = true
		return dash.Run(ctx, dash.Options{
			Port:   dashPort,
			Repo:   dashRepo,
			NoOpen: dashNoOpen,
			Poll:   dashPoll,
			Mock:   dashMock,
		}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	dashCmd.Flags().IntVar(&dashPort, "port", 7777, "puerto local para el server HTTP")
	dashCmd.Flags().StringVar(&dashRepo, "repo", "", "path al repo git (default: cwd)")
	dashCmd.Flags().BoolVar(&dashNoOpen, "no-open", false, "no abrir el browser automáticamente")
	dashCmd.Flags().IntVar(&dashPoll, "poll", 15, "segundos entre polls a gh")
	dashCmd.Flags().BoolVar(&dashMock, "mock", false, "usar datos mock en vez de gh (demo/offline)")
	rootCmd.AddCommand(dashCmd)
}
