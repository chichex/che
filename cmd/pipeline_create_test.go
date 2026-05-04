package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/agentregistry"
	"github.com/chichex/che/internal/pipeline"
)

type fakePipelineCreatePrompt struct {
	asks     []string
	confirms []bool
	choices  []int
	multis   [][]int
	labels   []string
}

func (f *fakePipelineCreatePrompt) Ask(label, def string) (string, error) {
	f.labels = append(f.labels, "ask:"+label)
	if len(f.asks) == 0 {
		return def, nil
	}
	ans := f.asks[0]
	f.asks = f.asks[1:]
	if ans == "" {
		return def, nil
	}
	return ans, nil
}

func (f *fakePipelineCreatePrompt) Confirm(label string, def bool) (bool, error) {
	f.labels = append(f.labels, "confirm:"+label)
	if len(f.confirms) == 0 {
		return def, nil
	}
	ans := f.confirms[0]
	f.confirms = f.confirms[1:]
	return ans, nil
}

func (f *fakePipelineCreatePrompt) Choose(label string, options []pipelineCreateOption) (int, error) {
	f.labels = append(f.labels, "choose:"+label)
	if len(f.choices) == 0 {
		return 0, nil
	}
	ans := f.choices[0]
	f.choices = f.choices[1:]
	return ans, nil
}

func (f *fakePipelineCreatePrompt) MultiChoose(label string, options []pipelineCreateOption) ([]int, error) {
	f.labels = append(f.labels, "multi:"+label)
	if len(f.multis) == 0 {
		return []int{0}, nil
	}
	ans := f.multis[0]
	f.multis = f.multis[1:]
	return ans, nil
}

func TestPipelineCreate_BuildsAndSavesWizardPipeline(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	agents := []agentregistry.Agent{
		{Name: "claude-opus", Model: "opus", Source: agentregistry.SourceBuiltin},
		{Name: "reviewer", Model: "sonnet", Source: agentregistry.SourceProject, Description: "reviews plans"},
	}
	prompt := &fakePipelineCreatePrompt{
		confirms: []bool{false, true, false, true},
		asks:     []string{"gate", "review gate"},
		choices:  []int{1, 2},
		multis:   [][]int{{0, 1}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "custom", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}

	dest := filepath.Join(root, ".che", "pipelines", "custom.json")
	got, err := pipeline.Load(dest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Entry == nil || got.Entry.Agents[0] != "reviewer" {
		t.Fatalf("entry drift: %#v", got.Entry)
	}
	if len(got.Steps) != 1 || got.Steps[0].Name != "gate" {
		t.Fatalf("steps drift: %#v", got.Steps)
	}
	if got.Steps[0].Aggregator != pipeline.AggregatorFirstBlocker {
		t.Errorf("aggregator = %q, want first_blocker", got.Steps[0].Aggregator)
	}
	if got.Steps[0].Comment != "review gate" {
		t.Errorf("comment = %q", got.Steps[0].Comment)
	}
	if !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "custom.json") {
		t.Errorf("stdout no muestra preview + path: %q", out.String())
	}
	wantLabels := []string{
		"confirm:clonar desde un pipeline existente",
		"confirm:agregar entry agent",
		"choose:entry agent",
		"ask:nombre del step ([a-z_][a-z0-9_]*)",
		"multi:agentes del step",
		"choose:aggregator",
		"ask:comment del step",
		"confirm:agregar otro step",
		"confirm:guardar pipeline",
	}
	if strings.Join(prompt.labels, "|") != strings.Join(wantLabels, "|") {
		t.Errorf("prompt labels drift:\n got %v\nwant %v", prompt.labels, wantLabels)
	}
}

func TestPipelineCreate_PromptedNameRetriesExisting(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{"taken": minimalPipeline}, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		asks:     []string{"taken", "fresh", "idea", ""},
		confirms: []bool{false, false, false, true},
		multis:   [][]int{{0}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	if _, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "fresh.json")); err != nil {
		t.Fatalf("fresh pipeline was not saved: %v", err)
	}
	if containsLabel(prompt.labels, "confirm:sobrescribir ") {
		t.Errorf("prompted name retry should avoid overwrite confirmation: %v", prompt.labels)
	}
	if got := countLabels(prompt.labels, "ask:nombre del pipeline"); got != 2 {
		t.Errorf("pipeline name prompts = %d, want 2; labels=%v", got, prompt.labels)
	}
}

