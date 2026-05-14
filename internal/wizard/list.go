package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/chichex/che/internal/repoctx"
)

// ListAction es el resultado externo del lister: que querés que el caller
// haga al cerrarse la pantalla. ListActionNone = volver al menu, Exit =
// salida total (ctrl+c/q), Resume = abrir wizard reanudando un draft,
// EditReady = abrir wizard sobre un pipeline ready en mode=edit (re-introduce
// status.stage=summary), Run = ejecutar un pipeline ready (H1 del flow de
// runner — abre la pantalla del runner skeleton). enter sobre ready / d / y
// se manejan inline (no salen del lister) — el caller solo ve
// Resume/EditReady/Run/Exit/None.
type ListAction string

const (
	ListActionNone      ListAction = "none"
	ListActionExit      ListAction = "exit"
	ListActionResume    ListAction = "resume"
	ListActionEditReady ListAction = "edit-ready"
	ListActionRun       ListAction = "run"
)

// listItem es la metadata renderizable de un pipeline en disco.
type listItem struct {
	path        string
	name        string
	description string
	// isDraft = Status != nil. Drafts llevan stage/stepIdx/stepMode/nSteps
	// para la sub-label "en paso N de M".
	isDraft  bool
	stage    string
	stepIdx  int
	stepMode string
	nSteps   int
	// when = LastSavedAt (drafts) o file ModTime (ready). Se usa para sort
	// desc + render "X ago".
	when time.Time
	// slug = wizard.Slug(name) si name != "", fallback al filename. H10 lo
	// usa para resolver el run dir (~/.che/runs/<slug>/) y leer el ultimo
	// manifest para el chip "last run".
	slug string
	// lastRun es el snapshot del run mas reciente del slug (H10). Si no hay
	// runs, lastRun.Status == RunStatusNever. Solo se popula para rows
	// ready — drafts no tienen runs por construccion.
	lastRun RunSummary
	// needsRepo = true si algun step del pipeline declara input pr/issue.
	// Sirve para decorar el row con el chip "[needs repo]" cuando el cwd
	// del proceso no esta dentro de un repo de github (la chequera vive en
	// el render — el flag persiste el "este pipeline asume repo" sin
	// re-parsear los steps por keystroke).
	needsRepo bool
	// isBuiltin = pipeline shippeado embedded en el binario (ver Builtins()).
	// Aparece siempre en la lista con chip [default]; no tiene path en el FS
	// hasta que el usuario lo personaliza (copy-on-edit). Enter ejecuta usando
	// el target sentinel "builtin:<slug>"; e / y disparan copy-on-edit a
	// ~/.che/pipelines/<slug>.yaml; d se rechaza con toast.
	isBuiltin     bool
	builtinSource []byte // YAML crudo del builtin para copy-on-edit
	// isProject = true si el pipeline vive en <projectRoot>/.che/pipelines/
	// en vez del global ~/.che/pipelines/. Solo informativo para el render
	// (chip [project]); el path absoluto del archivo va en `path`, asi que
	// e / d / enter funcionan igual que con un global.
	isProject bool
}

// listModel es el bubbletea model del lister "My pipelines".
type listModel struct {
	homeDir string
	// projectRoot es el dir cwd-efectivo donde buscar pipelines de scope
	// project (`<projectRoot>/.che/pipelines/`). Se resuelve una sola vez
	// en RunList via findProjectRoot(os.Getwd()) — walk-up al primer
	// ancestro que contenga `.che/pipelines/`. "" desactiva scope project
	// (mismo flow que dash cuando no hay ningun ancestro con esa carpeta).
	projectRoot string
	items       []listItem
	cursor      int
	width       int

	// toast = mensaje efimero post-accion ("editor fallo", "borrado",
	// "ejecución no implementada"). toastOK colorea verde/rojo. Se limpia
	// al navegar.
	toast   string
	toastOK bool

	// delConfirm = render del modal "borrar pipeline".
	delConfirm bool
	delCursor  int // 0 = confirmar, 1 = cancelar (default seguro)

	// historyMode (H10): cuando es true, el lister renderea el screen
	// "Run history" para el row en historyItem. r vuelve al listado;
	// up/down navegan entre runs; enter abre el detalle del run en
	// historyDetail. esc tambien sale del modo. La pantalla es inline
	// (no un program nuevo) porque el set de teclas es chico y el reuso
	// de Update simplifica el dispatch.
	historyMode    bool
	historyItem    listItem
	historyRuns    []RunSummary
	historyCursor  int
	historyDetail  bool
	historyDetailR RunSummary

	// loading marca que items todavia no se cargo. Mientras true, View()
	// muestra "Cargando..." en vez de la lista vacia. El fetch real lo
	// dispara Init() async — sin esto, alt-screen + read sincronico de
	// ~/.che/pipelines/*.yaml + manifests dejaba la pantalla en blanco
	// varios cientos de ms al entrar.
	loading bool

	// resultado para el caller
	action  ListAction
	target  string
	exitApp bool
}

