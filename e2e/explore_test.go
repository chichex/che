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
	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "--body-file"); len(comments) > 0 {
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
		WhenArgsMatch(`ingeniero senior`).
		CaptureStdin().
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "none", "42")
	harness.AssertContains(t, out, "Explored")
	harness.AssertContains(t, out, "https://github.com/acme/demo/issues/42")

	inv := env.Invocations()
	if views := inv.FindCalls("gh", "issue", "view", "42"); len(views) != 1 {
		t.Fatalf("expected 1 gh issue view, got %d", len(views))
	}
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 1 {
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "codex", "--validators", "none", "42")

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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "gemini", "--validators", "none", "42")

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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--agent", "opus", "--validators", "none", "42")

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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_invalid_effort.json", 0)

	r := env.Run("explore", "--validators", "none", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "effort")
	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "--body-file"); len(comments) > 0 {
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_no_recommended.json", 0)

	r := env.Run("explore", "--validators", "none", "42")
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_multiple_recommended.json", 0)

	r := env.Run("explore", "--validators", "none", "42")
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondExitWithError(1, "422 validation failed\n")

	r := env.Run("explore", "--validators", "none", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "comentario posteado")
	harness.AssertContains(t, r.Stderr, "label")

	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 1 {
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment `).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--validators", "none", "https://github.com/acme/demo/issues/42")
}

// TestExplore_Validators_DefaultOpusApproves: default --validators opus
// aprobando → label status:plan, 1 comment de executor + 1 de validator,
// reporte en stdout con ✓ para opus.
func TestExplore_Validators_DefaultOpusApproves(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-executor\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-opus\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "42")
	harness.AssertContains(t, out, "Explored")
	harness.AssertContains(t, out, "opus#1")
	harness.AssertContains(t, out, "approve")

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 2 {
		t.Fatalf("expected 2 issue comments (executor + 1 validator), got %d", len(comments))
	}
	// 2 claude calls: ejecutor + validator opus.
	if claudeCalls := inv.For("claude"); len(claudeCalls) != 2 {
		t.Fatalf("expected 2 claude calls (executor + opus validator), got %d", len(claudeCalls))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 issue edit (status:plan), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:plan")
}

// TestExplore_Validators_ChangesRequested_StillPlan: un validator reporta
// findings (sin needs_human) → status:plan se aplica igual, findings quedan
// en el comment y en el reporte. La iteración interactiva no es parte de
// v0.0.12.
func TestExplore_Validators_ChangesRequested_StillPlan(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_changes_requested.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-g\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "codex,gemini", "42")
	harness.AssertContains(t, out, "changes_requested")
	harness.AssertContains(t, out, "Explored")

	inv := env.Invocations()
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected status:plan to be applied despite changes_requested, got %d edits", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:plan")
	if strings.Contains(strings.Join(edits[0].Args, " "), "status:awaiting-human") {
		t.Fatalf("unexpected status:awaiting-human applied: %v", edits[0].Args)
	}
}

// TestExplore_Validators_NeedsHuman_AwaitingLabel: un validator reporta
// needs_human → label status:awaiting-human (no status:plan), comment
// adicional con las preguntas para el humano.
func TestExplore_Validators_NeedsHuman_AwaitingLabel(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_needs_human.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	// 3 validator-era comments (executor + 2 validators) + 1 human-request comment.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-g\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-human\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "codex,gemini", "42")
	harness.AssertContains(t, out, "needs_human")
	harness.AssertContains(t, out, "awaiting-human")

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 4 {
		t.Fatalf("expected 4 comments (executor + 2 validators + human-request), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 label edit, got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:awaiting-human")
	if strings.Contains(strings.Join(edits[0].Args, " "), "status:plan") {
		t.Fatalf("status:plan should NOT be applied when needs_human; got %v", edits[0].Args)
	}
}

// TestExplore_Validators_TechnicalNeedsHuman_DoesNotPause: un validator
// devuelve verdict=needs_human pero TODOS sus findings son kind=technical o
// kind=documented (needs_human=false porque respetó la regla). El flow NO
// debe pausar — transiciona a status:plan como si fuera changes_requested.
// Esto bloquea el bug que motivó este feature: preguntas técnicas del
// ejecutor siendo escaladas al humano.
func TestExplore_Validators_TechnicalNeedsHuman_DoesNotPause(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_technical_needs_human.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	// 3 comments esperados (executor + 2 validators). NO debe haber un
	// 4to "human-request" porque los findings technical/documented no
	// escalan al humano.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-g\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "--validators", "codex,gemini", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	// Cierra en status:plan, no awaiting.
	harness.AssertContains(t, r.Stdout, "Explored")
	if strings.Contains(r.Stdout+r.Stderr, "awaiting-human") {
		t.Fatalf("flow should NOT pause when findings are technical/documented only; got stdout: %s\nstderr: %s", r.Stdout, r.Stderr)
	}
	// Canonicalización: el crudo del fixture era needs_human pero sin
	// findings product → degrada a changes_requested. La línea de
	// Validation report en stdout debe reflejarlo.
	harness.AssertContains(t, r.Stdout, "Validation report")
	harness.AssertContains(t, r.Stdout, "codex#1: changes_requested")
	if strings.Contains(r.Stdout, "codex#1: needs_human") {
		t.Fatalf("stdout should NOT render raw verdict=needs_human after canonicalization; got: %s", r.Stdout)
	}

	inv := env.Invocations()
	// Exactamente 3 comments (executor + 2 validators), NO human-request.
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 3 {
		t.Fatalf("expected 3 comments (executor + 2 validators, NO human-request), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 label edit (status:plan), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:plan")
	if strings.Contains(strings.Join(edits[0].Args, " "), "status:awaiting-human") {
		t.Fatalf("status:awaiting-human should NOT be applied; got %v", edits[0].Args)
	}
}

// TestExplore_Validators_Duplicate: codex,codex,gemini → 2 invocaciones a
// codex (instance=1 e instance=2) + 1 a gemini. Verifica que el harness
// soporta múltiples calls al mismo binario y que el flow distingue instancias.
func TestExplore_Validators_Duplicate(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	// Dos respuestas consumables para codex (instances 1 y 2).
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).Consumable().
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).Consumable().
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "codex,codex,gemini", "42")
	harness.AssertContains(t, out, "codex#1")
	harness.AssertContains(t, out, "codex#2")
	harness.AssertContains(t, out, "gemini#1")

	inv := env.Invocations()
	if codexCalls := inv.For("codex"); len(codexCalls) != 2 {
		t.Fatalf("expected 2 codex calls, got %d", len(codexCalls))
	}
}

// TestExplore_Validators_NoneSkipsValidation: --validators none se comporta
// como v0.0.11 — solo ejecutor, sin validators.
func TestExplore_Validators_NoneSkipsValidation(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("url\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "--validators", "none", "42")

	inv := env.Invocations()
	inv.AssertNotCalled(t, "codex")
	inv.AssertNotCalled(t, "gemini")
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 1 {
		t.Fatalf("expected 1 comment (executor only), got %d", len(comments))
	}
}

// TestExplore_Validators_Invalid: --validators bogus → exit 3.
func TestExplore_Validators_Invalid(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("explore", "--validators", "bogus", "42")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	harness.AssertContains(t, r.Stderr, "validators")
	env.Invocations().AssertNotCalled(t, "gh")
}

// TestExplore_Validators_CountOutOfRange: 4+ validadores → exit 3. Un solo
// validador ahora es válido (1-3 items aceptados).
func TestExplore_Validators_CountOutOfRange(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("explore", "--validators", "codex,codex,codex,gemini", "42")
	if r.ExitCode == 0 {
		t.Fatalf("expected non-zero exit for 4 validators, got 0")
	}
	harness.AssertContains(t, r.Stderr, "1-3")
}

// TestExplore_Validators_InvalidResponse_Exit3: un validator devuelve un
// verdict fuera del enum → exit 3 sin aplicar labels ni postear human request.
// El ejecutor y los comments de los validators válidos ya se postearon (no
// hacemos rollback), pero el label NO transiciona.
func TestExplore_Validators_InvalidResponse_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_invalid_verdict.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)

	r := env.Run("explore", "--validators", "codex,gemini", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "verdict")

	inv := env.Invocations()
	if edits := inv.FindCalls("gh", "issue", "edit"); len(edits) > 0 {
		t.Fatalf("should not edit labels when validator response invalid: %+v", edits)
	}
}

// TestExplore_Resume_HappyPath_ConsolidatesAndClosesLoop: issue en
// awaiting-human con respuesta humana → validators iter=2 aprueban →
// executor consolida → body del issue actualizado, label a status:plan.
func TestExplore_Resume_HappyPath_ConsolidatesAndClosesLoop(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_awaiting_with_answer.json", 0)
	// Validators iter=2 ambos aprueban (las respuestas humanas cubrieron todo).
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	// Consolidación: executor (opus=claude) devuelve el plan final.
	env.ExpectAgent("claude").
		WhenArgsMatch(`consolidar un plan`).
		RespondStdoutFromFixture("explore/sonnet_consolidated.json", 0)
	// 2 comments de validators iter=2 + body update.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 --remove-label`).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "codex,gemini", "42")
	harness.AssertContains(t, out, "Resumed and consolidated")

	inv := env.Invocations()
	// 2 comments de validators (iter=2), no human-request.
	if n := len(inv.FindCalls("gh", "issue", "comment", "--body-file")); n != 2 {
		t.Fatalf("expected 2 validator comments at iter=2, got %d", n)
	}
	// body edit + labels edit.
	bodyEdits := inv.FindCalls("gh", "issue", "edit", "--body-file")
	if len(bodyEdits) != 1 {
		t.Fatalf("expected 1 body edit, got %d", len(bodyEdits))
	}
	labelEdits := inv.FindCalls("gh", "issue", "edit", "--remove-label")
	if len(labelEdits) != 1 {
		t.Fatalf("expected 1 label edit, got %d", len(labelEdits))
	}
	labelEdits[0].AssertArgsContain(t,
		"--remove-label", "status:awaiting-human",
		"--add-label", "status:plan")
}

