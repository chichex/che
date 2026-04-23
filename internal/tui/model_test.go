package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/startup"
)

// key arma un KeyMsg a partir del string que el Update espera via
// msg.String(). Para teclas especiales (enter, esc, up, down, left, right)
// usa el KeyType; para runes (letras, "+" / "-") usa KeyRunes.
func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
	case "esc":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEsc})
	case "up":
		return tea.KeyMsg(tea.Key{Type: tea.KeyUp})
	case "down":
		return tea.KeyMsg(tea.Key{Type: tea.KeyDown})
	case "left":
		return tea.KeyMsg(tea.Key{Type: tea.KeyLeft})
	case "right":
		return tea.KeyMsg(tea.Key{Type: tea.KeyRight})
	case " ":
		return tea.KeyMsg(tea.Key{Type: tea.KeySpace, Runes: []rune(" ")})
	}
	return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune(s)})
}

// step aplica una tecla y devuelve el nuevo Model.
func step(m Model, k string) Model {
	m2, _ := m.handleKey(key(k))
	return m2.(Model)
}

// ---- validate select: dos listas + cursor unificado ----

func TestMaybeAdvanceValidate_AmbasCargan(t *testing.T) {
	m := Model{
		screen:              screenValidateLoading,
		validatePlans:       []validate.PlanCandidate{{Number: 42, Title: "plan uno"}},
		validatePRs:         []validate.Candidate{{Number: 7, Title: "pr uno"}},
		validatePlansLoaded: true,
		validatePRsLoaded:   true,
	}
	out, _ := m.maybeAdvanceValidate()
	got := out.(Model)
	if got.screen != screenValidateSelect {
		t.Fatalf("want screenValidateSelect, got %v", got.screen)
	}
	if got.validateCursor != 0 {
		t.Fatalf("cursor debería resetearse a 0, got %d", got.validateCursor)
	}
}

func TestMaybeAdvanceValidate_EsperaAmbos(t *testing.T) {
	// Solo llega plans; todavía no transiciona.
	m := Model{
		screen:              screenValidateLoading,
		validatePlans:       []validate.PlanCandidate{{Number: 42}},
		validatePlansLoaded: true,
	}
	out, _ := m.maybeAdvanceValidate()
	got := out.(Model)
	if got.screen != screenValidateLoading {
		t.Fatalf("no debería transicionar antes de que lleguen los dos, got %v", got.screen)
	}
}

func TestMaybeAdvanceValidate_AmbasVaciasEmptyState(t *testing.T) {
	m := Model{
		screen:              screenValidateLoading,
		validatePlansLoaded: true,
		validatePRsLoaded:   true,
	}
	out, _ := m.maybeAdvanceValidate()
	got := out.(Model)
	if got.screen != screenResult {
		t.Fatalf("want screenResult, got %v", got.screen)
	}
	if got.resultKind != resultInfo {
		t.Fatalf("want resultInfo, got %v", got.resultKind)
	}
}

func TestMaybeAdvanceValidate_AmbasFallanError(t *testing.T) {
	m := Model{
		screen:              screenValidateLoading,
		validatePlansErr:    errFake("gh issue list boom"),
		validatePRsErr:      errFake("gh pr list boom"),
		validatePlansLoaded: true,
		validatePRsLoaded:   true,
	}
	out, _ := m.maybeAdvanceValidate()
	got := out.(Model)
	if got.screen != screenResult {
		t.Fatalf("want screenResult, got %v", got.screen)
	}
	if got.resultKind != resultError {
		t.Fatalf("want resultError, got %v", got.resultKind)
	}
	// Espera dos mensajes (uno por error).
	if len(got.resultLines) != 2 {
		t.Fatalf("want 2 error lines, got %d: %v", len(got.resultLines), got.resultLines)
	}
}

func TestMaybeAdvanceValidate_UnaFallaOtraConItems(t *testing.T) {
	// Planes falla pero PRs trae items → continuamos con PRs solo.
	m := Model{
		screen:              screenValidateLoading,
		validatePlansErr:    errFake("boom"),
		validatePRs:         []validate.Candidate{{Number: 1}},
		validatePlansLoaded: true,
		validatePRsLoaded:   true,
	}
	out, _ := m.maybeAdvanceValidate()
	got := out.(Model)
	if got.screen != screenValidateSelect {
		t.Fatalf("want screenValidateSelect, got %v", got.screen)
	}
}

