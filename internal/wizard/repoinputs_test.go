package wizard

import (
	"strings"
	"testing"

	"github.com/chichex/che/internal/repoctx"
)

// withFakeRepoctx instala un fake en repoctx.Detect y restaura el detector
// "no repo" + limpia el cache al final del test. Comparte la receta entre
// tests para no copiarla en cada subcaso.
func withFakeRepoctx(t *testing.T, fn func() repoctx.Info) {
	t.Helper()
	repoctx.SetDetectFn(fn)
	t.Cleanup(func() {
		repoctx.SetDetectFn(func() repoctx.Info { return repoctx.Info{} })
		repoctx.ResetForTest()
	})
}

// TestInputDisabled_NoRepoFlagsPrIssue valida que la pill pr/issue se
// reporta como disabled cuando repoctx.Detect dice "sin repo", y enabled
// cuando dice "con repo". El resto de los inputs (text/file/url/none/
// previous_output) nunca se reportan disabled.
func TestInputDisabled_NoRepoFlagsPrIssue(t *testing.T) {
	withFakeRepoctx(t, func() repoctx.Info { return repoctx.Info{} })

	if !inputDisabled(InputPR) {
		t.Errorf("InputPR should be disabled when no repo")
	}
	if !inputDisabled(InputIssue) {
		t.Errorf("InputIssue should be disabled when no repo")
	}
	for _, opt := range []string{InputText, InputFile, InputURL, InputNone, InputPreviousOutput} {
		if inputDisabled(opt) {
			t.Errorf("%s should NOT be disabled (no repo)", opt)
		}
	}
}

// TestInputDisabled_WithRepoEnablesAll valida la rama opuesta: con repo
// activo, ningun input se reporta disabled.
func TestInputDisabled_WithRepoEnablesAll(t *testing.T) {
	withFakeRepoctx(t, func() repoctx.Info {
		return repoctx.Info{InGitHubRepo: true, Repo: "chichex/che"}
	})

	for _, opt := range []string{InputText, InputPR, InputIssue, InputFile, InputURL, InputNone, InputPreviousOutput} {
		if inputDisabled(opt) {
			t.Errorf("%s should NOT be disabled (with repo)", opt)
		}
	}
}

// TestStepNeighborSkipDisabled_SkipsPrIssueWhenNoRepo verifica que al cyclar
// con left/right (delta -1/+1) el cursor SALTA las pills disabled. Sin esto
// el usuario aterriza en una opcion que no puede confirmar.
func TestStepNeighborSkipDisabled_SkipsPrIssueWhenNoRepo(t *testing.T) {
	withFakeRepoctx(t, func() repoctx.Info { return repoctx.Info{} })

	options := inputsForStepIdx(0) // [text pr issue file url none]
	// Desde "text", +1 deberia ir a "file" (saltando pr+issue).
	got := stepNeighborSkipDisabled(options, InputText, +1)
	if got != InputFile {
		t.Errorf("from text +1: got %q, want %q (skipping pr/issue)", got, InputFile)
	}
	// Desde "file", -1 deberia volver a "text" (saltando issue+pr).
	got = stepNeighborSkipDisabled(options, InputFile, -1)
	if got != InputText {
		t.Errorf("from file -1: got %q, want %q (skipping issue/pr)", got, InputText)
	}
}

// TestStepNeighborSkipDisabled_NoSkipWithRepo confirma que con repo activo
// el cyclado pasa por todas las opciones (incluida pr/issue).
func TestStepNeighborSkipDisabled_NoSkipWithRepo(t *testing.T) {
	withFakeRepoctx(t, func() repoctx.Info {
		return repoctx.Info{InGitHubRepo: true, Repo: "x/y"}
	})

	options := inputsForStepIdx(0)
	got := stepNeighborSkipDisabled(options, InputText, +1)
	if got != InputPR {
		t.Errorf("from text +1 with repo: got %q, want %q", got, InputPR)
	}
}

// TestRenderInputPills_DimsDisabled valida que el render aplica el sufijo
// "·off" (el mismo patron que las pills de CLIs no instaladas) sobre las
// pills disabled. Asi el usuario ve la pill pero entiende que no se puede
// elegir.
func TestRenderInputPills_DimsDisabled(t *testing.T) {
	withFakeRepoctx(t, func() repoctx.Info { return repoctx.Info{} })

	m := model{
		stepEdit: stepEditState{
			idx:   0,
			focus: StepFocusInput,
			input: InputText,
		},
	}
	out := renderInputPills(m)
	if !strings.Contains(out, "pr ·off") {
		t.Errorf("expected 'pr ·off' in pills output, got:\n%s", out)
	}
	if !strings.Contains(out, "issue ·off") {
		t.Errorf("expected 'issue ·off' in pills output, got:\n%s", out)
	}
	// La pill text seleccionada debe seguir apareciendo en formato "[text]".
	if !strings.Contains(out, "[text]") {
		t.Errorf("expected '[text]' selected pill, got:\n%s", out)
	}
}

// TestRenderListRow_NeedsRepoChip valida que el chip "[needs repo]" aparece
// en rows ready cuando el pipeline declara pr/issue Y el cwd no esta en un
// repo de github. Se omite cuando el row es draft o cuando hay repo activo.
func TestRenderListRow_NeedsRepoChip(t *testing.T) {
	cases := []struct {
		name    string
		repo    repoctx.Info
		item    listItem
		wantHas bool
	}{
		{
			name:    "ready + needsRepo + no repo",
			repo:    repoctx.Info{},
			item:    listItem{name: "a", needsRepo: true},
			wantHas: true,
		},
		{
			name:    "ready + needsRepo + with repo",
			repo:    repoctx.Info{InGitHubRepo: true, Repo: "x/y"},
			item:    listItem{name: "a", needsRepo: true},
			wantHas: false,
		},
		{
			name:    "ready + no needsRepo + no repo",
			repo:    repoctx.Info{},
			item:    listItem{name: "a", needsRepo: false},
			wantHas: false,
		},
		{
			name:    "draft skips chip even if needsRepo",
			repo:    repoctx.Info{},
			item:    listItem{name: "a", isDraft: true, needsRepo: true},
			wantHas: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeRepoctx(t, func() repoctx.Info { return tc.repo })
			got := renderListRow(tc.item)
			has := strings.Contains(got, "[needs repo]")
			if has != tc.wantHas {
				t.Errorf("[needs repo] presence: got %v, want %v\nrow:\n%s", has, tc.wantHas, got)
			}
		})
	}
}

// TestPipelineNeedsRepo_DetectsPrIssue verifica el flag a nivel pipeline
// (lo consume el lister para el chip "[needs repo]" y el preflight para
// el row "git repo context").
func TestPipelineNeedsRepo_DetectsPrIssue(t *testing.T) {
	cases := []struct {
		name  string
		steps []Step
		want  bool
	}{
		{
			name:  "empty pipeline",
			steps: nil,
			want:  false,
		},
		{
			name: "only text",
			steps: []Step{
				{Name: "a", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputText},
			},
			want: false,
		},
		{
			name: "step uses pr",
			steps: []Step{
				{Name: "a", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputPR},
			},
			want: true,
		},
		{
			name: "second step uses issue",
			steps: []Step{
				{Name: "a", CLI: "claude", Kind: KindPrompt, Content: "x", Input: InputText},
				{Name: "b", CLI: "claude", Kind: KindPrompt, Content: "y", Input: InputIssue},
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PipelineNeedsRepo(Pipeline{Steps: tc.steps})
			if got != tc.want {
				t.Errorf("PipelineNeedsRepo: got %v, want %v", got, tc.want)
			}
		})
	}
}