// TestExplore_Resume_StillNeedsHuman_PostsNewRequest: respuesta humana no
// cubre todo, validator iter=2 sigue needs_human → nuevo human-request
// posteado, sigue awaiting-human, NO se toca el body.
func TestExplore_Resume_StillNeedsHuman_PostsNewRequest(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_awaiting_with_answer.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_needs_human.json", 0)
	env.ExpectAgent("gemini").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	// 2 validators + 1 human-request = 3 comments. NO body edit.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("x\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 --add-label`).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "--validators", "codex,gemini", "42")
	harness.AssertContains(t, out, "awaiting-human")

	inv := env.Invocations()
	if n := len(inv.FindCalls("gh", "issue", "comment", "--body-file")); n != 3 {
		t.Fatalf("expected 3 comments (2 validators + human-request), got %d", n)
	}
	if n := len(inv.FindCalls("gh", "issue", "edit", "--body-file")); n != 0 {
		t.Fatalf("body should NOT be updated when still needs_human, got %d edits", n)
	}
	// claude (opus) no debería haber sido llamado — la consolidación es solo
	// al converger, no en este branch.
	inv.AssertNotCalled(t, "claude")
}

// TestExplore_Resume_NoHumanAnswer_Exit3: issue en awaiting-human sin
// comment humano posterior al human-request → exit 3, no se llama a nadie.
func TestExplore_Resume_NoHumanAnswer_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_awaiting_no_answer.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "respuestas humanas")

	inv := env.Invocations()
	inv.AssertNotCalled(t, "claude")
	inv.AssertNotCalled(t, "codex")
	inv.AssertNotCalled(t, "gemini")
	if n := len(inv.FindCalls("gh", "issue", "comment", "--body-file")); n != 0 {
		t.Fatalf("should not post any comment, got %d", n)
	}
}