func TestValidateItemAt_PlanVsPR(t *testing.T) {
	m := Model{
		validatePlans: []validate.PlanCandidate{
			{Number: 42, URL: "u1"},
			{Number: 43, URL: "u2"},
		},
		validatePRs: []validate.Candidate{
			{Number: 7, URL: "u7"},
			{Number: 8, URL: "u8"},
		},
	}
	cases := []struct {
		idx      int
		wantNum  int
		wantURL  string
		wantIsPR bool
	}{
		{0, 42, "u1", false},
		{1, 43, "u2", false},
		{2, 7, "u7", true},
		{3, 8, "u8", true},
	}
	for _, c := range cases {
		n, url, _, isPR := m.validateItemAt(c.idx)
		if n != c.wantNum || url != c.wantURL || isPR != c.wantIsPR {
			t.Errorf("idx=%d: got (%d,%q,%v), want (%d,%q,%v)",
				c.idx, n, url, isPR, c.wantNum, c.wantURL, c.wantIsPR)
		}
	}
}

func TestValidateSelectCursor_NavigaEntreListas(t *testing.T) {
	m := Model{
		screen: screenValidateSelect,
		validatePlans: []validate.PlanCandidate{
			{Number: 42}, {Number: 43},
		},
		validatePRs: []validate.Candidate{
			{Number: 7}, {Number: 8},
		},
	}
	// Down pasa a 1.
	m = step(m, "down")
	if m.validateCursor != 1 {
		t.Fatalf("after 1 down, want cursor=1, got %d", m.validateCursor)
	}
	// Down pasa a 2 — primer PR.
	m = step(m, "down")
	if m.validateCursor != 2 {
		t.Fatalf("after 2 downs, want cursor=2, got %d", m.validateCursor)
	}
	// Down pasa a 3 — segundo PR.
	m = step(m, "down")
	if m.validateCursor != 3 {
		t.Fatalf("after 3 downs, want cursor=3, got %d", m.validateCursor)
	}
	// Down wrap a 0.
	m = step(m, "down")
	if m.validateCursor != 0 {
		t.Fatalf("wrap: want cursor=0, got %d", m.validateCursor)
	}
	// Up wrap al último (3).
	m = step(m, "up")
	if m.validateCursor != 3 {
		t.Fatalf("wrap up: want cursor=3, got %d", m.validateCursor)
	}
}

// ---- validate select: render ----

func TestRenderValidateSelect_AmbasConItems(t *testing.T) {
	m := Model{
		validatePlans: []validate.PlanCandidate{
			{Number: 42, Title: "título del plan"},
		},
		validatePRs: []validate.Candidate{
			{Number: 7, Title: "título del PR"},
		},
	}
	out := renderValidateSelect(m)
	if !strings.Contains(out, "Planes pendientes") {
		t.Errorf("falta header de planes: %s", out)
	}
	if !strings.Contains(out, "PRs abiertos") {
		t.Errorf("falta header de PRs: %s", out)
	}
	if !strings.Contains(out, "#42") || !strings.Contains(out, "título del plan") {
		t.Errorf("falta item de plan: %s", out)
	}
	if !strings.Contains(out, "#7") || !strings.Contains(out, "título del PR") {
		t.Errorf("falta item de PR: %s", out)
	}
}

func TestRenderValidateSelect_UnaVacia(t *testing.T) {
	// Planes vacía → muestra "(sin ítems)" bajo header.
	m := Model{
		validatePRs: []validate.Candidate{{Number: 7, Title: "pr"}},
	}
	out := renderValidateSelect(m)
	if !strings.Contains(out, "(sin ítems)") {
		t.Errorf("empty state para planes vacíos: %s", out)
	}
	if !strings.Contains(out, "#7") {
		t.Errorf("PR debería aparecer igual: %s", out)
	}
}

func TestRenderValidateSelect_AmbasVaciasHintAmable(t *testing.T) {
	m := Model{}
	out := renderValidateSelect(m)
	// El hint debería ser el amable cuando total==0.
	if !strings.Contains(out, "no hay planes ni PRs") {
		t.Errorf("hint amable para ambas vacías: %s", out)
	}
}

// ---- iterate select ----

