package dash

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/chichex/che/internal/pipelines"
	"github.com/chichex/che/internal/runner"
	"github.com/chichex/che/internal/wizard"
	"gopkg.in/yaml.v3"
)

// runStarter es el contrato del backend que dispara un run nuevo. La interfaz
// existe para que los handlers se puedan testear sin spawnear procesos reales
// — el impl real (`runnerStarter`) llama a `runner.StartHeadless` + corre
// `Execute()` en goroutine; los tests pasan un mock que solo registra el
// llamado y devuelve un runID determinista.
//
// onDone se invoca cuando Execute() termina (ok o fallo) — el handler lo usa
// para liberar el lock por-slug y habilitar runs posteriores del mismo
// pipeline dentro de la misma sesion del dash. Es opcional (nil OK): los
// tests con mocks no necesitan simular el ciclo de vida del Execute.
type runStarter interface {
	Start(target, input string, onDone func()) (runID string, err error)
}

// runnerStarter es el impl real del runStarter sobre internal/runner. El
// dash le pasa runsRoot opcional para overridear ~/.che/runs/<slug>/ (por
// ahora siempre ""; lo dejamos preparado para sandboxing en tests
// integrados o flags futuros).
type runnerStarter struct {
	runsRoot string
}

// Start carga el pipeline, persiste el manifest inicial y dispara la
// ejecucion en background. Devuelve el runID apenas se materializa el
// run-dir asi el caller puede responder 201 sin esperar a que el pipeline
// termine. Errores de Execute() en la goroutine se logean con prefijo
// [dash] (idem el resto del paquete) — el manifest queda como auditoria
// en disco. onDone se invoca al final del Execute() (en defer asi tambien
// corre si Execute panickea) para que el handler libere el lock por-slug.
func (s *runnerStarter) Start(target, input string, onDone func()) (string, error) {
	hr, err := runner.StartHeadless(target, input, s.runsRoot)
	if err != nil {
		return "", err
	}
	go func() {
		defer func() {
			if onDone != nil {
				onDone()
			}
		}()
		if err := hr.Execute(); err != nil {
			log.Printf("[dash] run %s/%s execute: %v", filepath.Base(filepath.Dir(hr.RunDir)), hr.RunID, err)
		}
	}()
	return hr.RunID, nil
}

// runLock es el lock por-slug en memoria que evita arrancar dos runs del
// mismo pipeline al mismo tiempo desde el dash. La heuristica del manifest
// reciente cubre el caso "TUI corriendo en otra terminal" (otro proceso —
// el lock en memoria no lo ve); juntas, las dos cubren el 90% del caso
// real hasta que aterrice el lock cross-process del epico #50.
type runLock struct {
	mu     sync.Mutex
	active map[string]bool
}

func newRunLock() *runLock {
	return &runLock{active: make(map[string]bool)}
}

// tryAcquire intenta tomar el lock del slug. Devuelve true si el slug
// estaba libre y queda marcado como en curso; false si ya hay otro run del
// mismo slug arrancando desde el dash.
func (l *runLock) tryAcquire(slug string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[slug] {
		return false
	}
	l.active[slug] = true
	return true
}

// release libera el lock del slug. Solo se invoca en el path "no logre
// arrancar el run" — si arrancamos OK, el lock se libera cuando el watcher
// del runs-dir vea status terminal en el manifest (futuro). En la primera
// pasada el lock por slug queda activo hasta que el server reinicie:
// suficiente para evitar el double-click; el ciclo de vida cross-process
// queda fuera de scope (#50).
func (l *runLock) release(slug string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.active, slug)
}

// recentRunningWindow es el TTL para considerar un manifest reciente con
// status=running como "todavia en curso". TODO(#50): cuando aterrice el lock
// con heartbeat se reemplaza por chequeo del heartbeat real.
const recentRunningWindow = 60 * time.Second

// hasRecentRunningManifest devuelve true si el slug tiene un run con
// status=running cuyo started_at cae dentro de los ultimos recentRunningWindow.
// Se invoca antes de tomar el lock para cubrir el caso del run lanzado desde
// otro proceso (typicamente la TUI en otra terminal). Errores de IO se
// tratan como "no hay manifest reciente" — best-effort: preferimos arrancar
// el run a bloquearlo por un readdir roto.
func hasRecentRunningManifest(runsDir, slug string, now time.Time) bool {
	if runsDir == "" || slug == "" {
		return false
	}
	slugDir := filepath.Join(runsDir, slug)
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		return false
	}
	type entry struct {
		path      string
		startedAt time.Time
		status    string
	}
	var manifests []entry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mPath := filepath.Join(slugDir, e.Name(), "manifest.yaml")
		data, err := os.ReadFile(mPath)
		if err != nil {
			continue
		}
		var m struct {
			Status    string    `yaml:"status"`
			StartedAt time.Time `yaml:"started_at"`
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			continue
		}
		manifests = append(manifests, entry{path: mPath, startedAt: m.StartedAt, status: m.Status})
	}
	sort.SliceStable(manifests, func(i, j int) bool {
		return manifests[i].startedAt.After(manifests[j].startedAt)
	})
	if len(manifests) == 0 {
		return false
	}
	latest := manifests[0]
	if latest.status != runner.ManifestStatusRunning {
		return false
	}
	if latest.startedAt.IsZero() {
		return false
	}
	return now.Sub(latest.startedAt) < recentRunningWindow
}

