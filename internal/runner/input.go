package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/repoctx"
	"github.com/chichex/che/internal/wizard"
)

// inputUIState es el estado UI puro de R1. Cambia segun el kind:
//
//   - text          → textBuf multiline.
//   - pr / issue    → textBuf single-line con la referencia owner/repo#NNN
//     SI no hay repo en el cwd. Cuando repoctx.Detect().InGitHubRepo es
//     true, el R1 abre un picker (ghEntries / ghCursor) con los PRs / issues
//     abiertos del repo activo (mismo patron que el file picker, pero el
//     listado viene de gh en vez del filesystem).
//   - url           → textBuf single-line con la URL.
//   - file          → fileEntries / fileCursor / fileDir + textBuf como
//     filtro tipeado (no vital para H2; el cursor + enter alcanza).
//
// El cero-value es "sin estado" — initInputUI prepara la variante adecuada.
type inputUIState struct {
	kind string

	// Buffer de texto para text / pr / issue / url. Multiline solo cuando
	// kind=text.
	textBuf textBuffer

	// Estado del file picker (kind=file).
	fileDir     string     // dir actual listado
	fileEntries []fileItem // entradas visibles (dirs primero, luego files)
	fileCursor  int        // indice seleccionado dentro de fileEntries

	// Estado del picker de gh (kind=pr|issue + repo activo). repoMode=true
	// indica que el R1 esta operando sobre la lista de gh (ghEntries) en
	// vez del textBuf de toggle libre. Cuando ghLoadErr != "", el render
	// muestra el error en vez de la lista (la lista igual queda vacia).
	repoMode  bool
	repo      string // owner/name del repo activo (segun repoctx)
	ghEntries []GHListItem
	ghCursor  int
	ghLoadErr string
	// ghLoading marca que el picker esta esperando la respuesta async de
	// `gh pr list` / `gh issue list`. Mientras true, el render muestra
	// "Cargando..." en vez de la lista vacia (sin esto, alt-screen + fetch
	// sincronico en initInputUI dejaba la pantalla en blanco varios segundos
	// mientras gh corria — el usuario veia "todo desaparecer y reaparecer").
	ghLoading bool
}

// ghListLoadedMsg llega cuando el goroutine de carga del picker termina.
// Contiene los items + un eventual error; updateInput lo aplica al UI.
type ghListLoadedMsg struct {
	items []GHListItem
	err   error
}

// loadGHListCmd corre ghListFn(kind) en un goroutine y dispatchea
// ghListLoadedMsg al terminar. Asi el program puede renderear "Cargando..."
// inmediatamente y poblar la lista cuando llega.
func loadGHListCmd(kind string) tea.Cmd {
	return func() tea.Msg {
		items, err := ghListFn(kind)
		return ghListLoadedMsg{items: items, err: err}
	}
}

// fileItem es una entrada del file picker.
type fileItem struct {
	name  string
	path  string // absoluto
	isDir bool
}

// textBuffer es un input minimalista (runas + cursor + flag multiline). No
// reusamos textInput del wizard para evitar el import cruzado paquete →
// paquete: el shape interno es trivial y el doc explicitamente acepta la
// duplicacion (mismo razonamiento que styles).
type textBuffer struct {
	runes     []rune
	cursor    int
	multiline bool
}

func newTextBuffer(multiline bool) textBuffer {
	return textBuffer{multiline: multiline}
}

func (t textBuffer) value() string {
	return string(t.runes)
}

// handleKey aplica una tecla bubbletea al buffer. Devuelve true si la tecla
// fue consumida (caller no debe seguir interpretando).
func (t *textBuffer) handleKey(key string, runesIn []rune) bool {
	switch key {
	case "left":
		if t.cursor > 0 {
			t.cursor--
		}
		return true
	case "right":
		if t.cursor < len(t.runes) {
			t.cursor++
		}
		return true
	case "home", "ctrl+a":
		t.cursor = 0
		return true
	case "end", "ctrl+e":
		t.cursor = len(t.runes)
		return true
	case "backspace", "ctrl+h":
		if t.cursor > 0 {
			t.runes = append(t.runes[:t.cursor-1], t.runes[t.cursor:]...)
			t.cursor--
		}
		return true
	case "delete":
		if t.cursor < len(t.runes) {
			t.runes = append(t.runes[:t.cursor], t.runes[t.cursor+1:]...)
		}
		return true
	case "shift+enter", "alt+enter", "ctrl+j":
		if t.multiline {
			t.insertRune('\n')
			return true
		}
		return false
	case "enter", "tab", "shift+tab", "esc", "ctrl+c", "ctrl+s", "up", "down":
		// Reservadas al wizard / runner para foco / transiciones.
		return false
	}
	if len(runesIn) == 0 {
		return false
	}
	for _, r := range runesIn {
		if !t.multiline && (r == '\n' || r == '\r') {
			continue
		}
		if r < 0x20 && r != '\n' {
			continue
		}
		t.insertRune(r)
	}
	return true
}

