package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// TestHeadless_SmokeManifestDone es el smoke test del nucleo headless segun
// el AC del plan: un pipeline trivial con un solo step que ejecuta "echo
// ok" debe dejar el manifest final con status=done y exit_code=0 para ese
// step. Cubre el flow StartHeadless (via startHeadlessFromPipeline para
// bypassear wizard.IsValid, que depende del PATH del host) + Execute +
// closeManifest.
//
// Usamos un runsRoot bajo t.TempDir() para no tocar ~/.che. spawnCmdFn se
// mockea con /bin/echo para no depender de un CLI real (mismo patron que
// usan los tests de spawn.go).
func TestHeadless_SmokeManifestDone(t *testing.T) {
	prev := spawnCmdFn
	t.Cleanup(func() { spawnCmdFn = prev })
	spawnCmdFn = func(_ wizard.Step, _ string) (*exec.Cmd, error) {
		return exec.Command("/bin/echo", "ok"), nil
	}

	runsRoot := t.TempDir()
	p := wizard.Pipeline{
		Name: "headless-smoke",
		Steps: []wizard.Step{
			{
				Name:    "single",
				CLI:     "echo",
				Kind:    wizard.KindPrompt,
				Input:   wizard.InputNone,
				Content: "irrelevant",
			},
		},
	}

	hr, err := startHeadlessFromPipeline(p, "test:headless-smoke", "", runsRoot)
	if err != nil {
		t.Fatalf("startHeadlessFromPipeline: %v", err)
	}
	if hr.RunID == "" {
		t.Fatal("RunID vacio")
	}
	if hr.RunDir == "" {
		t.Fatal("RunDir vacio")
	}

	if err := hr.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Manifest final: status=done, 1 step con exit_code=0.
	mPath := filepath.Join(hr.RunDir, "manifest.yaml")
	data, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatalf("read manifest.yaml: %v", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if m.Status != ManifestStatusDone {
		t.Errorf("manifest.status: got %q want %q", m.Status, ManifestStatusDone)
	}
	if len(m.Steps) != 1 {
		t.Fatalf("manifest.steps len: got %d want 1", len(m.Steps))
	}
	if m.Steps[0].Status != string(StepStatusDone) {
		t.Errorf("manifest.steps[0].status: got %q want %q", m.Steps[0].Status, StepStatusDone)
	}
	if m.Steps[0].ExitCode != 0 {
		t.Errorf("manifest.steps[0].exit_code: got %d want 0", m.Steps[0].ExitCode)
	}

	// Smoke check del result.yaml: contiene "ok" en Output.
	rPath := filepath.Join(hr.RunDir, "step-01.result.yaml")
	rdata, err := os.ReadFile(rPath)
	if err != nil {
		t.Fatalf("read step-01.result.yaml: %v", err)
	}
	var r StepResult
	if err := yaml.Unmarshal(rdata, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(r.Output, "ok") {
		t.Errorf("step result.output no contiene 'ok': %q", r.Output)
	}

	// El run-dir vive bajo runsRoot/<slug>/<runID>/.
	wantPrefix := filepath.Join(runsRoot, wizard.Slug(p.Name))
	if !strings.HasPrefix(hr.RunDir, wantPrefix) {
		t.Errorf("RunDir no esta bajo runsRoot/<slug>: got %q want prefix %q", hr.RunDir, wantPrefix)
	}
}

// TestHeadless_StepFailsClosesManifestFailed verifica que un step con exit
// !=0 cierra el manifest con status=failed y devuelve error al caller. Es la
// otra mitad del happy path: confirma que el branch terminal de Execute no
// queda colgado en running.
func TestHeadless_StepFailsClosesManifestFailed(t *testing.T) {
	prev := spawnCmdFn
	t.Cleanup(func() { spawnCmdFn = prev })
	spawnCmdFn = func(_ wizard.Step, _ string) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "exit 7"), nil
	}

	runsRoot := t.TempDir()
	p := wizard.Pipeline{
		Name: "headless-fail",
		Steps: []wizard.Step{
			{Name: "boom", CLI: "echo", Kind: wizard.KindPrompt, Input: wizard.InputNone, Content: "x"},
		},
	}
	hr, err := startHeadlessFromPipeline(p, "test:headless-fail", "", runsRoot)
	if err != nil {
		t.Fatalf("startHeadlessFromPipeline: %v", err)
	}
	if err := hr.Execute(); err == nil {
		t.Fatal("Execute: queria error de step exit !=0, got nil")
	}

	mPath := filepath.Join(hr.RunDir, "manifest.yaml")
	data, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatalf("read manifest.yaml: %v", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if m.Status != ManifestStatusFailed {
		t.Errorf("manifest.status: got %q want %q", m.Status, ManifestStatusFailed)
	}
	if len(m.Steps) != 1 || m.Steps[0].ExitCode != 7 {
		t.Errorf("manifest.steps: want 1 step con exit_code=7; got %+v", m.Steps)
	}
}