func scriptExplorePrechecks(env *harness.Env) {
	env.ExpectGit(`^remote get-url origin`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in as acme\n", 0)
}

// TestExplore_Validators_NeedsHumanWithoutFindings_DegradesToPlan verifica el
// fix 2b: un validator que emite verdict=needs_human con findings=[] (o sin
// findings product) es un output inválido. El flow debe degradarlo a
// changes_requested in-place: label final status:plan (no awaiting-human),
// header del comment dice changes_requested (efectivo post-normalización),
// stderr contiene el warning, y NO se postea human-request.
func TestExplore_Validators_NeedsHumanWithoutFindings_DegradesToPlan(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_needs_human_no_findings.json", 0)
	// 2 comments: executor + 1 validator. NO human-request.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "--validators", "codex", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	combined := r.Stdout + r.Stderr
	harness.AssertContains(t, combined, "Explored")
	// Render efectivo: el reporte en stdout muestra changes_requested, no
	// needs_human (el crudo del fixture).
	harness.AssertContains(t, combined, "changes_requested")
	if strings.Contains(combined, "awaiting-human") {
		t.Fatalf("flow should NOT pause when needs_human has no product finding; got: %s", combined)
	}
	// Warning de degradación en stderr.
	harness.AssertContains(t, r.Stderr, "degrading to changes_requested")

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 2 {
		t.Fatalf("expected 2 comments (executor + 1 validator, NO human-request), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 label edit (status:plan), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:plan")
	if strings.Contains(strings.Join(edits[0].Args, " "), "status:awaiting-human") {
		t.Fatalf("status:awaiting-human should NOT be applied; got %v", edits[0].Args)
	}
}

// TestExplore_Validators_ApproveButProductFinding_PausesAndRendersAsNeedsHuman
// verifica el fix 2c: un validator con verdict=approve crudo pero con un
// finding kind=product needs_human=true debe pausar el flow (hasHumanGaps
// todavía dispara) y el render efectivo debe mostrar needs_human en los
// outputs, no approve.
func TestExplore_Validators_ApproveButProductFinding_PausesAndRendersAsNeedsHuman(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve_with_product_finding.json", 0)
	// 3 comments: executor + 1 validator + 1 human-request.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-human\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "--validators", "codex", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	// Verdict canonicalizado: la línea de Validation report en stdout debe
	// mostrar needs_human para codex#1 aunque el crudo del fixture era approve.
	// Asserto contra r.Stdout específicamente (no combined) para no pasar
	// falso-positivo por el JSON embebido del fixture echoeado vía streamPipe.
	harness.AssertContains(t, r.Stdout, "Validation report")
	harness.AssertContains(t, r.Stdout, "codex#1: needs_human")
	if strings.Contains(r.Stdout, "codex#1: approve") {
		t.Fatalf("stdout should NOT render raw verdict=approve after canonicalization; got: %s", r.Stdout)
	}
	combined := r.Stdout + r.Stderr
	harness.AssertContains(t, combined, "awaiting-human")

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 3 {
		t.Fatalf("expected 3 comments (executor + 1 validator + human-request), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 label edit, got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:awaiting-human")
	if strings.Contains(strings.Join(edits[0].Args, " "), "status:plan") {
		t.Fatalf("status:plan should NOT be applied when needs_human effective; got %v", edits[0].Args)
	}
}

// TestExplore_ExecutorBlockerQuestion_NotMirrored_DoesNotPause fija el
// contrato actual como memoria ejecutable: si el ejecutor deja una pregunta
// blocker=true kind=product pero el validator responde approve sin espejarla,
// el flow NO pausa (status:plan). El edge case queda documentado acá;
// cuando se resuelva en un issue separado este test se flipea.
func TestExplore_ExecutorBlockerQuestion_NotMirrored_DoesNotPause(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_with_product_blocker_question.json", 0)
	env.ExpectAgent("codex").
		WhenArgsMatch(`validador técnico`).
		RespondStdoutFromFixture("explore/sonnet_validator_approve.json", 0)
	// 2 comments: executor + 1 validator. NO human-request porque el
	// validator no espejó la question.
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-e\n", 0)
	env.ExpectGh(`^issue comment 42`).Consumable().RespondStdout("url-c\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "--validators", "codex", "42")
	if r.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	combined := r.Stdout + r.Stderr
	harness.AssertContains(t, combined, "Explored")
	if strings.Contains(combined, "awaiting-human") {
		t.Fatalf("flow should NOT pause when validator approves and doesn't mirror executor question; got: %s", combined)
	}

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 2 {
		t.Fatalf("expected 2 comments (executor + 1 validator, NO human-request), got %d", len(comments))
	}
	edits := inv.FindCalls("gh", "issue", "edit", "42")
	if len(edits) != 1 {
		t.Fatalf("expected 1 label edit (status:plan), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "status:plan")
}
