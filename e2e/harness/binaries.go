package harness

import (
	"sync"
	"testing"
)

// chePath and fakeBinPath are set by TestMain in the e2e package via
// SetBinaries. They point to pre-built che and chefake binaries that are
// shared across all tests in a given `go test` run.
var (
	mu         sync.Mutex
	chePath    string
	fakeBin    string
	cheVersion string // the -X cmd.Version ldflag the binary was built with
)

// TestVersion is the version string baked into the che binary that TestMain
// builds. Tests use this when scripting the releases server to decide what
// "already latest" looks like.
const TestVersion = "0.0.3-test"

// SetBinaries is called once from TestMain after building che and chefake.
// Calling it more than once panics (indicates a setup bug).
func SetBinaries(cheBin, fakeBinary, version string) {
	mu.Lock()
	defer mu.Unlock()
	if chePath != "" {
		panic("harness.SetBinaries called twice")
	}
	chePath = cheBin
	fakeBin = fakeBinary
	cheVersion = version
}

// CurrentVersion returns the version ldflag the che binary was built with.
func (e *Env) CurrentVersion() string {
	mu.Lock()
	defer mu.Unlock()
	return cheVersion
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
