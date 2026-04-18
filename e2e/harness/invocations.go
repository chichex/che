package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Invocation is one row of the fake's JSONL log.
type Invocation struct {
	Ts          string   `json:"ts"`
	Seq         int      `json:"seq"`
	Bin         string   `json:"bin"`
	Args        []string `json:"args"`
	StdinSHA    string   `json:"stdin_sha256"`
	StdinBytes  int      `json:"stdin_bytes"`
	StdoutBytes int      `json:"stdout_bytes"`
	StderrBytes int      `json:"stderr_bytes"`
	Exit        int      `json:"exit"`
	MatchedID   string   `json:"matched_id,omitempty"`
	DurationMs  int64    `json:"duration_ms"`
}

// InvocationLog provides assertions over the sequence of fake calls made
// during a test. It reads the log file on demand — call Invocations() after
// the che process has exited.
type InvocationLog struct {
	t     *testing.T
	calls []Invocation
}

// Invocations reads and parses the log. If the log file does not exist
// (nothing was invoked), it returns an empty log.
func (e *Env) Invocations() *InvocationLog {
	e.t.Helper()
	path := filepath.Join(e.ScriptDir, "_invocations.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &InvocationLog{t: e.t}
		}
		e.t.Fatalf("harness: open invocations log: %v", err)
	}
	defer f.Close()

	var calls []Invocation
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var inv Invocation
		if err := json.Unmarshal(line, &inv); err != nil {
			e.t.Fatalf("harness: parse invocation %q: %v", line, err)
		}
		calls = append(calls, inv)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].Seq < calls[j].Seq })
	return &InvocationLog{t: e.t, calls: calls}
}

// All returns every invocation in seq order.
func (l *InvocationLog) All() []Invocation { return l.calls }

// For returns invocations targeting the given fake identity.
func (l *InvocationLog) For(bin string) []Invocation {
	var out []Invocation
	for _, c := range l.calls {
		if c.Bin == bin {
			out = append(out, c)
		}
	}
	return out
}

// AssertCalled fails the test if bin was invoked a number of times other than want.
func (l *InvocationLog) AssertCalled(t *testing.T, bin string, want int) {
	t.Helper()
	got := len(l.For(bin))
	if got != want {
		t.Fatalf("invocations: expected %s called %d times, got %d\nall calls: %s",
			bin, want, got, l.summary())
	}
}

// AssertNotCalled fails if bin was invoked at all.
func (l *InvocationLog) AssertNotCalled(t *testing.T, bin string) {
	t.Helper()
	l.AssertCalled(t, bin, 0)
}

// CallOf returns the nth (1-indexed) invocation of bin, failing the test if
// fewer than n invocations were made.
func (l *InvocationLog) CallOf(bin string, n int) *Invocation {
	l.t.Helper()
	calls := l.For(bin)
	if n < 1 || n > len(calls) {
		l.t.Fatalf("invocations: %s call #%d not found (only %d calls)", bin, n, len(calls))
	}
	return &calls[n-1]
}

// FindCalls returns every invocation of bin whose args contain ALL the given
// needles. Order of returned calls matches invocation order.
func (l *InvocationLog) FindCalls(bin string, needles ...string) []Invocation {
	var out []Invocation
outer:
	for _, c := range l.For(bin) {
		joined := strings.Join(c.Args, " ")
		for _, n := range needles {
			if !strings.Contains(joined, n) {
				continue outer
			}
		}
		out = append(out, c)
	}
	return out
}

// AssertArgsContain fails if any of needles is not present in the args slice.
func (i *Invocation) AssertArgsContain(t *testing.T, needles ...string) {
	t.Helper()
	joined := strings.Join(i.Args, " ")
	for _, n := range needles {
		if !strings.Contains(joined, n) {
			t.Fatalf("invocation %s (seq=%d): args missing %q\nargs: %v", i.Bin, i.Seq, n, i.Args)
		}
	}
}

func (l *InvocationLog) summary() string {
	var parts []string
	for _, c := range l.calls {
		parts = append(parts, fmt.Sprintf("%d:%s %s (exit=%d)", c.Seq, c.Bin, strings.Join(c.Args, " "), c.Exit))
	}
	if len(parts) == 0 {
		return "(no invocations)"
	}
	return strings.Join(parts, "\n")
}
