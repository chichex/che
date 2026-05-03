// Package auditlog mantiene un comment dedicado en el issue raíz que
// registra cada transición de estado del pipeline (PRD §8). El comment
// es editable: cada nueva entrada se appendea al body existente vía
// `gh api PATCH .../comments/<id>` en vez de crear comments duplicados,
// para que el timeline quede compacto y filtrable como un solo bloque.
//
// Marker:
//
//	<!-- claude-cli: skill=audit-log -->
//
// Es un HTML comment (invisible al render) en la primera línea del body.
// Usamos el mismo formato que los headers de comments del paquete
// internal/comments pero con un namespace distinto (skill=audit-log) para
// que no se confunda con un comment de un flow concreto.
//
// Idempotencia:
//   - El comment se identifica buscando el marker en los comments del
//     issue. Si lo encuentra, edita; si no, crea uno nuevo.
//   - Append no detecta duplicados de líneas: si el caller llama dos veces
//     con el mismo from→to, se appendean dos entradas. La unicidad la
//     garantiza el caller (un solo Append por transición exitosa).
//
// El paquete NO sabe nada de la máquina de estados — recibe strings
// libres ("from", "to", "flow") y los serializa como una línea Markdown.
// Es deliberado: si mañana el modelo de labels cambia, este paquete no
// necesita actualizarse.
package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Marker es el HTML comment que identifica al comment del audit log
// dentro del thread del issue. Mantener exportado para que tests fuera
// del paquete (e2e) puedan grep contra el body.
const Marker = "<!-- claude-cli: skill=audit-log -->"

// Title es el header Markdown que prefija el comment para que un humano
// que abra el issue entienda qué es ese bloque sin leer el HTML.
const Title = "## Audit log (che pipelines)"

// Entry es una transición concreta a registrar en el log.
type Entry struct {
	// At es el timestamp del evento. Si zero al pasarse a Append, se usa
	// time.Now() automáticamente.
	At time.Time

	// Flow es el nombre del flow que generó la transición ("explore",
	// "execute", "validate-pr", etc.). Texto libre — el paquete no
	// valida.
	Flow string

	// From es el estado de origen (label crudo, ej. `che:state:idea`).
	From string

	// To es el estado destino. Vacío si la entrada describe una acción
	// que no transiciona (ej. lock acquired); el renderer omite la
	// flecha en ese caso.
	To string

	// Note es texto opcional que se agrega entre paréntesis al final de
	// la línea — usado para discriminar rollback vs success ("rollback",
	// "stale-evicted", etc.).
	Note string
}

// Options ajusta el comportamiento de Append. Cero-valor es válido (usa
// gh REST). Tests stubean los hooks para evitar shell-out.
type Options struct {
	// Now devuelve el "ahora" para entradas con At zero. Default time.Now.
	Now func() time.Time

	// ListComments devuelve los comments del issue/PR identificado por
	// number. Default `gh api /repos/.../issues/<n>/comments`. Tests
	// stubean. Cada Comment tiene ID y Body.
	ListComments func(number int) ([]Comment, error)

	// CreateComment postea un comment nuevo y devuelve su ID. Default
	// `gh issue comment <n> --body-file ...`.
	CreateComment func(number int, body string) (int64, error)

	// EditComment reemplaza el body de un comment existente. Default
	// `gh api PATCH /repos/.../issues/comments/<id>`.
	EditComment func(commentID int64, body string) error
}

// Comment es la vista mínima que necesitamos del comment. ID identifica
// al comment (no al issue) — necesario para PATCH.
type Comment struct {
	ID   int64
	Body string
}

// Append añade `entry` al comment de audit log del issue/PR identificado
// por `number`. Si no hay comment con el marker, lo crea; si sí, edita.
//
// El body resultante tiene este shape:
//
//	<!-- claude-cli: skill=audit-log -->
//	## Audit log (che pipelines)
//
//	- 2024-12-01T10:23:45Z · explore · che:state:idea → che:state:applying:explore
//	- 2024-12-01T10:24:15Z · explore · che:state:applying:explore → che:state:explore
//	- 2024-12-01T10:30:00Z · execute · che:state:explore → che:state:applying:execute (rollback)
//
// Devuelve el ID del comment (creado o editado) para que el caller pueda
// loguearlo.
func Append(number int, entry Entry, opts Options) (int64, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if entry.At.IsZero() {
		entry.At = now()
	}
	listComments := opts.ListComments
	if listComments == nil {
		listComments = ghListComments
	}
	createComment := opts.CreateComment
	if createComment == nil {
		createComment = ghCreateComment
	}
	editComment := opts.EditComment
	if editComment == nil {
		editComment = ghEditComment
	}

	line := renderEntry(entry)

	comments, err := listComments(number)
	if err != nil {
		return 0, fmt.Errorf("auditlog: list comments for #%d: %w", number, err)
	}
	for _, c := range comments {
		if !strings.Contains(c.Body, Marker) {
			continue
		}
		// Comment existente: appendear la línea al final.
		newBody := strings.TrimRight(c.Body, "\n") + "\n" + line
		if err := editComment(c.ID, newBody); err != nil {
			return 0, fmt.Errorf("auditlog: edit comment %d: %w", c.ID, err)
		}
		return c.ID, nil
	}
	// Sin comment previo: creamos uno con el marker + título + primera entrada.
	body := Marker + "\n" + Title + "\n\n" + line
	id, err := createComment(number, body)
	if err != nil {
		return 0, fmt.Errorf("auditlog: create comment on #%d: %w", number, err)
	}
	return id, nil
}

