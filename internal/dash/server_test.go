package dash

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer es el helper de todos los tests: MockSource + repo ficticio +
// poll de 15s (valor no relevante en tests, pero necesitamos uno válido).
func newTestServer(t *testing.T, repo string) *httptest.Server {
	t.Helper()
	s := NewServer(MockSource{}, repo, 15)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts
}

func TestDashboardHandler_Index(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q want text/html*", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	// Repo name interpolado.
	if !strings.Contains(got, "che-cli") {
		t.Errorf("body missing repo name 'che-cli'")
	}
	// Topbar.
	if !strings.Contains(got, "auto-loop") {
		t.Errorf("body missing 'auto-loop' topbar")
	}
	// Card de #42 → !55 con título del PR (la "fusion entidad" del mock).
	if !strings.Contains(got, "fusion entidad") {
		t.Errorf("body missing card title 'fusion entidad'")
	}
	// Dracula sanity check.
	if !strings.Contains(got, "#282A36") {
		t.Errorf("body missing Dracula bg hex")
	}
	// Step 2: rename mergeable → approved.
	// PR3: 9 columnas (idea, planning, plan, executing, executed, validating,
	// validated, closing, closed). "approved" desapareció como columna —
	// ahora la columna es "validated".
	if !strings.Contains(got, ">validated<") {
		t.Errorf("body missing column header 'validated'")
	}
	if !strings.Contains(got, ">closed<") {
		t.Errorf("body missing column header 'closed'")
	}
	if !strings.Contains(got, ">planning<") {
		t.Errorf("body missing column header 'planning'")
	}
	if strings.Contains(got, ">approved<") {
		t.Errorf("body still contains legacy column 'approved' (PR3 lo sustituye por 'validated')")
	}
	if strings.Contains(got, ">mergeable<") {
		t.Errorf("body still contains old column 'mergeable'")
	}
	// El detalle ahora se monta como modal overlay; el slot del modal vive
	// vacío hasta que htmx swappee el partial. Antes era #drawer-slot
	// (sidebar); el rename a #modal-slot acompaña al refactor del wrapper.
	if !strings.Contains(got, `id="modal-slot"`) {
		t.Errorf("body missing #modal-slot")
	}
	// Step 2: htmx + dash.js embedded.
	if !strings.Contains(got, `src="/static/htmx.min.js"`) {
		t.Errorf("body missing htmx script tag")
	}
	if !strings.Contains(got, `src="/static/dash.js"`) {
		t.Errorf("body missing dash.js script tag")
	}
	// Step 2: chips reales en cards (no más ct:tech).
	if strings.Contains(got, "ct:tech") {
		t.Errorf("body still contains non-existent label 'ct:tech'")
	}
	if !strings.Contains(got, "type:feature") {
		t.Errorf("body missing real label 'type:feature'")
	}
	// Step 2: cada card cliqueable via hx-get.
	if !strings.Contains(got, `hx-get="/drawer/42"`) {
		t.Errorf("body missing hx-get for card #42")
	}
	// Step 3: chip de mock mode presente en el topbar.
	if !strings.Contains(got, "mock mode") {
		t.Errorf("body missing 'mock mode' status chip (MockSource)")
	}
	// Step 3: wrapper del board con polling de HTMX.
	if !strings.Contains(got, `hx-get="/board"`) {
		t.Errorf("body missing hx-get=\"/board\" on the dash-board wrapper")
	}
	if !strings.Contains(got, `hx-trigger="load delay:200ms, every 15s"`) {
		t.Errorf("body missing hx-trigger 'load delay:200ms, every 15s' (primer poll inmediato + ticker)")
	}
}

func TestDashboardHandler_NotFound(t *testing.T) {
	srv := newTestServer(t, "repo")

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
}

func TestDrawerHandler_Found(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET /drawer/42: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "fusion entidad") {
		t.Errorf("drawer body missing title 'fusion entidad'")
	}
	if !strings.Contains(got, "!55") {
		t.Errorf("drawer body missing PR ref '!55'")
	}
	if !strings.Contains(got, `data-entity="42"`) {
		t.Errorf("drawer body missing data-entity=42 root")
	}
	if !strings.Contains(got, "iterate started") {
		t.Errorf("drawer body missing log entry 'iterate started'")
	}
}

