package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// Matcher describes how chefake should respond when an invocation to a given
// identity (bin) satisfies the match rules.
type Matcher struct {
	ID            string            `json:"id"`
	ArgsRegex     string            `json:"args_regex,omitempty"`
	StdinContains string            `json:"stdin_contains,omitempty"`
	Consume       bool              `json:"consume,omitempty"`
	CaptureStdin  bool              `json:"capture_stdin,omitempty"`
	Stdout        string            `json:"stdout,omitempty"`
	StdoutFile    string            `json:"stdout_file,omitempty"`
	Stderr        string            `json:"stderr,omitempty"`
	Exit          int               `json:"exit"`
	TouchFiles    map[string]string `json:"touch_files,omitempty"`
	BlockSeconds  int               `json:"block_seconds,omitempty"`
}

// ExpectBuilder is the fluent API used to script a single matcher. Values
// accumulated via When* are combined with AND; the match succeeds only if all
// specified conditions hold.
type ExpectBuilder struct {
	env *Env
	bin string
	m   Matcher
}

// Expect begins a matcher for the given fake identity. Prefer ExpectGh /
// ExpectAgent / ExpectGit for clarity.
func (e *Env) Expect(bin string) *ExpectBuilder {
	e.t.Helper()
	assertKnownIdentity(e.t, bin)
	return &ExpectBuilder{env: e, bin: bin, m: Matcher{ID: nextMatcherID(bin)}}
}

// ExpectGh scripts an invocation of gh where the joined args match argsRegex.
func (e *Env) ExpectGh(argsRegex string) *ExpectBuilder {
	return e.Expect("gh").WhenArgsMatch(argsRegex)
}

// ExpectAgent scripts an invocation of one of the agent CLIs.
func (e *Env) ExpectAgent(name string) *ExpectBuilder {
	return e.Expect(name)
}

// ExpectGit scripts an invocation of git where the joined args match argsRegex.
func (e *Env) ExpectGit(argsRegex string) *ExpectBuilder {
	return e.Expect("git").WhenArgsMatch(argsRegex)
}

// ExpectBrew scripts an invocation of brew where the joined args match argsRegex.
func (e *Env) ExpectBrew(argsRegex string) *ExpectBuilder {
	return e.Expect("brew").WhenArgsMatch(argsRegex)
}

// WhenArgsMatch filters the matcher by a regex over the joined args (argv[1:]).
func (b *ExpectBuilder) WhenArgsMatch(re string) *ExpectBuilder {
	b.m.ArgsRegex = re
	return b
}

// WhenStdinContains requires the process stdin to contain the given substring.
func (b *ExpectBuilder) WhenStdinContains(s string) *ExpectBuilder {
	b.m.StdinContains = s
	return b
}

// CaptureStdin tells the fake to write the full stdin to scripts/stdins/<seq>.bin
// so the test can assert against it later (AssertStdinMatchesGolden).
func (b *ExpectBuilder) CaptureStdin() *ExpectBuilder {
	b.m.CaptureStdin = true
	return b
}

// Consumable marks the matcher as one-shot: after the first match, the matcher
// is removed from the script file. Use this for ordered sequences like
// successive `gh issue create` calls that must return different URLs.
func (b *ExpectBuilder) Consumable() *ExpectBuilder {
	b.m.Consume = true
	return b
}

// RespondStdout sets the stdout body and exit code that the fake will emit.
func (b *ExpectBuilder) RespondStdout(body string, exitCode int) *MatcherRef {
	b.m.Stdout = body
	b.m.Exit = exitCode
	return b.commit()
}

// RespondStdoutFromFixture reads a fixture file and emits its contents as stdout.
// path is relative to the e2e/testdata directory.
func (b *ExpectBuilder) RespondStdoutFromFixture(path string, exitCode int) *MatcherRef {
	abs := fixturePath(b.env.t, path)
	b.m.StdoutFile = abs
	b.m.Exit = exitCode
	return b.commit()
}

