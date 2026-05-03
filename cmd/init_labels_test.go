package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunInitLabels_DryRun: el dry-run lista los labels esperados y NO
// invoca shell-out. Como Default() es lo que se resuelve sin config, el
// fixture es vacío.
func TestRunInitLabels_DryRun(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out, errOut bytes.Buffer
	if err := runInitLabels(&out, &errOut, mgr, "", true); err != nil {
		t.Fatalf("runInitLabels: %v", err)
	}
	got := out.String()
	wantSubstr := []string{
		"pipeline:",
		"source:",
		"labels esperados",
		"che:state:idea",
		"validated:approve",
		"plan-validated:approve",
		"ct:plan",
		"[dry-run]",
	}
	for _, s := range wantSubstr {
		if !strings.Contains(got, s) {
			t.Errorf("output sin %q:\n%s", s, got)
		}
	}
}

// TestRunInitLabels_DryRun_PipelineNotFound: pasar --pipeline a un nombre
// que no existe devuelve error humano.
func TestRunInitLabels_DryRun_PipelineNotFound(t *testing.T) {
	mgr, _ := pipelineFixture(t, nil, "")
	var out, errOut bytes.Buffer
	err := runInitLabels(&out, &errOut, mgr, "no-existe", true)
	if err == nil {
		t.Fatal("runInitLabels con --pipeline inválido debería errar")
	}
	if !strings.Contains(err.Error(), "no-existe") {
		t.Errorf("error sin nombre del pipeline: %v", err)
	}
}