func TestDrawerHandler_NotFound(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/drawer/9999")
	if err != nil {
		t.Fatalf("GET /drawer/9999: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
}

func TestDrawerHandler_Close(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/drawer/close")
	if err != nil {
		t.Fatalf("GET /drawer/close: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) != 0 {
		t.Errorf("body should be empty (HTMX clears slot); got %q", string(body))
	}
}

func TestStaticHandler_HTMX(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/static/htmx.min.js")
	if err != nil {
		t.Fatalf("GET /static/htmx.min.js: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body[:200]), "htmx") {
		t.Errorf("htmx.min.js body doesn't look like htmx bundle; head: %q", string(body[:200]))
	}
}

func TestStaticHandler_DashJS(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/static/dash.js")
	if err != nil {
		t.Fatalf("GET /static/dash.js: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Tras el refactor a modal el cierre se llama closeModal (antes
	// closeDrawer); el listener de htmx:afterSwap sigue presente como hook
	// reservado para futuros usos sobre #modal-slot.
	if !strings.Contains(string(body), "closeModal") {
		t.Errorf("dash.js body missing closeModal fn")
	}
	if !strings.Contains(string(body), "htmx:afterSwap") {
		t.Errorf("dash.js body missing htmx:afterSwap listener")
	}
}

// TestBoardRendersClickableRefs verifica que el HTML del board incluye
// links absolutos a github.com en los refs de issue/PR de cada card. El
// MockSource setea NWO="demo/che", así que esperamos URLs concretas.
//
// Cubre el step 3.5: sin esto los refs eran <span> inertes.
func TestBoardRendersClickableRefs(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	// Al menos un link a issue de la NWO mock (hay issues y fused en el board).
	if !strings.Contains(got, `href="https://github.com/demo/che/issues/`) {
		t.Errorf("body missing clickable issue href; want href=\"https://github.com/demo/che/issues/...\"")
	}
	// Al menos un link a PR (hay fused entities en el mock).
	if !strings.Contains(got, `href="https://github.com/demo/che/pull/`) {
		t.Errorf("body missing clickable PR href; want href=\"https://github.com/demo/che/pull/...\"")
	}
	// target=_blank para que los refs abran en nueva pestaña.
	if !strings.Contains(got, `target="_blank"`) {
		t.Errorf("body missing target=\"_blank\" on ref links")
	}
	// stopPropagation inline es crítico — sin esto el click abre el drawer junto con la tab.
	if !strings.Contains(got, "stopPropagation") {
		t.Errorf("body missing onclick=\"event.stopPropagation()\" on ref links")
	}
}

// TestDrawerRendersIssueBodyForFused verifica que el drawer fused renderiza
// (a) el tab switcher PR/Issue, (b) el pane con el body del issue original.
//
// Construimos el Server con una Source custom en vez de MockSource para
// poder fijar el IssueBody con un contenido conocido.
func TestDrawerRendersIssueBodyForFused(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO: "demo/che",
		Entities: []Entity{
			{
				Kind: KindFused, IssueNumber: 42, IssueTitle: "fused test",
				PRNumber: 55, PRTitle: "fused PR",
				IssueBody: "## Contexto\ntest body aquí",
			},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET /drawer/42: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	// El body del issue queda renderizado en el tab Issue.
	if !strings.Contains(got, "test body aquí") {
		t.Errorf("drawer missing issue body 'test body aquí'")
	}
	// Root del drawer con data-tab="pr" (default).
	if !strings.Contains(got, `data-tab="pr"`) {
		t.Errorf("drawer root missing data-tab=\"pr\"")
	}
	// Ambas pestañas presentes.
	if !strings.Contains(got, "tab-pr") {
		t.Errorf("drawer missing class tab-pr")
	}
	if !strings.Contains(got, "tab-issue") {
		t.Errorf("drawer missing class tab-issue")
	}
	// Pane del tab Issue presente.
	if !strings.Contains(got, "pane-issue") {
		t.Errorf("drawer missing class pane-issue")
	}
}

// fixedSource es una Source hardcodeada para tests que necesitan construir
// un snapshot con Entities específicas (sin depender de mockEntities()).
type fixedSource struct{ snap Snapshot }

func (f *fixedSource) Snapshot() Snapshot { return f.snap }

// TestColumnsOrder fija el contrato del orden left-to-right del board: 9
// columnas reflejando los 9 estados che:* (PR3). Si alguien reordena el
// slice o suma/quita una columna, el test rompe.
func TestColumnsOrder(t *testing.T) {
	want := []string{"idea", "planning", "plan", "executing", "executed", "validating", "validated", "closing", "closed"}
	if len(columnsOrder) != len(want) {
		t.Fatalf("columnsOrder len: got %d want %d", len(columnsOrder), len(want))
	}
	for i, c := range columnsOrder {
		if c.Key != want[i] {
			t.Errorf("columnsOrder[%d].Key: got %q want %q", i, c.Key, want[i])
		}
	}
}

// TestGroupByColumn_HotSemantics chequea que el badge "hot" prende para las
// 4 columnas transient (planning/executing/validating/closing) cuando hay
// una entidad con RunningFlow != "", y NO prende para las terminales (idea,
// plan, executed, validated, closed).
func TestGroupByColumn_HotSemantics(t *testing.T) {
	entities := []Entity{
		{Status: "planning", RunningFlow: "explore"},
		{Status: "executing", RunningFlow: "execute"},
		{Status: "validating", RunningFlow: "validate"},
		{Status: "closing", RunningFlow: "close"},
		// No-hot: status terminal aunque haya RunningFlow seteado (caso raro).
		{Status: "validated", RunningFlow: "iterate"},
		// No-hot: planning sin flow (transient pero idle).
		{Status: "plan"},
	}
	got := groupByColumn(entities)
	hotByKey := map[string]bool{}
	for _, c := range got {
		hotByKey[c.Key] = c.Hot
	}
	wantHot := []string{"planning", "executing", "validating", "closing"}
	for _, k := range wantHot {
		if !hotByKey[k] {
			t.Errorf("columna %q debería estar hot", k)
		}
	}
	wantNotHot := []string{"idea", "plan", "executed", "validated", "closed"}
	for _, k := range wantNotHot {
		if hotByKey[k] {
			t.Errorf("columna %q NO debería estar hot", k)
		}
	}
}

// TestBoardHandler_MockSource: el endpoint /board devuelve status-chip (oob)
// + columnas, listo para que HTMX swappee el innerHTML del wrapper.
func TestBoardHandler_MockSource(t *testing.T) {
	srv := newTestServer(t, "che-cli")

	resp, err := http.Get(srv.URL + "/board")
	if err != nil {
		t.Fatalf("GET /board: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	// Status chip con hx-swap-oob — se actualiza el chip del topbar "afuera"
	// del target del swap principal del board.
	if !strings.Contains(got, `hx-swap-oob="outerHTML"`) {
		t.Errorf("/board missing hx-swap-oob for status-chip")
	}
	if !strings.Contains(got, `id="status-chip"`) {
		t.Errorf("/board missing id=\"status-chip\"")
	}
	if !strings.Contains(got, "mock mode") {
		t.Errorf("/board missing 'mock mode' chip text (MockSource)")
	}
	// Columnas presentes (sample: idea, validated, closed). PR3 reemplaza
	// las 6 columnas viejas (incluyendo "backlog" y "approved") por las 9
	// che:* — chequeamos extremos + una del medio.
	if !strings.Contains(got, `data-status="idea"`) {
		t.Errorf("/board missing column data-status=idea")
	}
	if !strings.Contains(got, `data-status="validated"`) {
		t.Errorf("/board missing column data-status=validated")
	}
	if !strings.Contains(got, `data-status="closed"`) {
		t.Errorf("/board missing column data-status=closed")
	}
	// Adaptive polling PR: el partial AHORA incluye el wrapper <div
	// id="dash-board" class="dash-board"> con su propio hx-trigger y
	// hx-swap="outerHTML". Cada poll reemplaza el wrapper entero — así
	// el hx-trigger puede usar NextPollSec (adaptivo: 15s idle, 3s hot).
	if !strings.Contains(got, `id="dash-board"`) {
		t.Errorf("/board should include the .dash-board wrapper with id=\"dash-board\" (adaptive polling swaps outerHTML)")
	}
	if !strings.Contains(got, `hx-swap="outerHTML"`) {
		t.Errorf("/board wrapper should use hx-swap=\"outerHTML\" (adaptive polling requires replacing the wrapper to update hx-trigger)")
	}
	// MockSource sin flows locales → NextPollSec == PollInterval (15).
	if !strings.Contains(got, `hx-trigger="every 15s"`) {
		t.Errorf("/board (idle) should emit hx-trigger=\"every 15s\"; body: %s", got)
	}
}

// TestBoardLoading_FirstPollPending chequea que cuando el snapshot todavía
// no tuvo primer poll exitoso (LastOK zero) y NO estamos en mock, el render
// del board incluye el spinner de "loading" en lugar de las 9 columnas
// vacías. Evita el bug visual donde el board parece "no hay nada en el
// repo" durante los primeros segundos antes del primer gh poll.
func TestBoardLoading_FirstPollPending(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		// LastOK zero (sin primer poll), Mock false, Stale false.
	}}
	srv := httptest.NewServer(NewServer(src, "owner/repo", 15))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `class="board-loading"`) {
		t.Errorf("/ missing board-loading wrapper while LastOK is zero")
	}
	if !strings.Contains(got, `class="spinner"`) {
		t.Errorf("/ missing spinner while LastOK is zero")
	}
	// Las columnas NO deberían renderizarse mientras se muestra el spinner.
	if strings.Contains(got, `data-status="idea"`) {
		t.Errorf("/ should not render columns while LastOK is zero (got data-status=idea)")
	}
}

// TestBoardLoading_AfterFirstPoll chequea que una vez que LastOK ya no es
// zero (primer poll OK), el render muestra las 9 columnas y NO el spinner.
func TestBoardLoading_AfterFirstPoll(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
	}}
	srv := httptest.NewServer(NewServer(src, "owner/repo", 15))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if strings.Contains(got, `class="board-loading"`) {
		t.Errorf("/ should NOT include board-loading after first poll OK")
	}
	if !strings.Contains(got, `data-status="idea"`) {
		t.Errorf("/ should render columns after first poll OK")
	}
}

// ============================================================
// Step 4: POST /action/{flow}/{id}
// ============================================================

// fakeRunner captura las invocaciones a runAction sin spawnear procesos.
// Concurrency-safe para que los tests de doble dispatch no corran con -race.
type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
	err   error
}

type fakeCall struct {
	Flow      string
	TargetRef int // número que recibiría el subcomando (PR o issue según flow+Kind)
	EntityKey int // IssueNumber canónico usado como clave del overlay y del LogStore
	Repo      string
}

func (f *fakeRunner) run(flow string, targetRef, entityKey int, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Flow: flow, TargetRef: targetRef, EntityKey: entityKey, Repo: repo})
	return f.err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRunner) last() fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeCall{}
	}
	return f.calls[len(f.calls)-1]
}

