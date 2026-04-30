package cmd

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/chichex/che/internal/agentregistry"
	"github.com/spf13/cobra"
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "lista y gestiona los subagents disponibles para los pipelines",
	Long: `agents agrupa los subcomandos para inspeccionar los agents
descubiertos por che (§2 del PRD #50). v1 expone solo ` + "`agents list`" + `;
` + "`agents show <name>`" + ` y los wizards llegan en PRs siguientes.`,
}

var agentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "lista los agents descubiertos + built-in, indicando origen",
	Long: `list escanea las 4 ubicaciones oficiales de Claude Code (managed,
project ` + "`.claude/agents/`" + `, user ` + "`~/.claude/agents/`" + `, plugins instalados)
y los muestra junto con los 3 built-in de che (claude-opus, claude-sonnet,
claude-haiku). La columna SOURCE indica de dónde vino cada uno.

Si un agente custom tiene el mismo nombre que un built-in, el custom
gana y list emite un warning en stderr (precedencia §2.a del PRD).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return runAgentsList(cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	agentsCmd.AddCommand(agentsListCmd)
	rootCmd.AddCommand(agentsCmd)
}

func runAgentsList(out, errOut io.Writer) error {
	reg, errs := agentregistry.Discover(agentregistry.Options{IncludeBuiltins: true})
	for _, e := range errs {
		// CollisionWarning va a stderr como nota, no aborta. Los errores
		// de parse de archivos individuales tampoco abortan — es output
		// best-effort.
		var warn agentregistry.CollisionWarning
		if errors.As(e, &warn) {
			fmt.Fprintln(errOut, "warn:", warn.Error())
			continue
		}
		fmt.Fprintln(errOut, "warn:", e.Error())
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMODEL\tSOURCE\tDESCRIPTION")
	for _, a := range reg.All() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Name, dashIfEmpty(a.Model), a.Source, truncate(a.Description, 80))
	}
	return tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// truncate corta a n runes y agrega ellipsis si quedó cortado. Acepta
// utf-8: cuenta runes, no bytes.
func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
