// Package dash implementa el subcomando `che dash`: un servidor HTTP local
// que sirve un dashboard Kanban. Los datos vienen de una Source (MockSource
// con fixtures, o GhSource con poll a `gh`) — ver source.go y gh_source.go.
//
// El board se auto-refresca via HTMX polling cada PollInterval segundos:
// `GET /board` devuelve las columnas + un chip de status via `hx-swap-oob` para
// que el topbar refleje "OK / stale / connecting" sin recargar toda la página.
//
// Endpoints del detalle: `/drawer/{id}` y `/drawer/close` mantienen el path
// `/drawer/*` por compat con tests/htmx attrs aunque ahora el partial
// renderea un modal overlay (.modal-backdrop > .modal) en vez del sidebar
// original. El filename del template también queda como drawer.html.tmpl —
// solo cambia el wrapper exterior; las clases internas siguen con prefix
// drawer-* (drawer-hdr, drawer-tabs, etc.).
//
// Pasos siguientes: acciones reales en los botones del modal y stream SSE de
// logs en vivo.
package dash

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/pipeline"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/dashboard.html.tmpl templates/drawer.html.tmpl templates/board.html.tmpl templates/loop.html.tmpl
var templatesFS embed.FS

//go:embed static/htmx.min.js static/dash.js
var staticFS embed.FS

// Options son los flags que toma `che dash`.
type Options struct {
	Port int
	// PortExplicit indica que el usuario pasó --port en la CLI. Cuando es
	// false (default), si Port está ocupado escaneamos los siguientes
	// portFallbackRange puertos hasta encontrar uno libre — patrón pensado
	// para el caso típico de tener varios `che dash` corriendo en paralelo
	// (uno por repo). Cuando es true, respetamos el pedido y fallamos con
	// "address already in use" en vez de hacer fallback silencioso.
	PortExplicit bool
	Repo         string // "" => cwd
	NoOpen       bool
	Poll         int    // segundos entre polls del GhSource; también usado como hx-trigger
	Mock         bool   // true => MockSource en vez de GhSource
	Version      string // versión del binario che que arrancó el dash; se muestra en el topbar
}

// portFallbackRange es cuántos puertos consecutivos prueba listenWithFallback
// después del pedido. 50 cubre con holgura el caso de tener varios dashs
// abiertos sin chocar con servicios random más arriba.
const portFallbackRange = 50

