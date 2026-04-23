// Package idea implements flow 01 — anotar una idea, clasificarla, y crear
// los issues en GitHub. La lógica vive acá (pura, testeable) para que el
// subcomando `che idea` y la TUI compartan la misma implementación.
package idea

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chichex/che/internal/labels"
	"github.com/chichex/che/internal/output"
)

// ErrInvalidResponse indica que la respuesta del LLM parseó pero violó el
// contrato (JSON sin items, type/size fuera del enum, title/idea vacíos).
// Expuesto como sentinel para que callers externos (p.ej. explore) puedan
// distinguir con errors.Is un error irremediable (LLM alucinó, reintentar no
// sirve) de un fallo de invocación (red, binario caído).
var ErrInvalidResponse = errors.New("invalid llm response")

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

// Opts agrupa el writer de stdout (payload estable: URLs creadas,
// "Done.") y el logger estructurado (progress + errores). Si Out es nil,
// el flow corre silencioso (NopSink).
type Opts struct {
	Stdout io.Writer
	Out    *output.Logger
}

var (
	validTypes = map[string]bool{"feature": true, "fix": true, "mejora": true, "ux": true}
	validSizes = map[string]bool{"XS": true, "S": true, "M": true, "L": true, "XL": true}
)

// Classify invoca al LLM con el prompt de clasificación de ideas y devuelve
// la respuesta validada. Expuesto para que otros flows (explore) puedan
// reclassificar issues que no nacieron de `che idea` sin duplicar el prompt
// ni las reglas de validación (type/size enums, criterios mínimos).
//
// El shape devuelto es idéntico al que usa `Run`: items[] con type, size,
// title, criteria, etc. El caller que solo necesite type+size puede leer
// Items[0].
func Classify(text string, log *output.Logger) (*Response, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: text is empty", ErrInvalidResponse)
	}
	if log == nil {
		log = output.New(nil)
	}
	resp, err := callClaude(text, log)
	if err != nil {
		return nil, err
	}
	if err := validate(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Run ejecuta el flow completo.
func Run(text string, opts Opts) ExitCode {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	log := opts.Out
	if log == nil {
		log = output.New(nil)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		log.Error("idea text is empty")
		return ExitSemantic
	}

	log.Info("chequeando repo git y auth de GitHub")
	if err := precheckGitHubRemote(); err != nil {
		log.Error("github remote invalido", output.F{Cause: err})
		return ExitRetry
	}
	if err := precheckGhAuth(); err != nil {
		log.Error("gh auth fallo", output.F{Cause: err})
		return ExitRetry
	}

	log.Step("consultando a claude")
	resp, err := callClaude(text, log)
	if err != nil {
		log.Error("calling claude failed", output.F{Agent: "claude", Cause: err})
		return ExitRetry
	}

	if err := validate(resp); err != nil {
		log.Error("claude response invalid", output.F{Cause: err})
		return ExitSemantic
	}

	log.Success("idea clasificada", output.F{Detail: fmt.Sprintf("%d item(s)", len(resp.Items))})
	if err := ensureLabels(resp.Items, log); err != nil {
		log.Error("no se pudieron asegurar labels", output.F{Cause: err})
		return ExitRetry
	}

	fmt.Fprintf(stdout, "Creating %d issue(s)…\n", len(resp.Items))

	created := []string{}
	for i, item := range resp.Items {
		log.Step(fmt.Sprintf("creating issue %d/%d", i+1, len(resp.Items)), output.F{Detail: item.Title})
		url, err := createIssue(item)
		if err != nil {
			log.Error(fmt.Sprintf("creating issue %d/%d failed", i+1, len(resp.Items)), output.F{Cause: err})
			rollback(created, log)
			return ExitRetry
		}
		created = append(created, url)
		log.Success("issue creado", output.F{
			URL: url,
			Labels: []string{
				"type:" + item.Type,
				"size:" + strings.ToLower(item.Size),
				labels.CheIdea,
				labels.CtPlan,
			},
		})
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

func callClaude(userText string, log *output.Logger) (*Response, error) {
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
			log.Step("claude: " + line)
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
		return fmt.Errorf("%w: items array is empty", ErrInvalidResponse)
	}
	for i, it := range r.Items {
		if !validTypes[it.Type] {
			return fmt.Errorf("%w: item %d: type %q not in [feature fix mejora ux]", ErrInvalidResponse, i, it.Type)
		}
		if !validSizes[strings.ToUpper(it.Size)] {
			return fmt.Errorf("%w: item %d: size %q not in [XS S M L XL]", ErrInvalidResponse, i, it.Size)
		}
		if strings.TrimSpace(it.Title) == "" {
			return fmt.Errorf("%w: item %d: title is required", ErrInvalidResponse, i)
		}
		if strings.TrimSpace(it.Idea) == "" {
			return fmt.Errorf("%w: item %d: idea is required", ErrInvalidResponse, i)
		}
	}
	return nil
}

// ensureLabels garantiza que los labels type:*, size:* y che:idea que se
// van a aplicar existan en el repo. Delega en labels.Ensure (idempotente).
func ensureLabels(items []Item, log *output.Logger) error {
	seen := map[string]bool{}
	var names []string
	for _, it := range items {
		for _, l := range []string{
			"type:" + it.Type,
			"size:" + strings.ToLower(it.Size),
			labels.CheIdea,
			labels.CtPlan,
		} {
			if !seen[l] {
				seen[l] = true
				names = append(names, l)
			}
		}
	}
	for _, lbl := range names {
		log.Step("asegurando label", output.F{Labels: []string{lbl}})
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
		"--label", labels.CheIdea,
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

func rollback(created []string, log *output.Logger) {
	var orphans []string
	for i := len(created) - 1; i >= 0; i-- {
		url := created[i]
		log.Step("rollback: cerrando", output.F{URL: url})
		if err := exec.Command("gh", "issue", "close", url).Run(); err != nil {
			orphans = append(orphans, url)
		}
	}
	if len(orphans) > 0 {
		log.Warn(fmt.Sprintf("could not close %d issue(s) created before failure", len(orphans)))
		for _, u := range orphans {
			log.Warn("orphan", output.F{URL: u})
		}
		log.Info("close them manually (`gh issue close <url>`) before re-running")
	}
}
