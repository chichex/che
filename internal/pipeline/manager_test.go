package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writePipeline es helper que materializa un .json de pipeline en disco.
// Se usa para armar layouts realistas (`.che/pipelines/<name>.json`)
// sin replicar la struct entera en cada test.
func writePipeline(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeConfig escribe `.che/pipelines.config.json` con el contenido dado.
func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".che")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipelines.config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

const validPipelineJSON = `{
  "version": 1,
  "steps": [
    {"name": "explore", "agents": ["claude-opus"]},
    {"name": "execute", "agents": ["claude-opus"]}
  ]
}
`

// TestManager_EmptyRepo: repo sin `.che/pipelines/` ni config. Resolve
// debe caer al built-in sin error — es el caso default explícito del
// PRD §7.b ("Si no existe el config file → usa el built-in").
func TestManager_EmptyRepo(t *testing.T) {
	tmp := t.TempDir()
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("List = %v, want []", got)
	}
	r, err := m.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceBuiltin {
		t.Errorf("Source = %q, want %q", r.Source, SourceBuiltin)
	}
	if r.Path != "" {
		t.Errorf("Path = %q, want empty", r.Path)
	}
	if !reflect.DeepEqual(r.Pipeline, Default()) {
		t.Errorf("Pipeline drift vs Default()")
	}
}

// TestManager_FlagWins: si el caller pasa flag !"", gana sobre todo lo
// demás (incluyendo un default custom).
func TestManager_FlagWins(t *testing.T) {
	tmp := t.TempDir()
	writePipeline(t, tmp, "fast", validPipelineJSON)
	writePipeline(t, tmp, "thorough", validPipelineJSON)
	writeConfig(t, tmp, `{"version":1,"default":"thorough"}`)

	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r, err := m.Resolve("fast")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Name != "fast" || r.Source != SourceFlag {
		t.Errorf("got Name=%q Source=%q, want fast/flag", r.Name, r.Source)
	}
}

// TestManager_ConfigDefault: sin flag, lee `default` del config.
func TestManager_ConfigDefault(t *testing.T) {
	tmp := t.TempDir()
	writePipeline(t, tmp, "myproject", validPipelineJSON)
	writeConfig(t, tmp, `{"version":1,"default":"myproject"}`)

	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r, err := m.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Name != "myproject" || r.Source != SourceConfig {
		t.Errorf("got Name=%q Source=%q, want myproject/config", r.Name, r.Source)
	}
	if r.Path == "" {
		t.Error("Path empty for SourceConfig, want non-empty")
	}
}

// TestManager_FlagPipelineMissing: pasaste --pipeline foo pero foo.json
// no existe. Error explícito, no fallback silencioso al built-in.
func TestManager_FlagPipelineMissing(t *testing.T) {
	tmp := t.TempDir()
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = m.Resolve("ghost")
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if !strings.Contains(le.Reason, "ghost") {
		t.Errorf("Reason = %q, expected to mention 'ghost'", le.Reason)
	}
}

// TestManager_ConfigPointsToMissing: config dice default=foo pero foo
// no está. Error claro — no caer al built-in para no enmascarar
// configuraciones rotas.
func TestManager_ConfigPointsToMissing(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, `{"version":1,"default":"missing"}`)
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = m.Resolve("")
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
}

// TestManager_BadPipelineFile: un .json mal formado no aborta NewManager
// (la index sigue funcionando), pero Get/Resolve sobre ese nombre sí
// devuelven error con Path apuntando al archivo.
func TestManager_BadPipelineFile(t *testing.T) {
	tmp := t.TempDir()
	writePipeline(t, tmp, "good", validPipelineJSON)
	writePipeline(t, tmp, "broken", `{ this is not json`)

	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.List()
	wantList := []string{"broken", "good"}
	if !reflect.DeepEqual(got, wantList) {
		t.Errorf("List = %v, want %v", got, wantList)
	}
	if _, err := m.Get("good"); err != nil {
		t.Errorf("Get(good) = %v, want nil", err)
	}
	_, err = m.Get("broken")
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Path == "" {
		t.Error("Path empty, want path of broken.json")
	}
}

// TestManager_BadConfig: si el config existe pero es inválido, NewManager
// aborta — un config corrompido afecta toda corrida sin flag, mejor
// fallar temprano con mensaje claro.
func TestManager_BadConfig(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, `not json`)
	_, err := NewManager(tmp)
	if err == nil {
		t.Fatal("NewManager succeeded, want error from bad config")
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if !strings.Contains(le.Path, "pipelines.config.json") {
		t.Errorf("Path = %q, want config path", le.Path)
	}
}