// Run arranca el server y bloquea hasta que ctx se cancele o haya un error
// fatal. stdout se usa para mensajes informativos ("listening on ..."), y
// stderr para errores.
func Run(ctx context.Context, opts Options, stdout, stderr io.Writer) error {
	if err := ValidateRepo(opts.Repo); err != nil {
		return err
	}
	if opts.Poll <= 0 {
		opts.Poll = 15
	}

	repoName := RepoName(opts.Repo)

	var source Source
	if opts.Mock {
		source = MockSource{}
		fmt.Fprintf(stdout, "che dash: mock mode (no gh polling)\n")
	} else {
		gs, err := NewGhSource(opts.Repo, time.Duration(opts.Poll)*time.Second)
		if err != nil {
			return err
		}
		// El poller corre en background; el ctx del Run lo para en shutdown.
		go gs.Run(ctx)
		source = gs
	}

	srv := NewServer(source, repoName, opts.Poll)
	srv.repoPath = opts.Repo
	srv.version = opts.Version
	root, err := dashPipelineRoot(opts.Repo)
	if err != nil {
		return err
	}
	srv.pipelineRoot = root
	mgr, err := pipeline.NewManager(root)
	if err != nil {
		return fmt.Errorf("init pipeline manager: %w", err)
	}
	resolved, err := mgr.Resolve("")
	if err != nil {
		return fmt.Errorf("resolve dashboard pipeline: %w", err)
	}
	srv.pipeline = resolved.Pipeline

	ln, err := listenWithFallback(opts.Port, !opts.PortExplicit)
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	if actualPort := ln.Addr().(*net.TCPAddr).Port; actualPort != opts.Port {
		// Aviso explícito cuando hicimos fallback — el operador suele
		// tener bookmark de :7777 y queremos que vea el cambio antes de
		// abrir el browser.
		fmt.Fprintf(stdout, "che dash: puerto %d ocupado, usando %d\n", opts.Port, actualPort)
	}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	url := "http://" + addr
	fmt.Fprintf(stdout, "che dash listening on %s (repo: %s)\n", url, repoName)
	fmt.Fprintln(stdout, "Ctrl-C to stop.")

	if !opts.NoOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(stderr, "warning: could not open browser: %v\n", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	// Step 6: auto-loop tick goroutine. Se para cuando ctx se cancela
	// via el done channel. Corre en fase con el poll interval — si el
	// operador tiene PollInterval=15, el loop evalúa cada 15s después
	// del primer tick. No hay startup delay porque el snapshot inicial
	// del MockSource / GhSource puede estar vacío y runTick detecta
	// no-op (nada que dispatchar).
	loopDone := make(chan struct{})
	go srv.runLoop(loopDone)

	select {
	case <-ctx.Done():
		// Shutdown HTTP. El poller ya recibió la señal via el mismo ctx
		// que cerró el server; sale solo en el siguiente select. El
		// loop tick también se corta via close(loopDone).
		close(loopDone)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		close(loopDone)
		return err
	}
}

func dashPipelineRoot(repo string) (string, error) {
	if repo == "" {
		return os.Getwd()
	}
	return filepath.Abs(repo)
}

// Server es el handler HTTP del dashboard. Mantiene una referencia a la
// Source para poder leer snapshots actualizados en cada request, y los
// datos estáticos del page (repo, poll interval).
type Server struct {
	source       Source
	repoName     string
	repoPath     string // cwd del subproceso che <flow>; "" => heredar del server
	pollInterval int    // segundos
	version      string // versión del binario (ej "v0.0.58"); "" en dev

	tmpl *template.Template
	mux  *http.ServeMux

	// runAction dispara un subcomando che en background. Inyectable para
	// poder testear el handler POST /action sin spawnear procesos reales.
	// Default: spawnChe, que hace exec.Command sobre el mismo binario
	// (os.Executable()) o el que haya en $PATH.
	//
	// targetRef = número que recibe el subcomando como argumento (PRNumber
	// para fused validate/iterate, IssueNumber para el resto — ver
	// resolveTargetRef). entityKey = EntityKey de la entidad; se usa como clave
	// del overlay de running y del LogStore para que el modal y el stream SSE la
	// encuentren.
	runAction func(flow string, targetRef, entityKey int, repo string) error

	// mu protege running y autoRunning. El map running trackea flows
	// disparados desde el dashboard que todavía no se reflejan en el
	// snapshot de la Source — evita doble dispatch y pinta el badge ⟳
	// instantáneo sin esperar al próximo poll. autoRunning es un map
	// paralelo que distingue "disparado por el auto-loop engine" de
	// "disparado por el humano" para pintar un chip extra en el drawer.
	mu          sync.Mutex
	running     map[int]string // EntityKey → flow corriendo
	autoRunning map[int]bool   // EntityKey → disparado por auto-loop (step 6)

	// loop es el estado del auto-loop engine (step 6): master switch +
	// flags por regla + contador de rounds. Protegido por su propio
	// mutex interno — ver loop.go. El mutex es separado del `mu` de
	// arriba porque los dominios son independientes y no queremos
	// bloquear handlers HTTP mientras el tick evalúa.
	loop *loopState

	// logs es el buffer pub/sub de los logs del subproceso. Populado por
	// spawnChe (o por tests) y consumido por el handler SSE /stream/{id}.
	// Separado del lock `mu` porque el dominio es distinto y el RWMutex
	// interno del LogStore admite múltiples readers (Snapshot) en paralelo.
	logs *LogStore

	// pipeline es el pipeline activo del repo para gates/auto-loop dinámicos.
	// NewServer usa pipeline.Default() para tests y mock mode. Run setea
	// pipelineRoot y activePipeline() lo re-resuelve por tick para no quedar con
	// un snapshot stale si el usuario edita `.che/pipelines/` mientras el dash
	// sigue abierto.
	pipeline     pipeline.Pipeline
	pipelineRoot string
}

// allowedFlows es la allowlist de flows disparables desde el dashboard. Se
// chequea en el handler antes de pasar el string a exec.Command — nunca
// interpolamos input del request directo en un spawn. Los 5 flows del
// lifecycle están habilitados; el click del botón ES la decisión humana
// del close (memoria feedback_close_no_gate: warnea pero no rechaza).
var allowedFlows = map[string]bool{
	"explore":  true,
	"execute":  true,
	"iterate":  true,
	"validate": true,
	"close":    true,
}

// resolveTargetRef decide qué número pasar al subcomando che. Centraliza la
// regla: `che validate|iterate|close <N>` operan sobre el PR — para
// entidades fused (hay PR abierto) queremos el PR number, no el issue.
// `che explore` / `che execute` son issue-first por diseño y siempre
// reciben IssueNumber. La URL del POST sigue siendo con IssueNumber como
// clave canónica: coincide con data-entity del modal, con el key del
// LogStore y con el overlay local de running — toda la UI razona en
// IssueNumber; este helper es el único lugar donde se traduce a PRNumber
// para el subproceso.
func resolveTargetRef(e Entity, flow string) int {
	if e.Kind == KindPR {
		// Adopt: no hay issue linkeado, el único ref posible es el PR.
		// Los flows permitidos en adopt (validate/close) ya operan sobre
		// PR; validate+iterate+close en stateref.go caen al PR cuando no
		// encuentran labels che:* en el issue.
		return e.PRNumber
	}
	if e.Kind == KindFused && (flow == "validate" || flow == "iterate" || flow == "close") && e.PRNumber > 0 {
		return e.PRNumber
	}
	return e.IssueNumber
}

// NewServer construye el handler. pollInterval en segundos, mínimo 1 para
// que el hx-trigger sea válido.
func NewServer(source Source, repoName string, pollInterval int) *Server {
	if pollInterval <= 0 {
		pollInterval = 15
	}
	s := &Server{
		source:       source,
		repoName:     repoName,
		pollInterval: pollInterval,
		running:      map[int]string{},
		autoRunning:  map[int]bool{},
		loop:         newLoopState(),
		logs:         NewLogStore(),
		pipeline:     pipeline.Default(),
	}
	s.runAction = s.spawnChe
	tmpl := template.New("dash").Funcs(s.templateFuncs())
	tmpl = template.Must(tmpl.ParseFS(templatesFS,
		"templates/dashboard.html.tmpl",
		"templates/drawer.html.tmpl",
		"templates/board.html.tmpl",
		"templates/loop.html.tmpl",
	))
	s.tmpl = tmpl
	s.mux = s.buildMux()
	return s
}

func (s *Server) activePipeline() pipeline.Pipeline {
	if s.pipelineRoot == "" {
		return s.pipeline
	}
	mgr, err := pipeline.NewManager(s.pipelineRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dash auto-loop: init pipeline manager failed: %v — using last pipeline snapshot\n", err)
		return s.pipeline
	}
	resolved, err := mgr.Resolve("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dash auto-loop: resolve pipeline failed: %v — using last pipeline snapshot\n", err)
		return s.pipeline
	}
	s.pipeline = resolved.Pipeline
	return s.pipeline
}

// ServeHTTP delega al mux interno; lo implementamos explícito para poder
// usar Server como http.Handler sin exponer el mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// pageData es el contexto del template del dashboard. Columns vienen pre-
// agrupadas para evitar lógica condicional dentro del template (los range
// quedan más limpios y los tests pueden inspeccionar el shape).
type pageData struct {
	Repo         string
	Columns      []columnData
	Snapshot     Snapshot
	PollInterval int
	// NextPollSec es el intervalo que el partial board-partial va a pasar
	// al hx-trigger del wrapper. Adaptivo: cuando hay flows locales en curso
	// (s.running no vacío) bajamos a hotPollSec; en idle usamos PollInterval.
	// El objetivo es que las transiciones de label (che:executing →
	// che:executed) se reflejen en el board rápido sin hammering durante
	// horas de inactividad.
	NextPollSec int
	ActiveLoops int    // entidades con RunningFlow != "" (loops o flows en curso)
	NWO         string // nameWithOwner — usado por los templates para armar URLs a github.com
	Version     string // versión del binario che — útil para validar que el dash corre el build esperado
	// Loop es el estado del auto-loop engine (step 6). Se pasa al
	// partial "auto-loop-toggle" para renderear el pill con el label
	// correcto ("auto-loop OFF" / "auto-loop ON (3/4)"). El popover en
	// sí se renderea sobre /loop (lazy) — acá solo el pill.
	Loop loopPopoverData
	// Adopt indica si la columna "adopt" (PRs untracked) está visible. Se
	// propaga via query param ?adopt=1 desde el handler. El template lo usa
	// (a) para pintar el toggle del header como checked, (b) para computar
	// grid-template-columns (10 cols vs 9), (c) para propagar el flag al
	// hx-get de /board y al EventSource de /stream (reconexión sin perder
	// el estado del toggle).
	Adopt bool
}

