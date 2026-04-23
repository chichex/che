package dash

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if !strings.Contains(got, `hx-trigger="every 15s"`) {
		t.Errorf("body missing hx-trigger for 15s default poll")
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
	// El partial NO debería incluir el wrapper <div class="dash-board">,
	// solo su contenido (chip + columnas). Ese wrapper es persistente.
	if strings.Contains(got, `class="dash-board"`) {
		t.Errorf("/board should not include the .dash-board wrapper; got: %s", got)
	}
}
