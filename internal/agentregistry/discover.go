package agentregistry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options controla el descubrimiento. Cada campo permite overridear un
// valor que en producción sale del entorno — esto deja a los tests
// pasar fixtures sin tocar HOME ni env vars de proceso.
type Options struct {
	// CWD es el punto de partida del walk-up para encontrar el primer
	// `.claude/agents/` del proyecto. Default: os.Getwd().
	CWD string
	// HomeDir es el home del usuario (host de `~/.claude/agents/` y
	// `~/.claude/plugins/`). Default: os.UserHomeDir().
	HomeDir string
	// ManagedDir apunta al directorio de agentes gestionados por org
	// admin. Si está vacío y el env var CHE_AGENTS_MANAGED_DIR tampoco
	// está seteado, la fuente managed queda deshabilitada (caso default).
	ManagedDir string
	// IncludeBuiltins decide si los 3 built-in (opus/sonnet/haiku) se
	// agregan al registry. Default true; tests que validan el parser
	// solo lo bajan a false.
	IncludeBuiltins bool
}

// Discover escanea las 4 ubicaciones oficiales + agrega los built-in
// y devuelve un Registry con la precedencia ya aplicada.
//
// Errores parciales (un .md mal formado, un dir sin permisos) se
// devuelven en el slice junto al Registry — el discovery es
// best-effort: un agente roto no debe tirar todo el listado.
func Discover(opts Options) (*Registry, []error) {
	if opts.CWD == "" {
		if cwd, err := os.Getwd(); err == nil {
			opts.CWD = cwd
		}
	}
	if opts.HomeDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.HomeDir = home
		}
	}
	if opts.ManagedDir == "" {
		opts.ManagedDir = os.Getenv("CHE_AGENTS_MANAGED_DIR")
	}

	var (
		all  []Agent
		errs []error
	)

	if opts.ManagedDir != "" {
		agents, err := scanFlat(opts.ManagedDir, SourceManaged)
		if err != nil {
			errs = append(errs, err...)
		}
		all = append(all, agents...)
	}

	if projectDir := walkUpForClaudeAgents(opts.CWD); projectDir != "" {
		agents, err := scanFlat(projectDir, SourceProject)
		if err != nil {
			errs = append(errs, err...)
		}
		all = append(all, agents...)
	}

	if opts.HomeDir != "" {
		userDir := filepath.Join(opts.HomeDir, ".claude", "agents")
		agents, err := scanFlat(userDir, SourceUser)
		if err != nil {
			errs = append(errs, err...)
		}
		all = append(all, agents...)

		pluginsRoot := filepath.Join(opts.HomeDir, ".claude", "plugins")
		pluginAgents, pErr := scanPlugins(pluginsRoot)
		if pErr != nil {
			errs = append(errs, pErr...)
		}
		all = append(all, pluginAgents...)
	}

	if opts.IncludeBuiltins {
		all = append(all, builtins()...)
	}

	reg, collisions := buildRegistry(all)
	return reg, append(errs, collisions...)
}

// walkUpForClaudeAgents walkea desde dir hacia raíz buscando el primer
// `<X>/.claude/agents/` que sea directorio. Devuelve el path absoluto
// o "" si no encontró. No corta en git roots — cualquier ancestro vale,
// matchea el comportamiento documentado de Claude Code.
func walkUpForClaudeAgents(dir string) string {
	if dir == "" {
		return ""
	}
	cur, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(cur, ".claude", "agents")
		if isDir(candidate) {
			return candidate
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// scanFlat recorre dir buscando *.md (no recursivo). Cada archivo se
// parsea como agent file; el nombre canónico viene del frontmatter
// `name`. Errores de parse individuales se acumulan en errs.
func scanFlat(dir string, src Source) ([]Agent, []error) {
	if !isDir(dir) {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("agentregistry: read %s: %w", dir, err)}
	}
	var (
		agents []Agent
		errs   []error
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fm, err := ParseFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		agents = append(agents, Agent{
			Name:        fm.Name,
			Description: fm.Description,
			Model:       fm.Model,
			Source:      src,
			Path:        path,
		})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, errs
}

// scanPlugins itera <pluginsRoot>/<plugin>/agents/**/*.md. El primer
// dir bajo pluginsRoot es el plugin name; los subdirs bajo agents/
// contribuyen al namespace canónico (`plugin:subdir:name`).
//
// Si el plugin no tiene `agents/`, se saltea silenciosamente —
// no todos los plugins exponen subagents.
func scanPlugins(pluginsRoot string) ([]Agent, []error) {
	if !isDir(pluginsRoot) {
		return nil, nil
	}
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		return nil, []error{fmt.Errorf("agentregistry: read %s: %w", pluginsRoot, err)}
	}
	var (
		agents []Agent
		errs   []error
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		plugin := e.Name()
		agentsDir := filepath.Join(pluginsRoot, plugin, "agents")
		if !isDir(agentsDir) {
			continue
		}
		walkErr := filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("agentregistry: walk %s: %w", path, err))
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
				return nil
			}
			fm, err := ParseFile(path)
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			rel, relErr := filepath.Rel(agentsDir, filepath.Dir(path))
			subdir := ""
			if relErr == nil && rel != "." {
				subdir = filepath.ToSlash(rel)
			}
			canonical := plugin + ":"
			if subdir != "" {
				canonical += strings.ReplaceAll(subdir, "/", ":") + ":"
			}
			canonical += fm.Name
			agents = append(agents, Agent{
				Name:        canonical,
				Description: fm.Description,
				Model:       fm.Model,
				Source:      SourcePlugin,
				Path:        path,
			})
			return nil
		})
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("agentregistry: walk plugin %s: %w", plugin, walkErr))
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, errs
}
