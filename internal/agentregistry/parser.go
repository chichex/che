package agentregistry

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Frontmatter es el subset del frontmatter YAML que che consume.
//
// che ignora deliberadamente `color`, `tools`, `capabilities`, `hooks`,
// `mcpServers`, `permissionMode` y cualquier campo desconocido — los
// maneja Claude Code al invocar el subagent (§2.a del PRD). El parser
// es tolerante: campos no listados acá no fallan el parse.
type Frontmatter struct {
	Name        string
	Description string
	// Model: opus | sonnet | haiku | inherit. Vacío si no se declaró.
	Model string
}

// ErrNoFrontmatter indica que el archivo no abrió con `---`.
var ErrNoFrontmatter = errors.New("agentregistry: missing frontmatter opening `---`")

// ErrNameMissing indica que el frontmatter no declaró `name`. che usa
// `name` como id canónico, así que es el único campo realmente requerido.
var ErrNameMissing = errors.New("agentregistry: frontmatter missing required `name`")

// ParseFile lee un archivo .md de Claude Code y devuelve el subset de
// frontmatter que che usa. Wrapper alrededor de Parse para evitar que
// los call-sites tengan que abrir el file ellos mismos.
func ParseFile(path string) (Frontmatter, error) {
	f, err := os.Open(path)
	if err != nil {
		return Frontmatter{}, err
	}
	defer f.Close()
	fm, err := Parse(f)
	if err != nil {
		return Frontmatter{}, fmt.Errorf("%s: %w", path, err)
	}
	return fm, nil
}

// Parse lee un .md con frontmatter YAML + body y extrae los 3 campos
// que che consume. Tolera cualquier otro campo (los ignora silenciosamente).
//
// Formato pinneado contra la doc oficial de Claude Code (ver
// testdata/contract/canonical.md). Si Claude Code agrega un campo nuevo
// che lo ignora hasta que se actualice acá explícitamente.
func Parse(r io.Reader) (Frontmatter, error) {
	sc := bufio.NewScanner(r)
	// 1 MiB buffer: el default de 64 KiB trunca silenciosamente líneas
	// largas (caso bufio.Scanner-gotcha — frontmatter con descripciones
	// extensas estaría dentro del rango riesgoso).
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !sc.Scan() {
		return Frontmatter{}, ErrNoFrontmatter
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return Frontmatter{}, ErrNoFrontmatter
	}

	var fm Frontmatter
	closed := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		key, value, ok := parseScalarLine(line)
		if !ok {
			continue
		}
		switch key {
		case "name":
			fm.Name = value
		case "description":
			fm.Description = value
		case "model":
			fm.Model = value
		}
	}
	if err := sc.Err(); err != nil {
		return Frontmatter{}, err
	}
	if !closed {
		return Frontmatter{}, errors.New("agentregistry: missing frontmatter closing `---`")
	}
	if fm.Name == "" {
		return Frontmatter{}, ErrNameMissing
	}
	return fm, nil
}

// parseScalarLine extrae un par `key: value` top-level.
//
// Devuelve ok=false (skip silencioso) para:
//   - líneas vacías o sólo whitespace
//   - comentarios (`# …`)
//   - líneas indentadas (parte de un valor nested o array)
//   - items de lista (`- foo`)
//   - líneas sin `:` (no son key/value)
//
// Esto le da al parser tolerancia ante campos como `tools: [...]` o un
// array multilínea de `capabilities` sin meterlos al output.
func parseScalarLine(line string) (key, value string, ok bool) {
	trim := strings.TrimSpace(line)
	if trim == "" || strings.HasPrefix(trim, "#") {
		return "", "", false
	}
	// Indentado → nested (parte del valor del key anterior).
	if line != trim && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
		return "", "", false
	}
	if strings.HasPrefix(trim, "-") {
		return "", "", false
	}
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	value = unquoteScalar(value)
	return key, value, true
}

// unquoteScalar quita comillas envolventes (simples o dobles) si las
// hay. No interpreta escapes — es lo que necesita un frontmatter típico
// de Claude Code donde las descripciones quoted son raras.
func unquoteScalar(v string) string {
	if len(v) < 2 {
		return v
	}
	if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}