// newActionServer monta un Server con fakeRunner inyectado + una Source
// fija con issue 42 en status=plan (para que execute/validate apliquen).
func newActionServer(t *testing.T) (*httptest.Server, *Server, *fakeRunner) {
	t.Helper()
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "plan ready", Status: "plan"},
			{Kind: KindFused, IssueNumber: 7, IssueTitle: "fused", PRNumber: 12, Status: "executing", RunningFlow: "execute"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/fakerepo"
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, s, fr
}

// TestAction_DispatchesFlow chequea el happy path: POST /action/execute/42
// llama al runner una vez con los args correctos y responde 200 con el
// drawer refreshado (ya muestra el chip ⟳).
func TestAction_DispatchesFlow(t *testing.T) {
	ts, _, fr := newActionServer(t)

	resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST /action/execute/42: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if fr.count() != 1 {
		t.Fatalf("runner calls: got %d want 1", fr.count())
	}
	got := fr.last()
	// execute es issue-first: TargetRef=EntityKey=IssueNumber.
	if got.Flow != "execute" || got.TargetRef != 42 || got.EntityKey != 42 || got.Repo != "/tmp/fakerepo" {
		t.Errorf("runner call: got %+v want {execute target=42 key=42 /tmp/fakerepo}", got)
	}
	// Response carga el drawer con el chip de running (overlay local)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "⟳ execute") {
		t.Errorf("drawer response missing running chip '⟳ execute'; body head: %s", string(body[:min(400, len(body))]))
	}
}