// listLoadedMsg llega cuando el goroutine de carga termina.
type listLoadedMsg struct {
	items []listItem
	err   error
}

// loadListItemsCmd corre loadListItems(home, projectRoot) en un goroutine y
// dispatchea listLoadedMsg al terminar.
func loadListItemsCmd(home, projectRoot string) tea.Cmd {
	return func() tea.Msg {
		items, err := loadListItems(home, projectRoot)
		return listLoadedMsg{items: items, err: err}
	}
}

// listEditorFinishedMsg es el msg que devuelve openEditorCmd cuando el
// usuario cierra el editor desde el lister. Tiene que ser un tipo distinto
// del editorFinishedMsg del wizard porque ambos paquetes comparten el
// dispatch en Update — sin distincion el lister recibiria los msgs del
// wizard si llegaran a co-existir, y al reves.
//
// Hoy (programs separados) no se mezclan, pero mantenemos tipos separados
// para no acoplar la semantica de "reload pipeline" con "reload list".
//
// NOTE: openEditorCmd ya retorna editorFinishedMsg. En el lister, el
// program corre con listModel — Update solo recibe los mensajes del program
// activo, asi que reusar editorFinishedMsg directamente es seguro. El
// wrapper queda comentado por si en el futuro hay UI compartida.

// loadListItems lee los pipelines visibles desde el usuario: primero
// `<projectRoot>/.che/pipelines/` (si projectRoot != ""), despues
// `~/.che/pipelines/`, y al final los builtins embedded. Devuelve la lista
// ordenada por when desc (mas reciente primero), excepto los builtins que
// quedan al final (when=zero por construccion).
//
// Reglas de colision por slug: project gana sobre global; global gana
// sobre builtin. El caso "shadow del builtin" se mantiene (un archivo del
// usuario con el slug del builtin lo oculta).
//
// Archivos ilegibles o con YAML invalido del FS se skipean en silencio
// (pipelines corruptos no deben volar el lister; al hacer enter sobre uno
// tampoco habria como reanudarlo). Builtins que no parsean SI rompen el
// load — eso es bug del binario, no del usuario, y queremos verlo.
//
// Si ningun dir existe, devolvemos solo los builtins (caso "primer uso").
func loadListItems(homeDir, projectRoot string) ([]listItem, error) {
	seenSlugs := map[string]bool{}
	var items []listItem

	// 1. Project: solo si projectRoot != "". Gana en colisiones por slug.
	if projectRoot != "" {
		projectDir := filepath.Join(projectRoot, pipelinesSubdirRel)
		pItems, pSlugs, err := loadFSItems(projectDir, homeDir)
		if err != nil {
			return nil, err
		}
		for _, it := range pItems {
			it.isProject = true
			items = append(items, it)
		}
		for s := range pSlugs {
			seenSlugs[s] = true
		}
	}

	// 2. Global: skipear los slugs ya cubiertos por project.
	dir, err := PipelinesDir(homeDir)
	if err != nil {
		return nil, err
	}
	gItems, gSlugs, err := loadFSItems(dir, homeDir)
	if err != nil {
		return nil, err
	}
	for _, it := range gItems {
		if seenSlugs[it.slug] {
			continue
		}
		items = append(items, it)
	}
	for s := range gSlugs {
		seenSlugs[s] = true
	}

	// 3. Builtins: skipear los slugs ya cubiertos por project/global.
	builtins, err := Builtins()
	if err != nil {
		// El binario tiene un builtin invalido — abortamos el load para que
		// el caller surface el error en vez de ocultarlo.
		return nil, err
	}
	for _, b := range builtins {
		if seenSlugs[b.Slug] {
			continue
		}
		items = append(items, builtinToListItem(b))
	}

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].when.After(items[j].when)
	})
	return items, nil
}

// pipelinesSubdirRel es el subdir relativo donde viven los YAML de
// pipelines bajo project root o home. Duplicado de
// `internal/pipelines/paths.go` para no introducir un ciclo de imports
// wizard → pipelines → wizard.
const pipelinesSubdirRel = ".che/pipelines"

