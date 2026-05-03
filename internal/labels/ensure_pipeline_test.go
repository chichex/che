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