func TestPipelineCreate_PromptedNameRetriesInvalid(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		asks:     []string{"../evil", "fresh", "idea", ""},
		confirms: []bool{false, false, false, true},
		multis:   [][]int{{0}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	if _, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "fresh.json")); err != nil {
		t.Fatalf("fresh pipeline was not saved: %v", err)
	}
	if _, err := pipeline.Load(filepath.Join(root, ".che", "evil.json")); err == nil {
		t.Fatalf("create escribió fuera de .che/pipelines")
	}
	if got := countLabels(prompt.labels, "ask:nombre del pipeline"); got != 2 {
		t.Errorf("pipeline name prompts = %d, want 2; labels=%v", got, prompt.labels)
	}
}

func TestPipelineCreate_StepNameRetriesUntilValid(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		asks:     []string{"Bad Name", "valid_step", ""},
		confirms: []bool{false, false, false, true},
		multis:   [][]int{{0}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "validated", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	got, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "validated.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Steps[0].Name != "valid_step" {
		t.Fatalf("step name = %q, want valid_step", got.Steps[0].Name)
	}
	if got := countLabels(prompt.labels, "ask:nombre del step ([a-z_][a-z0-9_]*)"); got != 2 {
		t.Errorf("step name prompts = %d, want 2; labels=%v", got, prompt.labels)
	}
}

func TestPipelineCreate_StepNameClosedStdinReturnsError(t *testing.T) {
	prompt := newPipelineCreateStdioPrompt(strings.NewReader(""), &bytes.Buffer{})

	_, err := promptStepName(prompt)
	if err == nil {
		t.Fatalf("esperaba error con stdin cerrado, got nil")
	}
	if !strings.Contains(err.Error(), "stdin cerrado/no interactivo") {
		t.Fatalf("error = %q, want stdin cerrado/no interactivo", err.Error())
	}
}

func TestPipelineCreate_CancelDoesNotSave(t *testing.T) {
	mgr, root := pipelineFixture(t, nil, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		confirms: []bool{false, false, false, false},
		asks:     []string{"idea", ""},
		multis:   [][]int{{0}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "cancelled", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	if _, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "cancelled.json")); err == nil {
		t.Fatalf("archivo guardado aunque el usuario canceló")
	}
	if !strings.Contains(out.String(), "cancelado") {
		t.Errorf("stdout no menciona cancelación: %q", out.String())
	}
}

func TestPipelineCreate_ConflictWithoutForce(t *testing.T) {
	mgr, _ := pipelineFixture(t, map[string]string{"taken": minimalPipeline}, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	var out bytes.Buffer
	err := runPipelineCreate(&out, mgr, agents, &fakePipelineCreatePrompt{}, "taken", false)
	if err == nil {
		t.Fatalf("esperaba error, got nil")
	}
	if !strings.Contains(err.Error(), "ya existe") {
		t.Errorf("error no menciona conflicto: %v", err)
	}
}

func TestPipelineCreate_ForceExistingRequiresOverwriteConfirm(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{"taken": minimalPipeline}, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		confirms: []bool{false, false, false},
		asks:     []string{"idea", ""},
		multis:   [][]int{{0}},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "taken", true); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	got, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "taken.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Name != "explore" {
		t.Fatalf("archivo fue sobrescrito aunque overwrite=false: %#v", got)
	}
	if !strings.Contains(out.String(), "no se sobrescribió") {
		t.Errorf("stdout no menciona cancelación de overwrite: %q", out.String())
	}
	if !containsLabel(prompt.labels, "confirm:sobrescribir ") {
		t.Errorf("no pidió confirmación de overwrite: %v", prompt.labels)
	}
}

func TestPipelineCreate_CloneExistingPipeline(t *testing.T) {
	mgr, root := pipelineFixture(t, map[string]string{"src": minimalPipeline}, "")
	agents := []agentregistry.Agent{{Name: "claude-opus", Source: agentregistry.SourceBuiltin}}
	prompt := &fakePipelineCreatePrompt{
		confirms: []bool{true, true},
		choices:  []int{1},
	}
	var out bytes.Buffer
	if err := runPipelineCreate(&out, mgr, agents, prompt, "copy", false); err != nil {
		t.Fatalf("runPipelineCreate: %v", err)
	}
	got, err := pipeline.Load(filepath.Join(root, ".che", "pipelines", "copy.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Name != "explore" {
		t.Fatalf("clone drift: %#v", got)
	}
}

func containsLabel(labels []string, prefix string) bool {
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

func countLabels(labels []string, want string) int {
	count := 0
	for _, label := range labels {
		if label == want {
			count++
		}
	}
	return count
}
