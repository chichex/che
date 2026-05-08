// Package skills detecta skills/custom commands instalados en los CLIs que
// che orquesta (claude, codex, gemini, opencode), tanto a nivel usuario como
// dentro del proyecto actual. Es solo lectura: no modifica nada en disco.
package skills

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Scope indica si un skill se cargo desde la home del usuario o desde el
// proyecto en el cwd.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// Skill representa una unidad invocable detectada para un CLI dado. Source
// es el path absoluto al archivo que origino la entrada (SKILL.md o .toml)
// para que el caller pueda mostrarlo o abrirlo.
type Skill struct {
	Name        string
	Description string
	Scope       Scope
	Source      string
}

// CLI agrupa todo lo detectado para un CLI. Si Installed es false, Skills
// queda vacio — no escaneamos paths de un CLI que el usuario no tiene.
type CLI struct {
	Name      string
	Installed bool
	BinPath   string
	Skills    []Skill
}

// Detect inspecciona los 4 CLIs soportados y devuelve un CLI por cada uno
// (siempre 4 entradas, en orden fijo). cwd se usa para resolver el scope
// "project". Si Home() falla, igual se devuelven entradas marcando los CLIs
// como no instalados — el caller puede mostrar el resultado sin romper.
func Detect(cwd string) []CLI {
	home, _ := os.UserHomeDir()

	specs := []struct {
		name        string
		userDir     string
		projectDir  string
		scanFn      func(dir string, scope Scope) []Skill
	}{
		{"claude", joinIfHome(home, ".claude/skills"), filepath.Join(cwd, ".claude/skills"), scanSkillDirs},
		{"codex", joinIfHome(home, ".codex/skills"), filepath.Join(cwd, ".codex/skills"), scanSkillDirs},
		{"gemini", joinIfHome(home, ".gemini/commands"), filepath.Join(cwd, ".gemini/commands"), scanGeminiCommands},
		{"opencode", joinIfHome(home, ".config/opencode/skills"), filepath.Join(cwd, ".opencode/skills"), scanSkillDirs},
	}

	out := make([]CLI, 0, len(specs))
	for _, s := range specs {
		bin, installed := lookPath(s.name)
		c := CLI{Name: s.name, Installed: installed, BinPath: bin}
		if installed {
			if s.userDir != "" {
				c.Skills = append(c.Skills, s.scanFn(s.userDir, ScopeUser)...)
			}
			if s.projectDir != "" {
				c.Skills = append(c.Skills, s.scanFn(s.projectDir, ScopeProject)...)
			}
			sort.SliceStable(c.Skills, func(i, j int) bool {
				if c.Skills[i].Scope != c.Skills[j].Scope {
					return c.Skills[i].Scope == ScopeUser
				}
				return c.Skills[i].Name < c.Skills[j].Name
			})
		}
		out = append(out, c)
	}
	return out
}

// joinIfHome evita devolver un path "relativo a /" cuando no pudimos
// resolver la home — preferimos saltearnos el scope user antes que escanear
// "/.claude/skills" por accidente.
func joinIfHome(home, sub string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, sub)
}

var lookPath = func(name string) (string, bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return p, true
}

// scanSkillDirs cubre el layout claude/codex/opencode: cada skill es un
// subdirectorio con un SKILL.md adentro. El SKILL.md puede empezar con
// frontmatter YAML (--- name/description ---) o ser markdown plano (en cuyo
// caso usamos el nombre del dir y la primera linea no vacia como descr).
func scanSkillDirs(dir string, scope Scope) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		name, desc := parseSkillMarkdown(path, e.Name())
		skills = append(skills, Skill{
			Name:        name,
			Description: desc,
			Scope:       scope,
			Source:      path,
		})
	}
	return skills
}

// scanGeminiCommands cubre el layout de gemini: cada custom command es un
// .toml suelto en ~/.gemini/commands/. Solo nos interesa el campo
// description (single-line string) — si no esta, dejamos descripcion vacia.
func scanGeminiCommands(dir string, scope Scope) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var skills []Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		desc := extractTomlDescription(path)
		skills = append(skills, Skill{
			Name:        strings.TrimSuffix(e.Name(), ".toml"),
			Description: desc,
			Scope:       scope,
			Source:      path,
		})
	}
	return skills
}

// parseSkillMarkdown extrae name/description de un SKILL.md. Si hay
// frontmatter YAML ("---\n...\n---"), preferimos sus campos; si no, el
// dirName se usa como name y la primera linea no vacia (post-frontmatter)
// como descripcion. La descripcion se trunca al primer parrafo para que
// el listado quede legible.
func parseSkillMarkdown(path, dirName string) (string, string) {
	f, err := os.Open(path)
	if err != nil {
		return dirName, ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		name           = dirName
		desc           string
		inFrontmatter  bool
		seenFrontmatter bool
		bodyLines      []string
	)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			if strings.TrimSpace(line) == "---" {
				inFrontmatter = true
				seenFrontmatter = true
				continue
			}
		}
		if inFrontmatter {
			if strings.TrimSpace(line) == "---" {
				inFrontmatter = false
				continue
			}
			if k, v, ok := splitYAMLPair(line); ok {
				switch k {
				case "name":
					if v != "" {
						name = v
					}
				case "description":
					if v != "" {
						desc = v
					}
				}
			}
			continue
		}
		if seenFrontmatter && desc != "" {
			break
		}
		bodyLines = append(bodyLines, line)
	}

	if desc == "" {
		desc = firstParagraph(bodyLines)
	}
	return name, desc
}

// splitYAMLPair parsea una linea "key: value" del frontmatter, sacando
// comillas alrededor del value. No es un parser YAML real — no maneja
// listas, anchors ni multiline. Suficiente para los campos name/description
// que usan los SKILL.md en la practica.
func splitYAMLPair(line string) (string, string, bool) {
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:idx])
	v := strings.TrimSpace(line[idx+1:])
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return k, v, true
}

// firstParagraph junta las primeras lineas no vacias hasta encontrar una
// linea en blanco o un heading. Devuelve string vacio si todo es vacio.
func firstParagraph(lines []string) string {
	var collected []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			if len(collected) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			if len(collected) > 0 {
				break
			}
			continue
		}
		collected = append(collected, t)
	}
	return strings.Join(collected, " ")
}

// extractTomlDescription saca el valor de la clave description del TOML
// asumiendo el formato comun (description = "..." en una sola linea). No
// soporta multiline strings — si gemini guarda algo mas exotico devolvemos
// "" y dejamos que la UI muestre solo el nombre del archivo.
func extractTomlDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "description") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		v := strings.TrimSpace(line[eq+1:])
		if len(v) >= 2 && v[0] == '"' {
			end := strings.LastIndexByte(v, '"')
			if end > 0 {
				return v[1:end]
			}
		}
	}
	return ""
}
