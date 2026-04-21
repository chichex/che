package closing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/labels"
)

// Helper para construir PRs con labels/refs sin repetir struct literal.
func mkPR(num int, opts ...func(*PullRequest)) *PullRequest {
	pr := &PullRequest{Number: num, State: "OPEN", HeadBranch: "feat/x"}
	for _, o := range opts {
		o(pr)
	}
	return pr
}

func withLabel(name string) func(*PullRequest) {
	return func(p *PullRequest) {
		p.Labels = append(p.Labels, struct {
			Name string `json:"name"`
		}{Name: name})
	}
}

func withMergeable(state string) func(*PullRequest) {
	return func(p *PullRequest) { p.Mergeable = state }
}

func withMergeStateStatus(state string) func(*PullRequest) {
	return func(p *PullRequest) { p.MergeStateStatus = state }
}

func withClosing(nums ...int) func(*PullRequest) {
	return func(p *PullRequest) {
		for _, n := range nums {
			p.ClosingIssuesReferences = append(p.ClosingIssuesReferences, struct {
				Number int    `json:"number"`
				State  string `json:"state"`
			}{Number: n, State: "OPEN"})
		}
	}
}

func TestBlockingVerdict(t *testing.T) {
	cases := []struct {
		name string
		pr   *PullRequest
		want string
	}{
		{"sin labels", mkPR(1), ""},
		{"validated:approve no bloquea", mkPR(1, withLabel(labels.ValidatedApprove)), ""},
		{"changes-requested bloquea", mkPR(1, withLabel(labels.ValidatedChangesRequested)), labels.ValidatedChangesRequested},
		{"needs-human bloquea", mkPR(1, withLabel(labels.ValidatedNeedsHuman)), labels.ValidatedNeedsHuman},
		{"otros labels no bloquean", mkPR(1, withLabel("ct:plan"), withLabel("status:executed")), ""},
		{"approve + changes-requested (worst case gana)", mkPR(1,
			withLabel(labels.ValidatedApprove),
			withLabel(labels.ValidatedChangesRequested)), labels.ValidatedChangesRequested},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BlockingVerdict(c.pr)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestHasConflicts(t *testing.T) {
	cases := []struct {
		name string
		pr   *PullRequest
		want bool
	}{
		{"mergeable=MERGEABLE", mkPR(1, withMergeable("MERGEABLE")), false},
		{"mergeable=UNKNOWN", mkPR(1, withMergeable("UNKNOWN")), false},
		{"mergeable=CONFLICTING", mkPR(1, withMergeable("CONFLICTING")), true},
		{"mergeStateStatus=DIRTY", mkPR(1, withMergeStateStatus("DIRTY")), true},
		{"clean", mkPR(1, withMergeable("MERGEABLE"), withMergeStateStatus("CLEAN")), false},
		{"behind no cuenta como conflict", mkPR(1, withMergeable("MERGEABLE"), withMergeStateStatus("BEHIND")), false},
		{"lowercase mergeable conflicting", mkPR(1, withMergeable("conflicting")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasConflicts(c.pr)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestAggregateCIState(t *testing.T) {
	mk := func(state string) Check { return Check{Name: "test", State: state} }

	cases := []struct {
		name   string
		checks []Check
		want   CIState
	}{
		{"empty → none", nil, CINone},
		{"todos success", []Check{mk("SUCCESS"), mk("SUCCESS")}, CIGreen},
		{"uno failure", []Check{mk("SUCCESS"), mk("FAILURE")}, CIFailing},
		{"uno pending, resto success", []Check{mk("SUCCESS"), mk("PENDING")}, CIPending},
		{"pending + failure → failing (failure tiene prioridad)", []Check{mk("PENDING"), mk("FAILURE")}, CIFailing},
		{"in_progress", []Check{mk("IN_PROGRESS"), mk("SUCCESS")}, CIPending},
		{"queued", []Check{mk("QUEUED")}, CIPending},
		{"skipped cuenta como green", []Check{mk("SUCCESS"), mk("SKIPPED"), mk("NEUTRAL")}, CIGreen},
		{"cancelled como failure", []Check{mk("SUCCESS"), mk("CANCELLED")}, CIFailing},
		{"timed_out como failure", []Check{mk("TIMED_OUT")}, CIFailing},
		{"action_required como failure", []Check{mk("ACTION_REQUIRED")}, CIFailing},
		{"case insensitive", []Check{mk("success"), mk("failure")}, CIFailing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := aggregateCIState(c.checks)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestFailingChecks(t *testing.T) {
	checks := []Check{
		{Name: "lint", State: "SUCCESS"},
		{Name: "unit", State: "FAILURE"},
		{Name: "integration", State: "PENDING"},
		{Name: "e2e", State: "CANCELLED"},
	}
	got := failingChecks(checks)
	if len(got) != 2 {
		t.Fatalf("want 2 failing, got %d", len(got))
	}
	names := []string{got[0].Name, got[1].Name}
	if names[0] != "unit" || names[1] != "e2e" {
		t.Fatalf("unexpected failing checks: %+v", got)
	}
}

func TestProblemsList(t *testing.T) {
	cases := []struct {
		name     string
		conflict bool
		ci       CIState
		want     []string
	}{
		{"nothing", false, CIGreen, nil},
		{"none ci", false, CINone, nil},
		{"solo conflicts", true, CIGreen, []string{"conflicts"}},
		{"solo ci failing", false, CIFailing, []string{"ci-failing"}},
		{"solo ci pending", false, CIPending, []string{"ci-pending"}},
		{"conflicts + ci failing", true, CIFailing, []string{"conflicts", "ci-failing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := problemsList(c.conflict, c.ci)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestFirstClosingIssue(t *testing.T) {
	cases := []struct {
		name string
		pr   *PullRequest
		want int
	}{
		{"sin refs", mkPR(1), 0},
		{"un ref", mkPR(1, withClosing(42)), 42},
		{"multiples refs → devuelve primero", mkPR(1, withClosing(42, 43, 44)), 42},
		{"ref con num=0 ignorado", mkPR(1, withClosing(0, 7)), 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := firstClosingIssue(c.pr)
			if got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestWorktreePathFor(t *testing.T) {
	root := "/tmp/repo"
	cases := []struct {
		name     string
		issueNum int
		branch   string
		want     string
	}{
		{"con issueNum", 42, "feat/foo", filepath.Join(root, ".worktrees", "issue-42")},
		{"sin issueNum, branch simple", 0, "hotfix", filepath.Join(root, ".worktrees", "pr-hotfix")},
		{"sin issueNum, branch con slash", 0, "feat/foo-bar", filepath.Join(root, ".worktrees", "pr-feat-foo-bar")},
		{"branch con chars raros", 0, "user/fix!?.go", filepath.Join(root, ".worktrees", "pr-user-fix--.go")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := worktreePathFor(root, c.issueNum, c.branch)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSanitizeBranchSlug(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"main", "main"},
		{"feat/foo", "feat-foo"},
		{"user/fix.go", "user-fix.go"},
		{"", "pr"},
		{"a/b/c/d", "a-b-c-d"},
		{"weird!@#", "weird---"},
		{"a/b", "a-b"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := sanitizeBranchSlug(c.in); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildFixPrompt_IncludesContext(t *testing.T) {
	pr := &PullRequest{
		Number:     42,
		Title:      "fix the thing",
		URL:        "https://github.com/acme/demo/pull/42",
		HeadBranch: "fix/the-thing",
	}
	checks := []Check{
		{Name: "unit-tests", State: "FAILURE", Workflow: "CI", Link: "https://example.com/run/1"},
	}

	prompt := buildFixPrompt(pr, true, checks)

	mustContain := []string{
		"PR #42",
		"fix the thing",
		"fix/the-thing",
		"Conflictos con main",
		"CI rojo",
		"unit-tests",
		"FAILURE",
		"workflow=CI",
		"https://example.com/run/1",
		"git push",
	}
	for _, s := range mustContain {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", s, prompt)
		}
	}
}

func TestBuildFixPrompt_OnlyConflicts(t *testing.T) {
	pr := &PullRequest{Number: 7, Title: "t", HeadBranch: "b"}
	prompt := buildFixPrompt(pr, true, nil)
	if !strings.Contains(prompt, "Conflictos con main") {
		t.Errorf("missing conflicts section")
	}
	if strings.Contains(prompt, "CI rojo") {
		t.Errorf("should not mention CI when no failing checks")
	}
}

func TestBuildFixPrompt_OnlyCI(t *testing.T) {
	pr := &PullRequest{Number: 7, Title: "t", HeadBranch: "b"}
	checks := []Check{{Name: "lint", State: "FAILURE"}}
	prompt := buildFixPrompt(pr, false, checks)
	if strings.Contains(prompt, "Conflictos con main") {
		t.Errorf("should not mention conflicts when not conflicting")
	}
	if !strings.Contains(prompt, "CI rojo") {
		t.Errorf("missing CI section")
	}
}

func TestCIState_String(t *testing.T) {
	cases := map[CIState]string{
		CINone:    "none",
		CIGreen:   "green",
		CIFailing: "failing",
		CIPending: "pending",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("CIState(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestJoinInts(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{42}, "#42"},
		{[]int{1, 2, 3}, "#1, #2, #3"},
	}
	for _, c := range cases {
		if got := joinInts(c.in); got != c.want {
			t.Errorf("joinInts(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPullRequest_HasLabel(t *testing.T) {
	pr := mkPR(1, withLabel("foo"), withLabel("bar"))
	if !pr.HasLabel("foo") {
		t.Errorf("expected HasLabel(foo)=true")
	}
	if !pr.HasLabel("bar") {
		t.Errorf("expected HasLabel(bar)=true")
	}
	if pr.HasLabel("baz") {
		t.Errorf("expected HasLabel(baz)=false")
	}
}

func TestGroupCloseable(t *testing.T) {
	mk := func(n int, lbls ...string) validate.PullRequest {
		pr := validate.PullRequest{Number: n}
		for _, l := range lbls {
			pr.Labels = append(pr.Labels, struct {
				Name string `json:"name"`
			}{Name: l})
		}
		return pr
	}
	nums := func(cs []validate.Candidate) []int {
		out := make([]int, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Number)
		}
		return out
	}
	cases := []struct {
		name        string
		in          []validate.PullRequest
		wantReady   []int
		wantBlocked []int
	}{
		{"empty", nil, nil, nil},
		{"sin labels → ready", []validate.PullRequest{mk(1), mk(2)}, []int{1, 2}, nil},
		{"approve → ready (target ideal)",
			[]validate.PullRequest{mk(1, labels.ValidatedApprove)}, []int{1}, nil},
		{"changes-requested → blocked",
			[]validate.PullRequest{mk(1, labels.ValidatedChangesRequested)}, nil, []int{1}},
		{"needs-human → blocked",
			[]validate.PullRequest{mk(1, labels.ValidatedNeedsHuman)}, nil, []int{1}},
		{"mix: preserva orden dentro de cada grupo",
			[]validate.PullRequest{
				mk(1, labels.ValidatedApprove),
				mk(2, labels.ValidatedChangesRequested),
				mk(3, labels.ValidatedNeedsHuman),
				mk(4),
			},
			[]int{1, 4}, []int{2, 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := groupCloseable(c.in)
			gotReady, gotBlocked := nums(got.Ready), nums(got.Blocked)
			if len(gotReady) != len(c.wantReady) {
				t.Fatalf("ready: want %v, got %v", c.wantReady, gotReady)
			}
			for i := range gotReady {
				if gotReady[i] != c.wantReady[i] {
					t.Errorf("ready[%d]: want %d, got %d", i, c.wantReady[i], gotReady[i])
				}
			}
			if len(gotBlocked) != len(c.wantBlocked) {
				t.Fatalf("blocked: want %v, got %v", c.wantBlocked, gotBlocked)
			}
			for i := range gotBlocked {
				if gotBlocked[i] != c.wantBlocked[i] {
					t.Errorf("blocked[%d]: want %d, got %d", i, c.wantBlocked[i], gotBlocked[i])
				}
			}
		})
	}
}

// TestMergePRArgs protege el contrato de que che close NUNCA pasa
// --delete-branch a gh pr merge, independientemente de --keep-branch.
// El delete remoto lo hacemos nosotros post-merge (gh api) porque
// --delete-branch falla cuando la branch está checkouteada en un worktree
// — el merge remoto ocurre igual, pero gh devuelve exit != 0 y el flow se
// caería con ExitRetry aunque el PR ya esté mergeado.
func TestMergePRArgs(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want []string
	}{
		{
			name: "ref numérico",
			ref:  "7",
			want: []string{"pr", "merge", "7", "--merge"},
		},
		{
			name: "ref URL funciona igual",
			ref:  "https://github.com/acme/demo/pull/7",
			want: []string{"pr", "merge", "https://github.com/acme/demo/pull/7", "--merge"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergePRArgs(c.ref)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
			for _, a := range got {
				if a == "--delete-branch" {
					t.Fatalf("mergePRArgs debe NO incluir --delete-branch (el delete remoto es post-merge): got %v", got)
				}
			}
		})
	}
}

// TestShouldCleanupWorktree codifica el contrato público de
// shouldCleanupWorktree, usado por el defer de Run para decidir si el
// worktree asociado al PR debe removerse al terminar.
//
// Casos de contrato:
//   - --keep-branch: preserva SIEMPRE, aunque el worktree lo hayamos creado
//     (el validador de v0.0.32 detectó que el comportamiento anterior
//     contradecía el help del flag).
//   - happy path sin --keep-branch: limpia el worktree asociado.
//   - failure path: limpia solo si lo creamos en este run (no dejar residuo
//     propio), pero no tocamos worktrees reusados.
func TestShouldCleanupWorktree(t *testing.T) {
	cases := []struct {
		name       string
		mergedOK   bool
		keepBranch bool
		wtOwned    bool
		want       bool
	}{
		{"happy path sin flags: borra todo", true, false, false, true},
		{"happy path con --keep-branch: preserva", true, true, false, false},
		{"happy path con --keep-branch, worktree owned: igual preserva (keep-branch manda)", true, true, true, false},
		{"happy path con worktree owned: borra", true, false, true, true},
		{"early-return con worktree owned: limpia residuo propio", false, false, true, true},
		{"early-return sin worktree owned: no toca nada", false, false, false, false},
		{"early-return con --keep-branch sin owned: no toca nada", false, true, false, false},
		{"early-return con --keep-branch y owned: preserva (keep-branch manda)", false, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldCleanupWorktree(c.mergedOK, c.keepBranch, c.wtOwned)
			if got != c.want {
				t.Fatalf("shouldCleanupWorktree(mergedOK=%v, keepBranch=%v, wtOwned=%v) = %v, want %v",
					c.mergedOK, c.keepBranch, c.wtOwned, got, c.want)
			}
		})
	}
}

// TestBranchOutcomeMessage cubre las cuatro ramas del stdout post-merge:
// keep-branch, "already removed" (auto-delete pre-merge detectado), "kept
// on remote" (delete remoto falló post-merge), y el caso default "Deleted
// branch". El branch "already removed" no se ejerce en e2e porque allí
// seteamos CHE_CLOSE_SKIP_REMOTE_CHECK=1 (que fuerza preRemoteKnown=false),
// así que este unit test es la única protección contra regresiones en ese
// mensaje.
//
// Precedencia esperada: keepBranch > already-removed > remoteDeleteFailed
// > default-deleted. keepBranch gana siempre (el usuario pidió preservar)
// y already-removed va antes que remoteDeleteFailed porque si la branch ya
// no estaba pre-merge no tiene sentido decir "delete failed".
func TestBranchOutcomeMessage(t *testing.T) {
	cases := []struct {
		name               string
		branch             string
		keepBranch         bool
		preRemoteKnown     bool
		preRemoteMissing   bool
		remoteDeleteFailed bool
		want               string
	}{
		{"keep-branch manda sobre todo lo demás", "feat/x", true, true, true, false, "Keeping branch feat/x (--keep-branch)"},
		{"keep-branch sin info de remote", "feat/x", true, false, false, false, "Keeping branch feat/x (--keep-branch)"},
		{"keep-branch gana a remoteDeleteFailed (imposible en la práctica pero robusto)", "feat/x", true, false, false, true, "Keeping branch feat/x (--keep-branch)"},
		{"already removed: known + missing", "feat/x", false, true, true, false, "Branch feat/x already removed"},
		{"delete remoto falló post-merge", "feat/x", false, true, false, true, "Branch feat/x kept on remote (delete failed)"},
		{"delete falló con remote desconocido pre-merge", "feat/x", false, false, false, true, "Branch feat/x kept on remote (delete failed)"},
		{"deleted: remote conocido y presente pre-merge", "feat/x", false, true, false, false, "Deleted branch feat/x"},
		{"deleted: remote desconocido (skip-check / red caída)", "feat/x", false, false, false, false, "Deleted branch feat/x"},
		{"deleted: known=false ignora preRemoteMissing=true", "feat/x", false, false, true, false, "Deleted branch feat/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := branchOutcomeMessage(c.branch, c.keepBranch, c.preRemoteKnown, c.preRemoteMissing, c.remoteDeleteFailed)
			if got != c.want {
				t.Fatalf("branchOutcomeMessage(%q, keep=%v, known=%v, missing=%v, deleteFailed=%v) = %q, want %q",
					c.branch, c.keepBranch, c.preRemoteKnown, c.preRemoteMissing, c.remoteDeleteFailed, got, c.want)
			}
		})
	}
}

// TestIsCheManagedWorktree verifica que solo aceptamos paths bajo
// `<repoRoot>/.worktrees/` como gestionados por che — el cleanup depende
// de esto para no tocar el worktree principal ni worktrees del usuario.
func TestIsCheManagedWorktree(t *testing.T) {
	root := t.TempDir()
	// Crear el árbol real — canonPath resuelve symlinks (macOS /var vs
	// /private/var) y para ser consistente necesita que los paths existan.
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	mustMkdir(filepath.Join(root, ".worktrees", "issue-42", "sub"))
	externalDir := t.TempDir()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"worktree bajo .worktrees/", filepath.Join(root, ".worktrees", "issue-42"), true},
		{"worktree anidado bajo .worktrees/", filepath.Join(root, ".worktrees", "issue-42", "sub"), true},
		{"repoRoot directo NO es managed", root, false},
		{".worktrees/ mismo NO es managed (hay que estar dentro)", filepath.Join(root, ".worktrees"), false},
		{"directorio afuera del repo", externalDir, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCheManagedWorktree(root, c.path); got != c.want {
				t.Fatalf("isCheManagedWorktree(%q, %q) = %v, want %v", root, c.path, got, c.want)
			}
		})
	}
}

func TestFormatOpusLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{"plain text", "hola", "hola", true},
		{"empty", "", "", false},
		{"system init", `{"type":"system","subtype":"init"}`, "sesión lista, arrancando…", true},
		{"assistant tool use", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`, "tool: Bash", true},
		{"result success", `{"type":"result","subtype":"success"}`, "agente terminó OK", true},
		{"result with subtype", `{"type":"result","subtype":"error_max_turns"}`, "agente terminó (error_max_turns)", true},
		{"non-event JSON", `{"type":"unknown"}`, "", false},
		{"malformed JSON", `{not-json`, `{not-json`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := formatOpusLine(c.line)
			if ok != c.ok {
				t.Fatalf("ok: got %v, want %v (out=%q)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
