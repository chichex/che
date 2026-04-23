package e2e_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestExplore_CommandExists forces cmd/explore.go into existence as a one-shot
// subcommand for scripting/CI use. The interactive TUI path reuses the same
// internal flow.
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
// found) → exit 2 without calling claude.
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

// TestExplore_IssueMissingCtPlan_ClassifiesAndContinues: issue fetched OK
// pero sin ct:plan → explore lo clasifica con el LLM, aplica los labels del
// pipeline (ct:plan, status:idea; type/size ya estaban en la fixture) y
// continúa el flujo normal hasta el post de comentario + edit de body +
// transición a status:plan.
func TestExplore_IssueMissingCtPlan_ClassifiesAndContinues(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 99`).RespondStdoutFromFixture("explore/gh_issue_view_without_ctplan.json", 0)
	// 1) clasificador — mismo prompt que `che idea`.
	env.ExpectAgent("claude").
		WhenArgsMatch(`clasificador de ideas`).
		RespondStdoutFromFixture("idea/sonnet_single.json", 0)
	// 2) explorer — prompt distinto.
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 99`).RespondStdout("https://github.com/acme/demo/issues/99#issuecomment-1\n", 0)
	env.ExpectGh(`^issue edit 99 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 99 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "99")
	harness.AssertContains(t, out, "Explored")

	inv := env.Invocations()
	if n := len(inv.For("claude")); n != 2 {
		t.Fatalf("expected 2 claude calls (classifier + explorer), got %d", n)
	}
	// El primer `gh issue edit` sin --body-file debería ser reclassify
	// (ct:plan + status:idea). La fixture ya tiene type:fix y size:s, así
	// que esos NO se re-agregan.
	reclassifyEdits := inv.FindCalls("gh", "issue", "edit", "99", "--add-label")
	if len(reclassifyEdits) < 3 {
		t.Fatalf("expected at least 3 add-label edits (reclassify + lock + transition), got %d", len(reclassifyEdits))
	}
	reclassifyEdits[0].AssertArgsContain(t, "--add-label", "ct:plan", "--add-label", "che:idea")
	reclassArgs := strings.Join(reclassifyEdits[0].Args, " ")
	if strings.Contains(reclassArgs, "type:feature") || strings.Contains(reclassArgs, "size:m") {
		t.Fatalf("reclassify edit debería preservar type/size existentes; args=%v", reclassifyEdits[0].Args)
	}
	// La última edit con --add-label es la transición final a che:plan.
	reclassifyEdits[len(reclassifyEdits)-1].AssertArgsContain(t,
		"--remove-label", "che:planning",
		"--add-label", "che:plan")
}

// TestExplore_IssueMissingCtPlan_NoTypeSize_AppliesInferred: issue sin
// ct:plan Y sin type/size → el LLM clasifica y el primer edit aplica también
// los labels inferidos (type:feature + size:m del fixture de idea).
func TestExplore_IssueMissingCtPlan_NoTypeSize_AppliesInferred(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 77`).RespondStdoutFromFixture("explore/gh_issue_view_without_labels.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`clasificador de ideas`).
		RespondStdoutFromFixture("idea/sonnet_single.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 77`).RespondStdout("https://github.com/acme/demo/issues/77#issuecomment-1\n", 0)
	env.ExpectGh(`^issue edit 77 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 77 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "77")
	harness.AssertContains(t, out, "Explored")

	inv := env.Invocations()
	if n := len(inv.For("claude")); n != 2 {
		t.Fatalf("expected 2 claude calls (classifier + explorer), got %d", n)
	}
	addEdits := inv.FindCalls("gh", "issue", "edit", "77", "--add-label")
	if len(addEdits) < 3 {
		t.Fatalf("expected at least 3 add-label edits (reclassify + lock + transition), got %d", len(addEdits))
	}
	addEdits[0].AssertArgsContain(t,
		"--add-label", "ct:plan",
		"--add-label", "che:idea",
		"--add-label", "type:feature",
		"--add-label", "size:m",
	)
	labelCreates := inv.FindCalls("gh", "label", "create")
	joined := ""
	for _, c := range labelCreates {
		joined += " " + strings.Join(c.Args, " ")
	}
	for _, expected := range []string{"ct:plan", "che:idea", "type:feature", "size:m"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected gh label create for %q; calls=%v", expected, labelCreates)
		}
	}
	addEdits[len(addEdits)-1].AssertArgsContain(t,
		"--remove-label", "che:planning",
		"--add-label", "che:plan")
}

// TestExplore_IssueMissingCtPlan_ClassifierFails_Exit2: si el LLM falla, exit 2.
func TestExplore_IssueMissingCtPlan_ClassifierFails_Exit2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 99`).RespondStdoutFromFixture("explore/gh_issue_view_without_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`clasificador de ideas`).
		RespondExitWithError(1, "network blip\n")

	r := env.Run("explore", "99")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	for _, c := range inv.For("claude") {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "ingeniero senior") {
			t.Fatalf("explorer prompt should not be invoked when classifier fails; got call with args=%v", c.Args)
		}
	}
	if edits := inv.FindCalls("gh", "issue", "edit", "99"); len(edits) > 0 {
		t.Fatalf("expected no gh issue edit on classifier failure, got %+v", edits)
	}
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) > 0 {
		t.Fatalf("expected no gh issue comment on classifier failure, got %+v", comments)
	}
}

