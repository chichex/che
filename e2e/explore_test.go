package e2e_test

import (
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestExplore_CommandExists forces cmd/explore.go into existence as a one-shot
// subcommand for scripting/CI use. The interactive TUI path will reuse the
// same internal flow.
func TestExplore_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("explore", "--help")
	harness.AssertContains(t, out, "explore")
	harness.AssertContains(t, out, "issue")
}

// TestExplore_MissingArg_ExitNonZero: cobra requires exactly one positional
// arg; without it, usage error and non-zero exit.
func TestExplore_MissingArg_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("explore")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit, got 0\nstdout: %s\nstderr: %s", r.Stdout, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_PrecheckNoGitHubRemote_Exit2: git remote get-url fails → exit 2
// without calling gh or claude.
func TestExplore_PrecheckNoGitHubRemote_Exit2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^remote get-url origin`).RespondExitWithError(1, "fatal: No such remote 'origin'\n")

	r := env.Run("explore", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "github remote")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_IssueViewFails_Exit2: gh issue view fails (network, auth, not
// found) → exit 2 without calling claude. We treat all gh view failures as
// retryable for simplicity; refine later if needed.
func TestExplore_IssueViewFails_Exit2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondExitWithError(1, "GraphQL: Could not resolve to an Issue\n")

	r := env.Run("explore", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_IssueMissingCtPlanLabel_Exit3: issue fetched OK but doesn't
// carry ct:plan → exit 3, no claude call, no state mutation.
func TestExplore_IssueMissingCtPlanLabel_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 99`).RespondStdoutFromFixture("explore/gh_issue_view_without_ctplan.json", 0)

	r := env.Run("explore", "99")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "ct:plan")
	env.Invocations().AssertNotCalled(t, "claude")
	if edits := env.Invocations().FindCalls("gh", "issue", "edit"); len(edits) > 0 {
		t.Fatalf("unexpected gh issue edit calls: %+v", edits)
	}
	if comments := env.Invocations().FindCalls("gh", "issue", "comment"); len(comments) > 0 {
		t.Fatalf("unexpected gh issue comment calls: %+v", comments)
	}
}