// findProjectRoot busca el ancestro mas cercano del cwd dado que tenga un
// directorio `.che/pipelines/`. Devuelve el cwd "efectivo" para scope
// project — empezando por el cwd mismo y subiendo. Si ninguno tiene
// `.che/pipelines/`, devuelve "" — el lister va a omitir el scope project.
//
// Misma semantica que `pipelines.FindProjectRoot` salvo el caso "no
// encontrado": aca devolvemos "" para que el lister no pague el costo de
// listar un dir que sabemos vacio. cwd="" devuelve "".
func findProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, pipelinesSubdirRel)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// loadFSItems lee los pipelines del FS. Devuelve los items + el set de
// slugs presentes (para el dedupe de builtins en loadListItems).
func loadFSItems(dir, homeDir string) ([]listItem, map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[string]bool{}, nil
		}
		return nil, nil, err
	}
	slugs := map[string]bool{}
	var items []listItem
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(dir, name)
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		p, lerr := Load(path)
		if lerr != nil {
			continue
		}
		item := listItem{
			path:        path,
			name:        p.Name,
			description: p.Description,
			nSteps:      len(p.Steps),
			when:        info.ModTime(),
		}
		if item.name == "" {
			// fallback razonable: el nombre del archivo sin .yaml.
			item.name = strings.TrimSuffix(name, ".yaml")
		}
		if p.Status != nil {
			item.isDraft = true
			item.stage = p.Status.Stage
			item.stepIdx = p.Status.StepIdx
			item.stepMode = p.Status.StepMode
			if !p.Status.LastSavedAt.IsZero() {
				item.when = p.Status.LastSavedAt
			}
		}
		// Slug + lastRun chip (H10): solo para rows ready. Drafts no tienen
		// runs (no llegaron a R3). El slug se resuelve igual que el runner
		// (Slug del name, fallback al filename sin extension).
		item.slug = Slug(item.name)
		if item.slug == "" {
			item.slug = strings.TrimSuffix(name, ".yaml")
		}
		if !item.isDraft {
			item.lastRun = LastRunFor(homeDir, item.slug)
		}
		// needsRepo se computa una vez al cargar la lista. La chequera del
		// chip pasa por repoctx.Detect() en el render — el flag de aca solo
		// dice "este pipeline asume repo".
		item.needsRepo = PipelineNeedsRepo(p)
		slugs[item.slug] = true
		items = append(items, item)
	}
	return items, slugs, nil
}

// builtinToListItem proyecta un BuiltinPipeline al shape que el lister
// renderea. when = epoch zero para que el sort por when desc lo deje al
// final (los items del FS siempre son mas recientes); en la practica con
// solo un builtin esto no se nota, pero deja la semantica consistente
// si en el futuro shipeamos varios.
func builtinToListItem(b BuiltinPipeline) listItem {
	return listItem{
		path:          "",
		name:          b.Pipeline.Name,
		description:   b.Pipeline.Description,
		nSteps:        len(b.Pipeline.Steps),
		when:          time.Time{}, // sort: queda al final
		slug:          b.Slug,
		needsRepo:     PipelineNeedsRepo(b.Pipeline),
		isBuiltin:     true,
		builtinSource: b.Source,
	}
}

// BuiltinTargetPrefix es el sentinel que el lister usa al devolver
// ListActionRun sobre un builtin. El runner detecta el prefijo y carga el
// pipeline via wizard.Builtins() en vez de tocar el FS. Exportado para
// que internal/pipelines tambien lo emita al resolver scope=builtin.
const BuiltinTargetPrefix = "builtin:"

// builtinTargetPrefix mantiene el simbolo previo (unexported) para no
// alterar callsites internos del paquete. Es alias de BuiltinTargetPrefix.
const builtinTargetPrefix = BuiltinTargetPrefix

// IsBuiltinTarget devuelve true si target tiene el prefijo de builtin.
// El runner lo usa para ramificar entre Load(path) y BuiltinBySlug.
func IsBuiltinTarget(target string) bool {
	return strings.HasPrefix(target, builtinTargetPrefix)
}

// BuiltinSlugFromTarget extrae el slug de un target sentinel. Si target
// no tiene el prefijo, devuelve "" — el caller debe haber chequeado con
// IsBuiltinTarget primero.
func BuiltinSlugFromTarget(target string) string {
	if !IsBuiltinTarget(target) {
		return ""
	}
	return strings.TrimPrefix(target, builtinTargetPrefix)
}

// BuiltinBySlug busca un builtin por slug. Devuelve (nil, nil) si no existe;
// (nil, err) si Builtins() falla (bug del binario).
func BuiltinBySlug(slug string) (*BuiltinPipeline, error) {
	bs, err := Builtins()
	if err != nil {
		return nil, err
	}
	for i := range bs {
		if bs[i].Slug == slug {
			return &bs[i], nil
		}
	}
	return nil, nil
}