func (t *textBuffer) insertRune(r rune) {
	t.runes = append(t.runes, 0)
	copy(t.runes[t.cursor+1:], t.runes[t.cursor:])
	t.runes[t.cursor] = r
	t.cursor++
}

// view renderiza el buffer con cursor visible. cursorBlock = bloquito
// solido coherente con el wizard.
func (t textBuffer) view() string {
	if len(t.runes) == 0 {
		return cursorBlock
	}
	if t.cursor >= len(t.runes) {
		return string(t.runes) + cursorBlock
	}
	var b strings.Builder
	b.WriteString(string(t.runes[:t.cursor]))
	b.WriteString(cursorBlock)
	rest := string(t.runes[t.cursor:])
	_ = utf8.RuneCountInString(rest)
	b.WriteString(rest)
	return b.String()
}

const cursorBlock = "▎"

// initInputUI prepara el estado UI segun el kind. Para file picker arranca
// listando $PWD; si falla cae a $HOME. El error de listing no es fatal —
// se refleja como inputErr al primer render (loadFileEntries lo retorna).
//
// Para pr / issue, si hay repo de github en el cwd (repoctx.Detect()), el
// R1 abre un picker con los items abiertos del repo en vez del input libre
// — el doc fija que el usuario no debe tener que tipear el ref a mano si
// el contexto lo evita. Si no hay repo o el listado de gh falla, caemos al
// textBuf como antes.
func initInputUI(kind string) inputUIState {
	switch kind {
	case wizard.InputText:
		return inputUIState{kind: kind, textBuf: newTextBuffer(true)}
	case wizard.InputFile:
		ui := inputUIState{kind: kind, textBuf: newTextBuffer(false)}
		dir, err := os.Getwd()
		if err != nil || dir == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				dir = home
			} else {
				dir = "/"
			}
		}
		ui.fileDir = dir
		ui.fileEntries, _ = loadFileEntries(dir)
		return ui
	case wizard.InputPR, wizard.InputIssue:
		ui := inputUIState{kind: kind, textBuf: newTextBuffer(false)}
		info := repoctx.Detect()
		if !info.InGitHubRepo {
			// Sin repo activo: fallback al textBuf libre. El R2 igual va a
			// rebotar (preflight nuevo "git repo context") — el textBuf
			// permite que un usuario que sabe el ref de otro repo lo
			// tipee igual.
			return ui
		}
		ui.repoMode = true
		ui.repo = info.Repo
		ui.ghLoading = true
		// El fetch real lo dispara RunModel.Init() via loadGHListCmd para
		// no bloquear el render del primer frame. Al volver de gh, el
		// handler de ghListLoadedMsg pobla ghEntries / ghLoadErr.
		return ui
	default:
		// url → single-line.
		return inputUIState{kind: kind, textBuf: newTextBuffer(false)}
	}
}

// loadFileEntries lee dir y devuelve dirs primero (alfabetico), luego files
// (alfabetico). Incluye ".." al tope si no estamos en root. Errores de
// permisos / dir inexistente se propagan — el caller los pone como
// inputErr.
func loadFileEntries(dir string) ([]fileItem, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []fileItem
	for _, e := range entries {
		// Skipear ocultos para no inflar el picker. El usuario que necesite
		// uno puede caer al $EDITOR / pasar la ruta absoluta tipeando — el
		// picker es un atajo, no la unica via.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		item := fileItem{
			name:  e.Name(),
			path:  filepath.Join(dir, e.Name()),
			isDir: e.IsDir(),
		}
		if item.isDir {
			dirs = append(dirs, item)
		} else {
			files = append(files, item)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].name < dirs[j].name })
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	out := make([]fileItem, 0, len(dirs)+len(files)+1)
	parent := filepath.Dir(dir)
	if parent != dir {
		out = append(out, fileItem{name: "..", path: parent, isDir: true})
	}
	out = append(out, dirs...)
	out = append(out, files...)
	return out, nil
}

