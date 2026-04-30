package cmd

import (
	"fmt"
	"io"

	"github.com/chichex/che/internal/agentregistry"
	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineValidateSkipAgents bool

var pipelineValidateCmd = &cobra.Command{
	Use:   "validate <name>",
	Short: "valida sintaxis + referencias a agentes de un pipeline",
	Long: `validate corre 2 chequeos sobre un pipeline:

  1. Schema/loader (` + "`pipeline.Validate`" + `): versión soportada, mínimo
     1 step, nombres válidos, aggregator preset, etc.
  2. Referencias a agentes: chequea que cada agente referenciado en
     entry y steps exista en el agentregistry (built-in + auto-discovered
     en .claude/agents/, plugins, etc.).

Errores se reportan con path/field cuando el loader puede ubicarlos
(ej. ` + "`steps[2].agents[1]: agent \"foo\" not found...`" + `).

Para skipear el chequeo de agentes (útil en CI antes de tener los
agentes custom instalados) pasá --skip-agents.`,
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

		// Discover puede emitir warnings (collisions, parse errors de
		// archivos individuales) que no rompen el registry. validate los
		// imprime en stderr pero no los trata como fatales — el chequeo
		// "agente referenciado existe" no depende de ellos.
		reg, regErrs := agentregistry.Discover(agentregistry.Options{IncludeBuiltins: true})
		for _, e := range regErrs {
			fmt.Fprintln(cmd.ErrOrStderr(), "warn:", e.Error())
		}

		has := func(name string) bool {
			_, ok := reg.Get(name)
			return ok
		}
		return runPipelineValidate(cmd.OutOrStdout(), mgr, has, args[0], pipelineValidateSkipAgents)
	},
}

func init() {
	pipelineValidateCmd.Flags().BoolVar(&pipelineValidateSkipAgents, "skip-agents", false,
		"skipea la verificación de existencia de agentes (sólo schema)")
	pipelineCmd.AddCommand(pipelineValidateCmd)
}

// runPipelineValidate carga + valida el pipeline y, si --skip-agents no
// está, chequea referencias contra el predicado `has` (típicamente
// envoltorio sobre `*agentregistry.Registry.Get`). Recibir el predicado
// en vez del registry concreto deja a los tests inyectar fakes sin
// depender de los internals del package agentregistry.
func runPipelineValidate(out io.Writer, mgr *pipeline.Manager, has func(string) bool, name string, skipAgents bool) error {
	p, path, err := lookupPipelineForShow(mgr, name)
	if err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}
	// Validación del schema. Get + Default ya validan; este Validate
	// extra es defensivo (si lookupPipelineForShow cambia o si alguien
	// llama esta función con un Pipeline no validado).
	if err := pipeline.Validate(p); err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}

	if !skipAgents {
		if err := pipeline.ValidateAgents(p, has); err != nil {
			return fmt.Errorf("%s", formatLoadError(err))
		}
	}

	src := path
	if src == "" {
		src = "<built-in>"
	}
	fmt.Fprintf(out, "ok: %s (%s) válido\n", name, src)
	return nil
}
