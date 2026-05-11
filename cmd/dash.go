package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/chichex/che/internal/dash"
	"github.com/spf13/cobra"
)

var (
	dashPort   int
	dashNoOpen bool
)

var dashCmd = &cobra.Command{
	Use:   "dash",
	Short: "abre el dashboard local de che en el browser",
	Long: `dash levanta un HTTP server local en 127.0.0.1 que sirve el
dashboard de che (master/detail de pipelines y runs) y abre el browser
apuntando ahi. El servidor queda corriendo hasta ctrl+c.

El dashboard de v1 trae datos mockeados — todavia no esta conectado al
runner real. Sirve para explorar el diseno y para integraciones futuras.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		return dash.Serve(ctx, dash.Options{
			Port:   dashPort,
			NoOpen: dashNoOpen,
			Stdout: cmd.OutOrStdout(),
		})
	},
}

func init() {
	dashCmd.Flags().IntVar(&dashPort, "port", 7878, "TCP port para el server (si esta ocupado, cae a un puerto efimero)")
	dashCmd.Flags().BoolVar(&dashNoOpen, "no-open", false, "no abrir el browser automaticamente")
	rootCmd.AddCommand(dashCmd)
}
