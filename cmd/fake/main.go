// chefake is a polymorphic fake used by e2e tests to stand in for external
// binaries (gh, claude, codex, gemini, git). Its identity is determined by
// os.Args[0] via symlinks created by the test harness.
//
// On invocation, chefake reads $CHE_FAKE_SCRIPT_DIR/<identity>.json, walks the
// matchers in order, and emits the first match. Every invocation is appended
// (under flock) to $CHE_FAKE_SCRIPT_DIR/_invocations.jsonl so tests can assert
// on calls after the fact.
//
// When no matcher matches, chefake exits 1 with a diagnostic pointing to the
// script file. Tests that invoke a subprocess without scripting a response
// always fail loudly.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type matcher struct {
	ID            string            `json:"id"`
	ArgsRegex     string            `json:"args_regex,omitempty"`
	StdinContains string            `json:"stdin_contains,omitempty"`
	Consume       bool              `json:"consume,omitempty"`
	CaptureStdin  bool              `json:"capture_stdin,omitempty"`
	Stdout        string            `json:"stdout,omitempty"`
	StdoutFile    string            `json:"stdout_file,omitempty"`
	Stderr        string            `json:"stderr,omitempty"`
	Exit          int               `json:"exit,omitempty"`
	Passthrough   bool              `json:"passthrough,omitempty"` // reserved for git passthrough mode
	PassthroughTo string            `json:"passthrough_to,omitempty"`
	// TouchFiles lista paths (relativos al cwd donde se ejecutó el fake) que
	// el fake debe crear con el contenido dado al matchear. Usado por tests
	// de execute para simular que el agente modificó archivos en el worktree.
	TouchFiles map[string]string `json:"touch_files,omitempty"`
}

type script struct {
	Matchers []matcher `json:"matchers"`
	Default  *struct {
		Exit   int    `json:"exit"`
		Stderr string `json:"stderr"`
	} `json:"default,omitempty"`
}

type invocation struct {
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

func main() {
	start := time.Now()
	identity := filepath.Base(os.Args[0])
	args := os.Args[1:]

	scriptDir := os.Getenv("CHE_FAKE_SCRIPT_DIR")
	if scriptDir == "" {
		fmt.Fprintf(os.Stderr, "chefake: CHE_FAKE_SCRIPT_DIR not set (identity=%s)\n", identity)
		os.Exit(2)
	}

	stdin, _ := io.ReadAll(os.Stdin)
	stdinHash := sha256.Sum256(stdin)
	stdinSHA := hex.EncodeToString(stdinHash[:])

	scriptPath := filepath.Join(scriptDir, identity+".json")
	scr, err := loadScript(scriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chefake: loading %s: %v\n", scriptPath, err)
		logInvocation(scriptDir, invocation{
			Ts: start.UTC().Format(time.RFC3339Nano), Bin: identity, Args: args,
			StdinSHA: stdinSHA, StdinBytes: len(stdin), Exit: 2,
			DurationMs: time.Since(start).Milliseconds(),
		})
		os.Exit(2)
	}

	joinedArgs := strings.Join(args, " ")
	var matched *matcher
	var matchedIdx int
	for i := range scr.Matchers {
		m := &scr.Matchers[i]
		if m.ArgsRegex != "" {
			re, err := regexp.Compile(m.ArgsRegex)
			if err != nil {
				continue
			}
			if !re.MatchString(joinedArgs) {
				continue
			}
		}
		if m.StdinContains != "" && !strings.Contains(string(stdin), m.StdinContains) {
			continue
		}
		matched = m
		matchedIdx = i
		break
	}

	seq := nextSeq(scriptDir)

	if matched == nil {
		msg := fmt.Sprintf("chefake: no matcher for %q (fake=%s, scriptDir=%s)\n", joinedArgs, identity, scriptDir)
		exit := 1
		if scr.Default != nil {
			if scr.Default.Stderr != "" {
				msg = scr.Default.Stderr
			}
			exit = scr.Default.Exit
		}
		fmt.Fprint(os.Stderr, msg)
		logInvocation(scriptDir, invocation{
			Ts: start.UTC().Format(time.RFC3339Nano), Seq: seq, Bin: identity, Args: args,
			StdinSHA: stdinSHA, StdinBytes: len(stdin), StderrBytes: len(msg), Exit: exit,
			DurationMs: time.Since(start).Milliseconds(),
		})
		os.Exit(exit)
	}

	if matched.CaptureStdin {
		stdinDir := filepath.Join(scriptDir, "stdins")
		_ = os.MkdirAll(stdinDir, 0o755)
		_ = os.WriteFile(filepath.Join(stdinDir, fmt.Sprintf("%d.bin", seq)), stdin, 0o644)
	}

	stdoutBody := matched.Stdout
	if matched.StdoutFile != "" {
		data, err := os.ReadFile(matched.StdoutFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chefake: reading stdout_file %s: %v\n", matched.StdoutFile, err)
			os.Exit(2)
		}
		stdoutBody = string(data)
	}

	// Side effects: touch_files escribe archivos en cwd para simular que
	// el agente produjo cambios.
	for relPath, content := range matched.TouchFiles {
		dir := filepath.Dir(relPath)
		if dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = os.WriteFile(relPath, []byte(content), 0o644)
	}

	_, _ = io.WriteString(os.Stdout, stdoutBody)
	_, _ = io.WriteString(os.Stderr, matched.Stderr)

	if matched.Consume {
		markConsumed(scriptPath, matchedIdx)
	}

	logInvocation(scriptDir, invocation{
		Ts: start.UTC().Format(time.RFC3339Nano), Seq: seq, Bin: identity, Args: args,
		StdinSHA: stdinSHA, StdinBytes: len(stdin),
		StdoutBytes: len(stdoutBody), StderrBytes: len(matched.Stderr),
		Exit: matched.Exit, MatchedID: matched.ID,
		DurationMs: time.Since(start).Milliseconds(),
	})
	os.Exit(matched.Exit)
}

func loadScript(path string) (*script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &script{}, nil
		}
		return nil, err
	}
	var s script
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// markConsumed rewrites the script file with the matcher removed. We hold an
// advisory lock so concurrent invocations don't clobber each other.
func markConsumed(scriptPath string, idx int) {
	lock := scriptPath + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	scr, err := loadScript(scriptPath)
	if err != nil || idx >= len(scr.Matchers) {
		return
	}
	scr.Matchers = append(scr.Matchers[:idx], scr.Matchers[idx+1:]...)
	data, _ := json.MarshalIndent(scr, "", "  ")
	_ = os.WriteFile(scriptPath, data, 0o644)
}

// nextSeq returns a monotonic sequence number scoped to scriptDir, via a
// flock-protected counter file.
func nextSeq(scriptDir string) int {
	path := filepath.Join(scriptDir, "_seq")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, _ := io.ReadAll(f)
	var cur int
	if len(data) > 0 {
		_, _ = fmt.Sscanf(string(data), "%d", &cur)
	}
	cur++
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d", cur)
	return cur
}

func logInvocation(scriptDir string, inv invocation) {
	path := filepath.Join(scriptDir, "_invocations.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, _ := json.Marshal(inv)
	_, _ = f.Write(append(data, '\n'))
}
