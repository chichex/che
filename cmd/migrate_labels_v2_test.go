package cmd

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/chichex/che/internal/pipelinelabels"
)

// TestV2MigrationPairs valida el contrato del helper: los 9 pares v1 → v2
// canónicos en el orden del embudo (idea → close), sin duplicados de V1 ni
// de V2. Es el espejo de TestMigrationPairs para v1→v2.
func TestV2MigrationPairs(t *testing.T) {
	pairs := v2MigrationPairs()

	want := []v2Pair{
		{V1: v1CheIdea, V2: pipelinelabels.StateIdea},
		{V1: v1ChePlanning, V2: pipelinelabels.StateApplyingExplore},
		{V1: v1ChePlan, V2: pipelinelabels.StateExplore},
		{V1: v1CheExecuting, V2: pipelinelabels.StateApplyingExecute},
		{V1: v1CheExecuted, V2: pipelinelabels.StateExecute},
		{V1: v1CheValidating, V2: pipelinelabels.StateApplyingValidatePR},
		{V1: v1CheValidated, V2: pipelinelabels.StateValidatePR},
		{V1: v1CheClosing, V2: pipelinelabels.StateApplyingClose},
		{V1: v1CheClosed, V2: pipelinelabels.StateClose},
	}
	if len(pairs) != len(want) {
		t.Fatalf("len: got %d, want %d", len(pairs), len(want))
	}
	for i, w := range want {
		if pairs[i] != w {
			t.Errorf("pair[%d]: got %+v, want %+v", i, pairs[i], w)
		}
	}
	seenV1 := map[string]bool{}
	seenV2 := map[string]bool{}
	for _, p := range pairs {
		if seenV1[p.V1] {
			t.Errorf("duplicate V1: %s", p.V1)
		}
		if seenV2[p.V2] {
			t.Errorf("duplicate V2: %s", p.V2)
		}
		seenV1[p.V1] = true
		seenV2[p.V2] = true
	}
}

// fakeMigrate captura las llamadas a applyMigrationFn para que los tests
// puedan asserts sin shell-out a gh.
type fakeMigrate struct {
	mu    sync.Mutex
	calls []fakeMigrateCall
}

type fakeMigrateCall struct {
	IssueNumber int
	V1, V2      string
}

func (f *fakeMigrate) apply(repo string, issueNumber int, v1, v2 string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeMigrateCall{issueNumber, v1, v2})
	return nil
}

// withFakes instala fakes para listIssuesFn y applyMigrationFn por la
// duración del test, restaurando los originales al volver. Devuelve el
// recorder para asserts.
func withFakes(t *testing.T, issues []migrateIssue) *fakeMigrate {
	t.Helper()
	prevList := listIssuesFn
	prevApply := applyMigrationFn
	rec := &fakeMigrate{}
	listIssuesFn = func(repo string) ([]migrateIssue, error) {
		return issues, nil
	}
	applyMigrationFn = rec.apply
	t.Cleanup(func() {
		listIssuesFn = prevList
		applyMigrationFn = prevApply
	})
	return rec
}

// mkIssue es un constructor breve para reducir verbosidad en los casos de
// test. labels acepta nombres directos (no requiere wrap struct).
func mkIssue(num int, title string, lbls ...string) migrateIssue {
	out := migrateIssue{Number: num, Title: title}
	for _, l := range lbls {
		out.Labels = append(out.Labels, migrateLbl{Name: l})
	}
	return out
}

// TestMigrateV2_NineLabels_OneToOne: un issue por cada uno de los 9 estados
// v1 → genera los 9 mappings esperados, idempotente segundo run.
func TestMigrateV2_NineLabels_OneToOne(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(1, "idea", v1CheIdea),
		mkIssue(2, "planning", v1ChePlanning),
		mkIssue(3, "plan", v1ChePlan),
		mkIssue(4, "executing", v1CheExecuting),
		mkIssue(5, "executed", v1CheExecuted),
		mkIssue(6, "validating", v1CheValidating),
		mkIssue(7, "validated", v1CheValidated),
		mkIssue(8, "closing", v1CheClosing),
		mkIssue(9, "closed", v1CheClosed),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false /* dryRun */); err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, buf.String())
	}
	if len(rec.calls) != 9 {
		t.Fatalf("expected 9 apply calls, got %d", len(rec.calls))
	}
	// Mapping correcto por número de issue.
	want := map[int][2]string{
		1: {v1CheIdea, pipelinelabels.StateIdea},
		2: {v1ChePlanning, pipelinelabels.StateApplyingExplore},
		3: {v1ChePlan, pipelinelabels.StateExplore},
		4: {v1CheExecuting, pipelinelabels.StateApplyingExecute},
		5: {v1CheExecuted, pipelinelabels.StateExecute},
		6: {v1CheValidating, pipelinelabels.StateApplyingValidatePR},
		7: {v1CheValidated, pipelinelabels.StateValidatePR},
		8: {v1CheClosing, pipelinelabels.StateApplyingClose},
		9: {v1CheClosed, pipelinelabels.StateClose},
	}
	for _, c := range rec.calls {
		exp, ok := want[c.IssueNumber]
		if !ok {
			t.Errorf("unexpected call for issue #%d", c.IssueNumber)
			continue
		}
		if c.V1 != exp[0] || c.V2 != exp[1] {
			t.Errorf("issue #%d: got %s→%s, want %s→%s", c.IssueNumber, c.V1, c.V2, exp[0], exp[1])
		}
	}
	// Caveat de validating debería aparecer en el output.
	if !strings.Contains(buf.String(), "validating") || !strings.Contains(buf.String(), "warn:") {
		t.Errorf("expected warning about che:validating mapping, got:\n%s", buf.String())
	}
	// Summary correcto.
	if !strings.Contains(buf.String(), "9 modified") {
		t.Errorf("expected '9 modified' summary, got:\n%s", buf.String())
	}
}

