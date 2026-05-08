package wizard

import (
	"strings"
	"testing"
	"time"
)

func TestMarshalDraftIncludesStatus(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-05-07T14:32:11-03:00")
	p := Pipeline{
		Status: &Status{
			Stage:       StageInfo,
			LastSavedAt: at,
		},
		Name:        "triage-checkout-flow",
		Description: "Toma una metrica anomala y dispara un triage.",
	}
	got, err := Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"status:",
		"stage: info",
		"last_saved_at:",
		"name: triage-checkout-flow",
		"description: Toma una metrica anomala",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in YAML, got:\n%s", want, s)
		}
	}
	if strings.Contains(s, "step_idx") {
		t.Errorf("step_idx should be omitted when not set, got:\n%s", s)
	}
}

func TestMarshalReadyOmitsStatus(t *testing.T) {
	p := Pipeline{
		Name: "ready-pipeline",
		Steps: []Step{
			{Name: "collect", CLI: "claude", Kind: "skill", Content: "collect-signals", Input: "text"},
		},
	}
	got, err := Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "status:") {
		t.Errorf("ready pipeline should not have status block, got:\n%s", s)
	}
	if !strings.Contains(s, "name: ready-pipeline") {
		t.Errorf("expected name line, got:\n%s", s)
	}
	if !strings.Contains(s, "- name: collect") {
		t.Errorf("expected step entry, got:\n%s", s)
	}
}

func TestRoundTripDraft(t *testing.T) {
	p := Pipeline{
		Status: &Status{Stage: StageStep, StepIdx: 1, StepMode: "create", LastSavedAt: time.Unix(1700000000, 0).UTC()},
		Name:   "x",
		Steps: []Step{
			{Name: "s1", CLI: "claude", Kind: "prompt", Content: "hola", Input: "text"},
		},
	}
	data, err := Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Status == nil {
		t.Fatal("round trip lost Status")
	}
	if got.Status.Stage != StageStep {
		t.Errorf("Stage: got %q want %q", got.Status.Stage, StageStep)
	}
	if got.Status.StepIdx != 1 {
		t.Errorf("StepIdx: got %d want 1", got.Status.StepIdx)
	}
	if got.Status.StepMode != "create" {
		t.Errorf("StepMode: got %q want create", got.Status.StepMode)
	}
	if len(got.Steps) != 1 || got.Steps[0].Name != "s1" {
		t.Errorf("steps round-trip lost data: %+v", got.Steps)
	}
}
