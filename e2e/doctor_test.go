package e2e_test

import (
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestDoctor_CommandExists is the smallest possible test that forces cmd/doctor.go
// to exist and register a `doctor` subcommand on the root command.
func TestDoctor_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("doctor", "--help")
	harness.AssertContains(t, out, "doctor")
	harness.AssertContains(t, out, "chequea")
}

// TestDoctor_AllGreen exercises the happy path: every required binary is on
// PATH, every optional binary is on PATH, `--version` responds 0 for all of
// them, `gh auth status` is OK, and the working dir is a git repo with a
// GitHub remote. The command must exit 0 and print one ✓ per check.
func TestDoctor_AllGreen(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptAllGreen(env, "https://github.com/acme/demo.git")

	out := env.MustRun("doctor")

	for _, line := range []string{
		"✓ git",
		"✓ github remote",
		"✓ gh",
		"✓ gh auth",
		"✓ claude",
		"✓ codex",
		"✓ gemini",
	} {
		harness.AssertContains(t, out, line)
	}
}

// TestDoctor_ClaudeMissing_Fails: claude is not on PATH → exit 1, ✗ for claude.
func TestDoctor_ClaudeMissing_Fails(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptAllGreen(env, "https://github.com/acme/demo.git")
	env.RemoveFake("claude")

	out := env.MustFail("doctor")
	harness.AssertContains(t, out, "✗ claude")
	harness.AssertContains(t, out, "not installed")
}

// TestDoctor_ClaudeVersionFails: claude is on PATH but `claude --version`
// exits non-zero → doctor exits 1.
func TestDoctor_ClaudeVersionFails(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	// Script everything green except override claude --version to fail.
	env.ExpectGit(`^--version$`).RespondStdout("git version 2.45.0\n", 0)
	env.ExpectGit(`^remote (-v|get-url)`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^--version$`).RespondStdout("gh version 2.50.0\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in to github.com as acme\n", 0)
	env.ExpectAgent("claude").WhenArgsMatch(`^--version$`).RespondExitWithError(1, "claude: corrupt install\n")
	env.ExpectAgent("codex").WhenArgsMatch(`^--version$`).RespondStdout("0.5.0\n", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`^--version$`).RespondStdout("0.3.0\n", 0)

	out := env.MustFail("doctor")
	harness.AssertContains(t, out, "✗ claude")
}

// TestDoctor_GhAuthFails: gh is installed but `gh auth status` exits non-zero.
func TestDoctor_GhAuthFails(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^--version$`).RespondStdout("git version 2.45.0\n", 0)
	env.ExpectGit(`^remote (-v|get-url)`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^--version$`).RespondStdout("gh version 2.50.0\n", 0)
	env.ExpectGh(`^auth status`).RespondExitWithError(1, "You are not logged into any GitHub hosts.\n")
	env.ExpectAgent("claude").WhenArgsMatch(`^--version$`).RespondStdout("1.0.0\n", 0)
	env.ExpectAgent("codex").WhenArgsMatch(`^--version$`).RespondStdout("0.5.0\n", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`^--version$`).RespondStdout("0.3.0\n", 0)

	out := env.MustFail("doctor")
	harness.AssertContains(t, out, "✗ gh auth")
}

// TestDoctor_NoGitHubRemote: repo has no origin remote → exit 1.
func TestDoctor_NoGitHubRemote(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^--version$`).RespondStdout("git version 2.45.0\n", 0)
	env.ExpectGit(`^remote get-url`).RespondExitWithError(1, "error: No such remote 'origin'\n")
	env.ExpectGh(`^--version$`).RespondStdout("gh version 2.50.0\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in\n", 0)
	env.ExpectAgent("claude").WhenArgsMatch(`^--version$`).RespondStdout("1.0.0\n", 0)
	env.ExpectAgent("codex").WhenArgsMatch(`^--version$`).RespondStdout("0.5.0\n", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`^--version$`).RespondStdout("0.3.0\n", 0)

	out := env.MustFail("doctor")
	harness.AssertContains(t, out, "✗ github remote")
}

// TestDoctor_CodexMissing_Warns: codex is optional, so its absence warns but
// does not fail doctor.
func TestDoctor_CodexMissing_Warns(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptAllGreen(env, "https://github.com/acme/demo.git")
	env.RemoveFake("codex")

	out := env.MustRun("doctor")
	harness.AssertContains(t, out, "⚠ codex")
	harness.AssertContains(t, out, "not installed")
}

// TestDoctor_CodexVersionFails_Warns: codex is installed but --version fails.
// Still only a warning.
func TestDoctor_CodexVersionFails_Warns(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^--version$`).RespondStdout("git version 2.45.0\n", 0)
	env.ExpectGit(`^remote (-v|get-url)`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^--version$`).RespondStdout("gh version 2.50.0\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in\n", 0)
	env.ExpectAgent("claude").WhenArgsMatch(`^--version$`).RespondStdout("1.0.0\n", 0)
	env.ExpectAgent("codex").WhenArgsMatch(`^--version$`).RespondExitWithError(1, "corrupt\n")
	env.ExpectAgent("gemini").WhenArgsMatch(`^--version$`).RespondStdout("0.3.0\n", 0)

	out := env.MustRun("doctor")
	harness.AssertContains(t, out, "⚠ codex")
}

// scriptAllGreen wires the six baseline responses that make doctor happy.
// Keep the regexes minimal — we want them to match any reasonable flag
// variant che might adopt (--version vs version, etc.).
func scriptAllGreen(env *harness.Env, remoteURL string) {
	env.ExpectGit(`^--version$`).RespondStdout("git version 2.45.0\n", 0)
	env.ExpectGit(`^remote (-v|get-url)`).RespondStdout(remoteURL+"\n", 0)
	env.ExpectGh(`^--version$`).RespondStdout("gh version 2.50.0\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in to github.com as acme\n", 0)
	env.ExpectAgent("claude").WhenArgsMatch(`^--version$`).RespondStdout("1.0.0\n", 0)
	env.ExpectAgent("codex").WhenArgsMatch(`^--version$`).RespondStdout("0.5.0\n", 0)
	env.ExpectAgent("gemini").WhenArgsMatch(`^--version$`).RespondStdout("0.3.0\n", 0)
}
