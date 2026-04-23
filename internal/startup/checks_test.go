package startup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner es un Runner inyectable para tests. Cada call matchea por
// un substring del comando completo (name + args concatenados con
// espacios). Si no matchea ningún script, devuelve un error explícito —
// los tests fallan loudly si invocan algo no scripted.
type fakeRunner struct {
	mu      sync.Mutex
	scripts []fakeScript
	calls   []string
	// blockUntil bloquea TODAS las calls hasta que este channel se
	// cierre. Útil para simular timeouts.
	blockUntil <-chan struct{}
}

type fakeScript struct {
	matchSubstr string
	stdout      []byte
	err         error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	scripts := append([]fakeScript(nil), f.scripts...)
	block := f.blockUntil
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	for _, s := range scripts {
		if strings.Contains(full, s.matchSubstr) {
			return s.stdout, s.err
		}
	}
	return nil, fmt.Errorf("fakeRunner: no matcher for %q", full)
}

func TestCheckMigrateLabels_DetectaLabelsViejos(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "label list",
			stdout: []byte(`[
				{"name":"status:idea"},
				{"name":"status:plan"},
				{"name":"status:foobar"}
			]`),
		}},
	}
	res := checkMigrateLabels(context.Background(), r)
	if !res.Triggered {
		t.Fatalf("debería triggerear con status:idea presente")
	}
	if len(res.OldLabels) != 2 {
		t.Errorf("debería encontrar 2 (idea, plan), got %v", res.OldLabels)
	}
}

func TestCheckMigrateLabels_NoTriggereaSinLabelsViejos(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "label list",
			stdout:      []byte(`[]`),
		}},
	}
	res := checkMigrateLabels(context.Background(), r)
	if res.Triggered {
		t.Errorf("no debería triggerear con lista vacía")
	}
	if res.Err != nil {
		t.Errorf("no debería haber error, got %v", res.Err)
	}
}

func TestCheckMigrateLabels_GhFallaEsSilencioso(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "label list",
			err:         errors.New("gh: not authenticated"),
		}},
	}
	res := checkMigrateLabels(context.Background(), r)
	if res.Triggered {
		t.Errorf("error de gh no debería triggerear el check")
	}
	if res.Err == nil {
		t.Errorf("Err debería propagarse para que el caller pueda loggear")
	}
}

func TestCheckVersion_SkipDev(t *testing.T) {
	r := &fakeRunner{}
	res := checkVersion(context.Background(), r, "dev")
	if res.Triggered {
		t.Errorf("dev nunca debería triggerear (build local)")
	}
	if len(r.calls) != 0 {
		t.Errorf("dev no debería llamar a gh, got calls=%v", r.calls)
	}
}

func TestCheckVersion_SkipVersionVacia(t *testing.T) {
	r := &fakeRunner{}
	res := checkVersion(context.Background(), r, "")
	if res.Triggered {
		t.Errorf("version vacía no debería triggerear")
	}
	if len(r.calls) != 0 {
		t.Errorf("version vacía no debería llamar a gh")
	}
}

func TestCheckVersion_TriggereaSiHayMasNueva(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "release view",
			stdout:      []byte("v0.0.50\n"),
		}},
	}
	res := checkVersion(context.Background(), r, "0.0.49")
	if !res.Triggered {
		t.Errorf("0.0.49 vs v0.0.50 debería triggerear")
	}
	if res.LatestVersion != "v0.0.50" {
		t.Errorf("LatestVersion mal: got %q", res.LatestVersion)
	}
}

func TestCheckVersion_NoTriggereaSiCoinciden(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "release view",
			stdout:      []byte("v0.0.49"),
		}},
	}
	res := checkVersion(context.Background(), r, "0.0.49")
	if res.Triggered {
		t.Errorf("0.0.49 == v0.0.49 (normalizado) no debería triggerear")
	}
}

func TestCheckVersion_GhFallaEsSilencioso(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{{
			matchSubstr: "release view",
			err:         errors.New("gh: rate limited"),
		}},
	}
	res := checkVersion(context.Background(), r, "0.0.49")
	if res.Triggered {
		t.Errorf("error de gh no debería triggerear el check")
	}
}

func TestCheckLocks_FiltraPorAntiguedad(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`[
		{"number":42,"title":"viejo","updatedAt":%q,"isPullRequest":false},
		{"number":55,"title":"reciente","updatedAt":%q,"isPullRequest":true}
	]`, old, recent)
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "repo view", stdout: []byte("chichex/che\n")},
			{matchSubstr: "search issues", stdout: []byte(body)},
		},
	}
	res := checkLocks(context.Background(), r)
	if !res.Triggered {
		t.Fatalf("debería triggerear (1 stale)")
	}
	if len(res.Locks) != 1 {
		t.Fatalf("debería haber 1 lock stale, got %d", len(res.Locks))
	}
	if res.Locks[0].Number != 42 {
		t.Errorf("debería ser #42 (el viejo), got #%d", res.Locks[0].Number)
	}
}

