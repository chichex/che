package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var (
	pipelineCloneReplace []string
	pipelineCloneForce   bool
)

var pipelineCloneCmd = &cobra.Command{
	Use:   "clone <src> <dst>",
	Short: "copia un pipeline aplicando sustituciones de strings",
	Long: `clone duplica un pipeline existente y opcionalmente sustituye refs
a agentes (PRD §1.c, ejemplo de quickstart caso B).

Sustituciones:
  --replace claude-opus=claude-sonnet
  --replace plan-reviewer-strict=my-reviewer

Las sustituciones aplican exact-match sobre los strings de
` + "`entry.agents`" + ` y ` + "`steps[*].agents`" + `. No tocan ` + "`name`" + ` de step
ni ` + "`aggregator`" + ` (esos son metadata, no refs reemplazables).

Ejemplo del PRD:
  che pipeline clone default fast --replace claude-opus=claude-sonnet

Si <src> es ` + "`default`" + ` y no hay archivo on-disk, clone usa el built-in
` + "`pipeline.Default()`" + ` como fuente.

Por seguridad ` + "`clone`" + ` falla si <dst> ya existe. Para sobrescribir
pasá --force.`,
	Args: cobra.ExactArgs(2),
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
		return runPipelineClone(cmd.OutOrStdout(), mgr, args[0], args[1], pipelineCloneReplace, pipelineCloneForce)
	},
}

func init() {
	pipelineCloneCmd.Flags().StringArrayVar(&pipelineCloneReplace, "replace", nil,
		"sustitución exact-match de agentes (formato old=new); puede repetirse")
	pipelineCloneCmd.Flags().BoolVar(&pipelineCloneForce, "force", false,
		"sobrescribe el destino si ya existe")
	pipelineCmd.AddCommand(pipelineCloneCmd)
}

// runPipelineClone resuelve src (con fallback al built-in para "default"),
// parsea las reglas --replace, aplica sustituciones, valida el resultado
// y lo escribe en .che/pipelines/<dst>.json.
func runPipelineClone(out io.Writer, mgr *pipeline.Manager, src, dst string, rawReplace []string, force bool) error {
	if src == "" || dst == "" {
		return fmt.Errorf("src y dst son obligatorios")
	}
	if src == dst {
		return fmt.Errorf("src y dst no pueden ser iguales (%q)", src)
	}

	rules, err := parseReplaceRules(rawReplace)
	if err != nil {
		return err
	}

	srcPipeline, _, err := lookupPipelineForShow(mgr, src)
	if err != nil {
		return fmt.Errorf("leyendo src %q: %s", src, formatLoadError(err))
	}

	cloned := applyReplacements(srcPipeline, rules)
	if err := pipeline.Validate(cloned); err != nil {
		return fmt.Errorf("pipeline resultante inválido: %s", formatLoadError(err))
	}

	dest := filepath.Join(mgr.PipelinesDir(), dst+".json")
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("%s ya existe — pasá --force para sobrescribir", dest)
		}
	}
	if err := writeClonedPipeline(dest, cloned); err != nil {
		return fmt.Errorf("escribir %s: %w", dest, err)
	}
	fmt.Fprintf(out, "creado %s (clonado de %q)\n", dest, src)
	return nil
}

// parseReplaceRules valida el shape "old=new" de cada regla. Reglas
// con old vacío o sin '=' se rechazan. new vacío SÍ es válido a nivel
// parse (Validate del pipeline luego rechaza agentes vacíos — así el
// usuario obtiene un error de loader puntual en vez de uno genérico
// del flag parser).
func parseReplaceRules(raw []string) (map[string]string, error) {
	rules := map[string]string{}
	for _, r := range raw {
		idx := strings.Index(r, "=")
		if idx < 0 {
			return nil, fmt.Errorf("--replace %q: formato inválido, usá old=new", r)
		}
		old := strings.TrimSpace(r[:idx])
		neu := strings.TrimSpace(r[idx+1:])
		if old == "" {
			return nil, fmt.Errorf("--replace %q: old no puede ser vacío", r)
		}
		if prev, dup := rules[old]; dup && prev != neu {
			return nil, fmt.Errorf("--replace duplicado para %q (%q vs %q)", old, prev, neu)
		}
		rules[old] = neu
	}
	return rules, nil
}

// applyReplacements devuelve un pipeline nuevo con las reglas
// aplicadas a entry.agents y steps[*].agents. NO muta el input.
func applyReplacements(p pipeline.Pipeline, rules map[string]string) pipeline.Pipeline {
	out := pipeline.Pipeline{
		Version: p.Version,
	}
	if p.Entry != nil {
		entry := pipeline.Entry{
			Agents:     replaceAll(p.Entry.Agents, rules),
			Aggregator: p.Entry.Aggregator,
		}
		out.Entry = &entry
	}
	out.Steps = make([]pipeline.Step, len(p.Steps))
	for i, s := range p.Steps {
		out.Steps[i] = pipeline.Step{
			Name:       s.Name,
			Agents:     replaceAll(s.Agents, rules),
			Aggregator: s.Aggregator,
			Comment:    s.Comment,
		}
	}
	return out
}

// replaceAll mapea cada agente vía rules. Strings sin entry en rules
// se devuelven tal cual.
func replaceAll(agents []string, rules map[string]string) []string {
	out := make([]string, len(agents))
	for i, a := range agents {
		if neu, ok := rules[a]; ok {
			out[i] = neu
			continue
		}
		out[i] = a
	}
	return out
}

func writeClonedPipeline(path string, p pipeline.Pipeline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
