// Package idea implements flow 01 — anotar una idea, clasificarla, y crear
// los issues en GitHub. La lógica vive acá (pura, testeable) para que el
// subcomando `che idea` y la TUI compartan la misma implementación.
package idea

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Response es lo que esperamos que devuelva el agente de clasificación.
type Response struct {
	Items []Item `json:"items"`
}

// Item es una idea ya clasificada lista para convertirse en issue.
type Item struct {
	Type         string   `json:"type"`
	Size         string   `json:"size"`
	Title        string   `json:"title"`
	Idea         string   `json:"idea"`
	ContextPaths []string `json:"context_paths"`
	ContextArea  string   `json:"context_area"`
	Dependencies []string `json:"dependencies"`
	Criteria     []string `json:"criteria"`
	Notes        string   `json:"notes"`
	SizeReason   string   `json:"size_reason"`
}

// ExitCode es el código de salida semántico para el caller.
type ExitCode int

const (
	ExitOK       ExitCode = 0
	ExitRetry    ExitCode = 2 // error remediable (red, gh falla, rollback aplicado)
	ExitSemantic ExitCode = 3 // entrada vacía, JSON inválido, enums fuera de rango
)

var (
	validTypes = map[string]bool{"feature": true, "fix": true, "mejora": true, "ux": true}
	validSizes = map[string]bool{"XS": true, "S": true, "M": true, "L": true, "XL": true}
)

// Run ejecuta el flow completo. Toma el texto de la idea por parámetro, hace
// los prechecks, invoca al agente, crea los issues y (si aplica) rollback.
func Run(text string, stdout, stderr io.Writer) ExitCode {
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(stderr, "error: idea text is empty")
		return ExitSemantic
	}

	if err := precheckGitHubRemote(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	resp, err := callClaude(text)
	if err != nil {
		fmt.Fprintf(stderr, "error: calling claude: %v\n", err)
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		fmt.Fprintf(stderr, "error: claude response: %v\n", err)
		return ExitSemantic
	}

	fmt.Fprintf(stdout, "Creating %d issue(s)…\n", len(resp.Items))

	created := []string{}
	for i, item := range resp.Items {
		url, err := createIssue(item)
		if err != nil {
			fmt.Fprintf(stderr, "error: creating issue %d/%d: %v\n", i+1, len(resp.Items), err)
			rollback(created, stderr)
			return ExitRetry
		}
		created = append(created, url)
		fmt.Fprintf(stdout, "Created %s\n", url)
	}

	fmt.Fprintln(stdout, "Done.")
	return ExitOK
}

func precheckGitHubRemote() error {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return fmt.Errorf("github remote: no origin configured")
	}
	url := strings.TrimSpace(string(out))
	if !strings.Contains(url, "github.com") {
		return fmt.Errorf("github remote: origin is not github.com: %s", url)
	}
	return nil
}

func precheckGhAuth() error {
	out, err := exec.Command("gh", "auth", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh auth: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func callClaude(userText string) (*Response, error) {
	prompt := buildPrompt(userText)
	cmd := exec.Command("claude",
		"-p",
		"--output-format", "json",
	)
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("exit %d: %s", ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var r Response
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("invalid JSON from claude: %w", err)
	}
	return &r, nil
}

func buildPrompt(userText string) string {
	// Placeholder prompt — fine-tunearemos contra claude real.
	return "system: devolvé JSON con items[] clasificando la idea.\n\nuser:\n" + userText
}

func validate(r *Response) error {
	if len(r.Items) == 0 {
		return fmt.Errorf("items array is empty")
	}
	for i, it := range r.Items {
		if !validTypes[it.Type] {
			return fmt.Errorf("item %d: type %q not in [feature fix mejora ux]", i, it.Type)
		}
		if !validSizes[strings.ToUpper(it.Size)] {
			return fmt.Errorf("item %d: size %q not in [XS S M L XL]", i, it.Size)
		}
		if strings.TrimSpace(it.Title) == "" {
			return fmt.Errorf("item %d: title is required", i)
		}
		if strings.TrimSpace(it.Idea) == "" {
			return fmt.Errorf("item %d: idea is required", i)
		}
	}
	return nil
}

func createIssue(item Item) (string, error) {
	body := renderBody(item)

	tmpDir, err := os.MkdirTemp("", "che-idea-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return "", err
	}

	args := []string{
		"issue", "create",
		"--title", item.Title,
		"--body-file", bodyFile,
		"--label", "type:" + item.Type,
		"--label", "size:" + strings.ToLower(item.Size),
		"--label", "status:idea",
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func renderBody(it Item) string {
	var sb strings.Builder
	sb.WriteString("## Idea\n")
	sb.WriteString(it.Idea + "\n\n")

	sb.WriteString("## Contexto detectado\n")
	if len(it.ContextPaths) > 0 {
		sb.WriteString("- Archivos/módulos relevantes: " + strings.Join(it.ContextPaths, ", ") + "\n")
	}
	if it.ContextArea != "" {
		sb.WriteString("- Área afectada: " + it.ContextArea + "\n")
	}
	if len(it.Dependencies) > 0 {
		sb.WriteString("- Dependencias: " + strings.Join(it.Dependencies, ", ") + "\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Criterios de éxito iniciales\n")
	for _, c := range it.Criteria {
		sb.WriteString("- [ ] " + c + "\n")
	}
	sb.WriteString("\n")

	if it.Notes != "" {
		sb.WriteString("## Notas / warnings\n")
		sb.WriteString(it.Notes + "\n\n")
	}

	sb.WriteString("## Clasificación\n")
	sb.WriteString("- Type: " + it.Type + "\n")
	sb.WriteString("- Size: " + strings.ToUpper(it.Size))
	if it.SizeReason != "" {
		sb.WriteString(" — " + it.SizeReason)
	}
	sb.WriteString("\n")

	return sb.String()
}

func rollback(created []string, stderr io.Writer) {
	var orphans []string
	for i := len(created) - 1; i >= 0; i-- {
		url := created[i]
		if err := exec.Command("gh", "issue", "close", url).Run(); err != nil {
			orphans = append(orphans, url)
		}
	}
	if len(orphans) > 0 {
		fmt.Fprintf(stderr, "warning: could not close %d issue(s) created before failure:\n", len(orphans))
		for _, u := range orphans {
			fmt.Fprintf(stderr, "  - %s\n", u)
		}
		fmt.Fprintln(stderr, "close them manually (`gh issue close <url>`) before re-running")
	}
}
