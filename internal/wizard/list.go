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
}

// listModel es el bubbletea model del lister "My pipelines".
type listModel struct {
	homeDir string
	items   []listItem
	cursor  int
	width   int

	// toast = mensaje efimero post-accion ("editor fallo", "borrado",
	// "ejecución no implementada"). toastOK colorea verde/rojo. Se limpia
	// al navegar.
	toast   string
	toastOK bool

	// delConfirm = render del modal "borrar pipeline".
	delConfirm bool
	delCursor  int // 0 = confirmar, 1 = cancelar (default seguro)

	// resultado para el caller
	action  ListAction
	target  string
	exitApp bool
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

// loadListItems lee ~/.che/pipelines/*.yaml, parsea cada uno, y devuelve la
// lista ordenada por when desc (mas reciente primero). Archivos ilegibles o
// con YAML invalido se skipean en silencio (decision: pipelines corruptos no
// deben volar el lister; al hacer enter sobre uno tampoco habria como
// reanudarlo). Si el dir no existe, devuelve lista vacia (caso "primer uso").
func loadListItems(homeDir string) ([]listItem, error) {
	dir, err := PipelinesDir(homeDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
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
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].when.After(items[j].when)
	})
	return items, nil
}

// RunList levanta el lister "My pipelines" usando $HOME real.
func RunList() (ListAction, string, bool, error) {
	return runListWithHome("")
}

// runListWithHome es el entrypoint testeable: permite forzar HomeDir desde
// los tests sin tocar $HOME del proceso.
func runListWithHome(home string) (ListAction, string, bool, error) {
	items, err := loadListItems(home)
	if err != nil {
		return ListActionNone, "", false, err
	}
	m := listModel{homeDir: home, items: items}
	final, err := tea.NewProgram(m).Run()
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

func (m listModel) Init() tea.Cmd { return nil }

func (m listModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		return m, nil
	}
	if em, ok := msg.(editorFinishedMsg); ok {
		// Refresh tras editor: el archivo pudo haber cambiado nombre,
		// status, etc. Si la lectura falla mantenemos el estado anterior y
		// el toast cuenta el porque.
		items, err := loadListItems(m.homeDir)
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
		m.action = ListActionRun
		m.target = sel.path
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
		m.action = ListActionEditReady
		m.target = sel.path
		return m, tea.Quit
	case "d":
		if len(m.items) == 0 {
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
		return m, openEditorCmd(sel.path)
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
	items, err := loadListItems(m.homeDir)
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
)

func (m listModel) View() string {
	if m.delConfirm {
		return m.viewDelete()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("My pipelines"))
	b.WriteString("\n\n")

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
	b.WriteString(hintStyle.Render("↑/↓ navegar · enter abrir · e editar ready · d borrar · y abrir en $EDITOR · esc volver · q salir"))
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

	when := relTime(it.when)
	whenPart := dimStyle.Render(fmt.Sprintf("%-10s", when))

	row := mutedItem.Render(namePart) + "  " + chip + "  " + whenPart

	if it.isDraft {
		sub := stageLabel(it)
		if sub != "" {
			row += "  " + dimStyle.Italic(true).Render(sub)
		}
	} else if it.nSteps > 0 {
		row += "  " + dimStyle.Italic(true).Render(fmt.Sprintf("%d steps", it.nSteps))
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
	b.WriteString(titleStyle.Render("Borrar pipeline"))
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
