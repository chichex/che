package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineShowJSON bool

var pipelineShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "imprime un pipeline por nombre (resumen humano o JSON)",
	Long: `show carga + valida un pipeline y lo imprime. Por default muestra
un resumen humano (entry + steps + agentes). Con --json escupe el JSON
canónico tal como se cargaría on-disk (útil para diff vs el archivo).

Si <name> coincide con el built-in implícito (` + "`default`" + ` cuando no hay
archivo on-disk), show imprime el built-in `+ "`pipeline.Default()`" + `.
Esto deja a los usuarios inspeccionar el shape canónico antes de hacer
` + "`che pipeline clone default mine`" + `.`,
	Args: cobra.ExactArgs(1),
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
		return runPipelineShow(cmd.OutOrStdout(), mgr, args[0], pipelineShowJSON)
	},
}

func init() {
	pipelineShowCmd.Flags().BoolVar(&pipelineShowJSON, "json", false,
		"emite el JSON canónico en vez del resumen humano")
	pipelineCmd.AddCommand(pipelineShowCmd)
}

// runPipelineShow resuelve el pipeline (on-disk + fallback al built-in
// para "default") y delega en el formateador elegido.
func runPipelineShow(out io.Writer, mgr *pipeline.Manager, name string, asJSON bool) error {
	p, path, err := lookupPipelineForShow(mgr, name)
	if err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}
	if asJSON {
		return writePipelineJSON(out, p)
	}
	return writePipelineSummary(out, name, path, p)
}

// lookupPipelineForShow centraliza la regla "name=default cae al
// built-in si no hay archivo". El resto de los nombres se resuelven
// estrictamente vía Manager.Get (error si falta).
//
// `path` queda vacío para el built-in implícito — el formateador lo
// usa para imprimir "<built-in>" en el header del resumen.
func lookupPipelineForShow(mgr *pipeline.Manager, name string) (pipeline.Pipeline, string, error) {
	if name == "" {
		return pipeline.Pipeline{}, "", fmt.Errorf("pipeline name no puede ser vacío")
	}
	if _, ok := mgr.Path(name); !ok {
		if name == "default" {
			return pipeline.Default(), "", nil
		}
	}
	p, err := mgr.Get(name)
	if err != nil {
		return pipeline.Pipeline{}, "", err
	}
	path, _ := mgr.Path(name)
	return p, path, nil
}

// writePipelineJSON emite el JSON canónico (indentado 2 espacios) — el
// mismo shape que `che pipeline new` deja en disco.
func writePipelineJSON(out io.Writer, p pipeline.Pipeline) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// writePipelineSummary imprime un resumen humano: header con name +
// path, entry (si hay), tabla de steps (NAME, AGENTS, AGGREGATOR).
func writePipelineSummary(out io.Writer, name, path string, p pipeline.Pipeline) error {
	if path == "" {
		fmt.Fprintf(out, "pipeline: %s (built-in)\n", name)
	} else {
		fmt.Fprintf(out, "pipeline: %s (%s)\n", name, path)
	}
	fmt.Fprintf(out, "version: %d\n", p.Version)
	if p.Entry != nil {
		agg := string(p.Entry.Aggregator)
		if agg == "" {
			agg = "-"
		}
		fmt.Fprintf(out, "entry: agents=%v aggregator=%s\n", p.Entry.Agents, agg)
	} else {
		fmt.Fprintln(out, "entry: -")
	}
	fmt.Fprintln(out, "")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tAGENTS\tAGGREGATOR")
	for _, s := range p.Steps {
		agg := string(s.Aggregator)
		if agg == "" {
			agg = "-"
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\n", s.Name, s.Agents, agg)
	}
	return tw.Flush()
}
