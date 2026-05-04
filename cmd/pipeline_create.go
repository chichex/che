package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chichex/che/internal/agentregistry"
	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var pipelineCreateForce bool

var pipelineCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "crea un pipeline con un wizard interactivo",
	Long: `create abre un wizard paso a paso para generar un pipeline JSON válido:
nombre, clonado opcional, entry agent opcional, steps, aggregator y confirmación
con preview tipo simulate antes de guardar en .che/pipelines/<name>.json.

A diferencia de pipeline clone, create puede construir el pipeline desde cero
interactivamente; su opción de clonar sólo copia una fuente elegida dentro del
wizard y no aplica sustituciones --replace.`,
	Args: cobra.MaximumNArgs(1),
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

		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		prompt := newPipelineCreateStdioPrompt(cmd.InOrStdin(), cmd.ErrOrStderr())
		return runPipelineCreate(cmd.OutOrStdout(), mgr, reg.All(), prompt, name, pipelineCreateForce)
	},
}

func init() {
	pipelineCreateCmd.Flags().BoolVar(&pipelineCreateForce, "force", false,
		"sobrescribe el archivo si ya existe")
	pipelineCmd.AddCommand(pipelineCreateCmd)
}

type pipelineCreatePrompt interface {
	Ask(label, def string) (string, error)
	Confirm(label string, def bool) (bool, error)
	Choose(label string, options []pipelineCreateOption) (int, error)
	MultiChoose(label string, options []pipelineCreateOption) ([]int, error)
}

type pipelineCreateOption struct {
	Label string
	Hint  string
}

type pipelineCreateStdioPrompt struct {
	in  *bufio.Reader
	out io.Writer
}

func newPipelineCreateStdioPrompt(in io.Reader, out io.Writer) *pipelineCreateStdioPrompt {
	return &pipelineCreateStdioPrompt{in: bufio.NewReader(in), out: out}
}

func (p *pipelineCreateStdioPrompt) Ask(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	text, err := p.in.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return def, nil
	}
	return text, nil
}

func (p *pipelineCreateStdioPrompt) Confirm(label string, def bool) (bool, error) {
	suffix := "y/N"
	if def {
		suffix = "Y/n"
	}
	for {
		ans, err := p.Ask(label+" ("+suffix+")", "")
		if err != nil {
			return false, err
		}
		if ans == "" {
			return def, nil
		}
		switch strings.ToLower(ans) {
		case "y", "yes", "s", "si", "sí":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "respondé y/n")
	}
}

func (p *pipelineCreateStdioPrompt) Choose(label string, options []pipelineCreateOption) (int, error) {
	for {
		fmt.Fprintln(p.out, label)
		for i, opt := range options {
			fmt.Fprintf(p.out, "  %d. %s", i+1, opt.Label)
			if opt.Hint != "" {
				fmt.Fprintf(p.out, " — %s", opt.Hint)
			}
			fmt.Fprintln(p.out)
		}
		ans, err := p.Ask("número", "")
		if err != nil {
			return -1, err
		}
		idx, err := strconv.Atoi(ans)
		if err == nil && idx >= 1 && idx <= len(options) {
			return idx - 1, nil
		}
		fmt.Fprintln(p.out, "opción inválida")
	}
}

func (p *pipelineCreateStdioPrompt) MultiChoose(label string, options []pipelineCreateOption) ([]int, error) {
	for {
		fmt.Fprintln(p.out, label)
		for i, opt := range options {
			fmt.Fprintf(p.out, "  %d. %s", i+1, opt.Label)
			if opt.Hint != "" {
				fmt.Fprintf(p.out, " — %s", opt.Hint)
			}
			fmt.Fprintln(p.out)
		}
		ans, err := p.Ask("números separados por coma", "")
		if err != nil {
			return nil, err
		}
		parts := strings.Split(ans, ",")
		seen := map[int]bool{}
		var picked []int
		valid := true
		for _, part := range parts {
			idx, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || idx < 1 || idx > len(options) {
				valid = false
				break
			}
			if !seen[idx-1] {
				seen[idx-1] = true
				picked = append(picked, idx-1)
			}
		}
		if valid && len(picked) > 0 {
			return picked, nil
		}
		fmt.Fprintln(p.out, "selección inválida")
	}
}