// updateInput es el handler de teclas de R1.
func (m RunModel) updateInput(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		// Volver al lister sin crear run dir — H2 no toca disco.
		m.exitApp = false
		return m, tea.Quit
	}

	switch m.inputUI.kind {
	case wizard.InputFile:
		return m.updateInputFile(key)
	case wizard.InputText:
		return m.updateInputText(key)
	case wizard.InputPR, wizard.InputIssue:
		if m.inputUI.repoMode {
			return m.updateInputGHPicker(key)
		}
		return m.updateInputSingleLine(key)
	default:
		// url → single-line + confirm con ctrl+s/enter.
		return m.updateInputSingleLine(key)
	}
}

func (m RunModel) updateInputText(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+s":
		return m.confirmInput()
	case "enter":
		// Para text (multiline) enter es ambiguo: el doc dice "enter sobre
		// el ultimo foco" confirma. Como el textarea es el unico foco en
		// R1, enter literal confirma — el usuario que quiera newline usa
		// shift+enter / alt+enter / ctrl+j (igual que el wizard S1).
		return m.confirmInput()
	}
	consumed := m.inputUI.textBuf.handleKey(key.String(), key.Runes)
	if consumed && m.inputErr != "" {
		m.inputErr = ""
	}
	return m, nil
}

func (m RunModel) updateInputSingleLine(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+s", "enter":
		return m.confirmInput()
	}
	consumed := m.inputUI.textBuf.handleKey(key.String(), key.Runes)
	if consumed && m.inputErr != "" {
		m.inputErr = ""
	}
	return m, nil
}

// updateInputGHPicker: navegacion del picker de PRs/issues abiertos. Mismo
// patron que updateInputFile (↑/↓ cursor, enter / ctrl+s confirman) pero
// sobre m.inputUI.ghEntries en vez de fileEntries. Si la lista esta vacia
// (ghLoadErr o repo sin items) el confirm rebota con un error inline —
// esc igual deja al usuario volver al lister para fixear el contexto.
func (m RunModel) updateInputGHPicker(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inputUI.ghLoading {
		// Mientras la lista esta cargando, ↑/↓/enter no tienen lista que
		// operar — los ignoramos en silencio. esc/ctrl+c los maneja
		// updateInput (caller) antes de bajar aca.
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.inputUI.ghCursor > 0 {
			m.inputUI.ghCursor--
			m.inputErr = ""
		}
		return m, nil
	case "down", "j":
		if m.inputUI.ghCursor < len(m.inputUI.ghEntries)-1 {
			m.inputUI.ghCursor++
			m.inputErr = ""
		}
		return m, nil
	case "enter", "ctrl+s":
		if len(m.inputUI.ghEntries) == 0 {
			if m.inputUI.ghLoadErr != "" {
				m.inputErr = m.inputUI.ghLoadErr
			} else {
				m.inputErr = "no hay items abiertos en el repo activo"
			}
			return m, nil
		}
		sel := m.inputUI.ghEntries[m.inputUI.ghCursor]
		ref := fmt.Sprintf("%s#%d", m.inputUI.repo, sel.Number)
		// Sembrar el textBuf con la referencia armada — confirmInput
		// resuelve el payload via resolveGH (mismo path que el textBuf).
		m.inputUI.textBuf = textBuffer{}
		m.inputUI.textBuf.runes = []rune(ref)
		m.inputUI.textBuf.cursor = len(m.inputUI.textBuf.runes)
		return m.confirmInput()
	}
	return m, nil
}