// hotPollSec es el intervalo de polling adaptivo cuando hay flows locales
// corriendo (s.running no vacío). 3s es suficientemente rápido para ver las
// transiciones de label casi al toque (y para el minGap del GhSource no
// duplicarse con un tick inmediato) sin saturar `gh`.
const hotPollSec = 3

// drawerData embebe la Entity y agrega el NWO del snapshot para que el
// template del drawer pueda construir links a github.com sin depender de
// pageData (que no llega al handler /drawer/{id}). Auto (step 6) señala
// si el RunningFlow actual fue disparado por el auto-loop engine vs el
// humano — se refleja como chip "auto" en el drawer.
type drawerData struct {
	Entity
	NWO  string
	Auto bool
}

// columnData representa una columna del Kanban con sus entidades ya
// filtradas. Hot dispara el badge pulsante (animación rosa) cuando hay
// trabajo activo en una columna transient (RunningFlow != "" en alguna
// entidad de planning, executing, validating o closing).
type columnData struct {
	Key      string // 1 de los 9: idea | planning | plan | executing | executed | validating | validated | closing | closed
	Title    string
	Hot      bool
	Entities []Entity
}

// columnsOrder fija el orden left-to-right del board. 10 columnas: "adopt"
// (opt-in, al inicio) + los 9 estados che:* (PR2). Las 4 transient (planning,
// executing, validating, closing) se intercalan entre sus pares terminales.
// closed es terminal y queda capada en el poller (ver ClosedCap en
// gh_source.go). adopt agrupa PRs untracked y solo se renderea cuando el
// toggle del header está ON (buildData filtra cuando adopt=false).
var columnsOrder = []struct {
	Key, Title string
}{
	{"adopt", "adopt"},
	{"idea", "idea"},
	{"planning", "planning"},
	{"plan", "plan"},
	{"executing", "executing"},
	{"executed", "executed"},
	{"validating", "validating"},
	{"validated", "validated"},
	{"closing", "closing"},
	{"closed", "closed"},
}

// hotColumns es el set de columnas que pueden mostrar el badge hot (animación
// pulsante). Son las 4 transient: cuando hay un flow corriendo sobre una
// entidad en esa columna, el badge late para indicar trabajo en curso.
var hotColumns = map[string]bool{
	"planning":   true,
	"executing":  true,
	"validating": true,
	"closing":    true,
}

// groupByColumn distribuye un slice de entidades en columnas según
// Entity.Column(). Mantiene el orden de aparición dentro de cada columna.
func groupByColumn(entities []Entity) []columnData {
	buckets := map[string][]Entity{}
	hot := map[string]bool{}
	for _, e := range entities {
		col := e.Column()
		buckets[col] = append(buckets[col], e)
		if e.RunningFlow != "" && hotColumns[col] {
			hot[col] = true
		}
	}
	out := make([]columnData, 0, len(columnsOrder))
	for _, c := range columnsOrder {
		out = append(out, columnData{
			Key:      c.Key,
			Title:    c.Title,
			Hot:      hot[c.Key],
			Entities: buckets[c.Key],
		})
	}
	return out
}

// templateFuncs son las funciones expuestas a los templates. Son helpers de
// presentación pura — no acceden a estado global ni hacen IO.
func (s *Server) templateFuncs() template.FuncMap {
	return template.FuncMap{
		// verdictChipClass devuelve la clase CSS del chip de verdict.
		"verdictChipClass": func(v string) string {
			switch v {
			case "approve":
				return "chip-green"
			case "changes-requested":
				return "chip-orange"
			case "needs-human":
				return "chip-red"
			}
			return ""
		},
		// typeChipClass mapea type:X a un chip color. feature=magenta, fix=red,
		// mejora=orange, ux=green. Mantiene la paleta consistente con el breadcrumb.
		"typeChipClass": func(t string) string {
			switch t {
			case "feature":
				return "chip-magenta"
			case "fix":
				return "chip-red"
			case "mejora":
				return "chip-orange"
			case "ux":
				return "chip-green"
			}
			return ""
		},
		// verdictColor devuelve la variable CSS del color de verdict.
		"verdictColor": func(v string) string {
			switch v {
			case "approve":
				return "var(--green)"
			case "changes-requested":
				return "var(--orange)"
			case "needs-human":
				return "var(--red)"
			}
			return "var(--text)"
		},
		// checksLabel sintetiza el chip "CI 8✓ 1·" / "CI 5✓ 2✗" / "" según
		// los counts. El prefijo "CI" es explícito para que no se confunda
		// con el task-list (checkboxes markdown) del body del issue —
		// estos son check runs de GitHub Actions, vienen de
		// `statusCheckRollup` del PR.
		"checksLabel": func(e Entity) string {
			out := ""
			if e.ChecksOK > 0 {
				out += fmt.Sprintf("%d✓", e.ChecksOK)
			}
			if e.ChecksPending > 0 {
				if out != "" {
					out += " "
				}
				out += fmt.Sprintf("%d·", e.ChecksPending)
			}
			if e.ChecksFail > 0 {
				if out != "" {
					out += " "
				}
				out += fmt.Sprintf("%d✗", e.ChecksFail)
			}
			if out == "" {
				return ""
			}
			return "CI " + out
		},
		// checksChipClass colorea el chip según el peor estado: fail > pending > ok.
		"checksChipClass": func(e Entity) string {
			if e.ChecksFail > 0 {
				return "chip-red"
			}
			if e.ChecksPending > 0 {
				return "chip-orange"
			}
			if e.ChecksOK > 0 {
				return "chip-green"
			}
			return ""
		},
		// hasChecks devuelve si la entidad tiene algún check populado.
		"hasChecks": func(e Entity) bool {
			return e.ChecksOK > 0 || e.ChecksPending > 0 || e.ChecksFail > 0
		},
		// humanAgo formatea un time.Time como "hace 3s"/"hace 1m". Para el
		// chip de status del topbar. Zero value → "nunca".
		"humanAgo": humanAgo,
		// errShort trunca un error.Error() a 40 chars para que quepa en el chip.
		"errShort": errShort,
		// renderMarkdown convierte GFM a HTML seguro (raw HTML del input
		// queda escapado por la opción WithUnsafe omitida en el renderer).
		"renderMarkdown": renderMarkdown,
		// Step 6 — auto-loop pill label helpers. Se exponen acá para
		// que el partial "auto-loop-toggle" los use tanto en render
		// inicial como en OOB swap. Ambos reciben loopPopoverData.
		"pillLabel":           pillLabel,
		"pillLabelHasSpinner": pillLabelHasSpinner,
		// Preflight gates (PR de gates UI, abril 2026): resolución del
		// estado de un botón hx-post a partir de RunningFlow + Gates.
		// flowBtnState centraliza la prioridad: RunningFlow gana sobre
		// gate ("flow X en curso" > "body sin plan consolidado") porque
		// el flow en curso es transient y va a destrabar solo, mientras
		// que la gate puede requerir acción del humano. El template lo
		// consume via `{{$btn := flowBtnState "validate" .RunningFlow .Gates "..."}}`.
		"flowBtnState": flowBtnState,
		// gateOf devuelve el FlowGate para un flow puntual con un fallback
		// "Available=true" si gates es nil (no rompe templates pre-gates,
		// p.ej. tests que arman drawerData a mano).
		"gateOf": gateOf,
	}
}

