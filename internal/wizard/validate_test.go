package wizard

import (
	"strings"
	"testing"
)

// withInstalledCLIs reemplaza la deteccion runtime por una lista fija para
// que los tests no dependan de PATH ni del harness completo. Devuelve una
// fn de cleanup que restaura el original.
func withInstalledCLIs(t *testing.T, names ...string) {
	t.Helper()
	orig := detectInstalledCLIs
	detectInstalledCLIs = func() []string { return append([]string(nil), names...) }
	t.Cleanup(func() { detectInstalledCLIs = orig })
}

func validStep(name string) Step {
	return Step{Name: name, CLI: "claude", Kind: KindPrompt, Content: "hola", Input: InputText}
}

func TestIsValid_OK(t *testing.T) {
	withInstalledCLIs(t, "claude", "codex")
	p := Pipeline{
		Name:  "demo",
		Steps: []Step{validStep("collect"), {Name: "digest", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputPreviousOutput}},
	}
	if err := IsValid(p); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestIsValid_EmptyName(t *testing.T) {
	withInstalledCLIs(t, "claude")
	p := Pipeline{Steps: []Step{validStep("a")}}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nombre del pipeline") {
		t.Errorf("expected name-empty msg, got: %v", err)
	}
}

func TestIsValid_NoSteps(t *testing.T) {
	withInstalledCLIs(t, "claude")
	err := IsValid(Pipeline{Name: "demo"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "al menos un step") {
		t.Errorf("expected no-steps msg, got: %v", err)
	}
}

func TestIsValid_DuplicateStepName(t *testing.T) {
	withInstalledCLIs(t, "claude")
	p := Pipeline{
		Name:  "demo",
		Steps: []Step{validStep("dup"), validStep("dup")},
	}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "duplica") {
		t.Errorf("expected duplicate msg, got: %v", err)
	}
}

func TestIsValid_PreviousOutputOnFirstStep(t *testing.T) {
	withInstalledCLIs(t, "claude")
	p := Pipeline{
		Name: "demo",
		Steps: []Step{
			{Name: "first", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputPreviousOutput},
		},
	}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "previous_output") {
		t.Errorf("expected previous_output msg, got: %v", err)
	}
	if !strings.Contains(err.Error(), "primer step") {
		t.Errorf("msg should mention primer step, got: %v", err)
	}
}

func TestIsValid_StepCLINotInstalled(t *testing.T) {
	withInstalledCLIs(t, "claude") // codex deliberately missing
	p := Pipeline{
		Name: "demo",
		Steps: []Step{
			{Name: "uses-codex", CLI: "codex", Kind: KindPrompt, Content: "x", Input: InputText},
		},
	}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "codex no esta instalado") {
		t.Errorf("expected step-cli-not-installed msg, got: %v", err)
	}
}

func TestIsValid_ValidatorCLINotInstalled(t *testing.T) {
	withInstalledCLIs(t, "claude") // gemini missing
	p := Pipeline{
		Name: "demo",
		Steps: []Step{
			{
				Name:    "first",
				CLI:     "claude",
				Kind:    KindPrompt,
				Content: "x",
				Input:   InputText,
				Validator: &Validator{
					CLI:     "gemini",
					Kind:    KindPrompt,
					Content: "verifica",
				},
				MaxLoops:   3,
				OnMaxLoops: OnMaxLoopsFail,
			},
		},
	}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "validator gemini no esta instalado") {
		t.Errorf("expected validator-cli-not-installed msg, got: %v", err)
	}
}

func TestIsValid_MultipleErrorsAggregated(t *testing.T) {
	withInstalledCLIs(t) // ningun CLI instalado
	p := Pipeline{
		// nombre vacio + step con CLI no instalado + previous_output en step 0
		Steps: []Step{
			{Name: "first", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputPreviousOutput},
		},
	}
	err := IsValid(p)
	if err == nil {
		t.Fatal("expected error")
	}
	lines := validationLines(err)
	if len(lines) < 3 {
		t.Errorf("expected ≥3 error lines, got %d: %v", len(lines), lines)
	}
}

func TestValidationLines_NilReturnsNil(t *testing.T) {
	if got := validationLines(nil); got != nil {
		t.Errorf("expected nil for nil err, got %v", got)
	}
}