// TestExplore_IssueMissingCtPlan_ClassifierInvalidResponse_Exit3: JSON
// inválido del classifier → exit 3 (ExitSemantic), no exit 2.
func TestExplore_IssueMissingCtPlan_ClassifierInvalidResponse_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 99`).RespondStdoutFromFixture("explore/gh_issue_view_without_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`clasificador de ideas`).
		RespondStdoutFromFixture("idea/sonnet_invalid_type.json", 0)

	r := env.Run("explore", "99")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3 (ExitSemantic via ErrInvalidResponse), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}

	inv := env.Invocations()
	for _, c := range inv.For("claude") {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "ingeniero senior") {
			t.Fatalf("explorer prompt should not be invoked when classifier response is invalid; got call with args=%v", c.Args)
		}
	}
	if edits := inv.FindCalls("gh", "issue", "edit", "99"); len(edits) > 0 {
		t.Fatalf("expected no gh issue edit on invalid classifier response, got %+v", edits)
	}
}

// TestExplore_IssueMissingCtPlan_WithPreexistingStatus_PreservesStatus:
// issue sin ct:plan pero con che:* preexistente → reclassify preserva el
// estado. Si ese estado era che:plan, el gate de "ya avanzó en el pipeline"
// corta con ExitSemantic.
func TestExplore_IssueMissingCtPlan_WithPreexistingStatus_PreservesStatus(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 55`).RespondStdoutFromFixture("explore/gh_issue_view_status_without_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`clasificador de ideas`).
		RespondStdoutFromFixture("idea/sonnet_single.json", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 55 `).RespondStdout("ok\n", 0)

	r := env.Run("explore", "55")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3 (ya avanzó tras reclassify que preserva el estado), got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "ya avanzó en el pipeline")

	inv := env.Invocations()
	edits := inv.FindCalls("gh", "issue", "edit", "55")
	if len(edits) != 1 {
		t.Fatalf("expected 1 gh issue edit (reclassify only; flow corta en gate), got %d", len(edits))
	}
	edits[0].AssertArgsContain(t, "--add-label", "ct:plan")
	reclassArgs := strings.Join(edits[0].Args, " ")
	if strings.Contains(reclassArgs, "che:idea") {
		t.Fatalf("reclassify debería preservar che:plan y NO agregar che:idea; args=%v", edits[0].Args)
	}
	for _, c := range inv.For("claude") {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "ingeniero senior") {
			t.Fatalf("explorer prompt should not be invoked when issue ends up already-planned; got args=%v", c.Args)
		}
	}
}

// TestExplore_IssueClosed_Exit3: issue is CLOSED → exit 3.
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

// TestExplore_IssueAlreadyPlanned_Exit3: issue already carries che:plan
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
	harness.AssertContains(t, r.Stderr, "ya avanzó en el pipeline")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestExplore_GoldenPath: issue OK → ejecutor OK → comment posteado → body
// actualizado con plan consolidado → label transitioned. Default agent es
// opus (invoca el binario `claude`).
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
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "42")
	harness.AssertContains(t, out, "Explored")
	harness.AssertContains(t, out, "https://github.com/acme/demo/issues/42")

	inv := env.Invocations()
	if views := inv.FindCalls("gh", "issue", "view", "42"); len(views) != 1 {
		t.Fatalf("expected 1 gh issue view, got %d", len(views))
	}
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 1 {
		t.Fatalf("expected 1 gh issue comment, got %d", len(comments))
	}
	// 1 body edit + 1 label transition = 2 edits totales sobre el issue.
	bodyEdits := inv.FindCalls("gh", "issue", "edit", "42", "--body-file")
	if len(bodyEdits) != 1 {
		t.Fatalf("expected 1 body edit, got %d", len(bodyEdits))
	}
	// El flow nuevo hace dos transiciones de máquina de estados:
	//   1) che:idea → che:planning (lock)
	//   2) che:planning → che:plan (éxito)
	labelEdits := inv.FindCalls("gh", "issue", "edit", "42", "--remove-label")
	if len(labelEdits) != 2 {
		t.Fatalf("expected 2 label edits (lock + transition), got %d", len(labelEdits))
	}
	labelEdits[0].AssertArgsContain(t,
		"--remove-label", "che:idea",
		"--add-label", "che:planning")
	labelEdits[1].AssertArgsContain(t,
		"--remove-label", "che:planning",
		"--add-label", "che:plan")
	// No aplicar ct:exec desde explore.
	if strings.Contains(strings.Join(labelEdits[1].Args, " "), "ct:exec") {
		t.Fatalf("explore must NOT apply ct:exec; args=%v", labelEdits[1].Args)
	}
	// Otros agentes NO deberían haberse invocado.
	inv.AssertNotCalled(t, "codex")
	inv.AssertNotCalled(t, "gemini")
}

// TestExplore_IssueCtPlanWithoutStatusIdea_EnsuresRemoveLabel: issue con
// ct:plan aplicado a mano (p. ej. vía `/issue`) pero sin status:idea → el
// repo puede no tener el label status:idea creado. `gh issue edit
// --remove-label status:idea` falla con "not found" si el label no está
// registrado en el repo (independiente de que el issue no lo tenga). El flow
// debe llamar a `gh label create status:idea --force` antes del edit para que
// la transición no explote.
func TestExplore_IssueCtPlanWithoutStatusIdea_EnsuresRemoveLabel(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 115`).RespondStdoutFromFixture("explore/gh_issue_view_ctplan_no_status.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 115`).RespondStdout("https://github.com/acme/demo/issues/115#issuecomment-1\n", 0)
	env.ExpectGh(`^issue edit 115 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 115 `).RespondStdout("ok\n", 0)

	out := env.MustRun("explore", "115")
	harness.AssertContains(t, out, "Explored")

	inv := env.Invocations()
	// El bug: si no Ensure-amos el label que vamos a --remove-label, gh
	// falla con "not found" y la transición nunca ocurre. El fix garantiza
	// que ambos extremos (Add y Remove) existan en el repo.
	createIdea := inv.FindCalls("gh", "label", "create", "che:idea")
	if len(createIdea) == 0 {
		t.Fatalf("expected `gh label create status:idea` before transition; calls=%v", inv.For("gh"))
	}
	createPlan := inv.FindCalls("gh", "label", "create", "che:plan")
	if len(createPlan) == 0 {
		t.Fatalf("expected `gh label create status:plan` before transition; calls=%v", inv.For("gh"))
	}
	// Dos label edits: 1) che:idea → che:planning (lock), 2) che:planning → che:plan.
	labelEdits := inv.FindCalls("gh", "issue", "edit", "115", "--remove-label")
	if len(labelEdits) != 2 {
		t.Fatalf("expected 2 label edits (lock + transition), got %d", len(labelEdits))
	}
	labelEdits[0].AssertArgsContain(t,
		"--remove-label", "che:idea",
		"--add-label", "che:planning")
	labelEdits[1].AssertArgsContain(t,
		"--remove-label", "che:planning",
		"--add-label", "che:plan")
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
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
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
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
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
// the enum → exit 3, no comment. El flow nuevo SÍ hace el lock edit
// (idea→planning) antes del agente, y el rollback (planning→idea) después
// — son 2 edits esperados, no 0.
func TestExplore_ClaudeInvalidEffort_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	scriptExploreStateTransitions(env, 42)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_invalid_effort.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "effort")
	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "--body-file"); len(comments) > 0 {
		t.Fatalf("unexpected gh issue comment calls: %+v", comments)
	}
}

