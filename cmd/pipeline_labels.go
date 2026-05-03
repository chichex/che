package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/pipeline"
	"github.com/chichex/che/internal/pipelinelabels"
	"github.com/spf13/cobra"
)

type pipelineLabelPair struct {
	Old string
	New string
}

type pipelineLabelClient interface {
	EnsureLabel(name string, skipExisting bool) error
	DeleteRepoLabel(name string) error
	SearchRefsWithLabel(name string) ([]int, error)
	AddLabels(number int, names ...string) error
	RemoveLabel(number int, name string) error
	IssueLabels(number int) ([]string, error)
}

type ghPipelineLabelClient struct{}

var (
	pipelineInitLabelsPipeline string
	pipelineInitLabelsSkip     bool

	pipelineMigrateLabelsPipeline string
	pipelineMigrateLabelsMaps     []string
	pipelineMigrateLabelsDryRun   bool

	pipelineResetPipeline string
	pipelineResetFrom     string
)

var pipelineInitLabelsCmd = &cobra.Command{
	Use:   "init-labels",
	Short: "crea en GitHub los labels derivados del pipeline activo",
	Long: `init-labels crea los labels che:state:<step> y
che:state:applying:<step> para el pipeline resuelto. Sirve para preparar
repos donde CI no tiene permisos para crear labels dinámicamente.

Con --skip-existing, chequea cada label y saltea los que ya existen; sin ese
flag usa gh label create --force, que es idempotente.`,
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
		resolved, err := mgr.Resolve(pipelineInitLabelsPipeline)
		if err != nil {
			return fmt.Errorf("resolve pipeline: %s", formatLoadError(err))
		}
		return runPipelineInitLabels(cmd.OutOrStdout(), ghPipelineLabelClient{}, resolved.Pipeline, pipelineInitLabelsSkip)
	},
}

var pipelineMigrateLabelsCmd = &cobra.Command{
	Use:   "migrate-labels",
	Short: "migra labels v1 a labels derivados del pipeline",
	Long: `migrate-labels reemplaza labels de la máquina vieja por labels v2
derivados del pipeline. Por default aplica la matriz del PRD para el pipeline
built-in default. Usá --map old=new para agregar o sobreescribir mapeos en
pipelines reorganizados.

El comando imprime un preview antes de aplicar. Con --dry-run sólo imprime el
plan sin tocar GitHub. Los labels validated:* viejos se remueven porque el
verdict pasa a vivir en el marker/audit log del pipeline.`,
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
		resolved, err := mgr.Resolve(pipelineMigrateLabelsPipeline)
		if err != nil {
			return fmt.Errorf("resolve pipeline: %s", formatLoadError(err))
		}
		pairs, err := migrationPairsForPipeline(resolved.Pipeline, pipelineMigrateLabelsMaps)
		if err != nil {
			return err
		}
		return runPipelineMigrateLabels(cmd.OutOrStdout(), ghPipelineLabelClient{}, pairs, pipelineMigrateLabelsDryRun)
	},
}

var pipelineResetCmd = &cobra.Command{
	Use:   "reset <entity>",
	Short: "limpia locks y labels applying huérfanos de un issue o PR",
	Long: `reset limpia de una entity los labels che:lock:* y
che:state:applying:<step> que quedan pegados tras un crash. Si se pasa
--from <step>, también deja la entity lista para reanudar desde ese step
aplicando che:state:<step> y removiendo otros che:state:* terminales.`,
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
		resolved, err := mgr.Resolve(pipelineResetPipeline)
		if err != nil {
			return fmt.Errorf("resolve pipeline: %s", formatLoadError(err))
		}
		number, err := labels.RefNumber(args[0])
		if err != nil {
			return fmt.Errorf("invalid entity %q: %w", args[0], err)
		}
		return runPipelineReset(cmd.OutOrStdout(), ghPipelineLabelClient{}, resolved.Pipeline, number, pipelineResetFrom)
	},
}

