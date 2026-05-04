package cmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/chichex/che/internal/agentregistry"
	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineSimulatePipeline string

var pipelineSimulateCmd = &cobra.Command{
	Use:   "simulate",
	Short: "dry-run: muestra qué pipeline se resolvería y qué agentes correrían",
	Long: `simulate es un dry-run completo: aplica la jerarquía de resolución
(--pipeline > config.default > built-in, PRD §7.b), reporta el origen
y para cada step lista los agentes que correrían + la metadata
relevante (aggregator, model, source).

NO invoca al LLM ni corre el motor real — sólo imprime el plan. Ideal
para validar que un cambio en config.default o en un .json se está
resolviendo como esperás antes de ejecutar.

Si --pipeline está vacío, simulate replica exactamente lo que haría una
corrida real sin flags.`,
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
		reg, regErrs := agentregistry.Discover(agentregistry.Options{IncludeBuiltins: true})
		for _, e := range regErrs {
			fmt.Fprintln(cmd.ErrOrStderr(), "warn:", e.Error())
		}
		lookup := func(name string) (agentInfo, bool) {
			a, ok := reg.Get(name)
			if !ok {
				return agentInfo{}, false
			}
			return agentInfo{
				Name:   a.Name,
				Model:  a.Model,
				Source: string(a.Source),
				Path:   a.Path,
			}, true
		}
		return runPipelineSimulate(cmd.OutOrStdout(), mgr, lookup, pipelineSimulatePipeline)
	},
}

// agentInfo es el subset de metadata del registry que simulate
// renderiza. Aislar en un struct propio del paquete cmd permite que los
// tests inyecten un fake lookup sin depender de los internals del
// package agentregistry (campos no exportados).
type agentInfo struct {
	Name   string
	Model  string
	Source string
	Path   string
}

func init() {
	pipelineSimulateCmd.Flags().StringVar(&pipelineSimulatePipeline, "pipeline", "",
		"nombre del pipeline a simular (vacío = aplicar jerarquía default)")
	pipelineCmd.AddCommand(pipelineSimulateCmd)
}

// runPipelineSimulate ejecuta Resolve(flag) y renderiza el resultado.
// `lookup` resuelve nombre → metadata (ok=false si el agente no está en
// el registry). El simulate marca esos como MISSING en la tabla — útil
// para detectar agentes referenciados pero no instalados antes de
// correr el pipeline.
func runPipelineSimulate(out io.Writer, mgr *pipeline.Manager, lookup func(string) (agentInfo, bool), flag string) error {
	r, err := mgr.Resolve(flag)
	if err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}

	src := r.Path
	if src == "" {
		src = "<built-in>"
	}
	return renderPipelinePreview(out, pipelinePreviewHeader{
		Name:   r.Name,
		Source: fmt.Sprintf("%s (%s)", r.Source, src),
	}, r.Pipeline, lookup)
}

type pipelinePreviewHeader struct {
	Name         string
	Source       string
	ShowComments bool
}

func renderPipelinePreview(out io.Writer, header pipelinePreviewHeader, p pipeline.Pipeline, lookup func(string) (agentInfo, bool)) error {
	fmt.Fprintf(out, "pipeline: %s\n", header.Name)
	fmt.Fprintf(out, "source:   %s\n", header.Source)
	fmt.Fprintln(out, "")

	if p.Entry != nil {
		fmt.Fprintln(out, "entry:")
		if err := writeAgentsTable(out, p.Entry.Agents, lookup); err != nil {
			return err
		}
		fmt.Fprintf(out, "  aggregator: %s\n", previewAggregator(p.Entry.Aggregator, len(p.Entry.Agents)))
		fmt.Fprintln(out, "")
	}

	for i, step := range p.Steps {
		fmt.Fprintf(out, "step[%d]: %s\n", i, step.Name)
		if err := writeAgentsTable(out, step.Agents, lookup); err != nil {
			return err
		}
		fmt.Fprintf(out, "  aggregator: %s\n", previewAggregator(step.Aggregator, len(step.Agents)))
		if header.ShowComments && step.Comment != "" {
			fmt.Fprintf(out, "  comment: %s\n", step.Comment)
		}
		fmt.Fprintln(out, "")
	}

	fmt.Fprintln(out, "(dry-run: no se invocó ningún agente)")
	return nil
}

func previewAggregator(agg pipeline.Aggregator, agents int) string {
	if agg != "" {
		return string(agg)
	}
	if agents > 1 {
		return "majority (default)"
	}
	return "- (1 agente)"
}

// writeAgentsTable imprime una tabla indentada con los agentes del step
// y el resultado del lookup en el registry (model + source + path o
// "MISSING" si no existe).
func writeAgentsTable(out io.Writer, agents []string, lookup func(string) (agentInfo, bool)) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  AGENT\tMODEL\tSOURCE\tPATH")
	for _, name := range agents {
		a, ok := lookup(name)
		if !ok {
			fmt.Fprintf(tw, "  %s\t-\tMISSING\t-\n", name)
			continue
		}
		path := a.Path
		if path == "" {
			path = "-"
		}
		model := a.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.Name, model, a.Source, path)
	}
	return tw.Flush()
}