// updateInputFile: navegacion del picker. up/down mueven cursor; enter
// entra al dir o selecciona el file; ctrl+s confirma con la entry actual.
func (m RunModel) updateInputFile(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		if m.inputUI.fileCursor > 0 {
			m.inputUI.fileCursor--
			m.inputErr = ""
		}
		return m, nil
	case "down", "j":
		if m.inputUI.fileCursor < len(m.inputUI.fileEntries)-1 {
			m.inputUI.fileCursor++
			m.inputErr = ""
		}
		return m, nil
	case "enter", " ":
		if len(m.inputUI.fileEntries) == 0 {
			return m, nil
		}
		sel := m.inputUI.fileEntries[m.inputUI.fileCursor]
		if sel.isDir {
			entries, err := loadFileEntries(sel.path)
			if err != nil {
				m.inputErr = "no se pudo leer dir: " + err.Error()
				return m, nil
			}
			m.inputUI.fileDir = sel.path
			m.inputUI.fileEntries = entries
			m.inputUI.fileCursor = 0
			m.inputErr = ""
			return m, nil
		}
		// File: confirmar usando esta ruta.
		m.inputUI.textBuf = textBuffer{}
		m.inputUI.textBuf.runes = []rune(sel.path)
		m.inputUI.textBuf.cursor = len(m.inputUI.textBuf.runes)
		return m.confirmInput()
	case "ctrl+s":
		// Confirm con lo que sea que este seleccionado.
		if len(m.inputUI.fileEntries) > 0 {
			sel := m.inputUI.fileEntries[m.inputUI.fileCursor]
			if !sel.isDir {
				m.inputUI.textBuf.runes = []rune(sel.path)
				m.inputUI.textBuf.cursor = len(m.inputUI.textBuf.runes)
				return m.confirmInput()
			}
		}
		m.inputErr = "selecciona un archivo (no un dir) antes de confirmar"
		return m, nil
	}
	return m, nil
}

// confirmInput valida + resuelve eager. Si la resolucion falla, deja el
// error en m.inputErr y se queda en R1 (criterio de aceptacion: foco
// vuelve al input). Si pasa, popula m.Input.{Value,ResolvedPayload} y
// transiciona a ScreenPreflight (R2 real, H3) corriendo los chequeos en
// el acto.
func (m RunModel) confirmInput() (tea.Model, tea.Cmd) {
	value := m.inputUI.textBuf.value()
	if m.inputUI.kind != wizard.InputFile {
		value = strings.TrimSpace(value)
	}
	if value == "" {
		m.inputErr = "el input no puede estar vacio"
		return m, nil
	}

	payload, err := resolveInput(m.inputUI.kind, value)
	if err != nil {
		m.inputErr = err.Error()
		return m, nil
	}

	m.Input.Kind = m.inputUI.kind
	m.Input.Value = value
	m.Input.ResolvedPayload = payload
	m.inputErr = ""
	return enterPreflight(m), nil
}

// viewInput renderiza R1 segun el kind.
func (m RunModel) viewInput() string {
	var b strings.Builder
	// Ultimo segmento del breadcrumb = "Input · <kind>" (text/pr/issue/
	// url/file/...). Asi la pantalla actual destaca con el kind exacto y
	// el header arriba sustituye al "Run · <name>" + label "Input · text"
	// que vivian apilados antes — info redundante una vez que el path
	// completo aparece en el header.
	last := "Input · " + m.inputUI.kind
	if m.inputUI.kind == "" {
		last = "Input"
	}
	crumb := append(runnerCrumb(m.Pipeline.Name), last)
	b.WriteString(breadcrumb(crumb...))
	b.WriteString("\n\n")

	switch m.inputUI.kind {
	case wizard.InputText:
		b.WriteString(dimStyle.Render("el step recibira este texto en stdin / como prompt"))
		b.WriteString("\n")
		b.WriteString(inputBoxBorder.Render(m.inputUI.textBuf.view()))
		b.WriteString("\n")
	case wizard.InputPR:
		if m.inputUI.repoMode {
			b.WriteString(dimStyle.Render(fmt.Sprintf("PRs abiertos en %s — ↑/↓ navegar · enter elegir", m.inputUI.repo)))
			b.WriteString("\n")
			b.WriteString(renderGHPicker(m.inputUI))
			b.WriteString("\n")
		} else {
			b.WriteString(dimStyle.Render("formato: owner/repo#NNN — se valida con `gh pr view`"))
			b.WriteString("\n")
			b.WriteString(inputBoxBorder.Render(m.inputUI.textBuf.view()))
			b.WriteString("\n")
		}
	case wizard.InputIssue:
		if m.inputUI.repoMode {
			b.WriteString(dimStyle.Render(fmt.Sprintf("issues abiertos en %s — ↑/↓ navegar · enter elegir", m.inputUI.repo)))
			b.WriteString("\n")
			b.WriteString(renderGHPicker(m.inputUI))
			b.WriteString("\n")
		} else {
			b.WriteString(dimStyle.Render("formato: owner/repo#NNN — se valida con `gh issue view`"))
			b.WriteString("\n")
			b.WriteString(inputBoxBorder.Render(m.inputUI.textBuf.view()))
			b.WriteString("\n")
		}
	case wizard.InputURL:
		b.WriteString(dimStyle.Render("http/https — fetch con timeout 10s al confirmar"))
		b.WriteString("\n")
		b.WriteString(inputBoxBorder.Render(m.inputUI.textBuf.view()))
		b.WriteString("\n")
	case wizard.InputFile:
		b.WriteString(dimStyle.Render(fmt.Sprintf("dir: %s", m.inputUI.fileDir)))
		b.WriteString("\n")
		b.WriteString(renderFilePicker(m.inputUI))
		b.WriteString("\n")
	default:
		b.WriteString(inputBoxBorder.Render(m.inputUI.textBuf.view()))
		b.WriteString("\n")
	}

	if m.inputErr != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("✗ " + m.inputErr))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch {
	case m.inputUI.kind == wizard.InputFile:
		b.WriteString(hintStyle.Render("↑/↓ navegar · enter abrir/seleccionar · ctrl+s confirmar · esc volver"))
	case m.inputUI.repoMode:
		b.WriteString(hintStyle.Render("↑/↓ navegar · enter / ctrl+s elegir · esc volver · ctrl+c salir"))
	default:
		b.WriteString(hintStyle.Render("ctrl+s / enter confirmar · esc volver · ctrl+c salir"))
	}
	b.WriteString("\n")
	return b.String()
}

