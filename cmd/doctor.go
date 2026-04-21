package cmd

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/chichex/che/internal/output"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "chequea que el entorno esté listo para usar che",
	Long: `doctor chequea que todos los binarios externos (gh, claude, codex, gemini, git)
estén instalados, que gh esté autenticado, y que el working dir sea un repo
con remote a GitHub. Devuelve exit 0 si todo está OK, o 1 si falta algo
mandatory.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		log := output.New(output.NewWriterSink(cmd.OutOrStdout()))
		return runDoctor(log)
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

type checkResult struct {
	label    string
	ok       bool
	optional bool
	detail   string
}

func runDoctor(log *output.Logger) error {
	results := []checkResult{
		checkBinaryResponds("git", "git"),
		checkGitHubRemote(),
		checkBinaryResponds("gh", "gh"),
		checkGhAuth(),
		checkBinaryResponds("claude", "claude"),
		checkBinaryResponds("codex", "codex", optional()),
		checkBinaryResponds("gemini", "gemini", optional()),
	}

	failed := 0
	for _, r := range results {
		fields := output.F{Detail: r.detail}
		switch {
		case r.ok:
			log.Success(r.label, fields)
		case r.optional:
			log.Warn(r.label, fields)
		default:
			log.Error(r.label, fields)
			failed++
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

type checkOpt func(*checkResult)

func optional() checkOpt { return func(r *checkResult) { r.optional = true } }

func checkBinaryResponds(label, bin string, opts ...checkOpt) checkResult {
	r := checkResult{label: label}
	for _, o := range opts {
		o(&r)
	}
	if _, err := exec.LookPath(bin); err != nil {
		r.detail = "not installed"
		return r
	}
	if err := exec.Command(bin, "--version").Run(); err != nil {
		r.detail = fmt.Sprintf("`%s --version` failed: %v", bin, err)
		return r
	}
	r.ok = true
	return r
}

func checkGhAuth() checkResult {
	r := checkResult{label: "gh auth"}
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		r.detail = strings.TrimSpace(string(out))
		if r.detail == "" {
			r.detail = err.Error()
		}
		return r
	}
	r.ok = true
	return r
}

func checkGitHubRemote() checkResult {
	r := checkResult{label: "github remote"}
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		r.detail = "no origin remote"
		return r
	}
	url := strings.TrimSpace(string(out))
	if !strings.Contains(url, "github.com") {
		r.detail = "origin is not github.com: " + url
		return r
	}
	r.ok = true
	return r
}