func init() {
	pipelineInitLabelsCmd.Flags().StringVar(&pipelineInitLabelsPipeline, "pipeline", "", "pipeline a resolver (default: config del repo o built-in)")
	pipelineInitLabelsCmd.Flags().BoolVar(&pipelineInitLabelsSkip, "skip-existing", false, "saltea labels que ya existen en vez de recrearlos con --force")
	pipelineCmd.AddCommand(pipelineInitLabelsCmd)

	pipelineMigrateLabelsCmd.Flags().StringVar(&pipelineMigrateLabelsPipeline, "pipeline", "", "pipeline a resolver para validar --map y labels target")
	pipelineMigrateLabelsCmd.Flags().StringArrayVar(&pipelineMigrateLabelsMaps, "map", nil, "mapeo old=new adicional o override; repetible")
	pipelineMigrateLabelsCmd.Flags().BoolVar(&pipelineMigrateLabelsDryRun, "dry-run", false, "solo imprime preview, sin tocar GitHub")
	pipelineCmd.AddCommand(pipelineMigrateLabelsCmd)

	pipelineResetCmd.Flags().StringVar(&pipelineResetPipeline, "pipeline", "", "pipeline a resolver para validar --from")
	pipelineResetCmd.Flags().StringVar(&pipelineResetFrom, "from", "", "step terminal al que resetear para reanudar")
	pipelineCmd.AddCommand(pipelineResetCmd)
}

func runPipelineInitLabels(out io.Writer, client pipelineLabelClient, p pipeline.Pipeline, skipExisting bool) error {
	for _, name := range pipelinelabels.Expected(p) {
		if err := client.EnsureLabel(name, skipExisting); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok: %s\n", name)
	}
	return nil
}

func migrationPairsForPipeline(p pipeline.Pipeline, overrides []string) ([]pipelineLabelPair, error) {
	pairs := defaultPipelineMigrationPairs()
	stepSet := map[string]bool{}
	for _, s := range p.Steps {
		stepSet[s.Name] = true
	}
	for _, raw := range overrides {
		oldName, newName, ok := strings.Cut(raw, "=")
		oldName = strings.TrimSpace(oldName)
		newName = strings.TrimSpace(newName)
		if !ok || oldName == "" || newName == "" {
			return nil, fmt.Errorf("invalid --map %q: expected old=new", raw)
		}
		parsed, err := pipelinelabels.Parse(newName)
		if err != nil || (parsed.Kind != pipelinelabels.KindState && parsed.Kind != pipelinelabels.KindApplying) {
			return nil, fmt.Errorf("invalid --map %q: new label must be che:state:<step> or che:state:applying:<step>", raw)
		}
		if !stepSet[parsed.Step] {
			return nil, fmt.Errorf("invalid --map %q: step %q is not in the resolved pipeline", raw, parsed.Step)
		}
		pairs = upsertPipelineLabelPair(pairs, pipelineLabelPair{Old: oldName, New: newName})
	}
	return pairs, nil
}

func defaultPipelineMigrationPairs() []pipelineLabelPair {
	return []pipelineLabelPair{
		{Old: v1CheIdea, New: pipelinelabels.StateLabel("idea")},
		{Old: v1ChePlanning, New: pipelinelabels.ApplyingLabel("explore")},
		{Old: v1ChePlan, New: pipelinelabels.StateLabel("explore")},
		{Old: v1CheExecuting, New: pipelinelabels.ApplyingLabel("execute")},
		{Old: v1CheExecuted, New: pipelinelabels.StateLabel("execute")},
		{Old: v1CheValidating, New: pipelinelabels.ApplyingLabel("validate_pr")},
		{Old: v1CheValidated, New: pipelinelabels.StateLabel("validate_pr")},
		{Old: v1CheClosing, New: pipelinelabels.ApplyingLabel("close")},
		{Old: v1CheClosed, New: pipelinelabels.StateLabel("close")},
		{Old: labels.ValidatedApprove},
		{Old: labels.ValidatedChangesRequested},
		{Old: labels.ValidatedNeedsHuman},
	}
}

func upsertPipelineLabelPair(pairs []pipelineLabelPair, pair pipelineLabelPair) []pipelineLabelPair {
	for i := range pairs {
		if pairs[i].Old == pair.Old {
			pairs[i] = pair
			return pairs
		}
	}
	return append(pairs, pair)
}

