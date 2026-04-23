package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/labels"
	"github.com/spf13/cobra"
)

// pair representa un renombre de label `Old` → `New` que `che migrate-labels`
// aplica via `gh label edit` sobre el repo. Se exporta a nivel de paquete
// (junto con migrationPairs) para poder testear el contrato sin levantar gh.
type pair struct {
	Old, New string
}

// migrationPairs devuelve los 5 renombres canónicos para mover un repo
// desde la máquina vieja (`status:*`, 5 estados) a la nueva (`che:*`, 9
// estados). El orden es el del embudo idea → plan → executing → executed
// → closed, pensado para que el output del subcomando se lea como una
// progresión natural en stdout.
//
// No incluye los 4 estados nuevos (`che:planning`, `che:validating`,
// `che:validated`, `che:closing`): no existen en repos viejos, los crea
// `labels.Ensure` lazy cuando un flow los aplica por primera vez.
func migrationPairs() []pair {
	return []pair{
		{Old: labels.StatusIdea, New: labels.CheIdea},
		{Old: labels.StatusPlan, New: labels.ChePlan},
		{Old: labels.StatusExecuting, New: labels.CheExecuting},
		{Old: labels.StatusExecuted, New: labels.CheExecuted},
		{Old: labels.StatusClosed, New: labels.CheClosed},
	}
}

var (
	migrateLabelsRepo   string
	migrateLabelsDryRun bool
)

var migrateLabelsCmd = &cobra.Command{
	Use:   "migrate-labels",
	Short: "renombra los labels viejos status:* a che:* en el repo",
	Long: `migrate-labels renombra in-place los labels de la máquina vieja
(status:idea, status:plan, status:executing, status:executed, status:closed)
a la máquina nueva (che:idea, che:plan, che:executing, che:executed,
che:closed) usando ` + "`gh label edit`" + `.

Es idempotente: si un label viejo no existe, lo saltea; si ya está
renombrado, lo saltea silenciosamente. Los 4 estados nuevos restantes
(che:planning, che:validating, che:validated, che:closing) no se crean
acá — los crea ` + "`labels.Ensure`" + ` lazy cuando un flow los aplica
por primera vez.

Flags:
  --repo     path al repo git (default: cwd)
  --dry-run  solo lista qué haría, sin tocar nada`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Cualquier fallo aquí es un runtime error (gh no auth, label edit
		// roto, etc.), no un problema de flags — no tiene sentido imprimir
		// el Usage de cobra.
		cmd.SilenceUsage = true
		return runMigrateLabels(cmd.OutOrStdout(), migrateLabelsRepo, migrateLabelsDryRun)
	},
}

func init() {
	migrateLabelsCmd.Flags().StringVar(&migrateLabelsRepo, "repo", "", "path al repo git (default: cwd)")
	migrateLabelsCmd.Flags().BoolVar(&migrateLabelsDryRun, "dry-run", false, "solo lista qué haría, sin tocar nada")
	rootCmd.AddCommand(migrateLabelsCmd)
}

// runMigrateLabels itera los pares y emite un renombre por cada uno.
// Reporta cada acción a stdout. Devuelve el primer error fatal (un fallo
// de `gh label edit` que no sea "label no existe").
func runMigrateLabels(out io.Writer, repo string, dryRun bool) error {
	for _, p := range migrationPairs() {
		oldExists, err := labelExists(repo, p.Old)
		if err != nil {
			return fmt.Errorf("checking label %s: %w", p.Old, err)
		}
		if !oldExists {
			// Caso idempotente: si el viejo no existe pero el nuevo sí,
			// asumimos que ya se migró antes; loggeamos silencioso.
			newExists, err := labelExists(repo, p.New)
			if err != nil {
				return fmt.Errorf("checking label %s: %w", p.New, err)
			}
			if newExists {
				fmt.Fprintf(out, "skip: %s ya renombrado a %s\n", p.Old, p.New)
			} else {
				fmt.Fprintf(out, "skip: %s no existe en repo\n", p.Old)
			}
			continue
		}
		if dryRun {
			fmt.Fprintf(out, "[dry-run] %s → %s\n", p.Old, p.New)
			continue
		}
		if err := renameLabel(repo, p.Old, p.New); err != nil {
			return fmt.Errorf("renaming %s → %s: %w", p.Old, p.New, err)
		}
		fmt.Fprintf(out, "ok: %s → %s\n", p.Old, p.New)
	}
	return nil
}

// labelExists chequea si un label con `name` exacto existe en el repo via
// `gh label list --search <name> --json name`. `--search` es fuzzy en gh,
// así que validamos exact match contra el JSON resultante.
//
// Alternativa descartada: `gh api repos/{owner}/{repo}/labels/<name>` —
// requiere parsear `gh repo view` para obtener owner/repo, lo cual añade
// otro path de fallo. `gh label list` corre con cwd=repo y resuelve el
// repo solo (mismo patrón que dash.go).
func labelExists(repo, name string) (bool, error) {
	cmd := exec.Command("gh", "label", "list", "--search", name, "--json", "name", "--limit", "100")
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gh label list: %s", strings.TrimSpace(string(out)))
	}
	var found []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return false, fmt.Errorf("parsing gh output: %w", err)
	}
	for _, l := range found {
		if l.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// renameLabel ejecuta `gh label edit <old> --name <new>` con cwd=repo.
func renameLabel(repo, oldName, newName string) error {
	cmd := exec.Command("gh", "label", "edit", oldName, "--name", newName)
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh label edit: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
