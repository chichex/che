package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

func TestPipelineE2E_SimulateBuiltinWithoutCustomConfig(t *testing.T) {
	t.Parallel()
	env := setupPipelineRepo(t)

	out := env.MustRun("pipeline", "simulate")
	harness.AssertContains(t, out, "pipeline: default")
	harness.AssertContains(t, out, "source:   built-in")
	harness.AssertContains(t, out, "step[0]: idea")
	env.Invocations().AssertNotCalled(t, "claude")
}

func TestPipelineE2E_RunCustomConfigFlagAndFrom(t *testing.T) {
	t.Parallel()
	env := setupPipelineRepo(t)
	writePipeline(t, env.RepoDir, "fast", `{
  "version": 1,
  "steps": [
    {"name": "plan", "agents": ["claude-opus"]},
    {"name": "execute", "agents": ["claude-opus"]}
  ]
}`)
	writePipeline(t, env.RepoDir, "slow", `{
  "version": 1,
  "steps": [
    {"name": "ignored", "agents": ["claude-opus"]}
  ]
}`)
	writeConfig(t, env.RepoDir, "slow")
	env.ExpectAgent("claude").WhenArgsMatch(`--output-format text`).RespondStdout("[next]\n", 0)

	out := env.MustRun("run", "--pipeline", "fast", "--from", "execute", "--input", "ship it")
	harness.AssertContains(t, out, "pipeline: fast")
	harness.AssertContains(t, out, "source:   flag")
	harness.AssertContains(t, out, "from:     execute (entry bypassed)")
	harness.AssertContains(t, out, "step[0]: execute")
	harness.AssertNotContains(t, out, "step[0]: plan")

	calls := env.Invocations().For("claude")
	if len(calls) != 1 {
		t.Fatalf("expected 1 agent call from --from execute, got %d", len(calls))
	}
}

func TestPipelineE2E_EntryCanStopBeforeSteps(t *testing.T) {
	t.Parallel()
	env := setupPipelineRepo(t)
	writePipeline(t, env.RepoDir, "gated", `{
  "version": 1,
  "entry": {"agents": ["claude-opus"], "aggregator": "first_blocker"},
  "steps": [
    {"name": "execute", "agents": ["claude-opus"]}
  ]
}`)
	env.ExpectAgent("claude").WhenArgsMatch(`--output-format text`).Consumable().RespondStdout("[stop]\n", 0)

	out := env.MustRun("run", "gated", "--input", "not actionable")
	harness.AssertContains(t, out, "entry: agent=claude-opus marker=[stop]")
	harness.AssertContains(t, out, "stopped: entry agent emitted [stop]")
	if calls := env.Invocations().For("claude"); len(calls) != 1 {
		t.Fatalf("entry stop should avoid step agents, got %d claude calls", len(calls))
	}
}

func TestPipelineE2E_AgentFailureStopsTechnically(t *testing.T) {
	t.Parallel()
	env := setupPipelineRepo(t)
	writePipeline(t, env.RepoDir, "fails", `{
  "version": 1,
  "steps": [
    {"name": "execute", "agents": ["claude-opus"]}
  ]
}`)
	env.ExpectAgent("claude").WhenArgsMatch(`--output-format text`).RespondExitWithError(1, "boom\n")

	r := env.Run("run", "fails")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on agent failure\nstdout:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
	}
	harness.AssertContains(t, r.Stdout+r.Stderr, "agent technical error")
}

func TestPipelineE2E_WizardGeneratesValidJSON(t *testing.T) {
	t.Parallel()
	env := setupPipelineRepo(t)
	stdin := strings.Join([]string{
		"n",          // clonar desde un pipeline existente
		"n",          // agregar entry agent
		"execute",    // nombre del step
		"1",          // claude-opus
		"quick path", // comment
		"n",          // agregar otro step
		"y",          // guardar
		"",
	}, "\n")

	out := env.RunWithStdin(stdin, "pipeline", "create", "quick")
	if out.ExitCode != 0 {
		t.Fatalf("pipeline create failed: stdout:\n%s\nstderr:\n%s", out.Stdout, out.Stderr)
	}
	harness.AssertContains(t, out.Stdout+out.Stderr, "creado")

	validate := env.MustRun("pipeline", "validate", "quick", "--skip-agents")
	harness.AssertContains(t, validate, "ok")
}

func setupPipelineRepo(t *testing.T) *harness.Env {
	t.Helper()
	env := harness.New(t)
	env.RemoveFake("git")
	runIn(t, env.RepoDir, "git", "init", "-q", "-b", "main")
	return env
}

func writePipeline(t *testing.T, repo, name, body string) {
	t.Helper()
	dir := filepath.Join(repo, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir pipeline dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write pipeline %s: %v", name, err)
	}
}

func writeConfig(t *testing.T, repo, name string) {
	t.Helper()
	dir := filepath.Join(repo, ".che")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .che: %v", err)
	}
	body := `{"version":1,"default":"` + name + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pipelines.config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