// TestAction_FusedValidateUsesPR chequea el mapeo clave: POST con el
// IssueNumber de una entidad fused dispara al subcomando con el PRNumber
// (resolveTargetRef), pero el overlay/LogStore quedan indexados al
// IssueNumber (clave canónica que el modal conoce). Regresión: antes
// pasábamos siempre IssueNumber al subproceso y `che validate <issue>`
// en che:executed caía al modo Plan y rechazaba con "no está en che:plan".
func TestAction_FusedValidateUsesPR(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindFused, IssueNumber: 122, PRNumber: 140, IssueTitle: "f", Status: "executed"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/validate/122", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	got := fr.last()
	if got.Flow != "validate" {
		t.Errorf("flow: got %q want validate", got.Flow)
	}
	if got.TargetRef != 140 {
		t.Errorf("TargetRef: got %d want 140 (PR — resolveTargetRef debe mapear fused+validate a PRNumber)", got.TargetRef)
	}
	if got.EntityKey != 122 {
		t.Errorf("EntityKey: got %d want 122 (IssueNumber canónico para overlay/LogStore)", got.EntityKey)
	}

	// iterate hace el mismo mapeo.
	fr2 := &fakeRunner{}
	s.runAction = fr2.run
	resp2, err := http.Post(ts.URL+"/action/iterate/122", "", nil)
	if err != nil {
		t.Fatalf("POST iterate: %v", err)
	}
	resp2.Body.Close()
	// 409 esperado (ya hay un validate en curso del dispatch anterior).
	// Relajamos: chequeamos que cuando SE dispare (lo liberamos abajo) use PR.
	s.clearRunning(122)
	resp3, err := http.Post(ts.URL+"/action/iterate/122", "", nil)
	if err != nil {
		t.Fatalf("POST iterate#2: %v", err)
	}
	resp3.Body.Close()
	if fr2.count() == 0 {
		t.Fatalf("iterate runner not called")
	}
	g2 := fr2.last()
	if g2.TargetRef != 140 || g2.EntityKey != 122 {
		t.Errorf("iterate mapping: got target=%d key=%d want target=140 key=122", g2.TargetRef, g2.EntityKey)
	}
}

// TestAction_FusedCloseUsesPR: close en fused también mapea a PRNumber
// (che close acepta <pr>, no issue). Same pattern que validate/iterate.
func TestAction_FusedCloseUsesPR(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindFused, IssueNumber: 122, PRNumber: 140, IssueTitle: "f", Status: "validated"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/close/122", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	got := fr.last()
	if got.Flow != "close" || got.TargetRef != 140 || got.EntityKey != 122 {
		t.Errorf("close mapping: got %+v want {close target=140 key=122 ...}", got)
	}
}

