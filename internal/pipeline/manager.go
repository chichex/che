package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Convención del layout en disco (PRD §7.a). Exportadas para que los
// subcomandos del manager (`che pipeline new`, `che pipeline use`)
// puedan reportar paths sin recomputar.
const (
	// PipelinesDirRel es el directorio donde viven los archivos de
	// pipeline (un `.json` por pipeline, el filename sin extensión es el
	// nombre canónico).
	PipelinesDirRel = ".che/pipelines"

	// ConfigFileRel es el archivo que declara qué pipeline es default
	// para el repo. Vive en `.che/`, NO dentro de `.che/pipelines/` —
	// los pipelines son data, el config es metadata, separados a
	// propósito.
	ConfigFileRel = ".che/pipelines.config.json"

	// ConfigVersion es la única versión de Config soportada en v1.
	// Cambia cuando se agregan campos breaking-change al config (ej.
	// defaults por kind de entity, ver "Decisiones cerradas" del PRD).
	ConfigVersion = 1
)

// Config es la representación on-disk de `.che/pipelines.config.json`.
//
// v1 sólo tiene `version` y `default`. Mantener la struct exportada y
// los json tags estables permite que `che pipeline use <name>` la
// regrabe sin perder campos no conocidos por la versión actual de che
// (cuando agreguemos campos en v2, las versiones viejas tienen que
// ignorarlos en lectura — eso lo decide ConfigVersion + el handler).
type Config struct {
	// Version siempre debe ser ConfigVersion en v1. El loader rechaza
	// otras para forzar upgrade explícito.
	Version int `json:"version"`

	// Default es el nombre del pipeline (sin extensión) que se usa cuando
	// `che run` no recibe `--pipeline`. Vacío = sin default explícito,
	// el manager cae al built-in `Default()`.
	Default string `json:"default,omitempty"`
}

// SourceKind describe de dónde vino un pipeline resuelto. Útil para que
// la UI del dash distinga "estás corriendo el built-in" vs "estás
// corriendo un pipeline custom" sin tener que mirar el path.
type SourceKind string

const (
	// SourceFlag = el caller pasó `--pipeline <name>` y resolvimos a
	// disco.
	SourceFlag SourceKind = "flag"

	// SourceConfig = sin flag, leímos `default` del
	// `.che/pipelines.config.json`.
	SourceConfig SourceKind = "config"

	// SourceBuiltin = ni flag ni config: caemos al built-in `Default()`.
	// Path queda vacío en este caso.
	SourceBuiltin SourceKind = "built-in"
)

// Resolved es el outcome de Manager.Resolve: pipeline elegido + origen
// + path on-disk (vacío si built-in). Pensado para que el caller pueda
// imprimir un breadcrumb tipo "running pipeline X (from .che/pipelines/X.json)"
// sin chequear branches.
type Resolved struct {
	Name     string
	Pipeline Pipeline
	Source   SourceKind
	Path     string
}

// Manager expone los pipelines de un repo: indexa archivos en
// `.che/pipelines/`, lee el config, y resuelve "qué pipeline corre"
// según la jerarquía flag > config.default > built-in (PRD §7.b).
//
// No es un watcher: NewManager se llama una vez por comando. Procesos
// long-running (dash) deben recargar al detectar cambios — eso queda
// para PR9b cuando el motor de reglas se reescriba.
type Manager struct {
	repoRoot     string
	pipelinesDir string
	configPath   string

	// Config es la lectura de `.che/pipelines.config.json` o el zero
	// value si el archivo no existe. Exportada para tests y para
	// `che pipeline list` (mostrar el default activo).
	Config Config

	// files: name canónico -> path absoluto. Se llena en NewManager
	// recorriendo `.che/pipelines/*.json`.
	files map[string]string
}

// NewManager indexa los pipelines del repo y carga el config si existe.
//
// Casos no-error (manager utilizable):
//   - `.che/pipelines/` no existe → 0 pipelines on-disk, Resolve cae a built-in.
//   - `.che/pipelines.config.json` no existe → Config = zero value.
//   - Un `.json` individual está roto → el archivo se indexa igual; el
//     parse error sale al hacer `Get` o `Resolve` sobre ese nombre.
//     Esto deja a `che pipeline list` funcionando aunque haya un solo
//     pipeline corrupto.
//
// Errores que sí abortan NewManager:
//   - filesystem error inesperado leyendo `.che/pipelines/` (permisos).
//   - `.che/pipelines.config.json` existe pero es JSON inválido o
//     declara una versión desconocida — el config corrompido afecta a
//     toda corrida sin flag, mejor fallar temprano.
func NewManager(repoRoot string) (*Manager, error) {
	m := &Manager{
		repoRoot:     repoRoot,
		pipelinesDir: filepath.Join(repoRoot, PipelinesDirRel),
		configPath:   filepath.Join(repoRoot, ConfigFileRel),
		files:        map[string]string{},
	}
	if err := m.scanFiles(); err != nil {
		return nil, err
	}
	if err := m.loadConfig(); err != nil {
		return nil, err
	}
	return m, nil
}