// FlowBtnState es el resultado de resolver el estado UI de un botón de
// acción (hx-post) en función de RunningFlow + Gates. Lo arma flowBtnState
// y lo consume el template para decidir si pone `disabled`, qué `title`
// poner y si renderear un mensaje de bloqueo bajo el botón.
type FlowBtnState struct {
	// Disabled indica que el botón debe ir con atributo disabled. Cubre
	// dos casos: hay un flow en curso o el gate dice que no aplica.
	Disabled bool
	// Title es el texto del atributo title (tooltip nativo del browser).
	// Siempre se setea — cuando habilitado es el hint de qué hace el flow,
	// cuando deshabilitado es el motivo del bloqueo.
	Title string
	// BlockedReason está poblado SOLO cuando el bloqueo viene de un gate
	// (no de RunningFlow). El template lo usa para mostrar un mensaje
	// inline debajo del botón con el motivo legible — feedback explícito
	// más allá del tooltip. RunningFlow se cubre con el chip ⟳ del header.
	BlockedReason string
}

// flowBtnState resuelve el estado UI de un botón de acción. Prioridades:
//  1. RunningFlow != "" → disabled, title="flow X en curso", sin reason
//     inline (el chip ⟳ ya cuenta la historia).
//  2. !Gates[flow].Available → disabled, title=Reason, BlockedReason=Reason
//     (el template renderea la línea bajo el botón).
//  3. default → enabled, title=baseTitle.
//
// Fallback restrictivo cuando Gates es nil o el flow no está en el map:
// asumimos "no disponible" en vez de pasar transparente. Razón: si en
// producción algún path no popula Gates (bug futuro en findEntity /
// overlayRunning, o un Source nuevo que olvida llamar a computeGates), los
// botones quedarían habilitados disparando flows que el handler /action
// igual rechazaría — UX inconsistente. Mejor que fallen visiblemente con
// "gates no computados" para que el bug se note al toque. Tests legacy
// que construyen Entity a mano sin pasar por overlayRunning deben llamar
// `e.Gates = computeGates(e)` explícito o usar los helpers de fixture.
func flowBtnState(flow, runningFlow string, gates FlowGates, baseTitle string) FlowBtnState {
	if runningFlow != "" {
		return FlowBtnState{Disabled: true, Title: "flow " + runningFlow + " en curso — esperá"}
	}
	if gates == nil {
		const reason = "gates no computados (bug del dash — reportar)"
		return FlowBtnState{Disabled: true, Title: reason, BlockedReason: reason}
	}
	g, ok := gates[flow]
	if !ok {
		// Flow desconocido (drift entre allowedFlows y allFlows). Mismo
		// fallback restrictivo — el test TestAllowedFlowsMatchAllFlows
		// previene este caso, pero defensa en profundidad.
		reason := fmt.Sprintf("gate ausente para flow=%q (drift entre allowedFlows y allFlows — reportar)", flow)
		return FlowBtnState{Disabled: true, Title: reason, BlockedReason: reason}
	}
	if !g.Available {
		reason := g.Reason
		if reason == "" {
			reason = "no disponible"
		}
		return FlowBtnState{Disabled: true, Title: reason, BlockedReason: reason}
	}
	return FlowBtnState{Disabled: false, Title: baseTitle}
}

// gateOf accede a un gate por nombre con fallback restrictivo (Available=
// false) cuando el map es nil o el flow no está. Mismo principio que
// flowBtnState: si el snapshot no tiene gates, queremos visibilidad del
// problema, no que la UI siga renderizando como si todo estuviera ok.
func gateOf(flow string, gates FlowGates) FlowGate {
	if gates == nil {
		return FlowGate{Available: false, Reason: "gates no computados (bug del dash — reportar)"}
	}
	if g, ok := gates[flow]; ok {
		return g
	}
	return FlowGate{Available: false, Reason: fmt.Sprintf("gate ausente para flow=%q (reportar)", flow)}
}

// mdRenderer es el goldmark configurado para GFM. Se construye una vez al
// import y se reutiliza; goldmark.Markdown es concurrent-safe para Convert.
var mdRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// renderMarkdown convierte un body GitHub-flavored a HTML. Si el Convert
// falla (input raro), devuelve el texto crudo dentro de un <pre> como
// fallback para no romper el drawer. Raw HTML del input queda escapado
// (no pasamos html.WithUnsafe), así que es seguro insertarlo como
// template.HTML.
func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(md), &buf); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(md) + "</pre>")
	}
	return template.HTML(buf.String())
}

// humanAgo devuelve un string tipo "hace 3s" / "hace 12s" / "hace 1m" / "hace 4m".
// Zero time → "nunca".
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "nunca"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("hace %ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("hace %dm", int(d.Minutes()))
	}
	return fmt.Sprintf("hace %dh", int(d.Hours()))
}

// errShort trunca un error a 40 chars (con "..." si se corta). nil → "".
func errShort(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= 40 {
		return msg
	}
	return msg[:37] + "..."
}