// copyBuiltinToFS escribe el YAML crudo del builtin a ~/.che/pipelines/<slug>
// .yaml y devuelve el path. Si el archivo ya existe (porque el usuario ya
// hizo copy-on-edit antes pero no aparece en la lista por algun race), no
// lo pisa — devolvemos el path existente. La proxima loadListItems va a
// detectar el shadow y ocultar al builtin.
func copyBuiltinToFS(homeDir, slug string, source []byte) (string, error) {
	dir, err := EnsureDir(homeDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, slug+".yaml")
	if _, err := os.Stat(path); err == nil {
		// Ya existe — no pisamos para evitar perder ediciones del usuario.
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(path, source, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// RunList levanta el lister "My pipelines" usando $HOME real y el cwd del
// proceso. El cwd se resuelve UNA sola vez al startup via os.Getwd() y se
// pasa por walk-up a findProjectRoot — analogo al dash.
func RunList() (ListAction, string, bool, error) {
	cwd, _ := os.Getwd() // si falla, cwd="" → scope project deshabilitado.
	return runListWithHome("", findProjectRoot(cwd))
}

// runListWithHome es el entrypoint testeable: permite forzar HomeDir y
// projectRoot desde los tests sin tocar $HOME ni el cwd del proceso. La
// carga de items la dispara Init() async para que el primer frame ya
// muestre "Cargando..." en vez de bloquear el render mientras se leen los
// manifests.
func runListWithHome(home, projectRoot string) (ListAction, string, bool, error) {
	m := listModel{homeDir: home, projectRoot: projectRoot, loading: true}
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return ListActionNone, "", false, err
	}
	mm, ok := final.(listModel)
	if !ok {
		return ListActionExit, "", true, nil
	}
	if mm.action == "" {
		mm.action = ListActionNone
	}
	return mm.action, mm.target, mm.exitApp, nil
}

func (m listModel) Init() tea.Cmd {
	if m.loading {
		return loadListItemsCmd(m.homeDir, m.projectRoot)
	}
	return nil
}

func (m listModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		return m, nil
	}
	if loaded, ok := msg.(listLoadedMsg); ok {
		m.loading = false
		if loaded.err != nil {
			m.toast = "no se pudo cargar la lista: " + loaded.err.Error()
			m.toastOK = false
		} else {
			m.items = loaded.items
		}
		return m, nil
	}
	if em, ok := msg.(editorFinishedMsg); ok {
		// Refresh tras editor: el archivo pudo haber cambiado nombre,
		// status, etc. Si la lectura falla mantenemos el estado anterior y
		// el toast cuenta el porque.
		items, err := loadListItems(m.homeDir, m.projectRoot)
		if err != nil {
			m.toast = "no se pudo refrescar la lista: " + err.Error()
			m.toastOK = false
			return m, nil
		}
		m.items = items
		if m.cursor >= len(m.items) {
			m.cursor = len(m.items) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		if em.err != nil {
			m.toast = "editor fallo: " + em.err.Error()
			m.toastOK = false
		} else {
			m.toast = "lista actualizada desde editor"
			m.toastOK = true
		}
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	if m.delConfirm {
		return m.updateDeleteConfirm(key)
	}
	if m.historyMode {
		return m.updateHistory(key)
	}

	switch key.String() {
	case "ctrl+c", "q":
		m.action = ListActionExit
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		m.action = ListActionNone
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		m.toast = ""
		return m, nil
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
		m.toast = ""
		return m, nil
	case "enter", " ":
		if len(m.items) == 0 {
			return m, nil
		}
		sel := m.items[m.cursor]
		if sel.isDraft {
			m.action = ListActionResume
			m.target = sel.path
			return m, tea.Quit
		}
		// ready: H1 del flow de runner — enter dispara la screen del
		// runner skeleton. El caller (cmd/root.go.runMyPipelines) rutea
		// ListActionRun a runner.Run(target).
		// Builtins se identifican con el sentinel "builtin:<slug>" — el
		// runner detecta el prefijo y carga el pipeline desde memoria via
		// wizard.Builtins() en vez de leer del FS.
		m.action = ListActionRun
		if sel.isBuiltin {
			m.target = builtinTargetPrefix + sel.slug
		} else {
			m.target = sel.path
		}
		return m, tea.Quit
	case "e":
		if len(m.items) == 0 {
			return m, nil
		}
		sel := m.items[m.cursor]
		if sel.isDraft {
			// Sobre drafts, "e" coincide con "enter" — reanudamos. Coherente
			// con la idea de que `e` = "voy a editar esto".
			m.action = ListActionResume
			m.target = sel.path
			return m, tea.Quit
		}
		if sel.isBuiltin {
			// Copy-on-edit: el builtin no es editable in-place porque vive
			// embedded en el binario. Lo escribimos al FS del usuario y
			// abrimos el wizard como ready edit sobre la copia. La proxima
			// vez que el lister cargue, el shadow del FS oculta al builtin
			// (la copia queda como su override personal).
			path, err := copyBuiltinToFS(m.homeDir, sel.slug, sel.builtinSource)
			if err != nil {
				m.toast = "no pude copiar el default: " + err.Error()
				m.toastOK = false
				return m, nil
			}
			m.action = ListActionEditReady
			m.target = path
			return m, tea.Quit
		}
		m.action = ListActionEditReady
		m.target = sel.path
		return m, tea.Quit
	case "d":
		if len(m.items) == 0 {
			return m, nil
		}
		sel := m.items[m.cursor]
		if sel.isBuiltin {
			// Builtins no se borran. Si el usuario quiere "ocultarlo",
			// puede crear un shadow en ~/.che/pipelines/ y borrarlo cuando
			// quiera — pero sacar el default mismo del binario seria
			// confuso (volveria al primer load).
			m.toast = "default embebido — no se puede borrar"
			m.toastOK = false
			return m, nil
		}
		m.delConfirm = true
		m.delCursor = 1 // default seguro = cancelar
		return m, nil
	case "y":
		if len(m.items) == 0 {
			return m, nil
		}
		sel := m.items[m.cursor]
		if sel.isBuiltin {
			// Mismo principio que `e`: el builtin no tiene path en el FS.
			// Copy-on-edit primero, despues el editor abre la copia. El
			// usuario edita y guarda como cualquier otro pipeline.
			path, err := copyBuiltinToFS(m.homeDir, sel.slug, sel.builtinSource)
			if err != nil {
				m.toast = "no pude copiar el default: " + err.Error()
				m.toastOK = false
				return m, nil
			}
			m.toast = "copia creada en ~/.che/pipelines/" + sel.slug + ".yaml"
			m.toastOK = true
			return m, openEditorCmd(path)
		}
		return m, openEditorCmd(sel.path)
	case "r":
		// H10: r abre la pantalla "Run history" del row seleccionado.
		// El listado es inline (no un program nuevo) — el resto de las
		// teclas se redirige al sub-handler updateHistory.
		if len(m.items) == 0 {
			return m, nil
		}
		sel := m.items[m.cursor]
		runs := RunHistoryFor(m.homeDir, sel.slug)
		m.historyMode = true
		m.historyItem = sel
		m.historyRuns = runs
		m.historyCursor = 0
		m.historyDetail = false
		m.historyDetailR = RunSummary{}
		m.toast = ""
		return m, nil
	}
	return m, nil
}

// updateHistory dispatchea las teclas mientras estamos en el sub-screen
// "Run history" o en el detalle de un run. esc sale del modo (vuelve al
// listado principal); enter sobre un run abre el detalle; enter / esc en
// el detalle vuelve al listado de runs (no al menu principal — el doc fija
// que esc en el detalle vuelve a la lista de runs).
func (m listModel) updateHistory(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.historyDetail {
		switch key.String() {
		case "ctrl+c", "q":
			m.action = ListActionExit
			m.exitApp = true
			return m, tea.Quit
		case "esc", "enter":
			m.historyDetail = false
			m.historyDetailR = RunSummary{}
			return m, nil
		}
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "q":
		m.action = ListActionExit
		m.exitApp = true
		return m, tea.Quit
	case "esc", "r":
		// r toggle off (volver al listado), esc tambien.
		m.historyMode = false
		m.historyItem = listItem{}
		m.historyRuns = nil
		m.historyCursor = 0
		return m, nil
	case "up", "k":
		if m.historyCursor > 0 {
			m.historyCursor--
		}
		return m, nil
	case "down", "j":
		if m.historyCursor < len(m.historyRuns)-1 {
			m.historyCursor++
		}
		return m, nil
	case "enter", " ":
		if len(m.historyRuns) == 0 {
			return m, nil
		}
		sel := m.historyRuns[m.historyCursor]
		m.historyDetail = true
		m.historyDetailR = sel
		return m, nil
	}
	return m, nil
}

func (m listModel) updateDeleteConfirm(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.action = ListActionExit
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		m.delConfirm = false
		return m, nil
	case "up", "k", "left", "h":
		m.delCursor = 0
		return m, nil
	case "down", "j", "right", "l":
		m.delCursor = 1
		return m, nil
	case "1":
		m.delCursor = 0
		return m.applyDelete()
	case "2":
		m.delCursor = 1
		return m.applyDelete()
	case "enter", " ":
		return m.applyDelete()
	}
	return m, nil
}

func (m listModel) applyDelete() (tea.Model, tea.Cmd) {
	m.delConfirm = false
	if m.delCursor != 0 {
		return m, nil
	}
	if len(m.items) == 0 || m.cursor < 0 || m.cursor >= len(m.items) {
		return m, nil
	}
	sel := m.items[m.cursor]
	if err := os.Remove(sel.path); err != nil && !os.IsNotExist(err) {
		m.toast = "no se pudo borrar: " + err.Error()
		m.toastOK = false
		return m, nil
	}
	items, err := loadListItems(m.homeDir, m.projectRoot)
	if err != nil {
		m.toast = "borrado, pero no pude refrescar: " + err.Error()
		m.toastOK = false
		return m, nil
	}
	m.items = items
	if m.cursor >= len(m.items) {
		m.cursor = len(m.items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.toast = "borrado: " + filepath.Base(sel.path)
	m.toastOK = true
	return m, nil
}

// chipReadyStyle / chipDraftStyle son los chips de estado en la lista. Verde
// (#50FA7B) y amarillo (#F1FA8C) — paleta dracula consistente con el resto
// del wizard. Bold para que el ojo los pesque a la primera entre el resto
// del row.
var (
	chipReadyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#50FA7B")).Bold(true)
	chipDraftStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Bold(true)
	// chipDefaultStyle marca builtin pipelines (cyan dracula). Va junto al
	// chip [ready] para distinguir lo embedded de lo creado por el usuario.
	chipDefaultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Bold(true)
	// chipProjectStyle marca pipelines de scope project (magenta dracula).
	// Va junto al chip principal [ready]/[draft] para distinguirlo del
	// global (sin chip).
	chipProjectStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Bold(true)
	// chipFailStyle / chipWarnStyle / chipInfoStyle son los chips del
	// "last run" por status (H10). Rojo para failed, amarillo para
	// cancelled, gris para interrupted/never. Done reusa chipReadyStyle
	// (verde — mismo color que [ready]).
	chipFailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555")).Bold(true)
	chipWarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Bold(true)
	chipInfoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4"))
	// chipNeedsRepoStyle es el chip discreto "[needs repo]" — gris dracula
	// sin bold, italic para diferenciarlo de los chips de status. La idea
	// es que se note pero sin competir visualmente con [ready] / [failed].
	chipNeedsRepoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Italic(true)
)

func (m listModel) View() string {
	if m.delConfirm {
		return m.viewDelete()
	}
	if m.historyMode {
		if m.historyDetail {
			return m.viewHistoryDetail()
		}
		return m.viewHistory()
	}
	var b strings.Builder
	b.WriteString(breadcrumb("My pipelines"))
	b.WriteString("\n\n")

	if m.loading {
		b.WriteString(dimStyle.Render("  Cargando pipelines…"))
		b.WriteString("\n")
		return b.String()
	}

	if len(m.items) == 0 {
		b.WriteString(dimStyle.Render("(no pipelines yet — usa \"Create pipeline\" desde el menu)"))
		b.WriteString("\n")
	}

	for i, it := range m.items {
		row := renderListRow(it)
		if i == m.cursor {
			b.WriteString(selectedItem.Render("> ") + row + "\n")
		} else {
			b.WriteString("  " + row + "\n")
		}
	}

	if m.toast != "" {
		b.WriteString("\n")
		if m.toastOK {
			b.WriteString(chipReadyStyle.Render(m.toast))
		} else {
			b.WriteString(errorStyle.Render(m.toast))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter abrir · e editar ready · d borrar · y abrir en $EDITOR · r history · esc volver · q salir"))
	b.WriteString("\n")
	return b.String()
}

// renderListRow arma una fila del listado: nombre + chip + tiempo relativo
// + (si es draft) sub-label "en paso N de M". Ancho fijo para columnas
// principales — asi los chips se alinean en una columna visual y la lista
// se lee como tabla.
func renderListRow(it listItem) string {
	name := it.name
	if name == "" {
		name = "(sin nombre)"
	}
	const nameWidth = 30
	displayName := truncList(name, nameWidth)
	pad := nameWidth - displayWidth(displayName)
	if pad < 0 {
		pad = 0
	}
	namePart := displayName + strings.Repeat(" ", pad)

	chip := chipReadyStyle.Render("[ready]")
	if it.isDraft {
		chip = chipDraftStyle.Render("[draft]")
	}
	if it.isBuiltin {
		// Builtins son ready por construccion (no hay drafts embedded). El
		// chip [default] reemplaza el "X ago" de when para no mentir con un
		// timestamp ficticio (when=zero en builtins).
		chip = chipDefaultStyle.Render("[default]") + "  " + chipReadyStyle.Render("[ready]")
	} else if it.isProject {
		// Project scope: prefijamos el chip [project] al [ready]/[draft]
		// para que el ojo distinga rapidamente entre global y project. Los
		// globals quedan sin chip de scope (caso "normal").
		chip = chipProjectStyle.Render("[project]") + "  " + chip
	}

	when := relTime(it.when)
	whenPart := dimStyle.Render(fmt.Sprintf("%-10s", when))

	row := mutedItem.Render(namePart) + "  " + chip + "  " + whenPart

	// Chip "[needs repo]" — discreto (gris dracula, sin bold) al lado del
	// chip principal. Aparece solo en rows ready cuando el pipeline tiene
	// algun step pr/issue Y el cwd no esta dentro de un repo de github
	// segun gh. Para drafts lo omitimos: el draft es justo lo que el
	// usuario esta editando, y mostrar el chip mientras todavia no termina
	// de declarar steps es ruido.
	if !it.isDraft && it.needsRepo && !repoctx.Detect().InGitHubRepo {
		row += "  " + chipNeedsRepoStyle.Render("[needs repo]")
	}

	if it.isDraft {
		sub := stageLabel(it)
		if sub != "" {
			row += "  " + dimStyle.Italic(true).Render(sub)
		}
	} else if it.nSteps > 0 {
		row += "  " + dimStyle.Italic(true).Render(fmt.Sprintf("%d steps", it.nSteps))
	}
	// H10: para rows ready agregamos una sub-linea con "last run: X ago" +
	// chip del status. Si no hay runs, omitimos la linea (no agregamos
	// "never" inline para no inflar el listado).
	if !it.isDraft && it.lastRun.Status != "" && it.lastRun.Status != RunStatusNever {
		chipText := ChipForStatus(it.lastRun.Status)
		var styledChip string
		switch it.lastRun.Status {
		case RunStatusDone:
			styledChip = chipReadyStyle.Render(chipText)
		case RunStatusFailed:
			styledChip = chipFailStyle.Render(chipText)
		case RunStatusCancelled:
			styledChip = chipWarnStyle.Render(chipText)
		case RunStatusInterrupted, RunStatusRunning:
			styledChip = chipInfoStyle.Render(chipText)
		default:
			styledChip = dimStyle.Render(chipText)
		}
		when := relTime(it.lastRun.StartedAt)
		if when == "" {
			when = "?"
		}
		row += "\n    " + dimStyle.Italic(true).Render(fmt.Sprintf("last run: %s", when)) + "  " + styledChip
	}
	return row
}

// stageLabel describe en una linea donde quedo el draft. Para
// stage=step + step_mode=edit mostramos "editando step N de M"; create →
// "creando step N de M"; summary → "en resumen"; info → "definiendo nombre".
func stageLabel(it listItem) string {
	switch it.stage {
	case StageInfo:
		return "definiendo nombre"
	case StageStep:
		// step_idx es 0-based; ojo que para mode=create idx puede ser
		// igual a nSteps (el step nuevo aun no se pusheo). Mostramos N+1
		// y total = max(idx+1, nSteps).
		human := it.stepIdx + 1
		total := it.nSteps
		if human > total {
			total = human
		}
		if total <= 0 {
			total = 1
		}
		mode := "editando"
		if it.stepMode != "edit" {
			mode = "creando"
		}
		return fmt.Sprintf("%s step %d de %d", mode, human, total)
	case StageSummary:
		if it.nSteps > 0 {
			return fmt.Sprintf("en resumen · %d steps", it.nSteps)
		}
		return "en resumen"
	}
	return ""
}

// relTime devuelve "ahora" / "5 min" / "2 h" / "3 d" segun la edad de t.
// "" si t es zero (no llego a persistirse).
func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// reloj corrido del usuario o archivo del futuro — no pretendamos
		// saber.
		return "ahora"
	case d < time.Minute:
		return "ahora"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d", int(d.Hours()/24))
	}
}

// truncList corta el nombre a max runas con ellipsis. La cuenta es por runa
// para que nombres con tildes no rompan el layout.
func truncList(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// displayWidth = numero de runas. No corregimos por glyphs CJK / emojis
// (no es critico aca: ningun nombre tipico va a tener doble-ancho).
func displayWidth(s string) int {
	return len([]rune(s))
}

func (m listModel) viewDelete() string {
	var b strings.Builder
	b.WriteString(breadcrumb("My pipelines", "Borrar pipeline"))
	b.WriteString("\n\n")
	if m.cursor >= 0 && m.cursor < len(m.items) {
		it := m.items[m.cursor]
		kind := "ready"
		if it.isDraft {
			kind = "draft"
		}
		b.WriteString(fmt.Sprintf("¿Borrar el pipeline %q (%s)?", it.name, kind))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(it.path))
		b.WriteString("\n\n")
	}

	options := []struct {
		idx   int
		digit string
		label string
		hint  string
	}{
		{0, "1", "borrar", "remueve el archivo del disco"},
		{1, "2", "cancelar", "volver al listado sin tocar"},
	}
	for _, o := range options {
		line := "  " + o.digit + ". " + o.label + "  " + dimStyle.Render(o.hint)
		if m.delCursor == o.idx {
			line = selectedItem.Render("> "+o.digit+". "+o.label) + "  " + dimStyle.Render(o.hint)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter confirmar · esc volver"))
	b.WriteString("\n")
	return modalBorder.Render(b.String())
}

// viewHistory renderea la pantalla "Run history" del row historyItem (H10):
// titulo + lista de runs con timestamp + status chip + duracion. Si no hay
// runs muestra placeholder. esc / r vuelven al listado principal; enter
// abre el detalle.
func (m listModel) viewHistory() string {
	var b strings.Builder
	b.WriteString(breadcrumb("My pipelines", "Run history"))
	b.WriteString("\n")
	if m.historyItem.name != "" {
		// Mantenemos el nombre del pipeline como subtitulo dimmed para no
		// inflar el ultimo segmento del breadcrumb (el spec lo fija como
		// "Run history" pelado) — pero el contexto sigue visible para que
		// el usuario sepa de que pipeline son los runs listados.
		b.WriteString(dimStyle.Render(m.historyItem.name))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if len(m.historyRuns) == 0 {
		b.WriteString(dimStyle.Render("(sin runs todavia para este pipeline)"))
		b.WriteString("\n\n")
		b.WriteString(hintStyle.Render("esc / r volver al listado · q salir"))
		b.WriteString("\n")
		return b.String()
	}
	for i, r := range m.historyRuns {
		row := renderHistoryRow(r)
		if i == m.historyCursor {
			b.WriteString(selectedItem.Render("> ") + row + "\n")
		} else {
			b.WriteString("  " + row + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter ver detalle · esc / r volver al listado · q salir"))
	b.WriteString("\n")
	return b.String()
}

// renderHistoryRow es el row del listado "Run history": run-id (truncado a
// 19 chars asi entra el timestamp completo "2026-05-08T14-32-11"), tiempo
// relativo, chip status, duracion.
func renderHistoryRow(r RunSummary) string {
	id := r.RunID
	if id == "" {
		id = "(sin id)"
	}
	const idWidth = 22
	if len([]rune(id)) > idWidth {
		id = string([]rune(id)[:idWidth-1]) + "…"
	}
	pad := idWidth - len([]rune(id))
	if pad < 0 {
		pad = 0
	}
	idPart := mutedItem.Render(id + strings.Repeat(" ", pad))

	when := relTime(r.StartedAt)
	if when == "" {
		when = "?"
	}
	whenPart := dimStyle.Render(fmt.Sprintf("%-10s", when))

	chipText := ChipForStatus(r.Status)
	var styledChip string
	switch r.Status {
	case RunStatusDone:
		styledChip = chipReadyStyle.Render(chipText)
	case RunStatusFailed:
		styledChip = chipFailStyle.Render(chipText)
	case RunStatusCancelled:
		styledChip = chipWarnStyle.Render(chipText)
	case RunStatusInterrupted, RunStatusRunning:
		styledChip = chipInfoStyle.Render(chipText)
	default:
		styledChip = dimStyle.Render(chipText)
	}

	row := idPart + "  " + whenPart + "  " + styledChip
	if dur := formatRunDuration(r.Duration()); dur != "" {
		row += "  " + dimStyle.Italic(true).Render(dur)
	}
	return row
}

// viewHistoryDetail renderea el detalle read-only de un run (H10): mismo
// layout que el R4/RF del runner pero sin teclas de retry/editor — esto
// es solo lectura del manifest. enter / esc vuelve al listado de runs.
func (m listModel) viewHistoryDetail() string {
	r := m.historyDetailR
	var b strings.Builder
	runID := r.RunID
	if runID == "" {
		runID = "(sin id)"
	}
	b.WriteString(breadcrumb("My pipelines", "Run history", runID))
	b.WriteString("\n")
	// El status chip vivia anexado al titulo previo. Lo bajamos a una
	// linea propia debajo del breadcrumb para no romper el patron del
	// header (ultimo segmento = nombre exacto de la pantalla, sin chips).
	switch r.Status {
	case RunStatusDone:
		b.WriteString(chipReadyStyle.Render("✓ done"))
	case RunStatusFailed:
		b.WriteString(chipFailStyle.Render("✗ failed"))
	case RunStatusCancelled:
		b.WriteString(chipWarnStyle.Render("! cancelled"))
	case RunStatusInterrupted:
		b.WriteString(chipInfoStyle.Render("? interrupted"))
	case RunStatusRunning:
		b.WriteString(chipInfoStyle.Render("⏳ running"))
	}
	b.WriteString("\n\n")
	b.WriteString(dimStyle.Render("run id: " + r.RunID))
	b.WriteString("\n")
	if !r.StartedAt.IsZero() {
		b.WriteString(dimStyle.Render("started: " + r.StartedAt.UTC().Format(time.RFC3339)))
		b.WriteString("\n")
	}
	if !r.FinishedAt.IsZero() {
		b.WriteString(dimStyle.Render("finished: " + r.FinishedAt.UTC().Format(time.RFC3339)))
		b.WriteString("\n")
	}
	if dur := formatRunDuration(r.Duration()); dur != "" {
		b.WriteString(dimStyle.Render("duracion: " + dur))
		b.WriteString("\n")
	}
	if r.RunDir != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("run dir: " + r.RunDir))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(hintStyle.Render("enter / esc volver al listado de runs · q salir"))
	b.WriteString("\n")
	return b.String()
}