// TestExplore_ClaudeNoRecommended_Exit3: no path has recommended=true → exit 3.
func TestExplore_ClaudeNoRecommended_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	scriptExploreStateTransitions(env, 42)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_no_recommended.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "recommended")
}

// TestExplore_ClaudeMultipleRecommended_Exit3: two paths have recommended=true
// → exit 3.
func TestExplore_ClaudeMultipleRecommended_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	scriptExploreStateTransitions(env, 42)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_multiple_recommended.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "recommended")
}

// TestExplore_LabelEditFailsAfterComment_Exit2_WarnsOrphan: label edit final
// (che:planning → che:plan) falla después de postear comment + actualizar
// body → exit 2, stderr warnea. El primer edit (lock idea→planning) pasa
// porque sin él el flow ni siquiera llega al agente.
func TestExplore_LabelEditFailsAfterComment_Exit2_WarnsOrphan(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_ok.json", 0)
	env.ExpectGh(`^issue comment 42`).RespondStdout("https://github.com/acme/demo/issues/42#issuecomment-999\n", 0)
	env.ExpectGh(`^issue edit 42 --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	// Primer edit (lock idea→planning) pasa, segundo (planning→plan) falla.
	// El rollback del defer también intenta un edit (planning→idea) que
	// también falla — todo sumado el flow sale con exit 2.
	env.ExpectGh(`^issue edit 42 --remove-label che:idea`).
		Consumable().
		RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit 42 `).RespondExitWithError(1, "422 validation failed\n")

	r := env.Run("explore", "42")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "comentario posteado")
	harness.AssertContains(t, r.Stderr, "label")

	if comments := env.Invocations().FindCalls("gh", "issue", "comment", "--body-file"); len(comments) != 1 {
		t.Fatalf("expected 1 gh issue comment (posted before edit failed), got %d", len(comments))
	}
}