// buildData prepara la data común de los templates (index + board partial).
// Las entidades se overlaye con el estado local (s.running) para que los
// flows disparados desde el dashboard se reflejen en la UI de inmediato,
// sin esperar al próximo poll de la Source.
//
// adopt controla si la columna "adopt" se renderea con PRs untracked. Con
// adopt=false (default), los Entities con Status="adopt" se filtran del
// snapshot antes de agrupar — el dash se ve idéntico a pre-adopt-mode.
// Con adopt=true, esas entities pasan y se agrupan en la columna "adopt".
func (s *Server) buildData(adopt bool) pageData {
	snap := s.source.Snapshot()
	snap.Entities = s.overlayRunning(snap.Entities)
	if !adopt {
		// Filtro defensivo antes del group-by: con el toggle OFF no queremos
		// ni que la columna se renderee vacía, ni que CapReached/ActiveLoops
		// lleven cuentas de adopts.
		filtered := make([]Entity, 0, len(snap.Entities))
		for _, e := range snap.Entities {
			if e.Status == "adopt" {
				continue
			}
			filtered = append(filtered, e)
		}
		snap.Entities = filtered
	}
	active := 0
	for _, e := range snap.Entities {
		if e.RunningFlow != "" {
			active++
		}
	}
	// Adaptive polling: si hay flows locales corriendo bajamos a hotPollSec
	// para ver las transiciones de label rápido. overlayRunning ya corrió,
	// pero el map s.running que mide "hay algo disparado desde el dash" es
	// source of truth local — lo leemos bajo lock. NO basta con ActiveLoops
	// porque ese incluye flows disparados por CLI que el poller ya tiene
	// indexados; esos no necesitan aceleración (el humano no está mirando
	// el board pidiendo resultado inmediato).
	s.mu.Lock()
	hot := len(s.running) > 0
	s.mu.Unlock()
	next := s.pollInterval
	if hot && next > hotPollSec {
		next = hotPollSec
	}
	cols := groupByColumn(snap.Entities)
	if !adopt {
		// Con el toggle OFF también dropeamos la columna "adopt" para que el
		// board quede idéntico a como estaba pre-feature (9 columnas, no 10).
		withoutAdopt := make([]columnData, 0, len(cols))
		for _, c := range cols {
			if c.Key == "adopt" {
				continue
			}
			withoutAdopt = append(withoutAdopt, c)
		}
		cols = withoutAdopt
	}
	return pageData{
		Repo:         s.repoName,
		Columns:      cols,
		Snapshot:     snap,
		PollInterval: s.pollInterval,
		NextPollSec:  next,
		ActiveLoops:  active,
		Version:      s.version,
		NWO:          snap.NWO,
		Loop:         s.buildLoopData(),
		Adopt:        adopt,
	}
}

// findEntity busca por EntityKey en el snapshot actual. Usado por el
// handler `/drawer/{id}` y por POST /action. El resultado lleva el
// RunningFlow del snapshot merged con s.running (local) — si hay dispatch
// local pendiente de poll, el drawer lo refleja sin esperar al próximo
// refresh — y .Gates poblado para que el template renderee disabled+title
// correctamente y el handler pueda barrear vía 409 si el gate no aplica.
//
// Clave canónica: IssueNumber para KindIssue/KindFused, PRNumber para
// KindPR (adopt sin issue linkeado). Ver Entity.EntityKey.
func (s *Server) findEntity(id int) (Entity, bool) {
	for _, e := range s.source.Snapshot().Entities {
		if e.EntityKey() == id {
			s.mu.Lock()
			if flow, ok := s.running[id]; ok && e.RunningFlow == "" {
				e.RunningFlow = flow
			}
			s.mu.Unlock()
			// Gates se computa post-overlay porque depende del estado
			// observable (status + verdict + lock + body), no de
			// RunningFlow per se. Si en el futuro alguna gate consulta
			// RunningFlow, el orden ya está bien (overlay primero).
			e.Gates = computeGates(e)
			return e, true
		}
	}
	return Entity{}, false
}

// overlayRunning aplica el estado local (s.running) sobre un slice de
// entidades: si la entidad del snapshot tiene RunningFlow vacío pero hay
// un flow local en vuelo para su issue, lo copia. También limpia entradas
// locales stale (el snapshot ya confirma RunningFlow != "" para esa entidad
// → ya no necesitamos overlay, la Source es source of truth).
func (s *Server) overlayRunning(in []Entity) []Entity {
	// Tomamos el snapshot de rounds ANTES del lock del server para mantener
	// el orden consistente (l.mu → s.mu no aplica acá; ver nota de locks en
	// runTick). Las entities con RunningFlow != "" (sea del snapshot o del
	// overlay local) reciben RunIter/RunMax para que el chip del board
	// muestre "⟳ <flow> N/5" en vez de "0/0".
	rounds := s.loop.roundsSnapshot()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Siempre mutamos: además del overlay/cap chip, computeGates necesita
	// correr sobre cada entity para que el template tenga `.Gates.<flow>`
	// disponible al renderar botones disabled+title. El alloc es trivial
	// (~50 entities en un dash típico). El fast-path "no-overlay + no-cap"
	// que existía pre-gates ya no aplica: las gates dependen de status +
	// verdict + body, valores que cambian aunque overlay y cap estén
	// quietos, así que no hay forma cheap de detectar "nada cambió".
	out := make([]Entity, len(in))
	for i, e := range in {
		key := e.EntityKey()
		if e.RunningFlow != "" {
			// Snapshot ya refleja el flow → limpiamos el overlay local.
			// Evita que s.running crezca sin bound si los handlers no
			// limpian explícitamente (ej: el subprocess ya terminó y el
			// label transient apareció en el siguiente poll).
			delete(s.running, key)
		} else if flow, ok := s.running[key]; ok {
			e.RunningFlow = flow
		}
		// Inyectar counter de rounds al chip magenta del card. RunMax es
		// el cap compartido; si nunca se disparó un flow para este id,
		// rounds[id]=0 → RunIter=0, lo cual es el estado correcto (el cap
		// va al renderer igual, para que se vea "⟳ execute 0/5").
		if e.RunningFlow != "" {
			e.RunIter = rounds[key]
			e.RunMax = LoopCap
		}
		// Cap-reached chip: rounds >= LoopCap + entity en status loopable.
		// Gate por status para que cards en closed/closing/idea no prendan
		// el chip (ahí el cap no tiene sentido — el auto-loop no iba a
		// dispatchar igual). Sí aplica durante un run (RunningFlow != "")
		// para cubrir el caso "este run es el #5 y el próximo tick corta".
		if rounds[key] >= LoopCap {
			switch e.Status {
			case "plan", "validated", "executed":
				e.CapReached = true
				e.RunMax = LoopCap
			}
		}
		// Computamos gates sobre el estado YA mutado (con RunningFlow del
		// overlay aplicado). Los gates no dependen de RunningFlow per se,
		// pero si en el futuro alguna regla sí (ej: "no permitir validate
		// mientras corre iterate") la lógica queda en el lugar correcto.
		e.Gates = computeGates(e)
		out[i] = e
	}
	return out
}

