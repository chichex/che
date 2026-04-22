// Package labels — archivo separado para el label "ortogonal" che:locked.
//
// A diferencia de los status:* que forman una máquina de estados (explore →
// plan → executing → executed → closed), che:locked es un mutex simple on/off
// aplicado al arrancar un flow y removido al terminar. No transiciona: el
// flow lo aplica con Lock y lo saca con Unlock en un defer.
//
// El label sirve tanto para issues como para PRs: en la API REST de GitHub un
// PR es un issue, así que el endpoint /repos/:o/:r/issues/:n/labels funciona
// para ambos sin distinguir tipo. Eso nos evita mantener dos code paths (uno
// con `gh issue edit`, otro con `gh pr edit`).
package labels

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CheLocked es el label que che aplica para indicar "hay un flow corriendo
// sobre este issue/PR ahora mismo". Cualquiera de los 5 flows lo aplica al
// arrancar y lo saca en su defer. Si un proceso muere sucio el label queda
// pegado — el escape hatch es `che unlock <ref>`.
const CheLocked = "che:locked"

// ErrAlreadyLocked es el sentinel devuelto por el gate de cada flow cuando
// encuentra che:locked aplicado antes de arrancar. Los callers lo mapean a
// ExitSemantic con un mensaje que sugiere `che unlock`.
var ErrAlreadyLocked = errors.New("ref is already locked (che:locked present)")

// LockedRef es el shape que devuelve ListLocked para alimentar la TUI.
// IsPR=true cuando el item es un pull request (misma query devuelve ambos
// tipos). Number + Repo son el par que identifica unívocamente el ref a la
// hora de unlockear.
type LockedRef struct {
	Number int
	Title  string
	URL    string
	IsPR   bool
	// Repo en formato "owner/name". La query de ListLocked usa `repo:{owner}/{name}`
	// para acotar al repo actual, así que todos los items comparten el mismo
	// valor — lo preservamos por claridad al construir refs para Unlock.
	Repo string
}

// Lock aplica el label che:locked al ref. Idempotente: si ya lo tenía, la
// llamada igual es un 200 (GitHub trata POST de label existente como no-op).
// Asegura primero que el label exista en el repo — de lo contrario GitHub
// lo crearía con color default, pasando por alto el estilo que Ensure fija.
func Lock(ref string) error {
	if err := Ensure(CheLocked); err != nil {
		return err
	}
	number, err := refNumber(ref)
	if err != nil {
		return fmt.Errorf("lock %s: %w", ref, err)
	}
	cmd := exec.Command("gh", "api",
		"-X", "POST",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels", number),
		"-f", "labels[]="+CheLocked,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api POST labels: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Unlock quita el label che:locked. Tolera 404 (label ausente) porque el
// caller lo usa en defers: que Unlock corra sobre un ref que nunca fue
// lockeado (falla del Lock inicial, o doble Unlock) no es un error real.
func Unlock(ref string) error {
	number, err := refNumber(ref)
	if err != nil {
		return fmt.Errorf("unlock %s: %w", ref, err)
	}
	cmd := exec.Command("gh", "api",
		"-X", "DELETE",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels/%s", number, CheLocked),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	// gh api sale con status != 0 ante 404. El body del 404 contiene "Label
	// does not exist" — lo tomamos como éxito idempotente. Cualquier otro
	// error (403, 500, red) sí se propaga.
	combined := string(out)
	if strings.Contains(combined, "Label does not exist") ||
		strings.Contains(combined, "HTTP 404") {
		return nil
	}
	return fmt.Errorf("gh api DELETE labels: %s", strings.TrimSpace(combined))
}

// IsLocked devuelve true si el ref tiene el label che:locked. Usa el mismo
// endpoint que validate.detectTarget (`gh api repos/.../issues/:n`), que
// funciona para issues y PRs uniformemente.
//
// Importante: hay una ventana de race entre IsLocked y Lock — dos runners
// pueden ambos ver "no locked" y ambos aplicar el label. GitHub trata el
// POST idempotentemente, así que no falla la llamada; los dos flows siguen.
// Esto no es un sistema distribuido: el escenario real es un humano corriendo
// un solo che contra un ref. El lock reduce la probabilidad de pisarse pero
// no la elimina; para eso haría falta CAS que GitHub no expone.
func IsLocked(ref string) (bool, error) {
	number, err := refNumber(ref)
	if err != nil {
		return false, fmt.Errorf("is-locked %s: %w", ref, err)
	}
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number),
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return false, fmt.Errorf("gh api issues/%d: %s", number, strings.TrimSpace(string(ee.Stderr)))
		}
		return false, err
	}
	return parseHasLabel(out, CheLocked)
}

