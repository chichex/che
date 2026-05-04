package dash

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chichex/che/internal/pipeline"
)

// TestListenWithFallback_PicksNextWhenBusy: motivación del cambio — correr
// `che dash` en dos repos a la vez con el puerto default no debe morir; el
// segundo elige otro puerto libre.
func TestListenWithFallback_PicksNextWhenBusy(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen busy: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	ln, err := listenWithFallback(busyPort, true)
	if err != nil {
		t.Fatalf("listenWithFallback: %v", err)
	}
	defer ln.Close()
	got := ln.Addr().(*net.TCPAddr).Port
	if got == busyPort {
		t.Fatalf("listener bound to busy port %d", got)
	}
	if got <= busyPort || got > busyPort+portFallbackRange {
		t.Fatalf("port %d out of fallback range (%d, %d]", got, busyPort, busyPort+portFallbackRange)
	}
}

// TestListenWithFallback_RespectsExplicit: cuando el usuario pinó el puerto
// con --port, no hacemos fallback silencioso — devolvemos el error original
// para que vea que su pedido no se cumplió.
func TestListenWithFallback_RespectsExplicit(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen busy: %v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	ln, err := listenWithFallback(busyPort, false)
	if err == nil {
		ln.Close()
		t.Fatal("expected error when explicit port busy, got nil")
	}
}

// TestListenWithFallback_FreePort: con el puerto libre no toca nada — caso
// happy path para que un cambio futuro al loop de fallback no rompa el caso
// "puerto vacante" silenciosamente.
func TestListenWithFallback_FreePort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, err := listenWithFallback(port, true)
	if err != nil {
		t.Fatalf("listenWithFallback: %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Fatalf("port: got %d want %d", got, port)
	}
}

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

// TestColumnsOrder fija el contrato del orden left-to-right del board: 10
// columnas — "adopt" al inicio (opt-in) + los 9 estados che:* (PR3). Si
// alguien reordena el slice o suma/quita una columna, el test rompe.
func TestColumnsOrder(t *testing.T) {
	want := []string{"adopt", "idea", "planning", "plan", "executing", "executed", "validating", "validated", "closing", "closed"}
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
			// IssueBody con "## Plan consolidado" para que el preflight
			// gate de validate-plan lo deje pasar (tests de tick + action
			// no testean parsing — eso vive en preflight_test.go).
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "plan ready", Status: "plan", IssueBody: "## Plan consolidado\n\n**Resumen:** ready\n"},
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
			// PRVerdict para que el gate de iterate (que también testea
			// abajo) lo deje pasar tras el clearRunning. Sin verdict el
			// gate bloquearía con "el PR no tiene verdict" — eso lo
			// cubren los tests de preflight, acá testeamos routing.
			{Kind: KindFused, IssueNumber: 122, PRNumber: 140, IssueTitle: "f", Status: "executed", PRVerdict: "changes-requested"},
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

func TestResolveTargetRef_FusedPRSideUsesPR(t *testing.T) {
	e := Entity{Kind: KindFused, IssueNumber: 122, PRNumber: 140}
	for _, flow := range []string{"validate", "iterate", "close"} {
		if got := resolveTargetRef(e, flow); got != 140 {
			t.Errorf("%s target: got %d want PRNumber 140", flow, got)
		}
	}
	for _, flow := range []string{"explore", "execute"} {
		if got := resolveTargetRef(e, flow); got != 122 {
			t.Errorf("%s target: got %d want IssueNumber 122", flow, got)
		}
	}
}