// TestExplore_IssueClosed_Exit3: issue is CLOSED → exit 3, stderr says so,
// no claude call.
func TestExplore_IssueClosed_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 7`).RespondStdoutFromFixture("explore/gh_issue_view_closed.json", 0)

	r := env.Run("explore", "7")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "closed")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_IssueAlreadyPlanned_Exit3: issue already carries status:planned
// (explore ran before) → exit 3, refuse to re-explore.
func TestExplore_IssueAlreadyPlanned_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_already_planned.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "already")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_GoldenPath: issue OK → ejecutor OK → comment posteado → label
// transitioned. Verifica el flow completo y los asserts sobre invocations.
// Default agent es opus (invoca el binario `claude`).
func TestExplore_GoldenPath(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		CaptureStdin().
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "42")
	harness.AssertContains(t, out, "Explored")
	harness.AssertContains(t, out, "https://github.com/acme/demo/issues/42")

	inv := env.Invocations()
	if views := inv.FindCalls("gh", "issue", "view", "42"); len(views) != 1 {
		t.Fatalf("expected 1 gh issue view, got %d", len(views))
	}
	if comments := inv.FindCalls("gh", "issue", "comment", "42"); len(comments) != 1 {
		t.Fatalf("expected 1 gh issue comment, got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 gh issue edit, got %d", len(edits))
	}
	edits[0].AssertArgsContain(t,
		"--remove-label", "status:idea",
		"--add-label", "status:plan")
	// No aplicar ct:exec desde explore — eso lo hace che execute al arrancar.
	if strings.Contains(strings.Join(edits[0].Args, " "), "ct:exec") {
		t.Fatalf("explore must NOT apply ct:exec; edits[0].Args=%v", edits[0].Args)
	}
	// Otros agentes NO deberían haberse invocado.
	inv.AssertNotCalled(t, "codex")
	inv.AssertNotCalled(t, "gemini")
}

// TestExplore_AgentCodex: --agent codex invoca el binario `codex`, no claude.
func TestExplore_AgentCodex(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "codex", "42")

	inv := env.Invocations()
	if len(inv.For("codex")) != 1 {
		t.Fatalf("expected 1 codex call, got %d", len(inv.For("codex")))
	}
	inv.AssertNotCalled(t, "claude")
	inv.AssertNotCalled(t, "gemini")
}

// TestExplore_AgentGemini: --agent gemini invoca el binario `gemini`.
func TestExplore_AgentGemini(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "gemini", "42")

	inv := env.Invocations()
	if len(inv.For("gemini")) != 1 {
		t.Fatalf("expected 1 gemini call, got %d", len(inv.For("gemini")))
	}
	inv.AssertNotCalled(t, "claude")
	inv.AssertNotCalled(t, "codex")
}

// TestExplore_AgentOpusExplicit: --agent opus usa claude (alias del binario).
func TestExplore_AgentOpusExplicit(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "opus", "42")

	inv := env.Invocations()
	if len(inv.For("claude")) != 1 {
		t.Fatalf("expected 1 claude call, got %d", len(inv.For("claude")))
	}
}

// TestExplore_InvalidAgent_ExitNonZero: --agent bogus rechazado.
func TestExplore_InvalidAgent_ExitNonZero(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("explore", "--agent", "bogus", "42")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit, got 0\nstdout: %s\nstderr: %s", r.Stdout, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "agent")
	env.Invocations().AssertNotCalled(t, "gh")
	env.Invocations().AssertNotCalled(t, "claude")
	env.Invocations().AssertNotCalled(t, "codex")
	env.Invocations().AssertNotCalled(t, "gemini")
}

// TestExplore_ClaudeInvalidEffort_Exit3: claude returns path.effort outside
// the enum → exit 3, no comment, no label edit.
func TestExplore_ClaudeInvalidEffort_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_invalid_effort.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "effort")
	if comments := env.Invocations().FindCalls("gh", "issue", "comment"); len(comments) > 0 {
		t.Fatalf("unexpected gh issue comment calls: %+v", comments)
	}
	if edits := env.Invocations().FindCalls("gh", "issue", "edit"); len(edits) > 0 {
		t.Fatalf("unexpected gh issue edit calls: %+v", edits)
	}
}

// TestExplore_ClaudeNoRecommended_Exit3: no path has recommended=true → exit 3.
func TestExplore_ClaudeNoRecommended_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_no_recommended.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "recommended")
}

// TestExplore_ClaudeMultipleRecommended_Exit3: two paths have recommended=true
// → exit 3 (the model failed to commit to one).
func TestExplore_ClaudeMultipleRecommended_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_multiple_recommended.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "recommended")
}

// TestExplore_LabelEditFailsAfterComment_Exit2_WarnsOrphan: label edit fails
// after comment posted → exit 2, stderr warns that comment is posted but
// label didn't transition. No rollback of the comment.
func TestExplore_LabelEditFailsAfterComment_Exit2_WarnsOrphan(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondExitWithError(1, "422 validation failed\n")

	r := env.Run("explore", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "comentario posteado")
	harness.AssertContains(t, r.Stderr, "label")

	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "42"); len(comments) != 1 {
		t.Fatalf("expected 1 gh issue comment (posted before edit failed), got %d", len(comments))
	}
}

// TestExplore_URLAsRef: gh normalizes URL refs; we pass the ref through
// untouched. Verifies the full URL reaches gh and the happy flow runs.
func TestExplore_URLAsRef(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view https://github\.com/acme/demo/issues/42`).
		RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment `).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "https://github.com/acme/demo/issues/42")
}

func scriptExplorePrechecks(env *harness.Env) {
	env.ExpectGit(`^remote get-url origin`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in as acme\n", 0)
}
