// Package idea implements flow 01 — anotar una idea, clasificarla, y crear
// los issues en GitHub. La lógica vive acá (pura, testeable) para que el
// subcomando `che idea` y la TUI compartan la misma implementación.
package idea

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chichex/che/internal/labels"
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

// Opts agrupa los writers y la callback de progreso. Si OnProgress es nil,
// el flow corre silencioso (modo CI).
type Opts struct {
	Stdout     io.Writer
	Stderr     io.Writer
	OnProgress func(string) // invocado por cada línea de output de los subprocesses
}

var (
	validTypes = map[string]bool{"feature": true, "fix": true, "mejora": true, "ux": true}
	validSizes = map[string]bool{"XS": true, "S": true, "M": true, "L": true, "XL": true}
)

// Run ejecuta el flow completo. Los writers de opts son el stdout/stderr
// "final" (URLs creadas, errores). OnProgress recibe las líneas que van
// saliendo de claude mientras corre — el caller decide qué hacer con ellas
// (mostrarlas en vivo en la TUI, escribirlas a log, o nada).
func Run(text string, opts Opts) ExitCode {
	stdout, stderr := opts.Stdout, opts.Stderr
	progress := opts.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(stderr, "error: idea text is empty")
		return ExitSemantic
	}

	progress("chequeando repo git y auth de GitHub…")
	if err := precheckGitHubRemote(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("consultando a claude…")
	resp, err := callClaude(text, progress)
	if err != nil {
		fmt.Fprintf(stderr, "error: calling claude: %v\n", err)
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		fmt.Fprintf(stderr, "error: claude response: %v\n", err)
		return ExitSemantic
	}

	progress(fmt.Sprintf("claude clasificó %d idea(s); preparando labels…", len(resp.Items)))
	if err := ensureLabels(resp.Items, progress); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitRetry
	}

	progress("creando issues…")
	fmt.Fprintf(stdout, "Creating %d issue(s)…\n", len(resp.Items))

	created := []string{}
	for i, item := range resp.Items {
		progress(fmt.Sprintf("creando issue %d/%d: %s", i+1, len(resp.Items), item.Title))
		url, err := createIssue(item)
		if err != nil {
			fmt.Fprintf(stderr, "error: creating issue %d/%d: %v\n", i+1, len(resp.Items), err)
			rollback(created, stderr, progress)
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

func callClaude(userText string, progress func(string)) (*Response, error) {
	prompt := buildPrompt(userText)
	cmd := exec.Command("claude", "-p", prompt, "--output-format", "text")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var fullOutput strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fullOutput.WriteString(line + "\n")
		if strings.TrimSpace(line) != "" {
			progress("claude: " + line)
		}
	}

	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderrBuf.String()))
		}
		return nil, err
	}

	return parseResponse(fullOutput.String())
}

// parseResponse extrae el JSON del output de claude, tolerando code fences
// y texto circundante.
func parseResponse(raw string) (*Response, error) {
	raw = strings.TrimSpace(raw)

	// Si viene con ```json ... ``` lo limpiamos.
	if strings.HasPrefix(raw, "```") {
		if nl := strings.Index(raw, "\n"); nl >= 0 {
			raw = raw[nl+1:]
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	// Si hay texto antes del primer `{`, cortamos hasta ahí.
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	// Idem después del último `}`.
	if i := strings.LastIndex(raw, "}"); i >= 0 && i < len(raw)-1 {
		raw = raw[:i+1]
	}

	var r Response
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("invalid JSON from claude: %w (raw: %q)", err, truncate(raw, 200))
	}
	return &r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func buildPrompt(userText string) string {
	return `Sos un clasificador de ideas para un backlog de ingeniería en GitHub.

Te voy a pasar una idea escrita en texto libre (como un commit message). Tu tarea:
1. Decidí si contiene una idea o varias ideas independientes (split).
2. Para cada idea, asigná type y size.
3. Armá title, body estructurado y criterios de éxito.

Tipos válidos: feature, fix, mejora, ux.
Tamaños válidos: XS (minutos), S (horas), M (1-2 días), L (1 semana), XL (varias semanas).

Devolvé EXCLUSIVAMENTE un objeto JSON con este shape — sin texto antes ni después, sin markdown code fences:

{
  "items": [
    {
      "type": "feature",
      "size": "M",
      "title": "Título imperativo corto, máx 60 chars",
      "idea": "Parafraseo accionable de la idea, 1-3 oraciones",
      "context_paths": ["ruta/al/archivo.go"],
      "context_area": "Área del producto (auth, billing, UI, etc.)",
      "dependencies": ["libs o módulos visibles"],
      "criteria": ["Criterio de éxito 1", "Criterio de éxito 2"],
      "notes": "Warnings, decisiones pendientes, coordinación necesaria",
      "size_reason": "Justificación corta del tamaño"
    }
  ]
}

Reglas:
- Si la idea cubre varias cosas independientes, items[] tiene un elemento por cada una.
- Si es una sola cosa, items[] tiene UN solo elemento.
- NUNCA devuelvas items[] vacío — si no podés clasificar, devolvé un único item con type=feature size=M y anotá en "notes" que la idea estaba ambigua.
- Los campos context_paths/dependencies pueden ir [] si no aplican, pero criteria[] tiene que tener al menos 1 criterio.

Idea del usuario:
<<<
` + userText + `
>>>`
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

// ensureLabels garantiza que los labels type:*, size:* y status:idea que se
// van a aplicar existan en el repo. Delega en labels.Ensure (idempotente).
func ensureLabels(items []Item, progress func(string)) error {
	seen := map[string]bool{}
	var names []string
	for _, it := range items {
		for _, l := range []string{
			"type:" + it.Type,
			"size:" + strings.ToLower(it.Size),
			labels.StatusIdea,
			labels.CtPlan,
		} {
			if !seen[l] {
				seen[l] = true
				names = append(names, l)
			}
		}
	}
	for _, lbl := range names {
		progress("asegurando label " + lbl)
		if err := labels.Ensure(lbl); err != nil {
			return err
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
		"--label", labels.StatusIdea,
		"--label", labels.CtPlan,
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

func rollback(created []string, stderr io.Writer, progress func(string)) {
	var orphans []string
	for i := len(created) - 1; i >= 0; i-- {
		url := created[i]
		progress("rollback: cerrando " + url)
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
