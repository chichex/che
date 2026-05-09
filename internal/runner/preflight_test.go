package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chichex/che/internal/repoctx"
	"github.com/chichex/che/internal/skills"
	"github.com/chichex/che/internal/wizard"
)

// repoctxSetForTest instala un fake en repoctx.Detect y restaura el
// detector "no repo" + limpia el cache en t.Cleanup.
func repoctxSetForTest(t *testing.T, info repoctx.Info) {
	t.Helper()
	repoctx.SetDetectFn(func() repoctx.Info { return info })
	t.Cleanup(func() {
		repoctx.SetDetectFn(func() repoctx.Info { return repoctx.Info{} })
		repoctx.ResetForTest()
	})
}

// TestBuildPreflightChecks_DistinctClis verifica que el builder dedupe
// CLIs y validators en una sola row por nombre + ordene alfabeticamente
// (ese orden es lo que los tests visuales asumen).
func TestBuildPreflightChecks_DistinctClis(t *testing.T) {
	p := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputText},
			{Name: "b", CLI: "gemini", Kind: wizard.KindPrompt, Input: wizard.InputPreviousOutput,
				Validator: &wizard.Validator{CLI: "claude", Kind: wizard.KindPrompt, Content: "v"}},
		},
	}
	checks := buildPreflightChecks(p, wizard.InputText, "")
	var labels []string
	for _, c := range checks {
		labels = append(labels, c.Label)
	}
	wantPrefix := []string{"cli claude instalado", "cli gemini instalado"}
	for _, w := range wantPrefix {
		found := false
		for _, l := range labels {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected label %q in %v", w, labels)
		}
	}
}

// TestBuildPreflightChecks_GhOnlyWhenNeeded valida que el row de gh auth
// aparece solo cuando algun step usa input pr|issue.
func TestBuildPreflightChecks_GhOnlyWhenNeeded(t *testing.T) {
	pNoGh := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputText},
		},
	}
	for _, c := range buildPreflightChecks(pNoGh, wizard.InputText, "") {
		if c.Label == "gh auth status" {
			t.Errorf("did not expect gh auth row when no step uses pr/issue, got: %v", c)
		}
	}

	pWithGh := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputIssue},
		},
	}
	found := false
	for _, c := range buildPreflightChecks(pWithGh, wizard.InputIssue, "chichex/che#1") {
		if c.Label == "gh auth status" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected gh auth row when step uses input=issue")
	}
}

// TestBuildPreflightChecks_FileReadOnlyWhenFile garantiza que el row
// "file readable" solo aparece para input=file.
func TestBuildPreflightChecks_FileReadOnlyWhenFile(t *testing.T) {
	p := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputText},
		},
	}
	for _, c := range buildPreflightChecks(p, wizard.InputText, "") {
		if strings.HasPrefix(c.Label, "file readable") {
			t.Errorf("did not expect file readable row for input=text, got: %v", c)
		}
	}

	pFile := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputFile},
		},
	}
	got := buildPreflightChecks(pFile, wizard.InputFile, "/tmp/x")
	hasRow := false
	for _, c := range got {
		if strings.HasPrefix(c.Label, "file readable") {
			hasRow = true
		}
	}
	if !hasRow {
		t.Errorf("expected file readable row for input=file")
	}
}

// TestSummarizePreflight_Verdict cubre las 3 ramas del verdict.
func TestSummarizePreflight_Verdict(t *testing.T) {
	cases := []struct {
		name string
		in   []PreflightCheck
		want preflightVerdict
	}{
		{"all ok", []PreflightCheck{{Status: PreflightOK}, {Status: PreflightOK}}, preflightVerdictAllOK},
		{"with fail", []PreflightCheck{{Status: PreflightOK}, {Status: PreflightFail}, {Status: PreflightWarn}}, preflightVerdictHasFail},
		{"only warn", []PreflightCheck{{Status: PreflightOK}, {Status: PreflightWarn}}, preflightVerdictOnlyWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizePreflight(tc.in); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunPreflightChecks_AllGreen ejercita el path "todo verde" con
// fakes en memoria para CLIs / skills / gh / disk. Sirve como red de
// seguridad por encima de los e2e (que solo cubren el subset claude).
func TestRunPreflightChecks_AllGreen(t *testing.T) {
	savedDetect := detectSkills
	savedGh := ghAuthFn
	savedDisk := diskFreeFn
	t.Cleanup(func() {
		detectSkills = savedDetect
		ghAuthFn = savedGh
		diskFreeFn = savedDisk
	})

	detectSkills = func() []skills.CLI {
		return []skills.CLI{
			{Name: "claude", Installed: true, Skills: []skills.Skill{{Name: "skill-x"}}},
		}
	}
	ghAuthFn = func() bool { return true }
	diskFreeFn = func(string) uint64 { return 9999 * 1024 * 1024 } // 9.7 GiB libres
	// Repo activo: el row "git repo context" se resuelve a OK con el
	// nombre del repo append-eado al label.
	repoctxSetForTest(t, repoctx.Info{InGitHubRepo: true, Repo: "chichex/che"})

	p := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindSkill, Content: "skill-x", Input: wizard.InputIssue},
		},
	}

	checks := buildPreflightChecks(p, wizard.InputIssue, "chichex/che#1")
	got := runPreflightChecks(checks, p, wizard.InputIssue, "chichex/che#1")

	for _, c := range got {
		if c.Status != PreflightOK {
			t.Errorf("row %q: got status %v, want OK (remedy=%q)", c.Label, c.Status, c.Remedy)
		}
	}
	if v := summarizePreflight(got); v != preflightVerdictAllOK {
		t.Errorf("verdict = %v, want allOK", v)
	}
}