// TestDrawerRendersCloseButton_FusedExecuted: close aparece en fused
// cuando el status es executed o validated (los que acepta che close).
func TestDrawerRendersCloseButton_FusedExecuted(t *testing.T) {
	for _, status := range []string{"executed", "validated"} {
		t.Run(status, func(t *testing.T) {
			src := &fixedSource{snap: Snapshot{
				NWO:    "demo/che",
				LastOK: time.Now(),
				Entities: []Entity{
					{Kind: KindFused, IssueNumber: 42, PRNumber: 55, IssueTitle: "t", PRTitle: "t", Status: status},
				},
			}}
			s := NewServer(src, "che-cli", 15)
			ts := httptest.NewServer(s)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/drawer/42")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `hx-post="/action/close/42"`) {
				t.Errorf("status=%s: drawer missing close button", status)
			}
		})
	}
}

// TestDrawerHidesCloseButton_FusedPreExecuted: close NO aparece si el
// PR está en un status anterior a executed (executing/validating no
// deberían ofrecer close — che close los rechaza).
func TestDrawerHidesCloseButton_FusedPreExecuted(t *testing.T) {
	for _, status := range []string{"executing", "validating", "closing", "closed"} {
		t.Run(status, func(t *testing.T) {
			src := &fixedSource{snap: Snapshot{
				NWO:    "demo/che",
				LastOK: time.Now(),
				Entities: []Entity{
					{Kind: KindFused, IssueNumber: 42, PRNumber: 55, IssueTitle: "t", PRTitle: "t", Status: status},
				},
			}}
			s := NewServer(src, "che-cli", 15)
			ts := httptest.NewServer(s)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/drawer/42")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), `hx-post="/action/close/42"`) {
				t.Errorf("status=%s: drawer should not show close button", status)
			}
		})
	}
}

// TestAction_IssueOnlyValidateUsesIssue: fused NO aplica (issue-only), así
// que validate pasa IssueNumber tal cual — no mapea a nada.
func TestAction_IssueOnlyValidateUsesIssue(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "i", Status: "plan"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/action/validate/42", "", nil)
	resp.Body.Close()
	got := fr.last()
	if got.TargetRef != 42 || got.EntityKey != 42 {
		t.Errorf("issue-only validate: got target=%d key=%d want target=42 key=42", got.TargetRef, got.EntityKey)
	}
}

// TestAction_InvalidFlow rechaza flows que no están en la allowlist.
// Protección primaria: no interpolar input del request directo en un exec.
func TestAction_InvalidFlow(t *testing.T) {
	ts, _, fr := newActionServer(t)

	resp, err := http.Post(ts.URL+"/action/foobar/42", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	if fr.count() != 0 {
		t.Errorf("runner should not have been called for invalid flow; got %d calls", fr.count())
	}
}

// TestAction_UnknownEntity rechaza ids que no están en el snapshot.
func TestAction_UnknownEntity(t *testing.T) {
	ts, _, fr := newActionServer(t)

	resp, err := http.Post(ts.URL+"/action/execute/999", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", resp.StatusCode)
	}
	if fr.count() != 0 {
		t.Errorf("runner should not be called for unknown entity")
	}
}

// TestAction_DoubleDispatch chequea que un segundo POST sobre la misma
// entity mientras el primer flow está corriendo devuelve 409 sin llamar
// al runner otra vez.
func TestAction_DoubleDispatch(t *testing.T) {
	ts, _, fr := newActionServer(t)

	resp1, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST #1: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("status #1: got %d want 200", resp1.StatusCode)
	}

	resp2, err := http.Post(ts.URL+"/action/validate/42", "", nil)
	if err != nil {
		t.Fatalf("POST #2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("status #2: got %d want 409", resp2.StatusCode)
	}
	if fr.count() != 1 {
		t.Errorf("runner calls: got %d want 1 (second dispatch should be rejected)", fr.count())
	}
}

// TestAction_SnapshotRunningBlocks: si el snapshot ya marca RunningFlow
// para la entidad (ej: el usuario disparó `che execute` por CLI y el
// poller lo levantó), el dashboard no deja disparar otro flow encima.
func TestAction_SnapshotRunningBlocks(t *testing.T) {
	ts, _, fr := newActionServer(t)

	// Issue 7 está en status=executing con RunningFlow=execute en el snapshot
	// — ver newActionServer.
	resp, err := http.Post(ts.URL+"/action/iterate/7", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d want 409", resp.StatusCode)
	}
	if fr.count() != 0 {
		t.Errorf("runner should not be called when snapshot already shows flow running")
	}
}

// TestAction_GETNotAllowed — el endpoint es POST-only. Evita triggers
// accidentales por bots o browsers haciendo prefetch. Go 1.22 ServeMux
// enruta al handler "GET /" (que es wildcard) cuando pedimos GET a un
// path solo registrado como POST; ese handler devuelve 404 para paths
// ≠ "/". Aceptamos 404 o 405 — lo importante es que NO ejecuta la
// acción (no llama al runner).
func TestAction_GETNotAllowed(t *testing.T) {
	ts, _, fr := newActionServer(t)

	resp, err := http.Get(ts.URL + "/action/execute/42")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 or 405", resp.StatusCode)
	}
	if fr.count() != 0 {
		t.Errorf("runner must not be called on GET /action/...; got %d calls", fr.count())
	}
}

