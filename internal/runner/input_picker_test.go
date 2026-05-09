package runner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/repoctx"
	"github.com/chichex/che/internal/wizard"
)

// stubGhCommand devuelve una factoria *exec.Cmd que escribe stdoutBody en
// stdout (via /bin/sh -c printf), captura los args invocados en *seen, y
// sale con exit 0. Usado por los tests de R1 para evitar tocar el binario
// real de gh.
func stubGhCommand(seen *[]string, stdoutBody string) func(context.Context, ...string) *exec.Cmd {
	return func(ctx context.Context, args ...string) *exec.Cmd {
		*seen = append([]string(nil), args...)
		// printf %s '<body>' — single-quoting suficiente para JSON sin
		// caracteres patologicos.
		body := strings.ReplaceAll(stdoutBody, "'", `'\''`)
		return exec.CommandContext(ctx, "/bin/sh", "-c", "printf %s '"+body+"'")
	}
}

// withFakeGhList instala un fake en ghListFn y restaura el default al cierre
// del test.
func withFakeGhList(t *testing.T, fn func(string) ([]GHListItem, error)) {
	t.Helper()
	saved := ghListFn
	ghListFn = fn
	t.Cleanup(func() { ghListFn = saved })
}

// TestInitInputUI_PRPickerWhenRepoActive valida que initInputUI activa el
// picker (repoMode=true) cuando repoctx reporta repo + ghListFn devuelve
// items, y los expone en ghEntries.
func TestInitInputUI_PRPickerWhenRepoActive(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{InGitHubRepo: true, Repo: "chichex/che"})
	withFakeGhList(t, func(kind string) ([]GHListItem, error) {
		if kind != "pr" {
			t.Errorf("ghListFn kind: got %q, want pr", kind)
		}
		return []GHListItem{
			{Number: 10, Title: "first"},
			{Number: 11, Title: "second"},
		}, nil
	})

	ui := initInputUI(wizard.InputPR)
	if !ui.repoMode {
		t.Errorf("expected repoMode=true with active repo")
	}
	if ui.repo != "chichex/che" {
		t.Errorf("repo: got %q, want chichex/che", ui.repo)
	}
	if len(ui.ghEntries) != 2 {
		t.Fatalf("ghEntries len: got %d, want 2", len(ui.ghEntries))
	}
	if ui.ghEntries[0].Number != 10 {
		t.Errorf("first entry number: got %d, want 10", ui.ghEntries[0].Number)
	}
	if ui.ghLoadErr != "" {
		t.Errorf("expected no ghLoadErr, got %q", ui.ghLoadErr)
	}
}

// TestInitInputUI_PRTextFallbackNoRepo valida que sin repo activo, R1 cae
// al textBuf libre (mismo comportamiento que antes del picker).
func TestInitInputUI_PRTextFallbackNoRepo(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{})
	// ghListFn no debe ser llamado en este path.
	withFakeGhList(t, func(string) ([]GHListItem, error) {
		t.Errorf("ghListFn should not be called when no repo")
		return nil, nil
	})

	ui := initInputUI(wizard.InputPR)
	if ui.repoMode {
		t.Errorf("expected repoMode=false without repo")
	}
	if len(ui.ghEntries) != 0 {
		t.Errorf("ghEntries should be empty: %+v", ui.ghEntries)
	}
}

// TestInitInputUI_PRPickerSurfaceLoadError valida que un error de gh queda
// expuesto en ghLoadErr para que renderGHPicker lo muestre. La lista igual
// queda vacia + repoMode true.
func TestInitInputUI_PRPickerSurfaceLoadError(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{InGitHubRepo: true, Repo: "x/y"})
	withFakeGhList(t, func(string) ([]GHListItem, error) {
		return nil, errors.New("gh not authed")
	})

	ui := initInputUI(wizard.InputPR)
	if !ui.repoMode {
		t.Errorf("expected repoMode=true even on error")
	}
	if ui.ghLoadErr == "" {
		t.Errorf("expected ghLoadErr to surface the error")
	}
}

// TestUpdateInputGHPicker_EnterTriggersResolveWithRef simula ↓ + enter sobre
// el picker y valida que confirmInput dispara resolveGH con el ref armado a
// partir del item bajo el cursor. ghCommand se fakea para capturar los
// argumentos sin tocar el binario real.
func TestUpdateInputGHPicker_EnterTriggersResolveWithRef(t *testing.T) {
	repoctxSetForTest(t, repoctx.Info{InGitHubRepo: true, Repo: "chichex/che"})
	withFakeGhList(t, func(string) ([]GHListItem, error) {
		return []GHListItem{
			{Number: 100, Title: "first"},
			{Number: 200, Title: "second"},
		}, nil
	})

	// Stub de exec.CommandContext para resolveGH: capturamos los args y
	// devolvemos un cmd que escribe stdout y exit 0.
	saved := ghCommand
	t.Cleanup(func() { ghCommand = saved })
	var seen []string
	ghCommand = stubGhCommand(&seen, `{"title":"fake"}`)

	m := RunModel{
		Pipeline: wizard.Pipeline{
			Name:  "p",
			Steps: []wizard.Step{{Name: "a", CLI: "claude", Kind: wizard.KindPrompt, Input: wizard.InputPR}},
		},
	}
	m = m.enterFirstScreen()
	if m.Screen != ScreenInput {
		t.Fatalf("expected ScreenInput, got %v", m.Screen)
	}
	if !m.inputUI.repoMode {
		t.Fatalf("expected repoMode=true")
	}

	// ↓ para mover al segundo item.
	mAny, _ := m.updateInputGHPicker(tea.KeyMsg{Type: tea.KeyDown})
	m = mAny.(RunModel)
	if m.inputUI.ghCursor != 1 {
		t.Errorf("ghCursor: got %d, want 1", m.inputUI.ghCursor)
	}

	// enter confirma — dispara confirmInput → resolveGH → ghCommand fake.
	mAny, _ = m.updateInputGHPicker(tea.KeyMsg{Type: tea.KeyEnter})
	m = mAny.(RunModel)

	// Tras confirm exitoso, esperamos haber pasado a Preflight + el ref
	// "chichex/che#200" en m.Input.Value.
	if m.Screen != ScreenPreflight {
		t.Errorf("expected screen Preflight after confirm, got %v (err=%q)", m.Screen, m.inputErr)
	}
	if m.Input.Value != "chichex/che#200" {
		t.Errorf("Input.Value: got %q, want chichex/che#200", m.Input.Value)
	}
	// Y los args que vio gh deben incluir el numero correcto.
	foundNum := false
	for _, a := range seen {
		if a == "200" {
			foundNum = true
		}
	}
	if !foundNum {
		t.Errorf("gh args should include item number 200; got %v", seen)
	}
}