// TestRunPreflightChecks_FailRows cubre los rows con status fail/warn:
// CLI no instalado, skill ausente, gh auth fail, disk bajo.
func TestRunPreflightChecks_FailRows(t *testing.T) {
	savedDetect := detectSkills
	savedGh := ghAuthFn
	savedDisk := diskFreeFn
	savedLook := lookPathFn
	t.Cleanup(func() {
		detectSkills = savedDetect
		ghAuthFn = savedGh
		diskFreeFn = savedDisk
		lookPathFn = savedLook
	})

	detectSkills = func() []skills.CLI {
		return []skills.CLI{
			{Name: "claude", Installed: false},
		}
	}
	lookPathFn = func(string) (string, error) { return "", os.ErrNotExist }
	ghAuthFn = func() bool { return false }
	diskFreeFn = func(string) uint64 { return 10 * 1024 * 1024 } // 10 MiB libres → warn
	// Sin repo: el row "git repo context" tambien debe fail.
	repoctxSetForTest(t, repoctx.Info{})

	p := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindSkill, Content: "missing-skill", Input: wizard.InputPR},
		},
	}

	checks := buildPreflightChecks(p, wizard.InputPR, "chichex/che#1")
	got := runPreflightChecks(checks, p, wizard.InputPR, "chichex/che#1")

	want := map[string]PreflightStatus{
		"cli claude instalado":          PreflightFail,
		"skill missing-skill en claude": PreflightFail,
		"gh auth status":                PreflightFail,
		"git repo context":              PreflightFail,
	}
	for label, status := range want {
		found := false
		for _, c := range got {
			if c.Label == label {
				found = true
				if c.Status != status {
					t.Errorf("row %q: got %v, want %v", label, c.Status, status)
				}
				if c.Remedy == "" {
					t.Errorf("row %q: expected remedy when not OK", label)
				}
			}
		}
		if !found {
			t.Errorf("expected row %q in checks", label)
		}
	}

	// Disk row should be Warn (no Fail) — bloqueo por warn solo si no hay
	// otros fails, pero el verdict aca tiene fails reales asi que no
	// importa. Verificamos solo el status del row.
	for _, c := range got {
		if strings.HasPrefix(c.Label, "disk space") {
			if c.Status != PreflightWarn {
				t.Errorf("disk row: got %v, want Warn", c.Status)
			}
		}
	}
}

// TestBuildPreflightChecks_GitRepoContextRowOnlyWhenGh valida que el row
// "git repo context" aparece SOLO cuando algun step usa pr/issue. Sin gh-
// related inputs no tiene sentido pedir un repo activo.
func TestBuildPreflightChecks_GitRepoContextRowOnlyWhenGh(t *testing.T) {
	pNoGh := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputText},
		},
	}
	for _, c := range buildPreflightChecks(pNoGh, wizard.InputText, "") {
		if c.Label == "git repo context" {
			t.Errorf("did not expect git repo row when no step uses pr/issue, got: %v", c)
		}
	}

	pWithGh := wizard.Pipeline{
		Name: "p",
		Steps: []wizard.Step{
			{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputPR},
		},
	}
	found := false
	for _, c := range buildPreflightChecks(pWithGh, wizard.InputPR, "x/y#1") {
		if c.Label == "git repo context" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected git repo row when step uses input=pr")
	}
}

// TestResolveCheck_GitRepoContextOK valida que con repo activo el row
// resuelve OK + el label gana el sufijo con el nombre del repo (asi el
// usuario sabe contra que esta resolviendo pr/issue).
func TestResolveCheck_GitRepoContextOK(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{InGitHubRepo: true, Repo: "chichex/che"})

	c := PreflightCheck{Label: "git repo context", Status: PreflightPending}
	got := resolveCheck(c, wizard.Pipeline{}, "", "", nil, nil)
	if got.Status != PreflightOK {
		t.Errorf("status: got %v, want OK", got.Status)
	}
	if !strings.Contains(got.Label, "chichex/che") {
		t.Errorf("label should include repo name, got %q", got.Label)
	}
}

// TestResolveCheck_GitRepoContextFail valida que sin repo el row reporta
// fail + remedio claro.
func TestResolveCheck_GitRepoContextFail(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{})

	c := PreflightCheck{Label: "git repo context", Status: PreflightPending}
	got := resolveCheck(c, wizard.Pipeline{}, "", "", nil, nil)
	if got.Status != PreflightFail {
		t.Errorf("status: got %v, want Fail", got.Status)
	}
	if got.Remedy == "" {
		t.Errorf("expected remedio when no repo")
	}
	if !strings.Contains(got.Remedy, "cd") {
		t.Errorf("remedio should hint at `cd`, got %q", got.Remedy)
	}
}

// TestResolveFileCheck_DisappearedFile cubre el path defensivo: archivo
// existe en R1, desapareci en R2.
func TestResolveFileCheck_DisappearedFile(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "no-existe.txt")
	c := PreflightCheck{Label: "file readable: " + missing, Status: PreflightPending}
	got := resolveFileCheck(c, missing)
	if got.Status != PreflightFail {
		t.Errorf("got %v, want Fail", got.Status)
	}
	if got.Remedy == "" {
		t.Errorf("expected remedy for disappeared file")
	}
}

// TestResolveFileCheck_HappyPath cubre el path: archivo existe + es
// regular file → OK.
func TestResolveFileCheck_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "ok.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := PreflightCheck{Label: "file readable: " + path, Status: PreflightPending}
	got := resolveFileCheck(c, path)
	if got.Status != PreflightOK {
		t.Errorf("got %v, want OK", got.Status)
	}
}