// RespondJSON marshals v to JSON and emits it as stdout with exit 0.
func (b *ExpectBuilder) RespondJSON(v any) *MatcherRef {
	data, err := json.Marshal(v)
	if err != nil {
		b.env.t.Fatalf("harness: RespondJSON marshal: %v", err)
	}
	b.m.Stdout = string(data)
	b.m.Exit = 0
	return b.commit()
}

// RespondExitWithError emits stderr and an error exit code (no stdout).
func (b *ExpectBuilder) RespondExitWithError(exitCode int, stderr string) *MatcherRef {
	b.m.Stderr = stderr
	b.m.Exit = exitCode
	return b.commit()
}

// TouchFile agenda la creación de un archivo con el contenido dado (relativo
// al cwd donde corre el fake) al matchear. Usado por los tests de execute
// para simular que el agente produjo cambios en el worktree.
func (b *ExpectBuilder) TouchFile(relPath, content string) *ExpectBuilder {
	if b.m.TouchFiles == nil {
		b.m.TouchFiles = map[string]string{}
	}
	b.m.TouchFiles[relPath] = content
	return b
}

// BlockSeconds hace que el fake, después de emitir stdout/stderr, duerma N
// segundos antes de salir. Los tests de signal handling lo usan para
// simular un agente que tarda: emite un sentinel (stdout), bloquea, y
// cuando el parent manda SIGTERM al process group el fake termina de una.
func (b *ExpectBuilder) BlockSeconds(n int) *ExpectBuilder {
	b.m.BlockSeconds = n
	return b
}

// MatcherRef is returned from a Respond* call so the test can refer back to
// the matcher if it needs to (future: for assertions like "this matcher was
// hit N times"). Currently mostly a placeholder.
type MatcherRef struct {
	ID  string
	Bin string
}

func (b *ExpectBuilder) commit() *MatcherRef {
	b.env.t.Helper()
	if err := appendMatcher(b.env.ScriptDir, b.bin, b.m); err != nil {
		b.env.t.Fatalf("harness: commit matcher: %v", err)
	}
	return &MatcherRef{ID: b.m.ID, Bin: b.bin}
}

// scriptFile is the on-disk schema. Keep the JSON tags aligned with
// cmd/fake/main.go — if they drift, the fake will silently fail to parse.
type scriptFile struct {
	Matchers []Matcher `json:"matchers"`
	Default  *struct {
		Exit   int    `json:"exit"`
		Stderr string `json:"stderr"`
	} `json:"default,omitempty"`
}

var matcherCounters sync.Map // bin -> *atomic.Int64

// nextMatcherID devuelve un ID único por bin ("gh-1", "gh-2", ...). Thread-safe
// porque los tests paralelos pueden llamar al helper concurrentemente; usar
// *atomic.Int64 evita la data race de incrementar un *int manualmente.
func nextMatcherID(bin string) string {
	v, _ := matcherCounters.LoadOrStore(bin, new(atomic.Int64))
	ptr := v.(*atomic.Int64)
	n := ptr.Add(1)
	return fmt.Sprintf("%s-%d", bin, n)
}

func appendMatcher(scriptDir, bin string, m Matcher) error {
	path := filepath.Join(scriptDir, bin+".json")
	var scr scriptFile
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &scr); err != nil {
			return fmt.Errorf("unmarshal existing script: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	scr.Matchers = append(scr.Matchers, m)
	data, err := json.MarshalIndent(&scr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func assertKnownIdentity(t testingTB, bin string) {
	t.Helper()
	idx := sort.SearchStrings(sortedIdentities, bin)
	if idx >= len(sortedIdentities) || sortedIdentities[idx] != bin {
		t.Fatalf("harness: unknown fake identity %q; registered=%v", bin, fakeIdentities)
	}
}

var sortedIdentities = func() []string {
	cp := append([]string(nil), fakeIdentities...)
	sort.Strings(cp)
	return cp
}()

// testingTB narrows the testing surface so helpers can accept both *testing.T
// and *testing.B if ever needed.
type testingTB interface {
	Helper()
	Fatalf(format string, args ...any)
}
