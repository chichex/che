package harness

import (
	"path/filepath"
	"runtime"
	"testing"
)

// fixturePath resolves a path relative to e2e/testdata. Because every test
// package (e2e/idea, e2e/doctor, …) has a different working dir, we anchor
// off the harness source file location to get a stable absolute path.
func fixturePath(t testingTB, rel string) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("harness: cannot locate harness source")
	}
	// self = .../e2e/harness/fixtures.go → testdata = .../e2e/testdata
	testdata := filepath.Join(filepath.Dir(filepath.Dir(self)), "testdata")
	return filepath.Join(testdata, rel)
}

// AssertContains fails the test if got does not contain want.
func AssertContains(t *testing.T, got, want string) {
	t.Helper()
	if !contains(got, want) {
		t.Fatalf("expected output to contain %q\ngot:\n%s", want, got)
	}
}

// AssertNotContains fails the test if got contains want.
func AssertNotContains(t *testing.T, got, want string) {
	t.Helper()
	if contains(got, want) {
		t.Fatalf("expected output to NOT contain %q\ngot:\n%s", want, got)
	}
}

func contains(haystack, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

// indexOf avoids importing strings from fixtures.go so the file stays tiny and
// self-documenting as "the helpers surface".
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