// ListLocked devuelve todos los issues + PRs con che:locked en el repo
// actual. Usa `gh search issues` (que incluye PRs pese al nombre — quirk
// documentado de la API de GitHub). Limita a 100 hits; en un repo sano
// los locks activos son pocos y un tope alto es suficiente sin paginar.
func ListLocked() ([]LockedRef, error) {
	cmd := exec.Command("gh", "search", "issues",
		"--label", CheLocked,
		"--state", "open",
		"--owner", "@me", // placeholder, sobreescrito abajo
		"--json", "number,title,url,repository,isPullRequest",
		"--limit", "100",
	)
	// `gh search issues` no acepta `--repo`; en su lugar se pasa "repo:X/Y"
	// como query literal. Construimos la query ACÁ para no asumir que el
	// helper ya conoce el repo — dejamos que gh resuelva `{owner}/{repo}`
	// con el mismo mecanismo que usamos en Lock/Unlock (template del remote
	// activo). Truco: `gh api repos/{owner}/{repo}` devuelve el nameWithOwner,
	// que es lo que necesitamos para armar la query.
	nwo, err := nameWithOwner()
	if err != nil {
		return nil, err
	}
	cmd = exec.Command("gh", "search", "issues",
		"repo:"+nwo,
		"label:"+CheLocked,
		"state:open",
		"--json", "number,title,url,isPullRequest",
		"--limit", "100",
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh search issues: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return parseListLocked(out, nwo)
}

// nameWithOwner devuelve "owner/name" del repo activo (el que gh resuelve
// como current, igual que los otros subcomandos). Lo necesitamos para
// `gh search issues` porque no acepta `--repo` con el mismo template.
func nameWithOwner() (string, error) {
	cmd := exec.Command("gh", "repo", "view", "--json", "nameWithOwner")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh repo view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	var probe struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return "", fmt.Errorf("parse gh repo view: %w", err)
	}
	if probe.NameWithOwner == "" {
		return "", errors.New("gh repo view: nameWithOwner vacío")
	}
	return probe.NameWithOwner, nil
}

// parseHasLabel parsea el output de `gh api repos/.../issues/:n` y devuelve
// si tiene el label buscado. Extraído como función pura para testear sin gh.
func parseHasLabel(data []byte, label string) (bool, error) {
	var probe struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, fmt.Errorf("parse issue labels: %w", err)
	}
	for _, l := range probe.Labels {
		if l.Name == label {
			return true, nil
		}
	}
	return false, nil
}

// parseListLocked parsea el output de `gh search issues --json ...`.
// Extraído como función pura para testear sin gh.
func parseListLocked(data []byte, repo string) ([]LockedRef, error) {
	var raw []struct {
		Number        int    `json:"number"`
		Title         string `json:"title"`
		URL           string `json:"url"`
		IsPullRequest bool   `json:"isPullRequest"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse gh search: %w", err)
	}
	out := make([]LockedRef, 0, len(raw))
	for _, r := range raw {
		out = append(out, LockedRef{
			Number: r.Number,
			Title:  r.Title,
			URL:    r.URL,
			IsPR:   r.IsPullRequest,
			Repo:   repo,
		})
	}
	return out, nil
}

// refNumber extrae el número de un ref. Acepta "42", "#42", URLs de GitHub
// (/pull/42 o /issues/42), o "owner/repo#42". Es una copia más chica de
// validate.resolveRefNumber — duplicarla acá evita un import circular
// (labels no debería depender de validate).
func refNumber(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, errors.New("empty ref")
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil {
		return n, nil
	}
	if i := strings.LastIndex(ref, "#"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(ref[i+1:])); err == nil {
			return n, nil
		}
	}
	for _, seg := range []string{"/pull/", "/issues/"} {
		if i := strings.Index(ref, seg); i >= 0 {
			rest := ref[i+len(seg):]
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				rest = rest[:j]
			}
			if n, err := strconv.Atoi(rest); err == nil {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("no se pudo extraer número del ref %q", ref)
}