// markRunning intenta reservar un flow para id. Si ya hay uno en curso
// (local o en snapshot), devuelve el flow existente y ok=false. Si reserva
// con éxito, ok=true. Llamado bajo lock por el handler de acción.
func (s *Server) markRunning(id int, flow string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.running[id]; ok && cur != "" {
		return cur, false
	}
	// También chequeamos el snapshot: si el último poll ya vio un flow
	// corriendo, no dejamos disparar otro encima. Acceso al snapshot
	// fuera de lock sería ok (Snapshot() es concurrency-safe), pero
	// mantenerlo bajo lock simplifica razonar sobre ordenamientos.
	for _, e := range s.source.Snapshot().Entities {
		if e.EntityKey() == id && e.RunningFlow != "" {
			return e.RunningFlow, false
		}
	}
	s.running[id] = flow
	return flow, true
}

// clearRunning libera la reserva local. Se llama cuando el subproceso
// che termina (éxito o error); a partir de ahí el snapshot del poller
// manda. Si el subproceso aplicó labels transient, overlayRunning ve el
// snapshot "ya lo refleja" y limpia el map en el próximo buildData.
// También limpia el flag autoRunning (step 6) — la entity deja de ser
// "auto" cuando el subproceso termina, independiente de quién la disparó.
//
// Después de liberar el lock, pedimos un Bump a la Source (si la soporta):
// el subproceso que terminó probablemente dejó labels transient → terminal
// (che:executing → che:executed, etc.) y queremos verlos en el board sin
// esperar al próximo tick. Fuera del lock para no retener el mutex durante
// el type-assert / el envío al canal.
func (s *Server) clearRunning(id int) {
	s.mu.Lock()
	delete(s.running, id)
	delete(s.autoRunning, id)
	s.mu.Unlock()
	s.bumpSource()
}

// bumpSource envía una señal de refresh a la Source si implementa Bumper.
// Type-assert opcional: MockSource no lo implementa (sus datos son
// estáticos); GhSource sí. No bloquea — Bump() es non-blocking por diseño.
func (s *Server) bumpSource() {
	if b, ok := s.source.(Bumper); ok {
		b.Bump()
	}
}

// spawnChe es el default de s.runAction. Lanza `che <flow> <targetRef>` en
// background y no bloquea el handler. Prefiere os.Executable() sobre
// exec.LookPath("che") para evitar la gotcha del brew cask vs el binario
// recién compilado (ver project_local_binary_staleness.md): si dash corre
// desde un binario puesto en dist/ o via go run, queremos re-ejecutar el
// mismo bin, no el que brew linkea en /opt/homebrew/bin.
//
// targetRef es el número que recibe el subcomando (PR para fused
// validate/iterate, issue para el resto — ver resolveTargetRef).
// entityKey es el IssueNumber que el overlay local y el LogStore usan
// como clave — el modal/SSE subscriben por IssueNumber, así que el stream
// de logs tiene que ir a ese slot aunque el subproceso reciba PRNumber.
//
// stdout/stderr se tee-ean a (a) os.Stderr del dashboard para que el
// operador siga viendo el trace en la consola y (b) el LogStore per-entityKey
// para que el modal del browser lo stremee vía SSE. Cada stream tiene su
// propia goroutine leyendo con bufio.Scanner (buffer expandido a 1 MiB
// para tolerar tool-use JSON line-terminated largos).
func (s *Server) spawnChe(flow string, targetRef, entityKey int, repo string) error {
	bin, err := os.Executable()
	if err != nil || bin == "" {
		// Fallback: che en $PATH.
		bin, err = exec.LookPath("che")
		if err != nil {
			return fmt.Errorf("no se pudo resolver el binario che: %w", err)
		}
	}
	var cmd *exec.Cmd
	if run, ok := decodeDynamicRunFlow(flow); ok {
		ref := "#" + strconv.Itoa(targetRef)
		if run.PR {
			ref = "!" + strconv.Itoa(targetRef)
		}
		cmd = exec.Command(bin, "run", "--auto", "--from", run.Step, "--input", ref)
	} else {
		cmd = exec.Command(bin, flow, strconv.Itoa(targetRef))
	}
	if repo != "" {
		cmd.Dir = repo
	}
	banner := fmt.Sprintf("che %s %d (en %s)", flow, targetRef, repo)
	if _, ok := decodeDynamicRunFlow(flow); ok {
		banner = fmt.Sprintf("che %s (en %s)", strings.Join(cmd.Args[1:], " "), repo)
	}
	return s.runCmdWithLogs(cmd, entityKey, banner)
}

// runCmdWithLogs setea pipes en cmd, arranca el proceso, tee-ea stdout/
// stderr al LogStore (y a os.Stderr como mirror), y cierra el run cuando
// los pipes se drenan. Extraído de spawnChe para poder testear el pipeline
// con un subproceso arbitrario (sh -c "echo ...") sin depender del
// binario che.
func (s *Server) runCmdWithLogs(cmd *exec.Cmd, id int, banner string) error {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	// Reset de la historia: cada dispatch arranca con buffer limpio. Si
	// quedaban subscribers del run anterior, ResetRun les cierra el canal.
	s.logs.ResetRun(id)
	// Marcador de inicio en el buffer — útil para el cliente que abre el
	// modal mid-run y ve desde dónde empieza.
	s.logs.Append(id, LogLine{
		Time:   time.Now(),
		Stream: "meta",
		Text:   "--- " + banner + " ---",
	})

	if err := cmd.Start(); err != nil {
		// Cerrar el run abierto por ResetRun para que los subscribers
		// eventuales no queden colgados esperando líneas que no vendrán.
		s.logs.Append(id, LogLine{Time: time.Now(), Stream: "meta", Text: fmt.Sprintf("--- spawn falló: %v ---", err)})
		s.logs.CloseRun(id)
		return fmt.Errorf("start %s: %w", banner, err)
	}

	// Dos goroutines para leer los pipes — corren hasta EOF (el subproceso
	// cierra sus fds al salir). WaitGroup sincroniza con cmd.Wait para que
	// el CloseRun se dispare recién cuando ambos streams drenaron.
	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipeToLog(&wg, stdoutPipe, os.Stderr, s.logs, id, "stdout")
	go streamPipeToLog(&wg, stderrPipe, os.Stderr, s.logs, id, "stderr")

	// Wait en goroutine: al terminar, liberamos la reserva local y
	// cerramos el run en el LogStore (EOF dispara `event: done` en los
	// clientes SSE conectados).
	//
	// Orden crítico: drenar los pipes ANTES de cmd.Wait(). Per Go docs de
	// StdoutPipe/StderrPipe: "Wait will close the pipe after seeing the
	// command exit... it is thus incorrect to call Wait before all reads
	// from the pipe have completed." Flakeaba TestRunCmdWithLogs_
	// TeeIntegration en Linux CI con "file already closed" cuando el
	// scheduler ejecutaba Wait antes que los readers drenaran.
	go func() {
		wg.Wait()
		waitErr := cmd.Wait()
		exitMsg := "--- flow terminó OK ---"
		if waitErr != nil {
			exitMsg = fmt.Sprintf("--- flow terminó con error: %v ---", waitErr)
		}
		s.logs.Append(id, LogLine{Time: time.Now(), Stream: "meta", Text: exitMsg})
		s.logs.CloseRun(id)
		s.clearRunning(id)
	}()
	return nil
}

