package repoctx

import (
	"testing"
)

// TestDetect_HappyPath simula gh respondiendo "owner/repo" y verifica que
// Detect() devuelve InGitHubRepo=true + el repo, y que el cache evita la
// segunda invocacion.
func TestDetect_HappyPath(t *testing.T) {
	calls := 0
	SetDetectFn(func() Info {
		calls++
		return Info{InGitHubRepo: true, Repo: "chichex/che"}
	})
	t.Cleanup(func() {
		SetDetectFn(defaultDetect)
		ResetForTest()
	})

	got := Detect()
	if !got.InGitHubRepo {
		t.Errorf("InGitHubRepo: got false, want true")
	}
	if got.Repo != "chichex/che" {
		t.Errorf("Repo: got %q, want chichex/che", got.Repo)
	}

	// Segunda llamada: debe venir del cache, no incrementar calls.
	_ = Detect()
	if calls != 1 {
		t.Errorf("expected 1 detect call (cache hit), got %d", calls)
	}
}

// TestDetect_NotInRepo simula gh fallando (exit !=0). Detect() debe
// devolver InGitHubRepo=false + Repo vacio.
func TestDetect_NotInRepo(t *testing.T) {
	SetDetectFn(func() Info {
		return Info{}
	})
	t.Cleanup(func() {
		SetDetectFn(defaultDetect)
		ResetForTest()
	})

	got := Detect()
	if got.InGitHubRepo {
		t.Errorf("InGitHubRepo: got true, want false")
	}
	if got.Repo != "" {
		t.Errorf("Repo: got %q, want empty", got.Repo)
	}
}

// TestSetDetectFn_ResetsCache verifica que SetDetectFn invalida el cache:
// despues de cambiar la fn, la proxima Detect() corre la nueva.
func TestSetDetectFn_ResetsCache(t *testing.T) {
	SetDetectFn(func() Info { return Info{InGitHubRepo: true, Repo: "a/b"} })
	t.Cleanup(func() {
		SetDetectFn(defaultDetect)
		ResetForTest()
	})

	if got := Detect(); got.Repo != "a/b" {
		t.Fatalf("first detect: got %q, want a/b", got.Repo)
	}

	// Cambiar la fn debe purgar el cache asi la proxima Detect() corre la
	// nueva.
	SetDetectFn(func() Info { return Info{InGitHubRepo: true, Repo: "c/d"} })
	if got := Detect(); got.Repo != "c/d" {
		t.Errorf("after SetDetectFn: got %q, want c/d", got.Repo)
	}
}
