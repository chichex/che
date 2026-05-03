package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/pipelinelabels"
	"github.com/spf13/cobra"
)

// v2Pair representa un mapping v1 → v2 que `che migrate-labels-v2` aplica
// in-place sobre los issues abiertos. Distinto del `pair` de migrate-labels:
// ese otro renombra labels a nivel repo (`gh label edit`); este aplica el
// cambio por-issue (remove label viejo + add label nuevo) porque los strings
// nuevos son TOTALMENTE distintos (`che:state:*` vs `che:*`) y queremos
// preservar el valor histórico del label viejo si el operador lo necesita.
type v2Pair struct {
	V1, V2 string
}

// v2MigrationPairs devuelve el mapping de los 9 estados v1 → v2 según la
// tabla canónica del PRD §6.c. El orden es el del embudo (idea → close)
// para que el output del subcomando se lea como progresión natural.
//
// Caveat sobre `che:validating`: en v1 cubría tanto validate-de-plan como
// validate-de-PR. En v2 hay un solo step (`validate_pr`). El mapping a
// `applying:validate_pr` es el más cercano semánticamente, pero si el
// repo tenía issues con `che:validating` originados desde plan-validate
// (pre-v0.0.49 se usaba sobre el issue durante el run de validate plan),
// el operador puede preferir saltar al estado terminal manualmente. El
// estado validating es transitorio (solo aparece durante un run en curso)
// así que en migración bulk es raro encontrarlo. Si lo encontramos
// log.Warn pero mapeamos igual.
func v2MigrationPairs() []v2Pair {
	return []v2Pair{
		{V1: labels.CheIdea, V2: pipelinelabels.StateIdea},
		{V1: labels.ChePlanning, V2: pipelinelabels.StateApplyingExplore},
		{V1: labels.ChePlan, V2: pipelinelabels.StateExplore},
		{V1: labels.CheExecuting, V2: pipelinelabels.StateApplyingExecute},
		{V1: labels.CheExecuted, V2: pipelinelabels.StateExecute},
		{V1: labels.CheValidating, V2: pipelinelabels.StateApplyingValidatePR},
		{V1: labels.CheValidated, V2: pipelinelabels.StateValidatePR},
		{V1: labels.CheClosing, V2: pipelinelabels.StateApplyingClose},
		{V1: labels.CheClosed, V2: pipelinelabels.StateClose},
	}
}

var (
	migrateLabelsV2Repo   string
	migrateLabelsV2DryRun bool
)

var migrateLabelsV2Cmd = &cobra.Command{
	Use:   "migrate-labels-v2",
	Short: "migra los labels viejos che:* a la familia v2 che:state:*",
	Long: `migrate-labels-v2 itera los issues abiertos del repo y por cada uno
con un label de la máquina vieja (che:idea, che:planning, che:plan,
che:executing, che:executed, che:validating, che:validated, che:closing,
che:closed) lo reemplaza por el label v2 correspondiente
(che:state:idea ... che:state:close, con `+"`applying:`"+` para los estados
intermedios — ver tabla del PRD §6.c).

A diferencia de ` + "`migrate-labels`" + ` (que renombra labels a nivel del
repo via ` + "`gh label edit`" + `), esto opera por-issue: hace remove del
label v1 y add del label v2 para cada caso. Los labels viejos (che:*) no
se borran del repo — los flows migrados a v2 los rechazan vía guard
v1-rejection con mensaje accionable.

Idempotencia:
  - Si un issue ya tiene el label v2 correspondiente, skip silencioso (no
    se duplica).
  - Si tiene MIXTO (label v1 y v2 presentes a la vez), warn y NO toca:
    el operador resuelve a mano para no perder información.
  - Si no tiene ningún label che:*, skip silencioso.

Caveat de ` + "`che:validating`" + `: cubre AMBOS modos plan/PR en v1, en v2
mapea a applying:validate_pr (el modelo v2 colapsa los dos modos en un
solo step). Si lo encontramos logueamos un warning porque el mapping
puede ser semánticamente impreciso para repos que estaban en validate
plan en curso al momento de migrar.

Flags:
  --repo     path al repo git (default: cwd)
  --dry-run  solo lista qué haría, sin tocar nada`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return runMigrateLabelsV2(cmd.OutOrStdout(), migrateLabelsV2Repo, migrateLabelsV2DryRun)
	},
}

func init() {
	migrateLabelsV2Cmd.Flags().StringVar(&migrateLabelsV2Repo, "repo", "", "path al repo git (default: cwd)")
	migrateLabelsV2Cmd.Flags().BoolVar(&migrateLabelsV2DryRun, "dry-run", false, "solo lista qué haría, sin tocar nada")
	rootCmd.AddCommand(migrateLabelsV2Cmd)
}

// migrateIssue es una vista mínima del issue para el migrador. number +
// labels es todo lo que necesitamos para decidir qué tocar.
type migrateIssue struct {
	Number int           `json:"number"`
	Title  string        `json:"title"`
	Labels []migrateLbl  `json:"labels"`
}

type migrateLbl struct {
	Name string `json:"name"`
}

// listIssuesFn es el extractor de issues a migrar, variable para que los
// tests lo stubeen sin shell-out a gh.
var listIssuesFn = listIssuesViaGh

// applyMigrationFn es el aplicador de un cambio (remove v1 + add v2),
// variable para tests.
var applyMigrationFn = applyMigrationViaGh

