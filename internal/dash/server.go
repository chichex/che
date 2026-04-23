// Package dash implementa el subcomando `che dash`: un servidor HTTP local
// que sirve un dashboard Kanban. Los datos vienen de una Source (MockSource
// con fixtures, o GhSource con poll a `gh`) — ver source.go y gh_source.go.
//
// El board se auto-refresca via HTMX polling cada PollInterval segundos:
// `GET /board` devuelve las columnas + un chip de status via `hx-swap-oob` para
// que el topbar refleje "OK / stale / connecting" sin recargar toda la página.
//
// Pasos siguientes: acciones reales en los botones del drawer y stream SSE de
// logs en vivo.
package dash

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

//go:embed templates/dashboard.html.tmpl templates/drawer.html.tmpl templates/board.html.tmpl
var templatesFS embed.FS

//go:embed static/htmx.min.js static/dash.js
var staticFS embed.FS

// Options son los flags que toma `che dash`.
type Options struct {
	Port   int
	Repo   string // "" => cwd
	NoOpen bool
	Poll   int  // segundos entre polls del GhSource; también usado como hx-trigger
	Mock   bool // true => MockSource en vez de GhSource
}

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

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
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

	select {
	case <-ctx.Done():
		// Shutdown HTTP. El poller ya recibió la señal via el mismo ctx
		// que cerró el server; sale solo en el siguiente select.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Server es el handler HTTP del dashboard. Mantiene una referencia a la
// Source para poder leer snapshots actualizados en cada request, y los
// datos estáticos del page (repo, poll interval).
type Server struct {
	source       Source
	repoName     string
	pollInterval int // segundos

	tmpl *template.Template
	mux  *http.ServeMux
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
	}
	tmpl := template.New("dash").Funcs(s.templateFuncs())
	tmpl = template.Must(tmpl.ParseFS(templatesFS,
		"templates/dashboard.html.tmpl",
		"templates/drawer.html.tmpl",
		"templates/board.html.tmpl",
	))
	s.tmpl = tmpl
	s.mux = s.buildMux()
	return s
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
	ActiveLoops  int    // entidades con RunningFlow != "" (loops o flows en curso)
	NWO          string // nameWithOwner — usado por los templates para armar URLs a github.com
}

// drawerData embebe la Entity y agrega el NWO del snapshot para que el
// template del drawer pueda construir links a github.com sin depender de
// pageData (que no llega al handler /drawer/{id}).
type drawerData struct {
	Entity
	NWO string
}

// columnData representa una columna del Kanban con sus entidades ya
// filtradas. Hot dispara el badge pulsante (animación rosa) cuando hay
// trabajo activo (RunningFlow != "" en alguna de las entidades).
type columnData struct {
	Key      string // "backlog" | "exploring" | "plan" | "executing" | "validating" | "approved"
	Title    string
	Hot      bool
	Entities []Entity
}

// columnsOrder fija el orden left-to-right del board.
var columnsOrder = []struct {
	Key, Title string
}{
	{"backlog", "backlog"},
	{"exploring", "exploring"},
	{"plan", "plan"},
	{"executing", "executing"},
	{"validating", "validating"},
	{"approved", "approved"},
}

// groupByColumn distribuye un slice de entidades en columnas según
// Entity.Column(). Mantiene el orden de aparición dentro de cada columna.
func groupByColumn(entities []Entity) []columnData {
	buckets := map[string][]Entity{}
	hot := map[string]bool{}
	for _, e := range entities {
		col := e.Column()
		buckets[col] = append(buckets[col], e)
		if e.RunningFlow != "" && (col == "exploring" || col == "executing" || col == "validating") {
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
		// checksLabel sintetiza el chip "8✓ 1·" / "5✓ 2✗" / "" según los counts.
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
			return out
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
	}
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
func (s *Server) buildData() pageData {
	snap := s.source.Snapshot()
	active := 0
	for _, e := range snap.Entities {
		if e.RunningFlow != "" {
			active++
		}
	}
	return pageData{
		Repo:         s.repoName,
		Columns:      groupByColumn(snap.Entities),
		Snapshot:     snap,
		PollInterval: s.pollInterval,
		ActiveLoops:  active,
		NWO:          snap.NWO,
	}
}

// findEntity busca por IssueNumber en el snapshot actual. Usado por el
// handler `/drawer/{id}`.
func (s *Server) findEntity(id int) (Entity, bool) {
	for _, e := range s.source.Snapshot().Entities {
		if e.IssueNumber == id {
			return e, true
		}
	}
	return Entity{}, false
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "dashboard.html.tmpl", s.buildData()); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Board partial para HTMX polling. Devuelve el chip de status (oob) + las
	// 6 columnas. El wrapper `.dash-board` queda en el DOM y su innerHTML se
	// swappea con el contenido de esta respuesta.
	mux.HandleFunc("GET /board", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "board-partial", s.buildData()); err != nil {
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
		// en los refs del header; Entity pelada no lo lleva.
		data := drawerData{Entity: entity, NWO: s.source.Snapshot().NWO}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, "drawer.html.tmpl", data); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// Cierre del drawer. Body vacío deja #drawer-slot vacío y colapsa el grid.
	mux.HandleFunc("GET /drawer/close", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
	})

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