func TestIterateItemAt_PlanVsPR(t *testing.T) {
	m := Model{
		iteratePlans: []validate.PlanCandidate{{Number: 10, URL: "p"}},
		iteratePRs:   []validate.Candidate{{Number: 20, URL: "r"}},
	}
	n0, url0, _, isPR0 := m.iterateItemAt(0)
	if n0 != 10 || url0 != "p" || isPR0 {
		t.Errorf("idx=0: got (%d,%q,%v); want plan", n0, url0, isPR0)
	}
	n1, url1, _, isPR1 := m.iterateItemAt(1)
	if n1 != 20 || url1 != "r" || !isPR1 {
		t.Errorf("idx=1: got (%d,%q,%v); want PR", n1, url1, isPR1)
	}
}

func TestMaybeAdvanceIterate_AmbasVaciasEmptyState(t *testing.T) {
	m := Model{
		screen:             screenIterateLoading,
		iteratePlansLoaded: true,
		iteratePRsLoaded:   true,
	}
	out, _ := m.maybeAdvanceIterate()
	got := out.(Model)
	if got.screen != screenResult || got.resultKind != resultInfo {
		t.Fatalf("want resultInfo, got screen=%v kind=%v", got.screen, got.resultKind)
	}
}

// ---- stepper del selector de validadores ----

func TestStepper_IncrementRespetaCapTotal(t *testing.T) {
	m := Model{
		screen:                  screenValidateValidators,
		validateValidatorCursor: 0,
		validateValidatorCount:  map[validate.Agent]int{validate.AgentOpus: 0},
	}
	// 3 incrementos sobre opus → cap=3 alcanzado.
	m = step(m, "right")
	m = step(m, "right")
	m = step(m, "right")
	if m.validateValidatorCount[validate.AgentOpus] != 3 {
		t.Fatalf("want opus=3, got %d", m.validateValidatorCount[validate.AgentOpus])
	}
	// Cuarto increment sobre opus → no-op.
	m = step(m, "right")
	if m.validateValidatorCount[validate.AgentOpus] != 3 {
		t.Fatalf("cap total no respetado: got %d", m.validateValidatorCount[validate.AgentOpus])
	}
	// Cambiar al agente 1 e intentar subirlo → también no-op (total ya 3).
	m = step(m, "down")
	m = step(m, "right")
	if m.validateValidatorCount[validate.AgentCodex] != 0 {
		t.Fatalf("cap total no respetado para otro agente: got %d", m.validateValidatorCount[validate.AgentCodex])
	}
}

func TestStepper_DecrementPisoCero(t *testing.T) {
	m := Model{
		screen:                  screenValidateValidators,
		validateValidatorCursor: 0,
		validateValidatorCount:  map[validate.Agent]int{validate.AgentOpus: 1},
	}
	m = step(m, "left")
	if m.validateValidatorCount[validate.AgentOpus] != 0 {
		t.Fatalf("decrement de 1 → 0, got %d", m.validateValidatorCount[validate.AgentOpus])
	}
	// Segundo decrement → no-op (piso 0).
	m = step(m, "left")
	if m.validateValidatorCount[validate.AgentOpus] != 0 {
		t.Fatalf("piso 0 no respetado: got %d", m.validateValidatorCount[validate.AgentOpus])
	}
}

func TestStepper_EnterConTotalZeroRechaza(t *testing.T) {
	m := Model{
		screen:                  screenValidateValidators,
		validateValidatorCursor: 0,
		validateValidatorCount:  map[validate.Agent]int{},
		validateChosenRef:       "42",
	}
	m = step(m, "enter")
	// Se queda en la pantalla de validadores (no transiciona a Running).
	if m.screen != screenValidateValidators {
		t.Fatalf("enter con total=0 no debería avanzar, got screen=%v", m.screen)
	}
}

func TestStepper_PlusMinusAtajos(t *testing.T) {
	// "+" / "-" son aliases de right/left.
	m := Model{
		screen:                  screenValidateValidators,
		validateValidatorCursor: 0,
		validateValidatorCount:  map[validate.Agent]int{},
	}
	m = step(m, "+")
	if m.validateValidatorCount[validate.AgentOpus] != 1 {
		t.Fatalf("'+' debería incrementar: got %d", m.validateValidatorCount[validate.AgentOpus])
	}
	m = step(m, "-")
	if m.validateValidatorCount[validate.AgentOpus] != 0 {
		t.Fatalf("'-' debería decrementar: got %d", m.validateValidatorCount[validate.AgentOpus])
	}
}