func runPipelineCreate(out io.Writer, mgr *pipeline.Manager, agents []agentregistry.Agent, prompt pipelineCreatePrompt, name string, force bool) error {
	if prompt == nil {
		return fmt.Errorf("pipeline create prompt no puede ser nil")
	}
	if len(agents) == 0 {
		return fmt.Errorf("agent registry vacío: no hay agentes para seleccionar")
	}
	name, promptedName, err := promptPipelineName(prompt, mgr, name, force)
	if err != nil {
		return err
	}
	dest := filepath.Join(mgr.PipelinesDir(), name+".json")
	destExists := pipelineFileExists(dest)
	if destExists && !force && !promptedName {
		return fmt.Errorf("%s ya existe — pasá --force para sobrescribir", dest)
	}

	created, err := promptPipeline(prompt, mgr, agents)
	if err != nil {
		return err
	}
	if err := pipeline.Validate(created); err != nil {
		return fmt.Errorf("pipeline generado inválido: %s", formatLoadError(err))
	}
	if err := pipeline.ValidateAgents(created, func(agentName string) bool {
		for _, a := range agents {
			if a.Name == agentName {
				return true
			}
		}
		return false
	}); err != nil {
		return fmt.Errorf("pipeline generado inválido: %s", formatLoadError(err))
	}

	lookup := func(agentName string) (agentInfo, bool) {
		for _, a := range agents {
			if a.Name == agentName {
				return agentInfo{Name: a.Name, Model: a.Model, Source: string(a.Source), Path: a.Path}, true
			}
		}
		return agentInfo{}, false
	}
	if err := renderPipelinePreview(out, pipelinePreviewHeader{
		Name:         name,
		Source:       "<wizard preview>",
		ShowComments: true,
	}, created, lookup); err != nil {
		return err
	}
	if destExists {
		overwrite, err := prompt.Confirm(fmt.Sprintf("sobrescribir %s", dest), false)
		if err != nil {
			return err
		}
		if !overwrite {
			fmt.Fprintln(out, "cancelado: no se sobrescribió ningún archivo")
			return nil
		}
	}
	save, err := prompt.Confirm("guardar pipeline", true)
	if err != nil {
		return err
	}
	if !save {
		fmt.Fprintln(out, "cancelado: no se guardó ningún archivo")
		return nil
	}
	if err := savePipelineFile(dest, created); err != nil {
		return fmt.Errorf("escribir %s: %w", dest, err)
	}
	fmt.Fprintf(out, "creado %s\n", dest)
	return nil
}

func promptPipelineName(prompt pipelineCreatePrompt, mgr *pipeline.Manager, name string, force bool) (string, bool, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		return name, false, nil
	}
	for {
		asked, err := prompt.Ask("nombre del pipeline", "")
		if err != nil {
			return "", true, err
		}
		asked = strings.TrimSpace(asked)
		if asked == "" {
			return "", true, fmt.Errorf("pipeline name no puede ser vacío")
		}
		if force || !pipelineFileExists(filepath.Join(mgr.PipelinesDir(), asked+".json")) {
			return asked, true, nil
		}
	}
}

func pipelineFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func promptPipeline(prompt pipelineCreatePrompt, mgr *pipeline.Manager, agents []agentregistry.Agent) (pipeline.Pipeline, error) {
	if prompt != nil {
		clone, err := prompt.Confirm("clonar desde un pipeline existente", false)
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		if clone {
			return promptPipelineCloneSource(prompt, mgr)
		}
	}

	agentOptions := pipelineCreateAgentOptions(agents)
	created := pipeline.Pipeline{Version: pipeline.CurrentVersion}
	entry, err := prompt.Confirm("agregar entry agent", false)
	if err != nil {
		return pipeline.Pipeline{}, err
	}
	if entry {
		idx, err := prompt.Choose("entry agent", agentOptions)
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		created.Entry = &pipeline.Entry{Agents: []string{agents[idx].Name}}
	}

	for {
		stepName, err := promptStepName(prompt)
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		picked, err := prompt.MultiChoose("agentes del step", agentOptions)
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		step := pipeline.Step{Name: stepName, Agents: make([]string, 0, len(picked))}
		for _, idx := range picked {
			step.Agents = append(step.Agents, agents[idx].Name)
		}
		if len(step.Agents) > 1 {
			aggIdx, err := prompt.Choose("aggregator", pipelineCreateAggregatorOptions())
			if err != nil {
				return pipeline.Pipeline{}, err
			}
			step.Aggregator = pipeline.ValidAggregators[aggIdx]
		}
		comment, err := prompt.Ask("comment del step", pipelineCreateDefaultStepComment(step))
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		step.Comment = strings.TrimSpace(comment)
		created.Steps = append(created.Steps, step)

		more, err := prompt.Confirm("agregar otro step", false)
		if err != nil {
			return pipeline.Pipeline{}, err
		}
		if !more {
			break
		}
	}
	return created, nil
}

func promptStepName(prompt pipelineCreatePrompt) (string, error) {
	for {
		stepName, err := prompt.Ask("nombre del step ([a-z_][a-z0-9_]*)", "")
		if err != nil {
			return "", err
		}
		stepName = strings.TrimSpace(stepName)
		if err := validatePromptStepName(stepName); err == nil {
			return stepName, nil
		}
	}
}

func validatePromptStepName(name string) error {
	return pipeline.Validate(pipeline.Pipeline{
		Version: pipeline.CurrentVersion,
		Steps: []pipeline.Step{{
			Name:   name,
			Agents: []string{"_wizard_placeholder"},
		}},
	})
}

func promptPipelineCloneSource(prompt pipelineCreatePrompt, mgr *pipeline.Manager) (pipeline.Pipeline, error) {
	names := append([]string{"default"}, mgr.List()...)
	options := make([]pipelineCreateOption, len(names))
	for i, name := range names {
		options[i] = pipelineCreateOption{Label: name}
	}
	idx, err := prompt.Choose("pipeline origen", options)
	if err != nil {
		return pipeline.Pipeline{}, err
	}
	cloned, _, err := lookupPipelineForShow(mgr, names[idx])
	if err != nil {
		return pipeline.Pipeline{}, fmt.Errorf("leyendo src %q: %s", names[idx], formatLoadError(err))
	}
	return cloned, nil
}

func pipelineCreateAgentOptions(agents []agentregistry.Agent) []pipelineCreateOption {
	options := make([]pipelineCreateOption, len(agents))
	for i, a := range agents {
		hint := string(a.Source)
		if a.Model != "" {
			hint += ", model " + a.Model
		}
		if a.Description != "" {
			hint += ": " + truncate(a.Description, 70)
		}
		options[i] = pipelineCreateOption{Label: a.Name, Hint: hint}
	}
	return options
}

func pipelineCreateAggregatorOptions() []pipelineCreateOption {
	options := make([]pipelineCreateOption, len(pipeline.ValidAggregators))
	for i, agg := range pipeline.ValidAggregators {
		options[i] = pipelineCreateOption{Label: string(agg), Hint: agg.Description()}
	}
	return options
}

func pipelineCreateDefaultStepComment(step pipeline.Step) string {
	if len(step.Agents) > 1 {
		return fmt.Sprintf("%s corre %d agentes y resuelve markers con %s", step.Name, len(step.Agents), step.Aggregator)
	}
	return fmt.Sprintf("%s corre %s", step.Name, step.Agents[0])
}