// TestManager_BadConfigUnknownVersion: alguien escribió version=99 en
// el config (downgrade de che, exporto futuro, typo). Loader debe
// rechazar pidiendo upgrade.
func TestManager_BadConfigUnknownVersion(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, `{"version":99,"default":""}`)
	_, err := NewManager(tmp)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "version" {
		t.Errorf("Field = %q, want version", le.Field)
	}
}

// TestManager_IgnoresNonJSONFiles: el directorio puede tener README,
// .swp, .gitkeep. List no debe incluirlos.
func TestManager_IgnoresNonJSONFiles(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range []string{"README.md", ".gitkeep", "fast.json.bak", "fast.json"} {
		body := ""
		if n == "fast.json" {
			body = validPipelineJSON
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.List()
	want := []string{"fast"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}

// TestManager_AcceptsUppercaseExtension: macOS fs es case-insensitive
// por default, así que `Foo.JSON` es indistinguible de `foo.json` para
// la mayoría de usuarios. Lo aceptamos para no morder casos edge.
func TestManager_AcceptsUppercaseExtension(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, ".che", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Mine.JSON"), []byte(validPipelineJSON), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.List()
	if !reflect.DeepEqual(got, []string{"Mine"}) {
		t.Errorf("List = %v, want [Mine] (case preserved)", got)
	}
}

// TestManager_PathReturns: Path devuelve el path absoluto cuando el
// pipeline existe en disco; ok=false cuando no.
func TestManager_PathReturns(t *testing.T) {
	tmp := t.TempDir()
	writePipeline(t, tmp, "fast", validPipelineJSON)
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if p, ok := m.Path("fast"); !ok || !strings.HasSuffix(p, "fast.json") {
		t.Errorf("Path(fast) = %q, %v; want fast.json suffix", p, ok)
	}
	if _, ok := m.Path("ghost"); ok {
		t.Errorf("Path(ghost) ok=true, want false")
	}
}

// TestManager_ConfigOnly_NoDefault: config existe pero declara
// default vacío — equivalente a "no hay default", Resolve cae al built-in.
func TestManager_ConfigOnly_NoDefault(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, `{"version":1}`)
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r, err := m.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceBuiltin {
		t.Errorf("Source = %q, want built-in", r.Source)
	}
}

// TestManager_PipelinesDir/ConfigPath devuelven los paths esperados
// aunque los archivos no existan — así los subcomandos de PR4 pueden
// imprimir "create your first pipeline at X" sin chequear.
func TestManager_PathHelpers(t *testing.T) {
	tmp := t.TempDir()
	m, err := NewManager(tmp)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.PipelinesDir(); !strings.HasSuffix(got, ".che/pipelines") {
		t.Errorf("PipelinesDir = %q, want .che/pipelines suffix", got)
	}
	if got := m.ConfigPath(); !strings.HasSuffix(got, ".che/pipelines.config.json") {
		t.Errorf("ConfigPath = %q, want .che/pipelines.config.json suffix", got)
	}
}

// TestManager_ConfigUnknownField: un campo desconocido en el config
// es probablemente un typo (`defaul` vs `default`). Strict mode debe
// rechazarlo.
func TestManager_ConfigUnknownField(t *testing.T) {
	tmp := t.TempDir()
	writeConfig(t, tmp, `{"version":1,"defaul":"x"}`)
	_, err := NewManager(tmp)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "defaul" {
		t.Errorf("Field = %q, want defaul", le.Field)
	}
}

// TestPipelineNameFromFilename cubre el helper interno con casos edge.
func TestPipelineNameFromFilename(t *testing.T) {
	cases := []struct {
		input    string
		wantName string
		wantOK   bool
	}{
		{"fast.json", "fast", true},
		{"My.Pipeline.json", "My.Pipeline", true},
		{"weird.JSON", "weird", true},
		{".json", "", false},
		{"fast.txt", "", false},
		{"nodot", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := pipelineNameFromFilename(tc.input)
		if got != tc.wantName || ok != tc.wantOK {
			t.Errorf("pipelineNameFromFilename(%q) = (%q,%v), want (%q,%v)",
				tc.input, got, ok, tc.wantName, tc.wantOK)
		}
	}
}
