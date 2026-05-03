package cmd

import (
	"fmt"
	"io"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

// Flags de `che init-labels`. Globales al package por consistencia con
// el resto de subcomandos (cobra los lee del flag set y `runInitLabels`
// los recibe explícitos para tests).
var (
	initLabelsPipelineFlag string
	initLabelsDryRun       bool
)

var initLabelsCmd = &cobra.Command{
	Use:   "init-labels",
	Short: "crea en el repo todos los labels que el pipeline necesita (idempotente)",
	Long: `init-labels resuelve el pipeline activo (jerarquía: --pipeline > config.default >
built-in) y para cada label que el pipeline requiere (estados terminales,
estados aplicantes, verdicts del validador, marker ct:plan, lock binario
che:locked) corre 'gh label create --force' — idempotente.

Pensado para CI: en repos nuevos, este subcomando hace que la primera
corrida del pipeline no pise un 404 al crear el primer label. Los flows
ya hacen Ensure por su cuenta cuando aplican un label, así que esto NO
es estrictamente necesario en runtime — pero es útil como pre-vuelo
explícito para detectar problemas de auth/permisos antes de gastar tiempo
ejecutando un agente.

NO crea los labels dinámicos 'che:lock:<ts>:...' (uno por run, con
timestamp único — no tiene sentido pre-crear).

Flags:
  --pipeline <name>  pipeline a usar (default: el del config o built-in)
  --dry-run          solo lista los labels esperados, no los crea`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		root, err := repoRootForPipeline()
		if err != nil {
			return err
		}
		mgr, err := pipeline.NewManager(root)
		if err != nil {
			return fmt.Errorf("init pipeline manager: %s", formatLoadError(err))
		}
		return runInitLabels(cmd.OutOrStdout(), cmd.ErrOrStderr(), mgr, initLabelsPipelineFlag, initLabelsDryRun)
	},
}

func init() {
	initLabelsCmd.Flags().StringVar(&initLabelsPipelineFlag, "pipeline", "",
		"nombre del pipeline a usar (default: el del config o built-in)")
	initLabelsCmd.Flags().BoolVar(&initLabelsDryRun, "dry-run", false,
		"solo lista los labels esperados, no los crea")
	rootCmd.AddCommand(initLabelsCmd)
}

// runInitLabels es la función testeable: resuelve el pipeline, computa el
// set esperado y, si no es dry-run, los crea uno por uno con
// labels.EnsureForPipeline. Devuelve el primer error.
//
// El parámetro `out` recibe el output humano (lista de labels y resumen).
// `errOut` se usa para warnings (en este subcomando, ninguno por ahora —
// el wiring queda listo por simetría con run/explore).
func runInitLabels(out, errOut io.Writer, mgr *pipeline.Manager, pipelineFlag string, dryRun bool) error {
	r, err := mgr.Resolve(pipelineFlag)
	if err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}
	src := r.Path
	if src == "" {
		src = "<built-in>"
	}
	fmt.Fprintf(out, "pipeline: %s\n", r.Name)
	fmt.Fprintf(out, "source:   %s (%s)\n", r.Source, src)

	expected := labels.ExpectedForPipeline(r.Pipeline)
	fmt.Fprintf(out, "labels esperados (%d):\n", len(expected))
	for _, l := range expected {
		fmt.Fprintf(out, "  %s\n", l)
	}

	if dryRun {
		fmt.Fprintln(out, "\n[dry-run] no se crearon labels")
		return nil
	}

	if err := labels.EnsureForPipeline(r.Pipeline); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nok: %d label(s) asegurado(s) en el repo\n", len(expected))
	_ = errOut // reservado para warnings futuros (drift, deprecaciones)
	return nil
}
