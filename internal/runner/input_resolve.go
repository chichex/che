package runner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chichex/che/internal/wizard"
)

// defaultMaxInputSize es el tope para input=file. CHE_MAX_INPUT_SIZE lo
// sobreescribe (formato: bytes literales). Default 10 MiB segun el doc.
const defaultMaxInputSize = 10 * 1024 * 1024

// httpFetchTimeout es el deadline del fetch para input=url. El doc fija 10s.
const httpFetchTimeout = 10 * time.Second

// httpClient es la variable swappable usada por resolveURL. Los tests lo
// reemplazan para apuntar a httptest.NewServer; el runtime usa el default.
// Mantener http.DefaultClient como base hace que cualquier env var (HTTPS_PROXY,
// etc.) siga aplicando sin laburo extra.
var httpClient = &http.Client{Timeout: httpFetchTimeout}

// ghCommand es la factoria del *exec.Cmd que ejecuta gh. Variable a nivel
// paquete para permitir que los tests reemplacen el binario sin tocar PATH.
// Default: gh real (o el que aparezca primero en $PATH; en el harness e2e
// el symlink chefake se interpone, lo cual es exactamente lo que queremos).
var ghCommand = func(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "gh", args...)
}

// resolveInput corre la resolucion eager segun el kind. Devuelve el
// payload listo para R3+ (stdin del subprocess). Errores se devuelven
// crudos — el caller (R1.confirmInput) los pone en m.inputErr para
// mostrarlos inline.
func resolveInput(kind, value string) (string, error) {
	switch kind {
	case wizard.InputText:
		return value, nil
	case wizard.InputFile:
		return resolveFile(value)
	case wizard.InputURL:
		return resolveURL(value)
	case wizard.InputPR:
		return resolveGH("pr", value)
	case wizard.InputIssue:
		return resolveGH("issue", value)
	case wizard.InputNone:
		return "", nil
	default:
		return "", fmt.Errorf("kind de input desconocido: %q", kind)
	}
}

// resolveFile lee un archivo enforzando el tope CHE_MAX_INPUT_SIZE.
// Errores comunes:
//   - ENOENT: "archivo no existe"
//   - is dir: "es un dir, no un archivo"
//   - too big: "archivo demasiado grande (X MiB / Y MiB max)"
//
// El stat va antes que el ReadFile asi nunca leemos un archivo gigante a
// memoria solo para validar el size despues.
func resolveFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("archivo no existe: %s", path)
		}
		return "", fmt.Errorf("no se pudo leer %s: %v", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("es un dir, no un archivo: %s", path)
	}
	max := maxInputSize()
	if info.Size() > max {
		return "", fmt.Errorf("archivo demasiado grande (%d bytes / %d max)", info.Size(), max)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no se pudo leer %s: %v", path, err)
	}
	return string(data), nil
}

// maxInputSize lee CHE_MAX_INPUT_SIZE como int64. Cualquier valor invalido
// (parse error, <= 0) cae al default.
func maxInputSize() int64 {
	raw := strings.TrimSpace(os.Getenv("CHE_MAX_INPUT_SIZE"))
	if raw == "" {
		return defaultMaxInputSize
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultMaxInputSize
	}
	return n
}

// resolveURL hace GET con timeout 10s y devuelve el body completo. Status
// no-2xx se reporta como error inline (criterio de aceptacion: foco vuelve
// al input).
func resolveURL(rawURL string) (string, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", fmt.Errorf("URL debe empezar con http:// o https://")
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("URL invalida: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch fallo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("lectura del body fallo: %v", err)
	}
	return string(body), nil
}

// ghRefRegex valida el formato owner/repo#NNN. Permitimos guiones / puntos
// / underscore en owner y repo (gh acepta lo mismo). El numero es
// 1-or-more digitos.
var ghRefRegex = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)

// resolveGH llama gh {pr|issue} view --repo owner/repo NNN --json
// title,body,comments. El stdout de gh es el payload (JSON dump). Si gh
// no esta en PATH o devuelve exit != 0, el error inline incluye el stderr
// de gh para que el usuario sepa que pasa.
//
// Validamos primero el formato antes de spawnear gh — asi un typo no
// gasta un round-trip de red.
func resolveGH(kind, ref string) (string, error) {
	m := ghRefRegex.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("formato esperado: owner/repo#NNN (recibido: %q)", ref)
	}
	owner, repo, num := m[1], m[2], m[3]
	repoSpec := owner + "/" + repo

	ctx, cancel := context.WithTimeout(context.Background(), httpFetchTimeout)
	defer cancel()

	cmd := ghCommand(ctx, kind, "view", "--repo", repoSpec, num, "--json", "title,body,comments")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// Surface stderr de gh — es lo unico util para un usuario que
		// ya sabe que hace `gh ... view` (auth missing, repo no existe,
		// numero no existe, etc).
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("gh %s view fallo: %s", kind, errMsg)
	}
	return stdout.String(), nil
}