// renderGHPicker es el render de la lista de PRs / issues abiertos del repo
// activo. Mismo patron que renderFilePicker (ventana centrada en el cursor,
// cap a 10 visibles segun el doc), pero la linea es "#NNN  titulo". Cuando
// la lista esta vacia, mostramos el error de carga (si lo hubo) o un
// placeholder neutro — el handler de teclas igual respeta esc para volver.
func renderGHPicker(ui inputUIState) string {
	if ui.ghLoading {
		label := "PRs"
		if ui.kind == wizard.InputIssue {
			label = "issues"
		}
		return dimStyle.Render(fmt.Sprintf("  Cargando %s abiertos en %s…", label, ui.repo))
	}
	if ui.ghLoadErr != "" {
		return errorStyle.Render("✗ "+ui.ghLoadErr) + "\n" + dimStyle.Render("  esc para volver al lister")
	}
	if len(ui.ghEntries) == 0 {
		return dimStyle.Render("(no hay items abiertos en " + ui.repo + ")")
	}
	const maxRows = 10
	start := 0
	end := len(ui.ghEntries)
	if end > maxRows {
		start = ui.ghCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		end = start + maxRows
		if end > len(ui.ghEntries) {
			end = len(ui.ghEntries)
			start = end - maxRows
			if start < 0 {
				start = 0
			}
		}
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		entry := ui.ghEntries[i]
		line := fmt.Sprintf("#%d  %s", entry.Number, entry.Title)
		if i == ui.ghCursor {
			b.WriteString(pickerSelected.Render("> " + line))
		} else {
			b.WriteString("  " + pickerNormal.Render(line))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderFilePicker(ui inputUIState) string {
	if len(ui.fileEntries) == 0 {
		return dimStyle.Render("(dir vacio)")
	}
	const maxRows = 12
	start := 0
	end := len(ui.fileEntries)
	if end > maxRows {
		// Centrar la ventana sobre el cursor.
		start = ui.fileCursor - maxRows/2
		if start < 0 {
			start = 0
		}
		end = start + maxRows
		if end > len(ui.fileEntries) {
			end = len(ui.fileEntries)
			start = end - maxRows
			if start < 0 {
				start = 0
			}
		}
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		entry := ui.fileEntries[i]
		marker := "  "
		name := entry.name
		if entry.isDir {
			name += "/"
		}
		if i == ui.fileCursor {
			b.WriteString(pickerSelected.Render("> " + name))
		} else {
			b.WriteString(marker + pickerNormal.Render(name))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