func TestStepper_RenderIncluyeTotal(t *testing.T) {
	m := Model{
		validateChosenRef:       "42",
		validateValidatorCursor: 0,
		validateValidatorCount: map[validate.Agent]int{
			validate.AgentOpus:  1,
			validate.AgentCodex: 1,
		},
	}
	out := renderValidateValidators(m)
	if !strings.Contains(out, "Total: 2") {
		t.Errorf("render debería incluir Total: 2, got: %s", out)
	}
	if !strings.Contains(out, "[ 1 ]") {
		t.Errorf("render debería incluir stepper [ 1 ]: %s", out)
	}
}

// ---- línea de contexto del header en flows en ejecución ----

func TestRenderRunSubject_RefVacioDevuelveVacio(t *testing.T) {
	if got := renderRunSubject("", "Fix login", 0); got != "" {
		t.Errorf("ref vacío debería devolver string vacío, got %q", got)
	}
}

func TestRenderRunSubject_IncluyeRefYTitulo(t *testing.T) {
	got := renderRunSubject("42", "Fix login bug", 0)
	if !strings.Contains(got, "#42") {
		t.Errorf("falta #42 en subject: %q", got)
	}
	if !strings.Contains(got, "Fix login bug") {
		t.Errorf("falta título en subject: %q", got)
	}
}

func TestRenderRunSubject_TruncaTitulosLargos(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := renderRunSubject("42", long, 0)
	if !strings.Contains(got, "…") {
		t.Errorf("título largo debería terminar con …: %q", got)
	}
	// El runtime real no debe imprimir 200 'a' seguidas.
	if strings.Contains(got, strings.Repeat("a", 100)) {
		t.Errorf("título no fue truncado: %q", got)
	}
}

func TestRenderRunning_IncluyeSubjectEntreTituloYElapsed(t *testing.T) {
	m := Model{}
	subject := renderRunSubject("42", "Fix login bug", 0)
	out := renderRunning(m, "Explorando issue…", subject, "Ctrl+C cancela")

	idxTitle := strings.Index(out, "Explorando issue")
	idxRef := strings.Index(out, "#42")
	idxElapsed := strings.Index(out, "transcurridos")
	if idxTitle < 0 || idxRef < 0 || idxElapsed < 0 {
		t.Fatalf("falta alguna pieza: title=%d ref=%d elapsed=%d (out=%q)",
			idxTitle, idxRef, idxElapsed, out)
	}
	if !(idxTitle < idxRef && idxRef < idxElapsed) {
		t.Errorf("orden esperado: título → #N → elapsed; got title=%d ref=%d elapsed=%d",
			idxTitle, idxRef, idxElapsed)
	}
}

func TestRenderRunning_SinSubjectNoMuestraLineaContexto(t *testing.T) {
	m := Model{}
	out := renderRunning(m, "Procesando idea…", "", "Ctrl+C cancela")
	if strings.Contains(out, "#") {
		t.Errorf("sin subject no debería haber #N en el header: %q", out)
	}
}

