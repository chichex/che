package harness

import (
	"os"
	"path/filepath"
)

// FakeBrewCaskBin synthesises a path structure that looks like a Homebrew
// cask install:
//
//	<HomeDir>/fake-brew/Caskroom/che/<version>/che  (empty file)
//	<HomeDir>/fake-brew/bin/che                      (symlink → Caskroom)
//
// It returns the path of the symlink. Tests use this as the
// CHE_UPGRADE_TARGET_PATH so the detector resolves the symlink and sees
// "Caskroom" in the resolved path → classifies as brew install.
func (e *Env) FakeBrewCaskBin(version string) string {
	e.t.Helper()
	base := filepath.Join(e.HomeDir, "fake-brew")
	cask := filepath.Join(base, "Caskroom", "che", version)
	if err := os.MkdirAll(cask, 0o755); err != nil {
		e.t.Fatalf("FakeBrewCaskBin mkdir: %v", err)
	}
	target := filepath.Join(cask, "che")
	if err := os.WriteFile(target, []byte("fake-brew-binary"), 0o755); err != nil {
		e.t.Fatalf("FakeBrewCaskBin write: %v", err)
	}
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		e.t.Fatalf("FakeBrewCaskBin mkdir bin: %v", err)
	}
	link := filepath.Join(binDir, "che")
	if err := os.Symlink(target, link); err != nil {
		e.t.Fatalf("FakeBrewCaskBin symlink: %v", err)
	}
	return link
}

// FakeDirectBin creates <HomeDir>/.local/bin/che with the given contents and
// returns its path. Used as CHE_UPGRADE_TARGET_PATH for direct-install tests.
func (e *Env) FakeDirectBin(content []byte) string {
	e.t.Helper()
	dir := filepath.Join(e.HomeDir, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatalf("FakeDirectBin mkdir: %v", err)
	}
	path := filepath.Join(dir, "che")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		e.t.Fatalf("FakeDirectBin write: %v", err)
	}
	return path
}
