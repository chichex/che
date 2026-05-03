package e2e_test

import (
	"strings"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestInitLabels_CommandExists fuerza la existencia del subcomando
// `init-labels` y de su help.
func TestInitLabels_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("init-labels", "--help")
	harness.AssertContains(t, out, "init-labels")
	harness.AssertContains(t, out, "labels")
}

// TestInitLabels_DryRun lista los labels esperados sin tocar gh. Como el
// repo no tiene `.che/pipelines/`, resuelve al built-in.
func TestInitLabels_DryRun(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	// init-labels llama a `git rev-parse --show-toplevel` para detectar
	// el repo root. Stubeamos con un path arbitrario — la lógica que sigue
	// (NewManager + Resolve) tolera que no haya `.che/` ahí.
	env.ExpectGit(`^rev-parse --show-toplevel`).RespondStdout("/tmp/fake-repo\n", 0)
	out := env.MustRun("init-labels", "--dry-run")
	for _, want := range []string{
		"pipeline:",
		"labels esperados",
		"che:state:idea",
		"validated:approve",
		"plan-validated:approve",
		"ct:plan",
		"[dry-run]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output sin %q:\n%s", want, out)
		}
	}
}

// TestInitLabels_RealRun corre la creación real con gh stubeado: cada
// `gh label create` debe matchear y responder OK; el output final debe
// indicar éxito.
func TestInitLabels_RealRun(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.ExpectGit(`^rev-parse --show-toplevel`).RespondStdout("/tmp/fake-repo\n", 0)
	// `gh label create <name> --force` se llama una vez por label.
	env.ExpectGh(`^label create `).RespondStdout("", 0)

	out := env.MustRun("init-labels")
	harness.AssertContains(t, out, "ok:")
	harness.AssertContains(t, out, "asegurado(s)")

	// Verificamos que se intentó crear los 9 estados v2 + 6 verdicts +
	// ct:plan + che:locked = 17 (estado y aplicante por step × 6 steps de
	// Default + 6 verdicts + ct:plan + che:locked = 19).
	inv := env.Invocations()
	calls := inv.FindCalls("gh", "label", "create")
	// El número exacto depende de Default(); chequeamos al menos una
	// muestra representativa de las 4 familias.
	mustHave := []string{
		"che:state:idea",
		"che:state:applying:execute",
		"validated:approve",
		"plan-validated:approve",
		"ct:plan",
		"che:locked",
	}
	joined := ""
	for _, c := range calls {
		joined += " " + strings.Join(c.Args, " ")
	}
	for _, expected := range mustHave {
		if !strings.Contains(joined, expected) {
			t.Errorf("expected gh label create %s; calls=%v", expected, calls)
		}
	}
}
