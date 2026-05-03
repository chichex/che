package e2e_test

import (
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestIdea_CommandExists forces cmd/idea.go into existence as a one-shot
// subcommand for scripting/CI use. The interactive TUI path will reuse the
// same internal flow.
func TestIdea_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("idea", "--help")
	harness.AssertContains(t, out, "idea")
	harness.AssertContains(t, out, "issue")
}

// TestIdea_EmptyInput_Exit3: no text provided in any form → exit 3 without
// touching any external tool.
func TestIdea_EmptyInput_Exit3(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	r := env.Run("idea", "--text", "")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "empty")
	env.Invocations().AssertNotCalled(t, "claude")
	env.Invocations().AssertNotCalled(t, "gh")
}

// TestIdea_PrecheckNoGitHubRemote_Exit2: git remote get-url fails → exit 2
// without calling claude or gh issue create.
func TestIdea_PrecheckNoGitHubRemote_Exit2(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^remote get-url origin`).RespondExitWithError(1, "fatal: No such remote 'origin'\n")

	r := env.Run("idea", "--text", "agregar login")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "github remote")
	env.Invocations().AssertNotCalled(t, "claude")
}

// TestIdea_GoldenPath_Single: happy path with one issue created.
func TestIdea_GoldenPath_Single(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptIdeaPrechecks(env)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		CaptureStdin().
		RespondStdoutFromFixture("idea/sonnet_single.json", 0)
	env.ExpectGh(`^issue create .*che:state:idea`).
		Consumable().
		RespondStdout("https://github.com/acme/demo/issues/47\n", 0)

	out := env.MustRun("idea", "--text", "agregar login con GitHub OAuth")
	harness.AssertContains(t, out, "https://github.com/acme/demo/issues/47")
	harness.AssertContains(t, out, "Done")

	creates := env.Invocations().FindCalls("gh", "issue", "create")
	if len(creates) != 1 {
		t.Fatalf("expected 1 issue create, got %d", len(creates))
	}
	creates[0].AssertArgsContain(t,
		"--label", "type:feature",
		"--label", "size:m",
		"--label", "che:state:idea",
		"--label", "ct:plan")
}

// TestIdea_GoldenPath_SplitIntoTwo: two issues created in order, labels
// differentiated per item.
func TestIdea_GoldenPath_SplitIntoTwo(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptIdeaPrechecks(env)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("idea/sonnet_split_2.json", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondStdout("https://github.com/acme/demo/issues/47\n", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondStdout("https://github.com/acme/demo/issues/48\n", 0)

	out := env.MustRun("idea", "--text", "varias cosas a la vez")
	harness.AssertContains(t, out, "/issues/47")
	harness.AssertContains(t, out, "/issues/48")

	creates := env.Invocations().FindCalls("gh", "issue", "create")
	if len(creates) != 2 {
		t.Fatalf("expected 2 issue create, got %d", len(creates))
	}
	creates[0].AssertArgsContain(t, "type:feature", "size:s")
	creates[1].AssertArgsContain(t, "type:fix", "size:xs")
}

// TestIdea_InvalidType_Exit3_NoIssuesCreated: claude returns an item with an
// unknown type value → exit 3 and no gh issue create.
func TestIdea_InvalidType_Exit3_NoIssuesCreated(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptIdeaPrechecks(env)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("idea/sonnet_invalid_type.json", 0)

	r := env.Run("idea", "--text", "algo")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "bug")

	// Validation fails before any issue is created. No label create either —
	// ensureLabels runs after validate.
	if creates := env.Invocations().FindCalls("gh", "issue", "create"); len(creates) > 0 {
		t.Fatalf("unexpected gh issue create calls: %+v", creates)
	}
	if labels := env.Invocations().FindCalls("gh", "label", "create"); len(labels) > 0 {
		t.Fatalf("unexpected gh label create calls: %+v", labels)
	}
}

// TestIdea_SecondCreateFails_RollbackClosesFirst: issue 2 fails → issue 1
// gets closed, exit 2, no orphan warning.
func TestIdea_SecondCreateFails_RollbackClosesFirst(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptIdeaPrechecks(env)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("idea/sonnet_split_2.json", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondStdout("https://github.com/acme/demo/issues/47\n", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondExitWithError(1, "422 validation failed\n")
	env.ExpectGh(`^issue close https://github\.com/acme/demo/issues/47`).RespondStdout("Closed\n", 0)

	r := env.Run("idea", "--text", "dos cosas")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstdout: %s\nstderr: %s", r.ExitCode, r.Stdout, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "creating issue 2/2")
	harness.AssertNotContains(t, r.Stderr, "could not close")

	closes := env.Invocations().FindCalls("gh", "issue", "close")
	if len(closes) != 1 {
		t.Fatalf("expected 1 issue close (rollback of /47), got %d", len(closes))
	}
	closes[0].AssertArgsContain(t, "/47")
}

// TestIdea_RollbackCloseFails_ReportsOrphans: during rollback, closing /47
// fails but /48 succeeds. Exit 2, orphan list mentions /47, not /48.
func TestIdea_RollbackCloseFails_ReportsOrphans(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	scriptIdeaPrechecks(env)
	env.ExpectAgent("claude").
		WhenArgsMatch(`-p`).
		RespondStdoutFromFixture("idea/sonnet_split_3.json", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondStdout("https://github.com/acme/demo/issues/47\n", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondStdout("https://github.com/acme/demo/issues/48\n", 0)
	env.ExpectGh(`^issue create`).Consumable().RespondExitWithError(1, "rate limited\n")
	env.ExpectGh(`^issue close https://github\.com/acme/demo/issues/48`).RespondStdout("Closed\n", 0)
	env.ExpectGh(`^issue close https://github\.com/acme/demo/issues/47`).RespondExitWithError(1, "not found\n")

	r := env.Run("idea", "--text", "tres cosas")
	if r.ExitCode != 2 {
		t.Fatalf("expected exit 2, got %d\nstderr: %s", r.ExitCode, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "could not close")
	harness.AssertContains(t, r.Stderr, "/47")
}

func scriptIdeaPrechecks(env *harness.Env) {
	env.ExpectGit(`^remote get-url origin`).RespondStdout("https://github.com/acme/demo.git\n", 0)
	env.ExpectGh(`^auth status`).RespondStdout("Logged in as acme\n", 0)
	// gh label create --force es idempotente; lo scripteamos permisivo para
	// cualquier label que el flow requiera.
	env.ExpectGh(`^label create `).RespondStdout("ok\n", 0)
}