// TestDrawerRendersActionButtons chequea que el drawer renderea los
// botones con hx-post apuntando a /action/{flow}/{id} (ya no disabled)
// y el "ver en GH" como <a href> a github.com.
func TestDrawerRendersActionButtons(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "plan ready", Status: "plan"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET /drawer/42: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `hx-post="/action/execute/42"`) {
		t.Errorf("drawer missing hx-post=/action/execute/42")
	}
	if !strings.Contains(got, `hx-post="/action/validate/42"`) {
		t.Errorf("drawer missing hx-post=/action/validate/42")
	}
	if !strings.Contains(got, `href="https://github.com/demo/che/issues/42"`) {
		t.Errorf("drawer missing 'ver en GH' href to issues/42")
	}
	if strings.Contains(got, "step 2: inerte") {
		t.Errorf("drawer still shows legacy 'step 2: inerte' title on action buttons")
	}
}

// TestDrawerRendersActionButtons_Fused verifica el variant Kind=1: los
// botones iterate/validate apuntan al IssueNumber (clave canónica de
// modal/LogStore/overlay); el server traduce a PRNumber para el
// subproceso vía resolveTargetRef — ver TestAction_FusedValidateUsesPR.
// "ver en GH" linkea al PR.
func TestDrawerRendersActionButtons_Fused(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindFused, IssueNumber: 42, PRNumber: 55, IssueTitle: "t", PRTitle: "t", Status: "validated"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET /drawer/42: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `hx-post="/action/iterate/42"`) {
		t.Errorf("fused drawer missing hx-post=/action/iterate/42 (IssueNumber canónico)")
	}
	if !strings.Contains(got, `hx-post="/action/validate/42"`) {
		t.Errorf("fused drawer missing hx-post=/action/validate/42 (IssueNumber canónico)")
	}
	// "ver en GH" del fused apunta al PR (es la vista principal del tab pr).
	if !strings.Contains(got, `href="https://github.com/demo/che/pull/55"`) {
		t.Errorf("fused drawer missing 'ver en GH' href to pull/55")
	}
}

// TestDrawerDisablesWhenRunning: si la entidad tiene RunningFlow, los
// botones de acción salen disabled (evita doble dispatch desde la UI).
func TestDrawerDisablesWhenRunning(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "running", Status: "plan", RunningFlow: "execute"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// hx-post presente (no lo sacamos del DOM) pero la prop disabled activa.
	if !strings.Contains(got, "disabled") {
		t.Errorf("drawer with RunningFlow should disable action buttons")
	}
}

// TestDrawerIssueOnlyValidated_ApprovedShowsExecute: rama issue-only
// Status=validated con PlanVerdict=approve (o vacío) — el siguiente paso
// es execute. El botón de re-validación (validate) sigue disponible por
// si el humano quiere otra ronda. iterate NO aparece en este caso
// (nada que iterar con un approve).
func TestDrawerIssueOnlyValidated_ApprovedShowsExecute(t *testing.T) {
	for _, verdict := range []string{"approve", ""} {
		name := verdict
		if name == "" {
			name = "no-verdict"
		}
		t.Run(name, func(t *testing.T) {
			src := &fixedSource{snap: Snapshot{
				NWO:    "demo/che",
				LastOK: time.Now(),
				Entities: []Entity{
					{Kind: KindIssue, IssueNumber: 122, IssueTitle: "approved plan", Status: "validated", PlanVerdict: verdict},
				},
			}}
			s := NewServer(src, "che-cli", 15)
			ts := httptest.NewServer(s)
			defer ts.Close()

			resp, err := http.Get(ts.URL + "/drawer/122")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			got := string(body)

			if !strings.Contains(got, `hx-post="/action/execute/122"`) {
				t.Errorf("validated+%s: missing execute button", verdict)
			}
			if !strings.Contains(got, `hx-post="/action/validate/122"`) {
				t.Errorf("validated+%s: missing re-validate button", verdict)
			}
			if strings.Contains(got, `hx-post="/action/iterate/122"`) {
				t.Errorf("validated+%s: no debe haber iterate (solo changes-requested)", verdict)
			}
			// close tampoco — close aplica sobre PR fused, no issue-only.
			if strings.Contains(got, `hx-post="/action/close/122"`) {
				t.Errorf("validated+%s: close no aplica en issue-only", verdict)
			}
		})
	}
}

// TestDrawerIssueOnlyValidated_ChangesRequestedShowsIterate: rama issue-only
// Status=validated + PlanVerdict=changes-requested → iterate es el próximo
// paso obligatorio (aplicar los findings). NO mostramos execute (execute
// rechaza con ese verdict — el gate está explícito en execute.go).
func TestDrawerIssueOnlyValidated_ChangesRequestedShowsIterate(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 122, IssueTitle: "rework", Status: "validated", PlanVerdict: "changes-requested"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/122")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `hx-post="/action/iterate/122"`) {
		t.Errorf("validated+changes-requested: missing iterate button")
	}
	if strings.Contains(got, `hx-post="/action/execute/122"`) {
		t.Errorf("validated+changes-requested: execute no debe ofrecerse (el gate lo rechaza)")
	}
	if strings.Contains(got, `hx-post="/action/validate/122"`) {
		t.Errorf("validated+changes-requested: re-validate no debe ofrecerse (iterar primero)")
	}
}