// loadPipelineForSlug busca un pipeline por slug usando
// pipelines.ResolveInDirs (orden project → global → builtin). Devuelve
// la pipeline, el target que el runner espera ("<path>" o
// "builtin:<slug>"), y found=false si no existe en ningun lado.
// Errores reales (IO, parse) se logean igual que en el resto del dash.
//
// Es un thin wrapper sobre pipelines.ResolveInDirs — la funcion se
// mantiene para no romper los call sites internos. cwd="" desactiva
// scope project (path usado por tests).
func loadPipelineForSlug(cwd, dir, slug string) (p wizard.Pipeline, target string, found bool, err error) {
	res, ok, err := pipelines.ResolveInDirs(cwd, dir, slug)
	if err != nil {
		return wizard.Pipeline{}, "", false, err
	}
	if !ok {
		return wizard.Pipeline{}, "", false, nil
	}
	return res.Pipeline, res.Target, true, nil
}

// createRunBody es el body JSON esperado por POST /api/pipelines/:slug/runs.
// `input` es opcional; cuando steps[0].input != "none" y no llega valor,
// devolvemos 400. Body vacio (Content-Length=0) tambien es valido y se
// trata como "sin input" — el handler decide segun el kind del pipeline.
type createRunBody struct {
	Input string `json:"input"`
}

// createRunResponse es el wire shape de la respuesta 201. `url` ayuda a la
// UI a navegar al run nuevo sin reconstruir la ruta.
type createRunResponse struct {
	RunID string `json:"run_id"`
	URL   string `json:"url"`
}

// handleCreateRun devuelve el handler de POST /api/pipelines/:slug/runs.
// El slug llega por header (puesto por el dispatcher antes de invocar el
// handler — mismo mecanismo que listRunsH / getRunH). cwd se resolvio una
// vez al startup del dash; vacio = scope project deshabilitado.
// dir = directorio global ya resuelto (`<home>/.che/pipelines/`).
func handleCreateRun(cwd, dir, runsDir string, starter runStarter, lock *runLock) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := extractSlug(r)
		if slug == "" {
			writeJSONError(w, http.StatusNotFound, "pipeline not found")
			return
		}

		// Validar slug + cargar pipeline (project → global → builtin).
		p, target, found, err := loadPipelineForSlug(cwd, dir, slug)
		if err != nil {
			log.Printf("[dash] createRun load %s: %v", slug, err)
			writeJSONError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "pipeline not found")
			return
		}

		// Parse del body — tolerante con body vacio (typ. che-funnel input=none).
		body, perr := parseCreateRunBody(r)
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "body invalido")
			return
		}

		// Validacion del input segun el kind del primer step.
		if requiresInput(p) && body.Input == "" {
			writeJSONError(w, http.StatusBadRequest, "input requerido")
			return
		}

		// 409 si hay run reciente con status=running (otro proceso —
		// typically TUI) O si el lock en memoria del dash esta tomado.
		if hasRecentRunningManifest(runsDir, slug, time.Now()) {
			writeJSONError(w, http.StatusConflict, "pipeline ya en curso")
			return
		}
		if !lock.tryAcquire(slug) {
			writeJSONError(w, http.StatusConflict, "pipeline ya en curso")
			return
		}

		// onDone libera el lock cuando Execute() del runner termina. Asi un
		// segundo POST al mismo slug, despues de que el primer run cierre,
		// puede arrancar sin esperar a que el server reinicie. Los tests
		// con mocks (recordingStarter) pueden ignorar el callback para
		// preservar el comportamiento "lock retenido durante el test".
		slugCopy := slug
		onDone := func() { lock.release(slugCopy) }
		runID, err := starter.Start(target, body.Input, onDone)
		if err != nil {
			lock.release(slug)
			log.Printf("[dash] starter.Start %s: %v", slug, err)
			writeJSONError(w, http.StatusInternalServerError, "no se pudo arrancar el run")
			return
		}

		resp := createRunResponse{
			RunID: runID,
			URL:   fmt.Sprintf("/api/pipelines/%s/runs/%s", slug, runID),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// parseCreateRunBody lee el JSON del request (Content-Length=0 OK), validando
// que sea un objeto. Decoder.DisallowUnknownFields es laxo aca — si el
// front-end manda campos extra futuros, mejor ignorarlos que romper.
func parseCreateRunBody(r *http.Request) (createRunBody, error) {
	var body createRunBody
	if r.Body == nil {
		return body, nil
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return body, err
	}
	if len(data) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return body, errors.New("invalid json")
	}
	return body, nil
}

// requiresInput devuelve true si el primer step del pipeline declara un
// input distinto de "none" (o "" tratado como "none" — defensive, mismo
// criterio que runner.firstInputKind). Step 0 es el unico que pide input
// crudo del usuario; steps siguientes leen previous_output via runner.
func requiresInput(p wizard.Pipeline) bool {
	if len(p.Steps) == 0 {
		return false
	}
	kind := p.Steps[0].Input
	if kind == "" {
		return false
	}
	return kind != wizard.InputNone
}
