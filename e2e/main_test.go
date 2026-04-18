package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestMain builds che and chefake once per `go test` invocation, then shares
// their paths with the harness. Individual test packages import this package
// indirectly via harness.New, which asserts the binaries were registered.
func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "che e2e tests are not supported on Windows; skipping")
		os.Exit(0)
	}

	tmp, err := os.MkdirTemp("", "che-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	cheBin := filepath.Join(tmp, "che")
	fakeBin := filepath.Join(tmp, "chefake")

	if err := goBuild(cheBin, ".", "-ldflags=-X github.com/chichex/che/cmd.Version="+harness.TestVersion); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build che: %v\n", err)
		os.Exit(1)
	}
	if err := goBuild(fakeBin, "./cmd/fake"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build chefake: %v\n", err)
		os.Exit(1)
	}

	harness.SetBinaries(cheBin, fakeBin, harness.TestVersion)
	os.Exit(m.Run())
}

func goBuild(out, pkg string, extraArgs ...string) error {
	args := []string{"build", "-o", out}
	args = append(args, extraArgs...)
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = projectRoot()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// projectRoot locates the module root from the test binary's working dir.
// `go test ./e2e/...` runs tests with cwd = the package dir, so we walk up
// until we find go.mod.
func projectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return wd
		}
		dir = parent
	}
}