// TestDrawerIssueOnlyValidated_NeedsHumanShowsHint: rama issue-only
// Status=validated + PlanVerdict=needs-human → no mostramos ningún botón
// de flow; el humano tiene que resolver a mano. El hint aparece en lugar.
func TestDrawerIssueOnlyValidated_NeedsHumanShowsHint(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 122, IssueTitle: "esc", Status: "validated", PlanVerdict: "needs-human"},
		},
	}}
	s := NewServer(src, "che-cli", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/122")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Ningún hx-post de flows (iterate/execute/validate) en el drawer body.
	for _, forbidden := range []string{
		`hx-post="/action/iterate/122"`,
		`hx-post="/action/execute/122"`,
		`hx-post="/action/validate/122"`,
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("validated+needs-human: encontré %s — no debería haber botones de flow", forbidden)
		}
	}
	if !strings.Contains(got, "needs-human") {
		t.Errorf("validated+needs-human: hint en el DOM ausente")
	}
}

// min es un helper — Go tiene builtin min en 1.21+ pero lo aliaseamos
// por claridad en el error message del happy path.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==================================================================
// Adaptive polling (NextPollSec + Bump hooks)
// ==================================================================

// bumpableSource es una Source que implementa Bumper contando calls.
// Alrededor de fixedSource — embebe el snapshot y agrega el counter.
type bumpableSource struct {
	snap     Snapshot
	bumpCalls atomic.Int64
}

func (b *bumpableSource) Snapshot() Snapshot { return b.snap }
func (b *bumpableSource) Bump()                { b.bumpCalls.Add(1) }

// TestBuildData_NextPollSecIsBaselineWhenIdle: sin flows locales corriendo,
// NextPollSec == PollInterval (tick regular).
func TestBuildData_NextPollSecIsBaselineWhenIdle(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	data := s.buildData()
	if data.NextPollSec != 15 {
		t.Errorf("idle NextPollSec: got %d want 15", data.NextPollSec)
	}
}

// TestBuildData_NextPollSecIsHotWhenRunning: con al menos un flow local
// en s.running, NextPollSec baja a hotPollSec para que las transiciones
// de label se vean casi inmediatamente.
func TestBuildData_NextPollSecIsHotWhenRunning(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	// Simular dispatch pendiente — mismo estado que deja POST /action
	// después de markRunning exitoso.
	s.mu.Lock()
	s.running[42] = "execute"
	s.mu.Unlock()

	data := s.buildData()
	if data.NextPollSec != hotPollSec {
		t.Errorf("hot NextPollSec: got %d want %d (hotPollSec)", data.NextPollSec, hotPollSec)
	}
}

// TestBuildData_NextPollSecCappedByBaseline: si PollInterval ya es menor
// que hotPollSec (ej: operador configuró --poll=1), no subir a hotPollSec
// por "estar hot" — sería un downgrade. El baseline manda.
func TestBuildData_NextPollSecCappedByBaseline(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 1) // baseline agresivo.
	s.mu.Lock()
	s.running[42] = "execute"
	s.mu.Unlock()
	data := s.buildData()
	if data.NextPollSec != 1 {
		t.Errorf("baseline < hotPollSec should win: got %d want 1", data.NextPollSec)
	}
}

// TestClearRunning_CallsBump: cuando termina un subproceso che (el flujo
// interno llama clearRunning), la Source recibe un Bump para que el poller
// refresque ASAP y el board refleje la transición de label che:executing →
// che:executed sin esperar al tick regular.
func TestClearRunning_CallsBump(t *testing.T) {
	src := &bumpableSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	// markRunning + clearRunning: el setup exacto que hace runCmdWithLogs
	// cuando termina un subproceso (clearRunning en el goroutine de Wait).
	if _, ok := s.markRunning(42, "execute"); !ok {
		t.Fatalf("markRunning should succeed on first call")
	}
	s.clearRunning(42)

	if got := src.bumpCalls.Load(); got != 1 {
		t.Errorf("clearRunning should call Bump exactly once; got %d", got)
	}
}

// TestActionHandler_CallsBumpAfterSpawn: el handler POST /action dispara
// un Bump después de spawn exitoso — el subcomando che típicamente aplica
// un label transient al inicio (che execute → che:executing) y queremos
// verlo rápido en el board. Separado del Bump de clearRunning (que cubre
// la transición de salida).
func TestActionHandler_CallsBumpAfterSpawn(t *testing.T) {
	src := &bumpableSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "plan ready", Status: "plan"},
		},
	}}
	s := NewServer(src, "repo", 15)
	// Runner fake que no falla — mismo shape que otros tests de action.
	fr := &fakeRunner{}
	s.runAction = fr.run
	s.repoPath = "/tmp/r"
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/execute/42", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if fr.count() != 1 {
		t.Fatalf("runner calls: got %d want 1", fr.count())
	}
	if got := src.bumpCalls.Load(); got != 1 {
		t.Errorf("action handler should call Bump once after successful spawn; got %d", got)
	}
}