func TestCheckLocks_NoTriggereaSinStale(t *testing.T) {
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`[
		{"number":42,"title":"reciente","updatedAt":%q,"isPullRequest":false}
	]`, recent)
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "repo view", stdout: []byte("chichex/che")},
			{matchSubstr: "search issues", stdout: []byte(body)},
		},
	}
	res := checkLocks(context.Background(), r)
	if res.Triggered {
		t.Errorf("locks recientes no deberían triggerear")
	}
}

func TestCheckLocks_GhFallaEsSilencioso(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "repo view", stdout: []byte("chichex/che")},
			{matchSubstr: "search issues", err: errors.New("gh down")},
		},
	}
	res := checkLocks(context.Background(), r)
	if res.Triggered {
		t.Errorf("error de gh no debería triggerear")
	}
}

func TestCheckLocks_FallaEnRepoView(t *testing.T) {
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "repo view", err: errors.New("gh: no auth")},
		},
	}
	res := checkLocks(context.Background(), r)
	if res.Triggered {
		t.Errorf("falla en repo view no debería triggerear")
	}
	if res.Err == nil {
		t.Errorf("Err debería propagarse")
	}
}

func TestRunChecks_OrdenCanonico(t *testing.T) {
	repo := makeRepo(t)
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "label list", stdout: []byte(`[]`)},
			{matchSubstr: "release view", stdout: []byte("v0.0.49")},
			{matchSubstr: "repo view", stdout: []byte("chichex/che")},
			{matchSubstr: "search issues", stdout: []byte(`[]`)},
		},
	}
	results := RunChecks(context.Background(), Options{
		RepoRoot:       repo,
		CurrentVersion: "0.0.49",
		Runner:         r,
		Timeout:        2 * time.Second,
	})
	if len(results) != 3 {
		t.Fatalf("esperaba 3 resultados, got %d", len(results))
	}
	wantNames := []string{CheckMigrateLabels, CheckVersion, CheckLocks}
	for i, name := range wantNames {
		if results[i].Name != name {
			t.Errorf("results[%d].Name: got %q, want %q", i, results[i].Name, name)
		}
	}
}

func TestRunChecks_SinGitDirDevuelveNil(t *testing.T) {
	dir := t.TempDir() // sin .git
	results := RunChecks(context.Background(), Options{
		RepoRoot:       dir,
		CurrentVersion: "0.0.49",
		Runner:         &fakeRunner{},
	})
	if results != nil {
		t.Errorf("sin .git debería devolver nil, got %v", results)
	}
}

func TestRunChecks_RespetaSkipped(t *testing.T) {
	repo := makeRepo(t)
	if err := MarkSkipped(repo, CheckVersion); err != nil {
		t.Fatalf("MarkSkipped: %v", err)
	}
	r := &fakeRunner{
		scripts: []fakeScript{
			{matchSubstr: "label list", stdout: []byte(`[]`)},
			{matchSubstr: "repo view", stdout: []byte("chichex/che")},
			{matchSubstr: "search issues", stdout: []byte(`[]`)},
		},
	}
	results := RunChecks(context.Background(), Options{
		RepoRoot:       repo,
		CurrentVersion: "0.0.49",
		Runner:         r,
		Timeout:        2 * time.Second,
	})
	// Buscamos que la call al check de version no se haya hecho.
	for _, c := range r.calls {
		if strings.Contains(c, "release view") {
			t.Errorf("version skipeado no debería llamar a 'release view': %v", r.calls)
		}
	}
	// El slot de version sigue ahí pero como no-triggered.
	for _, res := range results {
		if res.Name == CheckVersion && res.Triggered {
			t.Errorf("check skipeado no debería triggerear")
		}
	}
}

func TestRunChecks_TimeoutNoRompe(t *testing.T) {
	repo := makeRepo(t)
	block := make(chan struct{})
	defer close(block)
	r := &fakeRunner{blockUntil: block}
	results := RunChecks(context.Background(), Options{
		RepoRoot:       repo,
		CurrentVersion: "0.0.49",
		Runner:         r,
		Timeout:        50 * time.Millisecond,
	})
	// Esperamos retornar dentro del timeout sin panic. Los resultados
	// pueden estar vacíos o parciales — lo importante es que no rompa.
	if len(results) != 3 {
		t.Errorf("debería devolver 3 slots aunque haya timeout, got %d", len(results))
	}
	for _, res := range results {
		if res.Triggered {
			t.Errorf("nada debería triggerear si todo timeouteó: %+v", res)
		}
	}
}

func TestAnyTriggered(t *testing.T) {
	if AnyTriggered(nil) {
		t.Errorf("nil no debería triggerear")
	}
	if AnyTriggered([]Result{{Triggered: false}, {Triggered: false}}) {
		t.Errorf("ningún triggered → false")
	}
	if !AnyTriggered([]Result{{Triggered: false}, {Triggered: true}}) {
		t.Errorf("uno triggered → true")
	}
}