// streamPipeToLog lee líneas del pipe del subproceso, las escribe a `mirror`
// (os.Stderr por default — el operador en la consola sigue viendo todo) y
// las apendea al LogStore etiquetadas con el stream. Usa bufio.Scanner con
// MaxScanTokenSize = 1 MiB para tolerar líneas largas de tool-use (stream-
// json de claude puede emitir objetos grandes en una sola línea). Si aún
// así se excede, el Scanner termina sin error — emitimos un marker meta
// para que el cliente se entere.
func streamPipeToLog(wg *sync.WaitGroup, r io.Reader, mirror io.Writer, store *LogStore, id int, stream string) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	// 1 MiB: safety net para tool-use con output largo. El default de
	// bufio.Scanner es 64 KiB y líneas largas rompen silenciosamente.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		txt := sc.Text()
		if mirror != nil {
			// Mantener la traza en la consola del operador. Ignoramos el
			// error — si stderr del dashboard se rompió tenemos problemas
			// más grandes que un log perdido.
			fmt.Fprintln(mirror, txt)
		}
		store.Append(id, LogLine{
			Time:   time.Now(),
			Stream: stream,
			Text:   txt,
		})
	}
	if err := sc.Err(); err != nil {
		store.Append(id, LogLine{
			Time:   time.Now(),
			Stream: "meta",
			Text:   fmt.Sprintf("--- %s reader error: %v ---", stream, err),
		})
	}
}

// sseLine es el payload que se emite como `event: line` al cliente SSE.
// JSON-encoded porque el texto crudo puede contener newlines o caracteres
// que rompan el wire format de SSE (`\n\n` separa eventos). Con JSON el
// cliente parsea con JSON.parse y lista.
type sseLine struct {
	Time   string `json:"t"` // RFC3339Nano — el JS formatea a hh:mm:ss
	Stream string `json:"s"` // "stdout" | "stderr" | "meta"
	Text   string `json:"x"`
}

