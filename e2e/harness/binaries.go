package harness

import (
	"sync"
	"testing"
)

// chePath and fakeBinPath are set by TestMain in the e2e package via
// SetBinaries. They point to pre-built che and chefake binaries that are
// shared across all tests in a given `go test` run.
var (
	mu       sync.Mutex
	chePath  string
	fakeBin  string
)

// SetBinaries is called once from TestMain after building che and chefake.
// Calling it more than once panics (indicates a setup bug).
func SetBinaries(cheBin, fakeBinary string) {
	mu.Lock()
	defer mu.Unlock()
	if chePath != "" {
		panic("harness.SetBinaries called twice")
	}
	chePath = cheBin
	fakeBin = fakeBinary
}

func chePathOrFail(t *testing.T) string {
	mu.Lock()
	defer mu.Unlock()
	if chePath == "" {
		t.Fatalf("harness: che binary path not set; did TestMain call harness.SetBinaries?")
	}
	return chePath
}

func fakePath() string {
	mu.Lock()
	defer mu.Unlock()
	return fakeBin
}

func checkTestMainDidBuild(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if chePath == "" || fakeBin == "" {
		t.Fatalf("harness: TestMain must call harness.SetBinaries before tests run")
	}
}