// TestExplore_URLAsRef: gh normalizes URL refs; pasamos el ref intacto.
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
	env.ExpectGh(`^issue edit .* --body-file`).RespondStdout("ok\n", 0)
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
	env.ExpectGh(`^issue edit `).RespondStdout("ok\n", 0)

	env.MustRun("explore", "https://github.com/acme/demo/issues/42")
}

// TestExplore_MissingConsolidatedPlan_Exit3: si el agente devuelve JSON
// sin consolidated_plan, explore rechaza con ExitSemantic — no se postea
// comment ni se hace la transición final, pero sí ocurrió el lock edit
// (idea→planning) + rollback (planning→idea).
func TestExplore_MissingConsolidatedPlan_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptExplorePrechecks(env)
	scriptExploreStateTransitions(env, 42)
	env.ExpectGh(`^issue view 42`).RespondStdoutFromFixture("explore/gh_issue_view_with_ctplan.json", 0)
	env.ExpectAgent("claude").
		WhenArgsMatch(`ingeniero senior`).
		RespondStdoutFromFixture("explore/sonnet_explore_without_consolidated.json", 0)

	r := env.Run("explore", "42")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "consolidated_plan")

	inv := env.Invocations()
	if comments := inv.FindCalls("gh", "issue", "comment", "--body-file"); len(comments) > 0 {
		t.Fatalf("unexpected comment calls: %+v", comments)
	}
	// Esperamos exactamente 2 edits: lock (idea→planning) + rollback
	// (planning→idea). Ningún body edit ni transición a che:plan.
	bodyEdits := inv.FindCalls("gh", "issue", "edit", "42", "--body-file")
	if len(bodyEdits) > 0 {
		t.Fatalf("unexpected body edits: %+v", bodyEdits)
	}
	for _, e := range inv.For("gh") {
		joined := strings.Join(e.Args, " ")
		if strings.Contains(joined, "--add-label che:plan ") || strings.HasSuffix(joined, "--add-label che:plan") {
			t.Fatalf("transition to che:plan should NOT happen on missing consolidated_plan: %v", e.Args)
		}
	}
}

func scriptExplorePrechecks(env *harness.Env) {
	env.ExpectGit(`^remote get-url origin`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in as acme\n", 0)
	scriptCheLockDefault(env)
	// El flow ahora hace `gh label create` para los estados che:* en cada
	// transición (idea→planning, planning→plan o rollback). Catch-all para
	// que los tests no tengan que listar cada label individualmente.
	env.ExpectGh(`^label create che:`).RespondStdout("ok\n", 0)
}

// scriptExploreStateTransitions agrega catch-all matchers para las
// transiciones de máquina de estados que el flow nuevo dispara antes Y
// después del agente (idea→planning, planning→plan, planning→idea
// rollback). Tests que necesiten asertar args específicos del label edit
// no deben llamar este helper. Tests que solo quieran "que el flow corra"
// y no asertar sobre las transiciones, sí.
func scriptExploreStateTransitions(env *harness.Env, issueNum int) {
	env.ExpectGh(fmt.Sprintf(`^issue edit %d --remove-label che:`, issueNum)).RespondStdout("ok\n", 0)
}