// TestBoardPartial_HxTriggerIsAdaptive: el partial devuelve el wrapper
// .dash-board con hx-trigger = NextPollSec. Idle → "every 15s"; hot →
// "every 3s". Cubre el cambio de template y la integración con NextPollSec.
func TestBoardPartial_HxTriggerIsAdaptive(t *testing.T) {
	t.Run("idle → every 15s", func(t *testing.T) {
		srv := newTestServer(t, "repo")
		resp, err := http.Get(srv.URL + "/board")
		if err != nil {
			t.Fatalf("GET /board: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := string(body)
		if !strings.Contains(got, `hx-trigger="every 15s"`) {
			t.Errorf("idle /board: missing hx-trigger=\"every 15s\" in body")
		}
		if !strings.Contains(got, `id="dash-board"`) {
			t.Errorf("/board missing id=\"dash-board\" (required for HTMX outerHTML identity)")
		}
	})

	t.Run("hot → every 3s", func(t *testing.T) {
		src := &fixedSource{snap: Snapshot{
			LastOK: time.Now(),
			Entities: []Entity{
				{Kind: KindIssue, IssueNumber: 42, IssueTitle: "x", Status: "plan"},
			},
		}}
		s := NewServer(src, "repo", 15)
		s.mu.Lock()
		s.running[42] = "execute"
		s.mu.Unlock()
		ts := httptest.NewServer(s)
		defer ts.Close()
		resp, err := http.Get(ts.URL + "/board")
		if err != nil {
			t.Fatalf("GET /board: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := string(body)
		if !strings.Contains(got, `hx-trigger="every 3s"`) {
			t.Errorf("hot /board: missing hx-trigger=\"every 3s\"; body: %s", got)
		}
	})
}

// TestOverlayRunning_InjectsRoundsCounter: el chip magenta del card muestra
// ⟳ <flow> <RunIter>/<RunMax>. Esos campos nunca se asignaban, siempre
// mostraba "0/0". Ahora overlayRunning lee el counter del loopState y el
// cap (LoopCap) y los copia al Entity cuando RunningFlow != "".
func TestOverlayRunning_InjectsRoundsCounter(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	// 2 rounds ya consumidos para el issue 42.
	s.loop.incRounds(42)
	s.loop.incRounds(42)
	// Overlay local: el POST /action marcó 42 con "execute" antes del tick.
	s.mu.Lock()
	s.running[42] = "execute"
	s.mu.Unlock()

	in := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "validated", PlanVerdict: "approve"},
		{Kind: KindIssue, IssueNumber: 99, Status: "plan"}, // sin flow, no debe inyectar
	}
	out := s.overlayRunning(in)
	if out[0].RunningFlow != "execute" {
		t.Errorf("out[0].RunningFlow: got %q want %q", out[0].RunningFlow, "execute")
	}
	if out[0].RunIter != 2 {
		t.Errorf("out[0].RunIter: got %d want 2 (incRounds x2)", out[0].RunIter)
	}
	if out[0].RunMax != LoopCap {
		t.Errorf("out[0].RunMax: got %d want %d (LoopCap)", out[0].RunMax, LoopCap)
	}
	// Entity sin flow corriendo no debe tener RunIter/RunMax seteados.
	if out[1].RunIter != 0 || out[1].RunMax != 0 {
		t.Errorf("out[1] idle: got RunIter=%d RunMax=%d, want 0/0", out[1].RunIter, out[1].RunMax)
	}
}

// TestOverlayRunning_InjectsRoundsForSnapshotFlow: incluso cuando el
// RunningFlow viene del snapshot (label che:executing transient aplicado
// por el subproceso real) y no hay overlay local, los campos RunIter/
// RunMax se inyectan desde el loopState.
func TestOverlayRunning_InjectsRoundsForSnapshotFlow(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	s.loop.incRounds(42) // 1 round.

	in := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "executing", RunningFlow: "execute"},
	}
	out := s.overlayRunning(in)
	if out[0].RunIter != 1 {
		t.Errorf("out[0].RunIter: got %d want 1", out[0].RunIter)
	}
	if out[0].RunMax != LoopCap {
		t.Errorf("out[0].RunMax: got %d want %d", out[0].RunMax, LoopCap)
	}
}

// TestOverlayRunning_IdleNoOp: si no hay running local ni entities con
// RunningFlow del snapshot, overlayRunning devuelve el slice sin alocar.
// Fast path importante porque buildData se llama en cada /board.
func TestOverlayRunning_IdleNoOp(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	in := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
	}
	out := s.overlayRunning(in)
	// Identidad del slice (fast path retorna in directamente).
	if &out[0] != &in[0] {
		t.Errorf("idle overlay: expected identity return (no copy), got new slice")
	}
}
