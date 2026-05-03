package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/pipeline"
	"github.com/chichex/che/internal/pipelinelabels"
)

type fakePipelineLabelClient struct {
	ensured []string
	deleted []string
	added   map[int][]string
	removed map[int][]string
	labels  map[int][]string
	search  map[string][]pipelineLabelRef
}

func (f *fakePipelineLabelClient) EnsureLabel(name string, skipExisting bool) error {
	f.ensured = append(f.ensured, name)
	return nil
}

func (f *fakePipelineLabelClient) DeleteRepoLabel(name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakePipelineLabelClient) SearchRefsWithLabel(name string) ([]pipelineLabelRef, bool, error) {
	return append([]pipelineLabelRef(nil), f.search[name]...), false, nil
}

func (f *fakePipelineLabelClient) AddLabels(number int, names ...string) error {
	if f.added == nil {
		f.added = map[int][]string{}
	}
	f.added[number] = append(f.added[number], names...)
	return nil
}

func (f *fakePipelineLabelClient) RemoveLabel(number int, name string) error {
	if f.removed == nil {
		f.removed = map[int][]string{}
	}
	f.removed[number] = append(f.removed[number], name)
	return nil
}

func (f *fakePipelineLabelClient) IssueLabels(number int) ([]string, error) {
	return append([]string(nil), f.labels[number]...), nil
}

func TestPipelineInitLabels_ExpectedLabels(t *testing.T) {
	p := pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{
		{Name: "explore", Agents: []string{"claude-opus"}},
		{Name: "execute", Agents: []string{"claude-opus"}},
	}}
	fake := &fakePipelineLabelClient{}
	var out bytes.Buffer
	if err := runPipelineInitLabels(&out, fake, p, true); err != nil {
		t.Fatalf("runPipelineInitLabels: %v", err)
	}
	want := []string{
		pipelinelabels.StateLabel("explore"),
		pipelinelabels.ApplyingLabel("explore"),
		pipelinelabels.StateLabel("execute"),
		pipelinelabels.ApplyingLabel("execute"),
	}
	if !reflect.DeepEqual(fake.ensured, want) {
		t.Fatalf("ensured = %#v, want %#v", fake.ensured, want)
	}
	for _, label := range want {
		if !strings.Contains(out.String(), label) {
			t.Errorf("output missing %s: %q", label, out.String())
		}
	}
}

func TestDefaultPipelineMigrationPairs(t *testing.T) {
	pairs := defaultPipelineMigrationPairs()
	want := []pipelineLabelPair{
		{Old: v1CheIdea, New: pipelinelabels.StateLabel("idea")},
		{Old: v1ChePlanning, New: pipelinelabels.ApplyingLabel("explore")},
		{Old: v1ChePlan, New: pipelinelabels.StateLabel("explore")},
		{Old: v1CheExecuting, New: pipelinelabels.ApplyingLabel("execute")},
		{Old: v1CheExecuted, New: pipelinelabels.StateLabel("execute")},
		{Old: v1CheValidating, NewIssue: pipelinelabels.ApplyingLabel("validate_issue"), NewPR: pipelinelabels.ApplyingLabel("validate_pr")},
		{Old: v1CheValidated, NewIssue: pipelinelabels.StateLabel("validate_issue"), NewPR: pipelinelabels.StateLabel("validate_pr")},
		{Old: v1CheClosing, New: pipelinelabels.ApplyingLabel("close")},
		{Old: v1CheClosed, New: pipelinelabels.StateLabel("close")},
		{Old: labels.ValidatedApprove},
		{Old: labels.ValidatedChangesRequested},
		{Old: labels.ValidatedNeedsHuman},
		{Old: labels.PlanValidatedApprove},
		{Old: labels.PlanValidatedChangesRequested},
		{Old: labels.PlanValidatedNeedsHuman},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("pairs = %#v, want %#v", pairs, want)
	}
}

func TestMigrationPairsForPipeline_MapOverrideAndValidation(t *testing.T) {
	p := pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{
		{Name: "triage", Agents: []string{"claude-opus"}},
	}}
	pairs, err := migrationPairsForPipeline(p, []string{v1CheIdea + "=che:state:triage"})
	if err != nil {
		t.Fatalf("migrationPairsForPipeline: %v", err)
	}
	if pairs[0] != (pipelineLabelPair{Old: v1CheIdea, New: pipelinelabels.StateLabel("triage")}) {
		t.Fatalf("override did not replace first pair: %#v", pairs[0])
	}
	if _, err := migrationPairsForPipeline(p, []string{v1CheIdea + "=che:state:missing"}); err == nil {
		t.Fatalf("expected error for unknown step")
	}
}