// handleStream es el handler GET /stream/{id}. Streamea el buffer histórico
// + futuras líneas como Server-Sent Events. Corre hasta que el cliente se
// desconecta (r.Context().Done) o el flow termina (canal cerrado).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported (no Flusher)", http.StatusInternalServerError)
		return
	}

	// Si nunca se disparó un flow para este id, 404. Evita tener clientes
	// suscritos a ids inexistentes consumiendo slots en el LogStore.
	if !s.logs.Exists(id) {
		http.Error(w, "no log stream for id", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Defensa contra proxies que bufferan responses (nginx con gzip, etc.).
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 1. Snapshot: mandamos la historia actual. Si el cliente abrió el
	//    modal mid-run, ve todo lo emitido antes + lo que siga.
	for _, ln := range s.logs.Snapshot(id) {
		if err := writeSSELine(w, ln); err != nil {
			return
		}
	}
	flusher.Flush()

	// 2. Subscribe. Si el run ya terminó, Subscribe devuelve canal cerrado
	//    → mandamos done y salimos inmediatamente.
	ch, cancel := s.logs.Subscribe(id)
	defer cancel()

	// 3. Heartbeat. Ticker local que cada 15s escribe un comentario SSE
	//    (`:` prefijo = comment, el browser lo ignora) para mantener viva
	//    la conexión y detectar cortes tempranos. Cheap — 1 write cada 15s.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Cliente cerró la conexión (modal close, navegación, reload).
			// cancel() del defer libera el slot en el LogStore.
			return
		case ln, open := <-ch:
			if !open {
				// Flow terminó: canal cerrado por CloseRun. Mandamos done.
				_, _ = w.Write([]byte("event: done\ndata:\n\n"))
				flusher.Flush()
				return
			}
			if err := writeSSELine(w, ln); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSELine serializa una LogLine al wire format de SSE como un `event:
// line`. Devuelve error si el write falla (cliente desconectado). JSON es
// safe contra newlines en el payload.
func writeSSELine(w io.Writer, ln LogLine) error {
	b, err := json.Marshal(sseLine{
		Time:   ln.Time.Format(time.RFC3339Nano),
		Stream: ln.Stream,
		Text:   ln.Text,
	})
	if err != nil {
		// JSON de un struct con strings no debería fallar — defensivo.
		return err
	}
	_, err = fmt.Fprintf(w, "event: line\ndata: %s\n\n", b)
	return err
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Index. Mux de Go 1.22+ matchea "/" como prefijo; chequeamos r.URL.Path
	// explícito para devolver 404 a paths desconocidos.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		adopt := r.URL.Query().Get("adopt") == "1"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "dashboard.html.tmpl", s.buildData(adopt)); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Board partial para HTMX polling. Devuelve el chip de status (oob) + las
	// columnas (9 default, 10 cuando ?adopt=1). El wrapper `.dash-board`
	// queda en el DOM y su innerHTML se swappea con el contenido de esta
	// respuesta. El flag adopt se propaga desde el hx-get del wrapper.
	mux.HandleFunc("GET /board", func(w http.ResponseWriter, r *http.Request) {
		adopt := r.URL.Query().Get("adopt") == "1"
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "board-partial", s.buildData(adopt)); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Drawer partial. {id} es el IssueNumber.
	mux.HandleFunc("GET /drawer/{id}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		entity, ok := s.findEntity(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// Wrapper drawerData: el template necesita NWO para armar URLs a GH
		// en los refs del header; Entity pelada no lo lleva. Auto (step 6)
		// indica si el RunningFlow fue disparado por el auto-loop engine.
		data := drawerData{Entity: entity, NWO: s.source.Snapshot().NWO, Auto: s.isAutoRunning(id)}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "drawer.html.tmpl", data); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Cierre del modal. Body vacío deja #modal-slot vacío y desmonta el
	// overlay. Path mantiene el prefijo /drawer/* por compat con tests y
	// con los hx-get de los botones de cierre del partial.
	mux.HandleFunc("GET /drawer/close", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
	})

	// POST /action/{flow}/{id} — dispara un subcomando che en background.
	// flow se valida contra allowedFlows antes de pasarlo a exec.Command.
	// Responde con el partial del drawer refreshado (que ahora mostrará
	// el chip ⟳ y los botones disabled por RunningFlow != "").
	mux.HandleFunc("POST /action/{flow}/{id}", func(w http.ResponseWriter, r *http.Request) {
		flow := r.PathValue("flow")
		if !allowedFlows[flow] {
			http.Error(w, "invalid flow", http.StatusBadRequest)
			return
		}
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}
		entity, ok := s.findEntity(id)
		if !ok {
			http.Error(w, "entity not found", http.StatusNotFound)
			return
		}
		// Doble barrera: el botón ya viene disabled cuando el gate falla
		// (template lee .Gates), pero un cliente que haga el POST por
		// fuera del DOM (curl manual, JS modificado, htmx con cache) debe
		// recibir el mismo "no" con el motivo concreto. 409 Conflict en
		// vez de 400 porque la entity existe y es válida — solo el flow
		// no aplica en su estado actual.
		//
		// Para entities en columna "adopt" el gate ya restringe al set fijo
		// por kind (ver adoptGates en preflight.go) — cualquier flow fuera
		// del set cae acá con Reason="no aplica desde adopt".
		if g, ok := entity.Gates[flow]; ok && !g.Available {
			reason := g.Reason
			if reason == "" {
				reason = "flow no disponible para esta entity"
			}
			http.Error(w, reason, http.StatusConflict)
			return
		}
		// Reservar antes de spawnar: si hay un flow ya en curso para
		// esta entidad (local o en snapshot), devolver 409 sin tocar
		// nada. findEntity arriba ya consideró el merge del local.
		if _, okRes := s.markRunning(id, flow); !okRes {
			http.Error(w, "flow already running for this entity", http.StatusConflict)
			return
		}
		// Step 6: flows manuales también cuentan para el cap del loop
		// (son "rounds efectivas" sobre la entity). Incrementamos el
		// counter con el mismo criterio que el tick — ANTES de runAction
		// para evitar race con el próximo tick si el spawn tarda en volver.
		// Nota: el handler NO setea autoRunning — lo dejamos en false
		// para que el chip "auto" no se pinte sobre runs manuales.
		s.loop.incRounds(id)
		// targetRef = número que recibe el subcomando che. Para fused +
		// validate/iterate pasa a ser PRNumber; para el resto queda
		// IssueNumber. Ver resolveTargetRef. El id de la URL y del
		// overlay/LogStore sigue siendo el IssueNumber.
		targetRef := resolveTargetRef(entity, flow)
		if err := s.runAction(flow, targetRef, id, s.repoPath); err != nil {
			// Spawn falló: liberar la reserva para que el usuario pueda
			// re-intentar después de arreglar el problema (ej: `che` no
			// está en $PATH, repo path inválido).
			s.clearRunning(id)
			http.Error(w, "spawn failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// El subcomando che recién arrancado suele aplicar un label transient
		// al inicio (ej: che execute → che:executing). Con un Bump acá, el
		// poller vuelve antes del próximo tick regular y el board puede
		// reflejar esa transición en segundos en vez de esperar hasta 15s.
		// No importa si el label todavía no está escrito — bumpMinGap del
		// poller limita el costo y el próximo bump (via clearRunning) cubre
		// la transición de salida.
		s.bumpSource()
		// Re-render del drawer con el estado actualizado (ya refleja el
		// flow via overlay local). El browser ve el chip ⟳ instantáneo
		// y los botones disabled sin esperar al próximo poll. Auto=false
		// porque este dispatch vino del handler manual.
		entity.RunningFlow = flow
		data := drawerData{Entity: entity, NWO: s.source.Snapshot().NWO, Auto: false}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "drawer.html.tmpl", data); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// ==== Step 6: auto-loop endpoints ====
	// GET /loop              → popover HTML (hx-target="#loop-popover").
	// POST /loop/rule/{name} → flipea una regla, devuelve popover + OOB del pill.
	// POST /loop/bulk/{mode} → mode=on|off, prende/apaga todas a la vez.
	// (POST /loop/toggle se borró en v0.0.77 junto con el master switch.)
	mux.HandleFunc("GET /loop", s.handleLoopGet)
	mux.HandleFunc("POST /loop/rule/{name}", s.handleLoopRule)
	mux.HandleFunc("POST /loop/bulk/{mode}", s.handleLoopBulk)

	// GET /stream/{id} — Server-Sent Events del log en vivo del subproceso
	// `che <flow> <id>` disparado desde el dashboard. Flujo:
	//   1. Snapshot de la historia actual → evento `line` por cada entry.
	//   2. Subscribe al canal → cada LogLine nueva → evento `line`.
	//   3. Cuando el canal se cierra (flow terminó) → evento `done` y sale.
	//   4. Heartbeat cada 15s (SSE comment `: ping`) para detectar conexiones
	//      cortadas y mantener viva la conexión frente a proxies inquietos.
	mux.HandleFunc("GET /stream/{id}", s.handleStream)

	// Static. Servimos directo desde el embed FS.
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// fs.Sub sobre un embed.FS con paths válidos no debería fallar.
		// Panic es aceptable acá — es un bug de build, no runtime.
		panic(fmt.Errorf("fs.Sub static: %w", err))
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	return mux
}

// listenWithFallback intenta net.Listen sobre 127.0.0.1:port. Si falla y
// allowFallback es true, escanea hasta portFallbackRange puertos consecutivos
// hacia arriba buscando uno libre. Devuelve el listener resultante; el caller
// lee el puerto efectivo de ln.Addr() para construir la URL real.
//
// Sin fallback (allowFallback=false): un solo intento y propagamos el error
// envuelto. Pensado para `--port` explícito — si el usuario pinó un puerto y
// está ocupado, queremos romper visiblemente, no irnos a otro lado.
func listenWithFallback(port int, allowFallback bool) (net.Listener, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if !allowFallback {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	firstErr := err
	for i := 1; i <= portFallbackRange; i++ {
		next := port + i
		if next > 65535 {
			break
		}
		alt := net.JoinHostPort("127.0.0.1", strconv.Itoa(next))
		if ln, err := net.Listen("tcp", alt); err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("listen %s y los próximos %d puertos: %w", addr, portFallbackRange, firstErr)
}

// openBrowser abre url con el opener nativo de la plataforma.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported GOOS: %s", runtime.GOOS)
	}
	return cmd.Start()
}
