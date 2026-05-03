package labels

import (
	"reflect"
	"testing"

	"github.com/chichex/che/internal/pipeline"
)

// TestExpectedForPipeline_DefaultGolden bit-perfect el set para
// pipeline.Default(). Si Default cambia (se agrega/saca un step), este
// test rompe y obliga a regenerar.
func TestExpectedForPipeline_DefaultGolden(t *testing.T) {
	got := ExpectedForPipeline(pipeline.Default())

	want := []string{
		"che:locked",
		"che:state:applying:close",
		"che:state:applying:execute",
		"che:state:applying:explore",
		"che:state:applying:idea",
		"che:state:applying:validate_issue",
		"che:state:applying:validate_pr",
		"che:state:close",
		"che:state:execute",
		"che:state:explore",
		"che:state:idea",
		"che:state:validate_issue",
		"che:state:validate_pr",
		"ct:plan",
		"plan-validated:approve",
		"plan-validated:changes-requested",
		"plan-validated:needs-human",
		"validated:approve",
		"validated:changes-requested",
		"validated:needs-human",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpectedForPipeline(Default()) drift\n--- got ---\n%v\n--- want ---\n%v", got, want)
	}
}

// TestExpectedForPipeline_IncludesAllVerdicts: explícitamente, el set
// incluye los 6 verdicts (3 plan-validated + 3 validated).
func TestExpectedForPipeline_IncludesAllVerdicts(t *testing.T) {
	got := ExpectedForPipeline(pipeline.Default())
	gotSet := map[string]bool{}
	for _, l := range got {
		gotSet[l] = true
	}
	wantInclude := append([]string{}, AllValidated...)
	wantInclude = append(wantInclude, AllPlanValidated...)
	for _, l := range wantInclude {
		if !gotSet[l] {
			t.Errorf("ExpectedForPipeline missing verdict label %q", l)
		}
	}
}

// TestExpectedForPipeline_OrderIsAlphabetic: el orden del slice debe ser
// alfabético (deduplicado en map → ordenado para estabilidad).
func TestExpectedForPipeline_OrderIsAlphabetic(t *testing.T) {
	got := ExpectedForPipeline(pipeline.Default())
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("orden roto en idx %d: %q !< %q", i, got[i-1], got[i])
		}
	}
}

// TestExpectedForPipeline_NoLockLabels: los labels dinámicos
// `che:lock:<ts>:...` no se crean ahora — son por-run, no por-pipeline.
// Cubrir explícitamente para que un refactor que los agregue al set rompa
// este guard.
func TestExpectedForPipeline_NoLockLabels(t *testing.T) {
	got := ExpectedForPipeline(pipeline.Default())
	for _, l := range got {
		if len(l) > len("che:lock:") && l[:len("che:lock:")] == "che:lock:" {
			t.Errorf("label %q no debería estar en ExpectedForPipeline (lock dinámico)", l)
		}
	}
}

// TestStyleFor_KnownLabelsHaveColor: para cada label conocido del
// pipeline default verificamos que styleFor devuelve un color (no
// vacío) — si no, init-labels va a crear el label sin estilo y el
// dashboard queda gris.
//
// Verificamos también colores específicos para los grupos (applying,
// approve/changes-requested/needs-human). Si la tabla cambia (ej. el
// equipo elige otra paleta), este test rompe deliberadamente para
// forzar update.
func TestStyleFor_KnownLabelsHaveColor(t *testing.T) {
	cases := []struct {
		label     string
		wantColor string
	}{
		{"che:state:idea", "cccccc"},
		{"che:state:explore", "1d76db"},
		{"che:state:execute", "1d76db"},
		{"che:state:close", "0e8a16"},
		{"che:state:applying:explore", "fbca04"},
		{"che:state:applying:execute", "fbca04"},
		{"che:state:applying:close", "fbca04"},
		{"che:state:validate_issue", "1d76db"},
		{"che:state:validate_pr", "1d76db"},
		{"validated:approve", "0e8a16"},
		{"plan-validated:approve", "0e8a16"},
		{"validated:changes-requested", "f6a51e"},
		{"plan-validated:changes-requested", "f6a51e"},
		{"validated:needs-human", "b60205"},
		{"plan-validated:needs-human", "b60205"},
		{CtPlan, "5319e7"},
		{CheLocked, "d93f0b"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := styleFor(c.label)
			if got.Color != c.wantColor {
				t.Errorf("styleFor(%q).Color = %q, want %q", c.label, got.Color, c.wantColor)
			}
			if got.Description == "" {
				t.Errorf("styleFor(%q).Description está vacío — todo label conocido debería describirse", c.label)
			}
		})
	}
}

// TestStyleFor_UnknownReturnsZero: un label no listado devuelve
// LabelStyle{} (color y description vacíos). EnsureWithStyle interpreta
// eso como "no pisar el estilo manual del repo" — backward-compat.
func TestStyleFor_UnknownReturnsZero(t *testing.T) {
	got := styleFor("type:bug")
	if got.Color != "" || got.Description != "" {
		t.Errorf("styleFor(unknown).Color/Description debería ser vacío, got %+v", got)
	}
}
