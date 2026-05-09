package promptreview

import (
	"strings"
	"testing"
)

func TestParseReview_Strict(t *testing.T) {
	raw := `{"ok":false,"issues":["no es imperativo","puede pedir confirmacion"],"summary":"el prompt analiza pero no actua","suggested":"ejecuta gh pr merge --merge --delete-branch <PR>"}`
	r, err := parseReview(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.OK {
		t.Errorf("expected OK=false")
	}
	if len(r.Issues) != 2 {
		t.Errorf("issues len: %d", len(r.Issues))
	}
	if !strings.Contains(r.Suggested, "gh pr merge") {
		t.Errorf("suggested: %q", r.Suggested)
	}
}

func TestParseReview_TolerantWrappingProse(t *testing.T) {
	raw := "Aqui va mi analisis:\n\n{\"ok\":true,\"issues\":[],\"summary\":\"el prompt esta bien\",\"suggested\":\"\"}\n\n--- fin ---"
	r, err := parseReview(raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !r.OK {
		t.Errorf("expected OK=true")
	}
	if r.Summary != "el prompt esta bien" {
		t.Errorf("summary: %q", r.Summary)
	}
}

func TestParseReview_NoJSON(t *testing.T) {
	_, err := parseReview("solo prosa, no hay json")
	if err == nil {
		t.Errorf("expected error")
	}
}

func TestSetReviewFn_SwapAndRestore(t *testing.T) {
	called := false
	prev := SetReviewFn(func(p string) (Review, error) {
		called = true
		return Review{OK: true, Summary: "fake"}, nil
	})
	t.Cleanup(func() { SetReviewFn(prev) })

	r, err := Run("test")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Errorf("fake fn not called")
	}
	if r.Summary != "fake" {
		t.Errorf("summary: %q", r.Summary)
	}
}