// Con un contentWidth chico, el título se trunca agresivamente para
// que la línea completa entre. Esto previene la regresión donde el
// emulador cortaba el render silenciosamente (ver issue del "hace 1"
// sin unidad cuando el ancho no daba).
func TestRenderRunSubject_TruncaDinamicamenteConWidthChico(t *testing.T) {
	long := "este titulo es largo pero no tanto como para superar 70 chars siempre"
	// contentWidth = 20 deja ~14 chars para el título después del
	// overhead de "#42 — " (seis visibles).
	got := renderRunSubject("42", long, 20)
	if !strings.Contains(got, "#42") {
		t.Errorf("falta #42: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("con width chico el título debería truncarse: %q", got)
	}
}

// ---- formatLastAction adapta a ancho ----

func TestFormatLastAction_SinWidthUsaCapHistorico(t *testing.T) {
	la := &lastAction{
		Flow:  "execute",
		Ref:   "115",
		Title: strings.Repeat("x", 200),
	}
	got := formatLastAction(la, 0)
	if !strings.Contains(got, "#115") {
		t.Errorf("falta #115: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("título largo debería terminar con …: %q", got)
	}
	// El cap histórico es 40 runas, así que 100 'x' seguidas no deberían
	// aparecer aunque el título original tenga 200.
	if strings.Contains(got, strings.Repeat("x", 100)) {
		t.Errorf("título no fue truncado al cap histórico de 40: %q", got)
	}
}

func TestFormatLastAction_WidthChicoPreservaTimeYRef(t *testing.T) {
	la := &lastAction{
		Flow:  "execute",
		Ref:   "115",
		Title: "[Pricing #5] Mobile Provider — form de configuración del provider",
		At:    time.Now().Add(-3 * time.Minute),
	}
	// Ancho típico de la captura: ~70-80 columnas de contenido.
	got := formatLastAction(la, 60)
	if !strings.Contains(got, "#115") {
		t.Errorf("#115 se perdió: %q", got)
	}
	if !strings.Contains(got, "hace ") || !strings.Contains(got, "m") {
		t.Errorf("el 'hace Xm' debería sobrevivir al truncado: %q", got)
	}
}

func TestFormatLastAction_AnchoInsuficienteSueltaTitulo(t *testing.T) {
	la := &lastAction{
		Flow:  "execute",
		Ref:   "115",
		Title: "título que no entra",
		At:    time.Now().Add(-5 * time.Minute),
	}
	// Ancho muy chico: cabe el verbo + ref + tiempo pero no el título.
	got := formatLastAction(la, 32)
	if !strings.Contains(got, "#115") {
		t.Errorf("falta #115 con ancho mínimo: %q", got)
	}
	if strings.Contains(got, "«") {
		t.Errorf("con ancho chico no debería haber título: %q", got)
	}
}

// ---- last action + sugerencia de próximo paso ----

func TestSuggestedNext_MapeoPorFlow(t *testing.T) {
	cases := []struct {
		name     string
		la       lastAction
		wantScr  screen
		wantHave bool
	}{
		{"idea → explore", lastAction{Flow: "idea"}, screenExploreLoading, true},
		{"explore → validate", lastAction{Flow: "explore"}, screenValidateLoading, true},
		{"execute → validate", lastAction{Flow: "execute"}, screenValidateLoading, true},
		{"validate plan → execute", lastAction{Flow: "validate", IsPR: false}, screenExecuteLoading, true},
		{"validate PR → close", lastAction{Flow: "validate", IsPR: true}, screenCloseLoading, true},
		{"iterate plan → validate", lastAction{Flow: "iterate", IsPR: false}, screenValidateLoading, true},
		{"iterate PR → validate", lastAction{Flow: "iterate", IsPR: true}, screenValidateLoading, true},
		{"close → nada", lastAction{Flow: "close"}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scr, ok := suggestedNext(&c.la)
			if ok != c.wantHave {
				t.Fatalf("ok: got %v want %v", ok, c.wantHave)
			}
			if ok && scr != c.wantScr {
				t.Errorf("screen: got %v want %v", scr, c.wantScr)
			}
		})
	}
}

func TestSuggestedNext_NilNoHaySugerencia(t *testing.T) {
	if _, ok := suggestedNext(nil); ok {
		t.Fatalf("nil lastAction no debería sugerir nada")
	}
}

func TestRecordFlowSuccess_SeteaLastActionYMueveCursor(t *testing.T) {
	m := Model{cursor: 0}
	m = m.recordFlowSuccess("explore", "42", "mi issue", false)
	if m.lastAction == nil || m.lastAction.Flow != "explore" {
		t.Fatalf("lastAction mal seteado: %+v", m.lastAction)
	}
	if m.lastAction.Ref != "42" || m.lastAction.Title != "mi issue" {
		t.Errorf("campos mal: %+v", m.lastAction)
	}
	// Explore sugiere Validar → índice 3 en menuItems (0=idea,1=explore,2=ejecutar,3=validar).
	if m.cursor != 3 {
		t.Errorf("cursor debería apuntar a Validar (3), got %d", m.cursor)
	}
}

func TestRecordFlowSuccess_CloseNoMueveCursor(t *testing.T) {
	m := Model{cursor: 2}
	m = m.recordFlowSuccess("close", "7", "", true)
	// Close no tiene sugerencia → cursor intacto.
	if m.cursor != 2 {
		t.Errorf("cursor no debería moverse en close, got %d", m.cursor)
	}
	if m.lastAction == nil || m.lastAction.Flow != "close" {
		t.Errorf("lastAction igual debería grabarse: %+v", m.lastAction)
	}
}

func TestRenderMenu_SinLastActionNoMuestraLinea(t *testing.T) {
	m := Model{}
	out := renderMenu(m)
	if strings.Contains(out, "Última") {
		t.Errorf("sin lastAction no debería aparecer 'Última': %s", out)
	}
	if strings.Contains(out, "sugerido") {
		t.Errorf("sin lastAction no debería aparecer 'sugerido': %s", out)
	}
}

func TestRenderMenu_ConLastActionMuestraLineaYSugerido(t *testing.T) {
	m := Model{
		cursor: 3, // Validar (sugerido después de explore)
		lastAction: &lastAction{
			Flow:  "explore",
			Ref:   "42",
			Title: "mi issue",
		},
	}
	out := renderMenu(m)
	if !strings.Contains(out, "Última") {
		t.Errorf("falta línea 'Última': %s", out)
	}
	if !strings.Contains(out, "#42") {
		t.Errorf("falta ref #42 en línea de última acción: %s", out)
	}
	if !strings.Contains(out, "sugerido") {
		t.Errorf("falta marca 'sugerido' en el item del menú: %s", out)
	}
}

func TestRenderMenu_IdeaSinRefNoImprimeHash(t *testing.T) {
	m := Model{
		cursor:     1, // Explorar (sugerido tras idea)
		lastAction: &lastAction{Flow: "idea"},
	}
	out := renderMenu(m)
	if !strings.Contains(out, "Última") {
		t.Errorf("falta línea 'Última': %s", out)
	}
	// Idea sin ref no debería imprimir "#" (el único "#" del menú es en el item de ref).
	if strings.Contains(out, "#") {
		t.Errorf("idea sin ref no debería imprimir '#': %s", out)
	}
}

// ---- startup checks: arranque y dispatch ----

// Sin checks triggered, transicionamos directo al menú (cero ruido en
// happy path).
func TestStartupChecksLoaded_SinTriggeredVaAlMenu(t *testing.T) {
	m := Model{screen: screenStartupChecksLoading}
	out, _ := m.Update(startupChecksLoadedMsg{
		results: []startup.Result{
			{Name: startup.CheckMigrateLabels, Triggered: false},
			{Name: startup.CheckVersion, Triggered: false},
			{Name: startup.CheckLocks, Triggered: false},
		},
	})
	got := out.(Model)
	if got.screen != screenMenu {
		t.Errorf("sin checks triggered debería ir al menú, got %v", got.screen)
	}
	if len(got.startupChecks) != 0 {
		t.Errorf("startupChecks debería quedar vacío, got %d", len(got.startupChecks))
	}
}

// Con al menos un check triggered, mostramos la pantalla y filtramos
// los no-triggered.
func TestStartupChecksLoaded_FiltreaSoloTriggered(t *testing.T) {
	m := Model{screen: screenStartupChecksLoading}
	out, _ := m.Update(startupChecksLoadedMsg{
		results: []startup.Result{
			{Name: startup.CheckMigrateLabels, Triggered: true, OldLabels: []string{"status:idea"}},
			{Name: startup.CheckVersion, Triggered: false},
			{Name: startup.CheckLocks, Triggered: true, Locks: []startup.LockedItem{{Number: 42}}},
		},
	})
	got := out.(Model)
	if got.screen != screenStartupChecks {
		t.Fatalf("debería ir a screenStartupChecks, got %v", got.screen)
	}
	if len(got.startupChecks) != 2 {
		t.Errorf("debería filtrar a los 2 triggered, got %d", len(got.startupChecks))
	}
	if got.startupCursor != 0 {
		t.Errorf("cursor debería arrancar en 0, got %d", got.startupCursor)
	}
}

// "n" avanza al próximo check sin persistir nada.
func TestStartupChecksKey_NoEstaVez(t *testing.T) {
	m := Model{
		screen:        screenStartupChecks,
		startupChecks: []startup.Result{{Name: startup.CheckVersion, Triggered: true}},
		startupCursor: 0,
	}
	m = step(m, "n")
	// Sin más checks: vuelve al menú.
	if m.screen != screenMenu {
		t.Errorf("después del último check 'n' debería ir al menú, got %v", m.screen)
	}
}

// "esc" sale de la pantalla de chequeos.
func TestStartupChecksKey_EscSaltaTodos(t *testing.T) {
	m := Model{
		screen: screenStartupChecks,
		startupChecks: []startup.Result{
			{Name: startup.CheckVersion, Triggered: true},
			{Name: startup.CheckLocks, Triggered: true},
		},
	}
	m = step(m, "esc")
	if m.screen != screenMenu {
		t.Errorf("esc debería ir al menú, got %v", m.screen)
	}
	if len(m.startupChecks) != 0 {
		t.Errorf("esc debería limpiar la lista, got %d", len(m.startupChecks))
	}
}

// "n" sobre 2 checks avanza primero al segundo, después al menú.
func TestStartupChecksKey_NoAvanzaSecuencial(t *testing.T) {
	m := Model{
		screen: screenStartupChecks,
		startupChecks: []startup.Result{
			{Name: startup.CheckMigrateLabels, Triggered: true},
			{Name: startup.CheckVersion, Triggered: true},
		},
	}
	m = step(m, "n")
	if m.screen != screenStartupChecks {
		t.Errorf("después de 1 'n' con 2 checks, debería seguir en checks, got %v", m.screen)
	}
	if m.startupCursor != 1 {
		t.Errorf("cursor debería ser 1, got %d", m.startupCursor)
	}
	m = step(m, "n")
	if m.screen != screenMenu {
		t.Errorf("después del 2do 'n' debería ir al menú, got %v", m.screen)
	}
}

// "s" sobre el check de locks salta directo a screenLocksLoading.
func TestStartupChecksKey_SiLocksSaltaAPantallaDeLocks(t *testing.T) {
	m := Model{
		screen: screenStartupChecks,
		startupChecks: []startup.Result{
			{Name: startup.CheckLocks, Triggered: true, Locks: []startup.LockedItem{{Number: 42}}},
		},
	}
	out, _ := m.handleStartupChecksKey(key("s"))
	got := out.(Model)
	if got.screen != screenLocksLoading {
		t.Errorf("'s' sobre locks debería ir a screenLocksLoading, got %v", got.screen)
	}
}

func TestRenderStartupChecks_IncluyeMensajeYOpciones(t *testing.T) {
	m := Model{
		startupChecks: []startup.Result{{
			Name:           startup.CheckVersion,
			Triggered:      true,
			CurrentVersion: "0.0.49",
			LatestVersion:  "v0.0.50",
		}},
	}
	out := renderStartupChecks(m)
	if !strings.Contains(out, "0.0.49") || !strings.Contains(out, "v0.0.50") {
		t.Errorf("debería incluir versiones: %s", out)
	}
	if !strings.Contains(out, "[s]") || !strings.Contains(out, "[N]") {
		t.Errorf("debería incluir opciones [s]/[N]: %s", out)
	}
	if !strings.Contains(out, "che upgrade") {
		t.Errorf("debería sugerir 'che upgrade': %s", out)
	}
}

func TestFormatStartupCheckMessage_PorTipo(t *testing.T) {
	cases := []struct {
		name string
		c    startup.Result
		want string
	}{
		{
			"migrate-labels",
			startup.Result{Name: startup.CheckMigrateLabels, OldLabels: []string{"status:idea", "status:plan"}},
			"migrate-labels",
		},
		{
			"version",
			startup.Result{Name: startup.CheckVersion, CurrentVersion: "0.0.1", LatestVersion: "v0.0.2"},
			"upgrade",
		},
		{
			"locks",
			startup.Result{Name: startup.CheckLocks, Locks: []startup.LockedItem{{Number: 42}, {Number: 7}}},
			"#42",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatStartupCheckMessage(c.c)
			if !strings.Contains(got, c.want) {
				t.Errorf("mensaje no incluye %q: %s", c.want, got)
			}
		})
	}
}

// NewWithOptions con RunStartupChecks=false debe arrancar directo en el
// menú, sin pasar por screenStartupChecksLoading.
func TestNewWithOptions_NoChecksArrancaEnMenu(t *testing.T) {
	m := NewWithOptions("0.0.1", nil, Options{RunStartupChecks: false})
	if m.screen != screenMenu {
		t.Errorf("con RunStartupChecks=false debería arrancar en menú, got %v", m.screen)
	}
}

// ---- helpers ----

type fakeErr string

func (f fakeErr) Error() string { return string(f) }
func errFake(s string) error    { return fakeErr(s) }