func TestAction_KindPRSnapshotRunningBlocksDispatch(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		NWO:    "demo/che",
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan", Status: "validated", PRVerdict: "changes-requested", RunningFlow: "validate"},
		},
	}}
	s := NewServer(src, "repo", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/iterate/301", "", nil)
	if err != nil {
		t.Fatalf("POST iterate/301: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d want 409", resp.StatusCode)
	}
	if fr.count() != 0 {
		t.Fatalf("runner calls: got %d want 0", fr.count())
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
			// IssueBody con header "## Plan consolidado" para que el gate
			// de validate plan lo deje pasar — el test es sobre routing,
			// no sobre el preflight (eso vive en preflight_test.go).
			{Kind: KindIssue, IssueNumber: 42, IssueTitle: "i", Status: "plan", IssueBody: "## Plan consolidado\n\n**Resumen:** x\n"},
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
	snap      Snapshot
	bumpCalls atomic.Int64
}

func (b *bumpableSource) Snapshot() Snapshot { return b.snap }
func (b *bumpableSource) Bump()              { b.bumpCalls.Add(1) }

// TestBuildData_NextPollSecIsBaselineWhenIdle: sin flows locales corriendo,
// NextPollSec == PollInterval (tick regular).
func TestBuildData_NextPollSecIsBaselineWhenIdle(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	data := s.buildData(false)
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

	data := s.buildData(false)
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
	data := s.buildData(false)
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

// TestStatusChip_UsesNextPollSec: el chip expone data-poll-interval
// (y el texto "next in Xs" inicial) con NextPollSec — el intervalo real
// que usa HTMX — no con PollInterval (baseline). Antes usaba PollInterval
// lo cual producía oscilaciones en el countdown del JS cuando había flows
// hot: el swap llegaba cada 3s reseteando clientLastOk, pero el contador
// arrancaba en 15, alcanzando a bajar solo a 13 antes del próximo reset.
func TestStatusChip_UsesNextPollSec(t *testing.T) {
	t.Run("idle → data-poll-interval=15", func(t *testing.T) {
		src := &fixedSource{snap: Snapshot{
			LastOK:   time.Now(),
			Entities: []Entity{{Kind: KindIssue, IssueNumber: 42, IssueTitle: "x", Status: "plan"}},
		}}
		s := NewServer(src, "repo", 15)
		ts := httptest.NewServer(s)
		defer ts.Close()
		resp, err := http.Get(ts.URL + "/board")
		if err != nil {
			t.Fatalf("GET /board: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := string(body)
		if !strings.Contains(got, `data-poll-interval="15"`) {
			t.Errorf("idle: chip missing data-poll-interval=\"15\"")
		}
		if !strings.Contains(got, `next in 15s`) {
			t.Errorf("idle: chip missing \"next in 15s\" text")
		}
	})

	t.Run("hot → data-poll-interval=3", func(t *testing.T) {
		src := &fixedSource{snap: Snapshot{
			LastOK:   time.Now(),
			Entities: []Entity{{Kind: KindIssue, IssueNumber: 42, IssueTitle: "x", Status: "plan"}},
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
		if !strings.Contains(got, `data-poll-interval="3"`) {
			t.Errorf("hot: chip missing data-poll-interval=\"3\" (el counter tiene que coincidir con hx-trigger)")
		}
		if !strings.Contains(got, `next in 3s`) {
			t.Errorf("hot: chip missing \"next in 3s\" text")
		}
	})
}

// TestOverlayRunning_InjectsRoundsCounter: el chip magenta del card muestra
// ⟳ <flow> <RunIter>/<RunMax>. Esos campos nunca se asignaban, siempre
// mostraba "0/0". Ahora overlayRunning lee el counter del loopState y el
// cap efectivo y los copia al Entity cuando RunningFlow != "".
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

func TestOverlayRunning_InjectsDynamicRunMax(t *testing.T) {
	s := NewServer(&fixedSource{snap: Snapshot{LastOK: time.Now()}}, "repo", 15)
	s.loop.incRounds(42)
	s.mu.Lock()
	s.running[42] = "run#:idea"
	s.mu.Unlock()

	out := s.overlayRunning([]Entity{{Kind: KindIssue, IssueNumber: 42, Status: "idea", StateStep: "idea"}})
	if out[0].RunIter != 1 {
		t.Errorf("RunIter: got %d want 1", out[0].RunIter)
	}
	if out[0].RunMax != DynamicLoopCap {
		t.Errorf("RunMax: got %d want %d (DynamicLoopCap)", out[0].RunMax, DynamicLoopCap)
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

// TestOverlayRunning_IdlePopulatesGates: aún en estado idle (sin running
// local ni RunningFlow en snapshot), overlayRunning aloca y popula
// Entity.Gates para que el template tenga datos al renderar botones
// disabled+title. El fast-path original "retornar in sin copiar" se sacó al
// agregar preflight gates (PR de gates UI, abril 2026): el costo del copy
// es trivial (~50 entities en un dash típico) y la alternativa requería
// computar gates en el caller, esparciendo la responsabilidad.
func TestOverlayRunning_IdlePopulatesGates(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	in := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "plan", IssueBody: "## Plan consolidado\n\nx"},
	}
	out := s.overlayRunning(in)
	if out[0].Gates == nil {
		t.Fatalf("idle overlay: Gates no fue populado")
	}
	if !out[0].Gates[flowValidate].Available {
		t.Errorf("idle overlay: gate validate debería estar Available para issue plan con body consolidado, got Reason=%q", out[0].Gates[flowValidate].Reason)
	}
}

// TestOverlayRunning_SetsCapReachedWhenIdle: entity idle (sin RunningFlow)
// cuyo contador de rounds alcanzó LoopCap y está en status loopable debe
// salir con CapReached=true + RunMax=LoopCap. El auto-loop ya cortó — el
// humano tiene que ver visualmente que este card no se va a mover solo.
func TestOverlayRunning_SetsCapReachedWhenIdle(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	for i := 0; i < LoopCap; i++ {
		s.loop.incRounds(42)
	}
	in := []Entity{
		{Kind: KindFused, IssueNumber: 42, PRNumber: 100, Status: "executed"},
	}
	out := s.overlayRunning(in)
	if !out[0].CapReached {
		t.Errorf("out[0].CapReached: got false, want true (rounds=%d >= LoopCap=%d)",
			s.loop.roundsFor(42), LoopCap)
	}
	if out[0].RunMax != LoopCap {
		t.Errorf("out[0].RunMax: got %d want %d (cap debe inyectarse aunque no haya run)", out[0].RunMax, LoopCap)
	}
}

func TestOverlayRunning_KindPRCapUsesPRKey(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	for i := 0; i < LoopCap; i++ {
		s.loop.incRounds(301)
	}
	in := []Entity{
		{Kind: KindPR, PRNumber: 301, Status: "validated", PRVerdict: "changes-requested"},
	}
	out := s.overlayRunning(in)
	if !out[0].CapReached {
		t.Errorf("KindPR CapReached: got false, want true (rounds keyed by PRNumber)")
	}
	if out[0].RunMax != LoopCap {
		t.Errorf("KindPR RunMax: got %d want %d", out[0].RunMax, LoopCap)
	}
}

// TestOverlayRunning_CapReachedOnlyInLoopableStatus: si la entity ya
// cerró (Status=closed) o es una idea pre-plan, el cap no aplica — no
// queremos contaminar columnas terminales con chips de cap irrelevantes.
func TestOverlayRunning_CapReachedOnlyInLoopableStatus(t *testing.T) {
	src := &fixedSource{snap: Snapshot{LastOK: time.Now()}}
	s := NewServer(src, "repo", 15)
	for i := 0; i < LoopCap; i++ {
		s.loop.incRounds(42)
		s.loop.incRounds(43)
	}
	in := []Entity{
		{Kind: KindIssue, IssueNumber: 42, Status: "closed"},
		{Kind: KindIssue, IssueNumber: 43, Status: "idea"},
	}
	out := s.overlayRunning(in)
	for i, e := range out {
		if e.CapReached {
			t.Errorf("out[%d] Status=%q: CapReached=true, want false (cap irrelevante en este status)", i, e.Status)
		}
	}
}

// ==================================================================
// Adopt mode (columna opt-in "adopt")
// ==================================================================

// TestBuildData_AdoptOff_FiltersAdoptEntities: con adopt=false (toggle off),
// las entities con Status="adopt" se filtran del snapshot antes del group-by
// y la columna "adopt" NO aparece entre las Columns resultantes. Cubre el
// contrato "default sin cambio" — el dash se ve como pre-feature cuando el
// usuario no opta in.
func TestBuildData_AdoptOff_FiltersAdoptEntities(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			{Kind: KindPR, PRNumber: 301, Status: "adopt"},
			{Kind: KindFused, IssueNumber: 500, PRNumber: 302, Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	data := s.buildData(false)

	// Ninguna columna "adopt" en la salida.
	for _, c := range data.Columns {
		if c.Key == "adopt" {
			t.Errorf("adopt OFF: debería NO renderear columna 'adopt'; got %d entries", len(c.Entities))
		}
	}
	// La columna "plan" sigue teniendo el issue #42 (sanity check).
	var plan *columnData
	for i := range data.Columns {
		if data.Columns[i].Key == "plan" {
			plan = &data.Columns[i]
		}
	}
	if plan == nil || len(plan.Entities) != 1 {
		t.Errorf("adopt OFF: columna 'plan' debería tener issue #42; got %+v", plan)
	}
	if data.Adopt {
		t.Errorf("data.Adopt: got true want false")
	}
}

// TestBuildData_AdoptOn_KeepsAdoptEntities: con adopt=true, las entities
// adopt se conservan y se agrupan en la columna "adopt".
func TestBuildData_AdoptOn_KeepsAdoptEntities(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "plan"},
			{Kind: KindPR, PRNumber: 301, Status: "adopt", PRTitle: "orphan"},
			{Kind: KindFused, IssueNumber: 500, PRNumber: 302, Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	data := s.buildData(true)

	var adopt *columnData
	for i := range data.Columns {
		if data.Columns[i].Key == "adopt" {
			adopt = &data.Columns[i]
		}
	}
	if adopt == nil {
		t.Fatalf("adopt ON: falta columna 'adopt' en Columns=%+v", data.Columns)
	}
	if len(adopt.Entities) != 2 {
		t.Errorf("adopt ON: got %d entries en columna adopt, want 2", len(adopt.Entities))
	}
	if !data.Adopt {
		t.Errorf("data.Adopt: got false want true")
	}
}

// TestAdoptHandler_PropagatesQueryParam: GET / con ?adopt=1 renderea la
// columna adopt; sin el param, no.
func TestAdoptHandler_PropagatesQueryParam(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan PR", Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Sin adopt → la columna no aparece. Buscamos <div class="col"
	// data-status="adopt"> (render efectivo), no el selector CSS que
	// contiene la misma string literal.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `<div class="col" data-status="adopt"`) {
		t.Errorf("adopt OFF: index no debería renderear la columna <div class=\"col\" data-status=\"adopt\">")
	}
	if strings.Contains(string(body), "orphan PR") {
		t.Errorf("adopt OFF: index no debería contener el PR huérfano")
	}

	// Con ?adopt=1 → la columna aparece.
	resp2, err := http.Get(ts.URL + "/?adopt=1")
	if err != nil {
		t.Fatalf("GET /?adopt=1: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), `<div class="col" data-status="adopt"`) {
		t.Errorf("adopt ON: index debería renderear <div class=\"col\" data-status=\"adopt\">; body head=%s", string(body2[:min(600, len(body2))]))
	}
	if !strings.Contains(string(body2), "orphan PR") {
		t.Errorf("adopt ON: index debería contener el título del PR huérfano")
	}
	// hx-get del board también debe llevar ?adopt=1 para que los polls
	// subsiguientes sigan trayendo la columna.
	if !strings.Contains(string(body2), `hx-get="/board?adopt=1"`) {
		t.Errorf("adopt ON: hx-get del board debería incluir ?adopt=1")
	}
}

// TestAction_AdoptRejectsOutOfSetFlows: sobre una entity adopt, los flows
// fuera del set fijo por kind se rechazan con 409 (el gate los marca
// Available=false). El UI oculta los botones, pero un cliente manipulado
// podría intentarlo.
//
// Sets (ver adoptGates en preflight.go):
//   - KindPR (adopt):    validate
//   - KindFused (adopt): validate
//
// Cualquier otro flow para esos kinds → 409 con razón "no aplica desde
// adopt — usá explore/execute/validate ...".
func TestAction_AdoptRejectsOutOfSetFlows(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan", Status: "adopt"},
			{Kind: KindFused, IssueNumber: 500, PRNumber: 302, Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	// close ahora también está fuera del set para adopt (post abril 2026):
	// la decisión humana de cerrar/mergear vive en el state machine real,
	// no en la puerta de entrada.
	for _, flow := range []string{"iterate", "execute", "explore", "close"} {
		// Adopt KindPR: data-entity=PRNumber.
		resp, err := http.Post(ts.URL+"/action/"+flow+"/301", "", nil)
		if err != nil {
			t.Fatalf("POST adopt+%s: %v", flow, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("adopt+%s: got %d want 409", flow, resp.StatusCode)
		}
		// Adopt KindFused: data-entity=IssueNumber.
		resp2, err := http.Post(ts.URL+"/action/"+flow+"/500", "", nil)
		if err != nil {
			t.Fatalf("POST adopt-fused+%s: %v", flow, err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusConflict {
			t.Errorf("adopt-fused+%s: got %d want 409", flow, resp2.StatusCode)
		}
	}
	if fr.count() != 0 {
		t.Errorf("runner should not be called for out-of-set flows; got %d calls", fr.count())
	}
}

// TestAction_AdoptAllowsValidate: validate SÍ pasa sobre adopt para KindPR
// y KindFused (set fijo de adopt). Para KindPR, TargetRef=PRNumber
// (resolveTargetRef).
func TestAction_AdoptAllowsValidate(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan", Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/validate/301", "", nil)
	if err != nil {
		t.Fatalf("POST validate/301: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if fr.count() != 1 {
		t.Fatalf("runner calls: got %d want 1", fr.count())
	}
	got := fr.last()
	if got.Flow != "validate" || got.TargetRef != 301 || got.EntityKey != 301 {
		t.Errorf("got %+v want {validate target=301 key=301}", got)
	}
}

// TestDrawerAdopt_RendersOnlySetButtons: el drawer de una entidad adopt
// solo debe ofrecer los botones del set fijo por kind. Para KindPR =
// validate. iterate/execute/explore/close NO aparecen.
func TestDrawerAdopt_RendersOnlySetButtons(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan", Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/301")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)

	if !strings.Contains(got, `hx-post="/action/validate/301"`) {
		t.Errorf("adopt drawer: missing validate button")
	}
	// iterate/execute/explore/close no deben aparecer en adopt — fuera del set.
	for _, forbidden := range []string{
		`hx-post="/action/iterate/301"`,
		`hx-post="/action/execute/301"`,
		`hx-post="/action/explore/301"`,
		`hx-post="/action/close/301"`,
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("adopt drawer: encontré %s (no debería — adopt es puerta de entrada, close vive en el state machine)", forbidden)
		}
	}
	// Ref al PR sí, al issue no.
	if !strings.Contains(got, `href="https://github.com/demo/che/pull/301"`) {
		t.Errorf("adopt drawer: missing link al PR")
	}
	if strings.Contains(got, "#0") {
		t.Errorf("adopt drawer: no debería renderear '#0' como issue fantasma")
	}
}

// TestDrawerAdopt_FusedRendersOnlyValidate: drawer de KindFused adopt =
// solo botón validate (set fijo de adopt para fused). iterate/execute/
// explore/close NO aparecen.
func TestDrawerAdopt_FusedRendersOnlyValidate(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindFused, IssueNumber: 500, PRNumber: 302, IssueTitle: "issue", PRTitle: "fused PR", Status: "adopt"},
		},
	}}
	s := NewServer(src, "repo", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/500")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)

	if !strings.Contains(got, `hx-post="/action/validate/500"`) {
		t.Errorf("fused adopt drawer: missing validate button")
	}
	for _, forbidden := range []string{
		`hx-post="/action/iterate/500"`,
		`hx-post="/action/execute/500"`,
		`hx-post="/action/explore/500"`,
		`hx-post="/action/close/500"`,
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("fused adopt drawer: encontré %s (fuera del set)", forbidden)
		}
	}
}

// TestDrawerKindPR_PostAdoptValidated: una vez que validate aplicó
// che:validated al PR (validate.go:530-541, commit 955313e), el drawer
// debe ofrecer iterate + validate + close — no quedarse en el set
// fijo de adopt. Cubre el bug de abril 2026 donde KindPR post-adopt
// seguía mostrando solo validate y el card no podía progresar.
func TestDrawerKindPR_PostAdoptValidated(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindPR, PRNumber: 301, PRTitle: "orphan", Status: "validated", PRVerdict: "changes-requested"},
		},
	}}
	s := NewServer(src, "repo", 15)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/301")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)

	for _, want := range []string{
		`hx-post="/action/iterate/301"`,
		`hx-post="/action/validate/301"`,
		`hx-post="/action/close/301"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("KindPR validated drawer: missing %s", want)
		}
	}
	// El chip "adopt" naranja NO debe aparecer una vez que el PR pasó
	// a validated — antes se renderizaba hardcodeado para todo KindPR.
	if strings.Contains(got, `>adopt</span>`) {
		t.Errorf("KindPR validated drawer: chip 'adopt' no debería aparecer post-adopt")
	}
}

func TestBuildData_CustomPipelineUsesStepColumns(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "build", StateStep: "build", IssueTitle: "compile"},
		},
	}}
	s := NewServer(src, "repo", 15)
	s.pipeline = pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{
		{Name: "spec", Agents: []string{"claude-sonnet"}},
		{Name: "build", Agents: []string{"claude-opus"}},
		{Name: "ship", Agents: []string{"claude-haiku"}},
	}}

	data := s.buildData(false)
	if data.ColCount != 3 {
		t.Fatalf("ColCount: got %d want 3", data.ColCount)
	}
	want := []string{"spec", "build", "ship"}
	for i, key := range want {
		if data.Columns[i].Key != key {
			t.Fatalf("Columns[%d].Key: got %q want %q (all=%+v)", i, data.Columns[i].Key, key, data.Columns)
		}
	}
	if len(data.Columns[1].Entities) != 1 || data.Columns[1].Entities[0].IssueNumber != 42 {
		t.Fatalf("build column entities: got %+v", data.Columns[1].Entities)
	}
}

func TestDrawer_CustomPipelineRendersStepActions(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "build", StateStep: "build", IssueTitle: "compile"},
		},
	}}
	s := NewServer(src, "repo", 15)
	s.pipeline = pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{
		{Name: "spec", Agents: []string{"claude-sonnet"}},
		{Name: "build", Agents: []string{"claude-opus", "claude-haiku"}, Aggregator: pipeline.AggregatorFirstBlocker, Comment: "compile checks"},
	}}
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/drawer/42")
	if err != nil {
		t.Fatalf("GET drawer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)
	for _, want := range []string{
		`hx-post="/action/run/build/42"`,
		`claude-opus, claude-haiku`,
		`compile checks`,
		`che run --manual --from &lt;step&gt;`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("drawer missing %q\nbody=%s", want, got)
		}
	}
}

func TestActionRunFrom_CustomPipelineCallsDynamicRun(t *testing.T) {
	src := &fixedSource{snap: Snapshot{
		LastOK: time.Now(),
		NWO:    "demo/che",
		Entities: []Entity{
			{Kind: KindIssue, IssueNumber: 42, Status: "build", StateStep: "build", IssueTitle: "compile"},
		},
	}}
	s := NewServer(src, "repo", 15)
	s.pipeline = pipeline.Pipeline{Version: pipeline.CurrentVersion, Steps: []pipeline.Step{{Name: "build", Agents: []string{"claude-opus"}}}}
	fr := &fakeRunner{}
	s.runAction = fr.run
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/action/run/build/42", "", nil)
	if err != nil {
		t.Fatalf("POST run/build: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	got := fr.last()
	if got.Flow != "run#:build" || got.TargetRef != 42 || got.EntityKey != 42 {
		t.Fatalf("runner call: got %+v want flow=run#:build target=42 key=42", got)
	}
}

// TestColumn_Adopt: Entity.Column() devuelve "adopt" cuando Status=="adopt".
// Cubierto también en model_test pero duplicado como sanity del dispatcher.
func TestColumn_Adopt(t *testing.T) {
	e := Entity{Kind: KindPR, PRNumber: 42, Status: "adopt"}
	if got := e.Column(); got != "adopt" {
		t.Errorf("Column(): got %q want adopt", got)
	}
}