func runPipelineMigrateLabels(out io.Writer, client pipelineLabelClient, pairs []pipelineLabelPair, dryRun bool) error {
	fmt.Fprintln(out, "preview:")
	for _, pair := range pairs {
		if pair.New == "" {
			fmt.Fprintf(out, "  remove %s\n", pair.Old)
		} else {
			fmt.Fprintf(out, "  %s -> %s\n", pair.Old, pair.New)
		}
	}
	if dryRun {
		return nil
	}
	for _, pair := range pairs {
		if pair.New != "" {
			if err := client.EnsureLabel(pair.New, true); err != nil {
				return err
			}
		}
		refs, err := client.SearchRefsWithLabel(pair.Old)
		if err != nil {
			return fmt.Errorf("searching refs with %s: %w", pair.Old, err)
		}
		for _, number := range refs {
			if pair.New != "" {
				if err := client.AddLabels(number, pair.New); err != nil {
					return fmt.Errorf("adding %s to #%d: %w", pair.New, number, err)
				}
			}
			if err := client.RemoveLabel(number, pair.Old); err != nil {
				return fmt.Errorf("removing %s from #%d: %w", pair.Old, number, err)
			}
		}
		if err := client.DeleteRepoLabel(pair.Old); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok: %s", pair.Old)
		if pair.New != "" {
			fmt.Fprintf(out, " -> %s", pair.New)
		}
		fmt.Fprintln(out)
	}
	return nil
}

func runPipelineReset(out io.Writer, client pipelineLabelClient, p pipeline.Pipeline, number int, from string) error {
	if from != "" && !pipelineHasStep(p, from) {
		return fmt.Errorf("--from %q is not a step in the resolved pipeline", from)
	}
	names, err := client.IssueLabels(number)
	if err != nil {
		return err
	}
	removed := 0
	for _, name := range names {
		parsed, err := pipelinelabels.Parse(name)
		if err != nil {
			continue
		}
		if parsed.Kind == pipelinelabels.KindLock || parsed.Kind == pipelinelabels.KindApplying || (from != "" && parsed.Kind == pipelinelabels.KindState) {
			if err := client.RemoveLabel(number, name); err != nil {
				return err
			}
			removed++
			fmt.Fprintf(out, "removed: %s\n", name)
		}
	}
	if from != "" {
		state := pipelinelabels.StateLabel(from)
		if err := client.EnsureLabel(state, true); err != nil {
			return err
		}
		if err := client.AddLabels(number, state); err != nil {
			return err
		}
		fmt.Fprintf(out, "set: %s\n", state)
	}
	if removed == 0 && from == "" {
		fmt.Fprintln(out, "nothing to reset")
	}
	return nil
}

func pipelineHasStep(p pipeline.Pipeline, name string) bool {
	for _, s := range p.Steps {
		if s.Name == name {
			return true
		}
	}
	return false
}

func (ghPipelineLabelClient) EnsureLabel(name string, skipExisting bool) error {
	if skipExisting {
		exists, err := labelExists("", name)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	return labels.Ensure(name)
}

func (ghPipelineLabelClient) DeleteRepoLabel(name string) error {
	cmd := exec.Command("gh", "label", "delete", name, "--yes")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	combined := string(out)
	if strings.Contains(combined, "could not find") || strings.Contains(combined, "not found") || strings.Contains(combined, "HTTP 404") {
		return nil
	}
	return fmt.Errorf("deleting repo label %s: %s", name, strings.TrimSpace(combined))
}

func (ghPipelineLabelClient) SearchRefsWithLabel(name string) ([]int, error) {
	nwo, err := currentRepoNameWithOwner()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("gh", "search", "issues", "repo:"+nwo, fmt.Sprintf("label:%q", name), "--json", "number", "--limit", "100")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh search issues: %s", strings.TrimSpace(string(out)))
	}
	var raw []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh search: %w", err)
	}
	refs := make([]int, 0, len(raw))
	for _, r := range raw {
		refs = append(refs, r.Number)
	}
	return refs, nil
}

func (ghPipelineLabelClient) AddLabels(number int, names ...string) error {
	return labels.AddLabels(number, names...)
}

func (ghPipelineLabelClient) RemoveLabel(number int, name string) error {
	return labels.RemoveLabel(number, name)
}

func (ghPipelineLabelClient) IssueLabels(number int) ([]string, error) {
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh api issues/%d: %s", number, strings.TrimSpace(string(out)))
	}
	var raw struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse issue labels: %w", err)
	}
	outNames := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		outNames = append(outNames, l.Name)
	}
	return outNames, nil
}

func currentRepoNameWithOwner() (string, error) {
	cmd := exec.Command("gh", "repo", "view", "--json", "nameWithOwner")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh repo view: %s", strings.TrimSpace(string(out)))
	}
	var raw struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse gh repo view: %w", err)
	}
	if raw.NameWithOwner == "" {
		return "", fmt.Errorf("gh repo view: nameWithOwner vacío")
	}
	return raw.NameWithOwner, nil
}