// PipelinesDir devuelve el path absoluto a `.che/pipelines/` del repo,
// exista o no. Útil para mensajes ("create your first pipeline at X").
func (m *Manager) PipelinesDir() string { return m.pipelinesDir }

// ConfigPath devuelve el path absoluto a `.che/pipelines.config.json`.
func (m *Manager) ConfigPath() string { return m.configPath }

func (m *Manager) scanFiles() error {
	entries, err := os.ReadDir(m.pipelinesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pipeline: read %s: %w", m.pipelinesDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := pipelineNameFromFilename(e.Name())
		if !ok {
			continue
		}
		m.files[name] = filepath.Join(m.pipelinesDir, e.Name())
	}
	return nil
}

func (m *Manager) loadConfig() error {
	data, err := os.ReadFile(m.configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pipeline: read %s: %w", m.configPath, err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return configDecodeError(m.configPath, err, data)
	}
	if dec.More() {
		return &LoadError{Path: m.configPath, Reason: "trailing data after config JSON object"}
	}
	if cfg.Version != ConfigVersion {
		return &LoadError{
			Path:  m.configPath,
			Field: "version",
			Reason: fmt.Sprintf(
				"unknown config version %d (loader supports only %d) — upgrade che to load this config",
				cfg.Version, ConfigVersion,
			),
		}
	}
	m.Config = cfg
	return nil
}

func configDecodeError(path string, err error, data []byte) error {
	le := jsonDecodeError(err, data)
	le.Path = path
	if le.Reason != "" && !strings.HasPrefix(le.Reason, "invalid") &&
		!strings.HasPrefix(le.Reason, "expected") &&
		!strings.HasPrefix(le.Reason, "unknown field") {
		// Fallback genérico: prefijar para distinguir de errores de
		// pipeline files cuando el error sale por stderr.
		le.Reason = "invalid config: " + le.Reason
	}
	return le
}

// pipelineNameFromFilename devuelve el nombre canónico (filename sin
// `.json` final, case-insensitive) y true si el entry es un archivo
// JSON con nombre no-vacío.
//
// Acepta `.JSON` mayúsculas para no morder a usuarios en filesystems
// case-insensitive (macOS default), pero el name canónico preserva el
// case original — `MyPipeline.json` se indexa como `MyPipeline`.
func pipelineNameFromFilename(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	idx := strings.LastIndex(name, ".")
	if idx <= 0 {
		return "", false
	}
	if !strings.EqualFold(name[idx:], ".json") {
		return "", false
	}
	return name[:idx], true
}

// List devuelve los nombres de los pipelines indexados en `.che/pipelines/`,
// orden alfabético. NO incluye el built-in `default`: ése es un fallback
// implícito, no un archivo. Si querés materializarlo a disco usá
// `che pipeline new default`.
func (m *Manager) List() []string {
	names := make([]string, 0, len(m.files))
	for n := range m.files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Path devuelve el path absoluto del archivo de un pipeline por nombre.
// ok=false si no existe en disco — incluye el caso `name == "default"`
// cuando el repo no materializó un default custom (Resolve cae al
// built-in en ese caso).
func (m *Manager) Path(name string) (string, bool) {
	p, ok := m.files[name]
	return p, ok
}

// Get carga + valida un pipeline por nombre. Errores: *LoadError si el
// archivo no existe, no parsea o no valida.
//
// `Get("default")` lee el archivo si existe en disco; si no existe NO
// cae automáticamente al built-in — usar Resolve("") para eso (la
// resolución built-in es decisión de la jerarquía, no del lookup
// directo).
func (m *Manager) Get(name string) (Pipeline, error) {
	path, ok := m.files[name]
	if !ok {
		return Pipeline{}, &LoadError{
			Reason: fmt.Sprintf("pipeline %q not found in %s", name, m.pipelinesDir),
		}
	}
	return Load(path)
}

// Resolve elige el pipeline para una corrida según PRD §7.b:
//
//  1. flag `--pipeline <name>` (no vacío) → `.che/pipelines/<name>.json`.
//  2. `default` del config → `.che/pipelines/<default>.json`.
//  3. Sin lo anterior → built-in `Default()`.
//
// Errores se propagan SOLO cuando un pipeline nombrado (flag o config)
// falta o falla validar. El fallback built-in nunca falla — es
// código in-process.
func (m *Manager) Resolve(flag string) (Resolved, error) {
	if flag != "" {
		p, err := m.Get(flag)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{
			Name:     flag,
			Pipeline: p,
			Source:   SourceFlag,
			Path:     m.files[flag],
		}, nil
	}
	if m.Config.Default != "" {
		p, err := m.Get(m.Config.Default)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{
			Name:     m.Config.Default,
			Pipeline: p,
			Source:   SourceConfig,
			Path:     m.files[m.Config.Default],
		}, nil
	}
	return Resolved{
		Name:     "default",
		Pipeline: Default(),
		Source:   SourceBuiltin,
	}, nil
}