func TestRunPipelineMigrateLabels_AppliesRefsAndPreservesOldByDefault(t *testing.T) {
	fake := &fakePipelineLabelClient{
		search: map[string][]pipelineLabelRef{
			v1ChePlan:               {{Number: 12}},
			labels.ValidatedApprove: {{Number: 12}, {Number: 13}},
		},
	}
	pairs := []pipelineLabelPair{
		{Old: v1ChePlan, New: pipelinelabels.StateLabel("explore")},
		{Old: labels.ValidatedApprove},
	}
	var out bytes.Buffer
	if err := runPipelineMigrateLabels(&out, fake, pairs, false, false); err != nil {
		t.Fatalf("runPipelineMigrateLabels: %v", err)
	}
	if !reflect.DeepEqual(fake.ensured, []string{pipelinelabels.StateLabel("explore")}) {
		t.Fatalf("ensured = %#v", fake.ensured)
	}
	if !reflect.DeepEqual(fake.added[12], []string{pipelinelabels.StateLabel("explore")}) {
		t.Fatalf("added[12] = %#v", fake.added[12])
	}
	wantRemoved12 := []string{v1ChePlan, labels.ValidatedApprove}
	if !reflect.DeepEqual(fake.removed[12], wantRemoved12) {
		t.Fatalf("removed[12] = %#v, want %#v", fake.removed[12], wantRemoved12)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted = %#v", fake.deleted)
	}
	if !strings.Contains(out.String(), "preview:") || !strings.Contains(out.String(), "ok: che:plan") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestRunPipelineMigrateLabels_DeleteOldOptIn(t *testing.T) {
	fake := &fakePipelineLabelClient{search: map[string][]pipelineLabelRef{v1ChePlan: {{Number: 12}}}}
	var out bytes.Buffer
	if err := runPipelineMigrateLabels(&out, fake, []pipelineLabelPair{{Old: v1ChePlan, New: pipelinelabels.StateLabel("explore")}}, false, true); err != nil {
		t.Fatalf("runPipelineMigrateLabels: %v", err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{v1ChePlan}) {
		t.Fatalf("deleted = %#v", fake.deleted)
	}
}

func TestRunPipelineMigrateLabels_ValidateMappingUsesRefKind(t *testing.T) {
	fake := &fakePipelineLabelClient{
		search: map[string][]pipelineLabelRef{
			v1CheValidated: {{Number: 10}, {Number: 11, IsPR: true}},
		},
	}
	var out bytes.Buffer
	pair := pipelineLabelPair{
		Old:      v1CheValidated,
		NewIssue: pipelinelabels.StateLabel("validate_issue"),
		NewPR:    pipelinelabels.StateLabel("validate_pr"),
	}
	if err := runPipelineMigrateLabels(&out, fake, []pipelineLabelPair{pair}, false, false); err != nil {
		t.Fatalf("runPipelineMigrateLabels: %v", err)
	}
	if !reflect.DeepEqual(fake.added[10], []string{pipelinelabels.StateLabel("validate_issue")}) {
		t.Fatalf("issue added = %#v", fake.added[10])
	}
	if !reflect.DeepEqual(fake.added[11], []string{pipelinelabels.StateLabel("validate_pr")}) {
		t.Fatalf("pr added = %#v", fake.added[11])
	}
}

func TestRunPipelineReset_RemovesLockApplyingAndSetsFrom(t *testing.T) {
	p := pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{
		{Name: "explore", Agents: []string{"claude-opus"}},
	}}
	fake := &fakePipelineLabelClient{labels: map[int][]string{7: {
		pipelinelabels.LockLabelAt(time.Unix(1, 0), 42, "host"),
		pipelinelabels.ApplyingLabel("explore"),
		pipelinelabels.StateLabel("explore"),
		pipelinelabels.StateLabel("idea"),
		"unrelated",
	}}}
	var out bytes.Buffer
	if err := runPipelineReset(&out, fake, p, 7, "explore"); err != nil {
		t.Fatalf("runPipelineReset: %v", err)
	}
	wantRemoved := []string{
		pipelinelabels.LockLabelAt(time.Unix(1, 0), 42, "host"),
		pipelinelabels.ApplyingLabel("explore"),
		pipelinelabels.StateLabel("idea"),
	}
	if !reflect.DeepEqual(fake.removed[7], wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", fake.removed[7], wantRemoved)
	}
	if !reflect.DeepEqual(fake.added[7], []string{pipelinelabels.StateLabel("explore")}) {
		t.Fatalf("added = %#v", fake.added[7])
	}
	if !strings.Contains(out.String(), "set: che:state:explore") {
		t.Fatalf("output = %q", out.String())
	}
}