// renderEntry serializa un Entry como una línea Markdown bullet. Mantenida
// pública (lowercase pero testeada en archivo del paquete) para que tests
// puedan validar el formato sin pasar por Append.
//
// Formato:
//
//	- <RFC3339> · <flow> · <from> → <to> [(<note>)]
//
// Si To está vacío, se omite la flecha. Si Note está vacío, se omite el
// paréntesis.
func renderEntry(e Entry) string {
	var sb strings.Builder
	sb.WriteString("- ")
	sb.WriteString(e.At.UTC().Format(time.RFC3339))
	if e.Flow != "" {
		sb.WriteString(" · ")
		sb.WriteString(e.Flow)
	}
	if e.From != "" || e.To != "" {
		sb.WriteString(" · ")
		sb.WriteString(e.From)
		if e.To != "" {
			sb.WriteString(" → ")
			sb.WriteString(e.To)
		}
	}
	if e.Note != "" {
		sb.WriteString(" (")
		sb.WriteString(e.Note)
		sb.WriteString(")")
	}
	return sb.String()
}

// ghListComments corre `gh api /repos/{owner}/{repo}/issues/<n>/comments`.
// El endpoint devuelve issues Y PRs uniformemente (un PR es un issue en
// REST, mismo patrón que internal/labels.Lock).
//
// Default per_page es 30; subimos a 100 para minimizar paginación. Si
// alguien tiene >100 comments y el del audit log no está en los primeros
// 100, este paquete no lo encuentra y crea otro — caso patológico (un
// thread con 100+ comments es raro en repos manejados por che). Si pasa,
// el operador puede borrar el comment fantasma a mano.
func ghListComments(number int) ([]Comment, error) {
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments?per_page=100", number),
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api comments: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var raw []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}
	out2 := make([]Comment, 0, len(raw))
	for _, r := range raw {
		out2 = append(out2, Comment{ID: r.ID, Body: r.Body})
	}
	return out2, nil
}

// ghCreateComment postea un comment vía `gh issue comment <n> --body-file`
// (mismo patrón que el resto del codebase) y devuelve el ID del comment
// creado. La URL la imprime gh por stdout — para extraer el ID hacemos
// una llamada extra a la API (la URL contiene el number del issue, no el
// del comment). Es un costo aceptable: 1 comment de auditoría por flow,
// post-creación, una sola vez.
//
// Si no podemos extraer el ID (gh emite un formato inesperado), devolvemos
// 0 sin error: el comment quedó creado, el siguiente Append lo va a
// encontrar por el marker (no por el ID que el caller usa solo para log).
func ghCreateComment(number int, body string) (int64, error) {
	tmpDir, err := os.MkdirTemp("", "che-auditlog-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "audit.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return 0, err
	}
	cmd := exec.Command("gh", "issue", "comment", fmt.Sprintf("%d", number), "--body-file", bodyFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("gh issue comment: %s", strings.TrimSpace(string(out)))
	}
	// gh imprime la URL del comment, ej:
	// https://github.com/owner/repo/issues/42#issuecomment-1234567890
	url := strings.TrimSpace(string(out))
	if i := strings.LastIndex(url, "issuecomment-"); i >= 0 {
		idStr := url[i+len("issuecomment-"):]
		// Cortar por cualquier carácter no numérico que aparezca después.
		end := 0
		for end < len(idStr) && idStr[end] >= '0' && idStr[end] <= '9' {
			end++
		}
		if end > 0 {
			var id int64
			_, _ = fmt.Sscanf(idStr[:end], "%d", &id)
			return id, nil
		}
	}
	return 0, nil
}

// ghEditComment reemplaza el body de un comment existente vía REST. La
// API es PATCH /repos/{owner}/{repo}/issues/comments/{id} con `body` como
// field. Importante: NO usamos `gh issue comment --edit-last` porque ese
// no permite identificar el comment por marker — solo "el último". Si el
// humano agregó un comment después, --edit-last apunta al equivocado.
func ghEditComment(commentID int64, body string) error {
	tmpDir, err := os.MkdirTemp("", "che-auditlog-edit-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	bodyFile := filepath.Join(tmpDir, "body.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("gh", "api",
		"-X", "PATCH",
		fmt.Sprintf("repos/{owner}/{repo}/issues/comments/%d", commentID),
		"-F", "body=@"+bodyFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api PATCH comment %d: %s", commentID, strings.TrimSpace(string(out)))
	}
	return nil
}
