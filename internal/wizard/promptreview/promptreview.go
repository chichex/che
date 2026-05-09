// Package promptreview corre una review automatica del prompt que el
// usuario tipeo en S2 del wizard. Llama a `claude -p` con un system prompt
// que pide evaluar imperatividad, ambigüedad, asunciones interactivas
// (ej. pedir confirmacion humana sin TTY) y especificacion de tools. La
// respuesta se parsea como JSON con shape Review.
//
// Disenado para ser asincrono desde el caller (bubbletea Cmd) — la fn
// bloquea mientras gh corre, no mientras el TUI responde.
package promptreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Review es el resultado parseado de la respuesta del CLI revisor.
type Review struct {
	// OK = true cuando el revisor considera que no hay nada que sugerir.
	// Los demas campos pueden venir vacios.
	OK bool `json:"ok"`
	// Issues = lista compacta de problemas detectados (1 frase por item).
	// Sirve al modal para el toast resumen ("N issues encontrados").
	Issues []string `json:"issues"`
	// Summary = un parrafo libre con el analisis general.
	Summary string `json:"summary"`
	// Suggested = una version reescrita del prompt que el revisor cree
	// que arregla los issues. Puede quedar vacio si el revisor no quiso
	// proponer una alternativa concreta.
	Suggested string `json:"suggested"`
	// Raw = la respuesta cruda del CLI (post strip de markdown), util
	// para debug + para mostrar al usuario en "ver detalle" si el JSON
	// no parseo limpio.
	Raw string `json:"-"`
}

// timeout acota cuanto puede tardar el CLI revisor. claude -p suele
// resolver en 5-30s; pasamos 60s para no cortar prompts largos. Si el
// usuario quiere una respuesta mas rapida, ctrl+c desde el modal igual
// cancela el subprocess (lo maneja el caller).
const timeout = 60 * time.Second

var (
	mu       sync.Mutex
	reviewFn = defaultReview
)

// Run dispatchea la review del prompt. Devuelve la Review parseada o un
// error si el CLI fallo / el output no es parseable. Pensado para ser
// llamado dentro de un bubbletea tea.Cmd (bloqueante).
func Run(prompt string) (Review, error) {
	mu.Lock()
	fn := reviewFn
	mu.Unlock()
	return fn(prompt)
}

// SetReviewFn instala un fake (tests). Devuelve la fn anterior para
// restaurar via t.Cleanup.
func SetReviewFn(fn func(prompt string) (Review, error)) func(prompt string) (Review, error) {
	mu.Lock()
	defer mu.Unlock()
	prev := reviewFn
	reviewFn = fn
	return prev
}

// systemPrompt es lo que le decimos a claude para que devuelva JSON con
// el analisis. Pedimos imperatividad + ausencia de tools interactivas
// (que es el modo de falla que motivo este feature — claude pidiendo
// AskUserQuestion en no-TTY y saliendo exit 0 sin trabajo real).
const systemPrompt = `Sos un revisor de prompts para steps de un pipeline ejecutado por CLIs de IA (claude, codex, gemini). El usuario te va a mandar un prompt; evalualo segun estos criterios:

1. **Imperativo**: pide acciones concretas (ej. "ejecutá X", "creá Y") en vez de pedir analisis ("evalua X", "decidi como Y"). Los pipelines necesitan ACTUAR, no proponer.
2. **No interactivo**: NO debe asumir confirmacion humana, AskUserQuestion, ni preguntas en mitad del trabajo. El runner corre en no-TTY: si el modelo pide permiso, queda colgado y termina sin hacer nada con exit 0.
3. **Sin ambigüedad**: estado final esperado bien definido; criterios de exito explicitos.
4. **Tools claras**: si necesita una tool especifica (Bash, gh, git), mencionarla. Si NO debe usar ciertas tools, prohibirlas explicitamente.

Respondé SOLO con un JSON valido (sin texto antes o despues, sin code fences) con este shape exacto:
{"ok": <bool>, "issues": ["<frase corta>", ...], "summary": "<parrafo>", "suggested": "<version mejorada del prompt o vacio>"}

Reglas:
- ok=true SOLO si el prompt cumple los 4 criterios y no tenes nada que sugerir.
- issues: lista compacta, 1 frase por item, sin numeracion. Vacia si ok=true.
- summary: 1-2 parrafos. Si ok=true, una linea explicando por que esta bien.
- suggested: prompt reescrito que arregla los issues. Vacio si ok=true o si no podes proponer una alternativa concreta.
- NO uses markdown, code fences, ni texto fuera del JSON.`

// defaultReview ejecuta `claude -p <combined prompt>` y parsea la
// respuesta como JSON. Si el subproceso devuelve un wrapper con prosa
// alrededor del JSON (claude a veces lo hace pese a la instruccion)
// intentamos extraer el primer bloque que parsea como Review.
func defaultReview(prompt string) (Review, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Review{}, errors.New("prompt vacio")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	combined := systemPrompt + "\n\n--- PROMPT A REVISAR ---\n" + prompt
	cmd := exec.CommandContext(ctx, "claude", "-p", combined)
	out, err := cmd.Output()
	if err != nil {
		// Si fue exit no-cero, intentar exponer stderr (lo capturamos
		// solo si fue ExitError; otros errores los devolvemos crudos).
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Review{Raw: string(out) + "\n" + string(ee.Stderr)}, fmt.Errorf("claude exit %d: %s", ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return Review{}, fmt.Errorf("claude: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	r, perr := parseReview(raw)
	if perr != nil {
		return Review{Raw: raw}, perr
	}
	r.Raw = raw
	return r, nil
}

// parseReview tolera respuestas con prosa antes/despues del JSON: busca
// el primer "{" y el ultimo "}" matcheado y prueba unmarshal sobre ese
// substring. Si falla, devuelve error con el raw para que el caller lo
// muestre al usuario.
func parseReview(raw string) (Review, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return Review{}, errors.New("respuesta sin JSON")
	}
	body := raw[start : end+1]
	var r Review
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return Review{}, fmt.Errorf("respuesta no parseable como Review: %w", err)
	}
	return r, nil
}
