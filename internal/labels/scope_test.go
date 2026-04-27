package labels

import (
	"errors"
	"strings"
	"testing"
)

func TestScopeHint(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name: "read:org missing (caso reportado)",
			stderr: `gh: GraphQL: your token has not been granted the required scopes to execute this query. The 'login' field requires one of the following scopes: ['read:org'].
`,
			want: "→ ejecutá: gh auth refresh -s read:org,repo",
		},
		{
			name:   "multiple scopes listed",
			stderr: `requires one of the following scopes: read:org, read:user`,
			want:   "→ ejecutá: gh auth refresh -s read:org,read:user,repo",
		},
		{
			name:   "no scope error",
			stderr: `gh: connection refused`,
			want:   "",
		},
		{
			name:   "empty",
			stderr: ``,
			want:   "",
		},
		{
			name:   "scope already includes repo",
			stderr: `requires one of the following scopes: repo, read:org`,
			want:   "→ ejecutá: gh auth refresh -s repo,read:org",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scopeHint([]byte(c.stderr))
			if got != c.want {
				t.Errorf("scopeHint:\n  got:  %q\n  want: %q", got, c.want)
			}
		})
	}
}

func TestWrapGhError_AddsHintWhenScopeMissing(t *testing.T) {
	orig := errors.New("gh api POST labels: failed")
	stderr := []byte(`GraphQL: your token has not been granted the required scopes to execute this query. The 'login' field requires one of the following scopes: ['read:org'].`)
	wrapped := WrapGhError(orig, stderr)
	if wrapped == nil {
		t.Fatal("expected non-nil error")
	}
	msg := wrapped.Error()
	if !strings.Contains(msg, "gh auth refresh -s read:org") {
		t.Errorf("expected refresh hint; got: %s", msg)
	}
	if !errors.Is(wrapped, orig) {
		t.Errorf("expected wrapped error to wrap the original (errors.Is); got: %v", wrapped)
	}
}

func TestWrapGhError_PassthroughWhenNoScopeError(t *testing.T) {
	orig := errors.New("gh api POST labels: 404 Not Found")
	stderr := []byte(`HTTP 404: Not Found`)
	wrapped := WrapGhError(orig, stderr)
	if wrapped == nil {
		t.Fatal("expected non-nil error")
	}
	if wrapped.Error() != orig.Error() {
		t.Errorf("expected original message unchanged; got: %s", wrapped.Error())
	}
}

func TestWrapGhError_NilErrReturnsNil(t *testing.T) {
	if got := WrapGhError(nil, []byte("anything")); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}