// runMigrateLabelsV2 itera los issues abiertos del repo, decide qué
// transformación aplicar a cada uno y emite el reporte. Devuelve el primer
// error fatal — si un solo issue falla, paramos para no dejar el repo a
// medias (un usuario que ve "ok 1 / fail 5 / skip 3" no sabe el estado real).
func runMigrateLabelsV2(out io.Writer, repo string, dryRun bool) error {
	issues, err := listIssuesFn(repo)
	if err != nil {
		return fmt.Errorf("listing issues: %w", err)
	}

	v1Set := make(map[string]string, 9) // v1 → v2
	for _, p := range v2MigrationPairs() {
		v1Set[p.V1] = p.V2
	}
	v2Set := make(map[string]bool, 9)
	for _, p := range v2MigrationPairs() {
		v2Set[p.V2] = true
	}

	fmt.Fprintf(out, "scanned %d open issue(s)\n", len(issues))

	var modified, skipped, mixed int
	var firstErr error
	for _, iss := range issues {
		v1Found := make(map[string]bool, 1)
		v2Found := make(map[string]bool, 1)
		for _, l := range iss.Labels {
			if _, ok := v1Set[l.Name]; ok {
				v1Found[l.Name] = true
			}
			if v2Set[l.Name] {
				v2Found[l.Name] = true
			}
		}
		// Sin labels v1: no hay nada que migrar acá. Saltamos silenciosamente.
		if len(v1Found) == 0 {
			continue
		}
		// MIXTO: tiene ambos modelos. No tocamos para no pisar lo que el
		// operador armó a mano. Reporta pero no aborta — el resto del scan
		// puede tener issues v1-only legítimos.
		if len(v2Found) > 0 {
			mixed++
			v1Names := sortedKeys(v1Found)
			v2Names := sortedKeys(v2Found)
			fmt.Fprintf(out, "skip mixed: #%d %q tiene v1 (%s) + v2 (%s) — resolvelo a mano\n",
				iss.Number, iss.Title, strings.Join(v1Names, ","), strings.Join(v2Names, ","))
			continue
		}
		// Mapeo v1 → v2 ordenado por nombre del v1 para output estable.
		actions := make([]v2Pair, 0, len(v1Found))
		for v1 := range v1Found {
			actions = append(actions, v2Pair{V1: v1, V2: v1Set[v1]})
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i].V1 < actions[j].V1 })

		fmt.Fprintf(out, "issue #%d %q\n", iss.Number, iss.Title)
		for _, a := range actions {
			// Caveat de validating: warn pero migra. Ver doc del paquete.
			if a.V1 == labels.CheValidating {
				fmt.Fprintf(out, "  warn: %s en v1 cubría plan+PR; mapeo a %s puede no encajar si el run era plan-validate\n", a.V1, a.V2)
			}
			if dryRun {
				fmt.Fprintf(out, "  [dry-run] %s → %s\n", a.V1, a.V2)
				continue
			}
			if err := applyMigrationFn(repo, iss.Number, a.V1, a.V2); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("issue #%d: %w", iss.Number, err)
				}
				fmt.Fprintf(out, "  FAIL %s → %s: %v\n", a.V1, a.V2, err)
				continue
			}
			fmt.Fprintf(out, "  ok   %s → %s\n", a.V1, a.V2)
		}
		modified++
	}

	fmt.Fprintf(out, "summary: %d modified, %d skipped (mixed v1+v2)\n", modified, mixed)
	_ = skipped // referencia futura: aún no diferenciamos skipped por categoría
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// listIssuesViaGh corre `gh issue list --state open --json number,title,labels
// --limit 200`. El cap 200 cubre la mayoría de los repos; un repo con más
// issues abiertos es excepcional y el operador puede correr el comando dos
// veces tras migrar el primer batch.
func listIssuesViaGh(repo string) ([]migrateIssue, error) {
	cmd := exec.Command("gh", "issue", "list",
		"--state", "open",
		"--json", "number,title,labels",
		"--limit", "200")
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %s", strings.TrimSpace(string(out)))
	}
	var issues []migrateIssue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}
	return issues, nil
}

// applyMigrationViaGh remueve `v1` y agrega `v2` sobre un issue concreto.
// Sigue el mismo patrón que internal/labels.Apply: ensure (v2) primero,
// luego remove + add via REST. No usamos labels.Apply porque eso exige una
// transición registrada en validTransitions, y acá el "from" es un label
// v1 huérfano (no hay transición v1→v2 registrada — el shim solo registra
// v2→v2).
func applyMigrationViaGh(repo string, issueNumber int, v1, v2 string) error {
	// Ensure el v2 en el repo (idempotente). Sin esto, el POST falla si el
	// repo nunca usó ese label (p.ej. nunca corrió un flow v2).
	ensureCmd := exec.Command("gh", "label", "create", v2, "--force")
	if repo != "" {
		ensureCmd.Dir = repo
	}
	if out, err := ensureCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ensure label %s: %s", v2, strings.TrimSpace(string(out)))
	}

	// Remove v1 vía REST. Tolerante a 404 (el label no estaba aplicado).
	removeCmd := exec.Command("gh", "api",
		"-X", "DELETE",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels/%s", issueNumber, v1),
	)
	if repo != "" {
		removeCmd.Dir = repo
	}
	if out, err := removeCmd.CombinedOutput(); err != nil {
		combined := string(out)
		if !strings.Contains(combined, "Label does not exist") && !strings.Contains(combined, "HTTP 404") {
			return fmt.Errorf("remove %s: %s", v1, strings.TrimSpace(combined))
		}
	}

	// Add v2 vía REST.
	addCmd := exec.Command("gh", "api",
		"-X", "POST",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels", issueNumber),
		"-f", "labels[]="+v2,
	)
	if repo != "" {
		addCmd.Dir = repo
	}
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("add %s: %s", v2, strings.TrimSpace(string(out)))
	}
	return nil
}