// TestMigrateV2_AlreadyV2_Idempotent: issue ya migrado (solo v2 labels) →
// skipeado silenciosamente (sin call al applier, sin fila en output).
func TestMigrateV2_AlreadyV2_Idempotent(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(1, "ya migrado", "ct:plan", pipelinelabels.StateExplore),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected 0 apply calls (issue ya migrado), got %d", len(rec.calls))
	}
	if !strings.Contains(buf.String(), "0 modified") {
		t.Errorf("expected '0 modified' summary, got:\n%s", buf.String())
	}
}

// TestMigrateV2_Mixed_SkipsAndWarns: issue con v1 y v2 simultáneos →
// skipeado con warning, no se aplica nada.
func TestMigrateV2_Mixed_SkipsAndWarns(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(42, "mixto", "ct:plan", v1ChePlan, pipelinelabels.StateExplore),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 apply calls (mixto skipeado), got %d", len(rec.calls))
	}
	if !strings.Contains(buf.String(), "skip mixed") {
		t.Errorf("expected 'skip mixed' line, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "1 skipped") {
		t.Errorf("expected '1 skipped (mixed v1+v2)' summary, got:\n%s", buf.String())
	}
}

// TestMigrateV2_NoCheLabels_Skip: issue sin ningún label che:* → skipeado
// silencioso (sin fila en output), no apply.
func TestMigrateV2_NoCheLabels_Skip(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(99, "sin che:*", "ct:plan", "type:feature"),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected 0 apply calls (sin che:*), got %d", len(rec.calls))
	}
	// El issue NO debería aparecer en output (skip silencioso).
	if strings.Contains(buf.String(), "#99") {
		t.Errorf("issue sin che:* no debería aparecer en output, got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "0 modified") {
		t.Errorf("expected '0 modified' summary, got:\n%s", buf.String())
	}
}

// TestMigrateV2_DryRun_NoApply: dry-run no llama a applyMigrationFn ni
// modifica nada, solo lista qué haría.
func TestMigrateV2_DryRun_NoApply(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(1, "idea legacy", v1CheIdea),
		mkIssue(2, "plan legacy", v1ChePlan),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", true /* dryRun */); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 apply calls (dry-run), got %d", len(rec.calls))
	}
	out := buf.String()
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("expected '[dry-run]' marker, got:\n%s", out)
	}
	if !strings.Contains(out, v1CheIdea+" → "+pipelinelabels.StateIdea) {
		t.Errorf("expected idea mapping in dry-run, got:\n%s", out)
	}
	if !strings.Contains(out, v1ChePlan+" → "+pipelinelabels.StateExplore) {
		t.Errorf("expected plan mapping in dry-run, got:\n%s", out)
	}
}

// TestMigrateV2_Mixed_DoesNotShortCircuit: un issue mixto en el medio del
// scan no debe abortar el procesamiento del resto.
func TestMigrateV2_Mixed_DoesNotShortCircuit(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(1, "ok", v1CheIdea),
		mkIssue(2, "mixto", v1ChePlan, pipelinelabels.StateExplore),
		mkIssue(3, "ok2", v1CheExecuted),
	}
	rec := withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("expected 2 apply calls (issue mixto skipeado, otros 2 procesados), got %d", len(rec.calls))
	}
	if !strings.Contains(buf.String(), "2 modified") {
		t.Errorf("expected '2 modified' summary, got:\n%s", buf.String())
	}
}

// TestMigrateV2_OutputIncludesIssueTitleAndMappings: el output estructurado
// nombra cada issue tocado con su número y título, y cada mapping aplicado.
func TestMigrateV2_OutputIncludesIssueTitleAndMappings(t *testing.T) {
	issues := []migrateIssue{
		mkIssue(7, "Mi issue cool", v1CheExecuted),
	}
	withFakes(t, issues)
	var buf bytes.Buffer
	if err := runMigrateLabelsV2(&buf, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "#7") {
		t.Errorf("expected issue #7 in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Mi issue cool") {
		t.Errorf("expected issue title in output, got:\n%s", out)
	}
	if !strings.Contains(out, v1CheExecuted+" → "+pipelinelabels.StateExecute) {
		t.Errorf("expected mapping in output, got:\n%s", out)
	}
}
