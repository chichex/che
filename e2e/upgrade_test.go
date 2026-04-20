package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/chichex/che/e2e/harness"
)

// TestUpgrade_CommandExists forces cmd/upgrade.go into existence.
func TestUpgrade_CommandExists(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	out := env.MustRun("upgrade", "--help")
	harness.AssertContains(t, out, "upgrade")
	harness.AssertContains(t, out, "actualiza")
}

// TestUpgrade_AlreadyLatest: the server reports the same tag as the local
// binary's Version. Upgrade short-circuits without touching any external tool.
func TestUpgrade_AlreadyLatest(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.StartReleasesServer().SetLatest("v"+env.CurrentVersion(), nil)

	out := env.MustRun("upgrade")
	harness.AssertContains(t, out, "Already on latest")
	env.Invocations().AssertNotCalled(t, "brew")
}

// TestUpgrade_FetchFails: the server returns 500. Upgrade must exit !=0 and
// print a diagnostic. No external tool is invoked.
func TestUpgrade_FetchFails(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.StartReleasesServer().SetStatus(500, "boom")

	out := env.MustFail("upgrade")
	harness.AssertContains(t, out, "checking latest version")
	env.Invocations().AssertNotCalled(t, "brew")
}

// TestUpgrade_Check_FlagReportsAvailable: --check reports the new version
// without invoking any installer.
func TestUpgrade_Check_FlagReportsAvailable(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.StartReleasesServer().SetLatest("v9.9.9", nil)

	out := env.MustRun("upgrade", "--check")
	harness.AssertContains(t, out, "v9.9.9")
	harness.AssertContains(t, out, "available")
	env.Invocations().AssertNotCalled(t, "brew")
}

// TestUpgrade_BrewPath_Success: target binary is under /Caskroom/. Upgrade
// delegates to brew uninstall + install.
func TestUpgrade_BrewPath_Success(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.StartReleasesServer().SetLatest("v9.9.9", nil)
	env.WithInstallPath(env.FakeBrewCaskBin(env.CurrentVersion()))
	// `brew update` es pre-requisito (v0.0.30): sin esto brew puede
	// re-instalar del cache local del tap, que no refleja la release
	// recién publicada.
	env.ExpectBrew(`^update$`).RespondStdout("Already up-to-date.\n", 0)
	env.ExpectBrew(`^uninstall --cask che$`).RespondStdout("Uninstalling che...\n", 0)
	env.ExpectBrew(`^install --cask chichex/tap/che$`).RespondStdout("Installed\n", 0)

	out := env.MustRun("upgrade")
	harness.AssertContains(t, out, "Upgrading:")
	harness.AssertContains(t, out, "brew")

	inv := env.Invocations()
	inv.AssertCalled(t, "brew", 3)
	inv.CallOf("brew", 1).AssertArgsContain(t, "update")
	inv.CallOf("brew", 3).AssertArgsContain(t, "install", "--cask", "chichex/tap/che")
}

// TestUpgrade_DirectPath_Success: target binary is ~/.local/bin/che. Upgrade
// downloads the tarball, extracts it, and replaces the binary.
func TestUpgrade_DirectPath_Success(t *testing.T) {
	t.Parallel()
	env := harness.New(t)

	wantBody := []byte(fmt.Sprintf("fake-che-9.9.9-%s-%s\n", runtime.GOOS, runtime.GOARCH))
	tarball := harness.BuildFakeReleaseTarball(t, wantBody)

	assetName := fmt.Sprintf("che_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	env.StartReleasesServer().SetLatest("v9.9.9", map[string][]byte{
		assetName: tarball,
	})

	targetPath := env.FakeDirectBin([]byte("old-binary"))
	env.WithInstallPath(targetPath)
	env.SetEnv("CHE_SKIP_CODESIGN", "1")

	out := env.MustRun("upgrade")
	harness.AssertContains(t, out, "Downloading")
	harness.AssertContains(t, out, "Upgraded to")

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if !bytes.Equal(got, wantBody) {
		t.Fatalf("upgraded binary contents mismatch\nwant: %q\ngot:  %q", wantBody, got)
	}

	env.Invocations().AssertNotCalled(t, "brew")
}

// TestUpgrade_UnknownInstall_Fails: target path matches neither brew nor home.
// Detector returns "unknown" and che exits 3 with a clear message.
func TestUpgrade_UnknownInstall_Fails(t *testing.T) {
	t.Parallel()
	env := harness.New(t)
	env.StartReleasesServer().SetLatest("v9.9.9", nil)
	env.WithInstallPath("/tmp/weird-location-for-che/che")

	r := env.Run("upgrade")
	if r.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %d\nstdout: %s\nstderr: %s", r.ExitCode, r.Stdout, r.Stderr)
	}
	harness.AssertContains(t, r.Stderr, "unknown install location")
}
