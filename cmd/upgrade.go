package cmd

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const defaultReleasesAPI = "https://api.github.com/repos/chichex/che/releases/latest"

var (
	upgradeCheck bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "actualiza che a la última versión publicada",
	Long: `upgrade consulta la última release en GitHub y actualiza el binario si
hay una versión nueva. Detecta automáticamente si che fue instalado vía
brew (cask) o vía el install.sh (binario en ~/.local/bin).

Env vars:
  CHE_RELEASES_API_URL     URL del endpoint de releases (default: api.github.com).
  CHE_UPGRADE_TARGET_PATH  Override del path del binario a actualizar.
  CHE_SKIP_CODESIGN        Si está seteada en macOS, saltea codesign/xattr.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpgrade(cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeCheck, "check", false, "solo reporta si hay nueva versión, sin actualizar")
	rootCmd.AddCommand(upgradeCmd)
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type installMethod int

const (
	installUnknown installMethod = iota
	installBrew
	installDirect
)

func runUpgrade(stdout, stderr io.Writer) error {
	latest, err := fetchLatest(releasesAPIURL())
	if err != nil {
		fmt.Fprintf(stderr, "error: checking latest version: %v\n", err)
		os.Exit(2)
	}

	current := Version
	if normalizeVersion(current) == normalizeVersion(latest.TagName) && current != "dev" {
		fmt.Fprintf(stdout, "Already on latest version (%s)\n", latest.TagName)
		return nil
	}

	if upgradeCheck {
		fmt.Fprintf(stdout, "%s → %s available\n", current, latest.TagName)
		return nil
	}

	fmt.Fprintf(stdout, "Upgrading: %s → %s\n", current, latest.TagName)

	target, err := resolveTargetPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		os.Exit(2)
	}
	method := detectInstall(target)

	switch method {
	case installBrew:
		return upgradeViaBrew(stdout, stderr, latest.TagName)
	case installDirect:
		return upgradeDirect(stdout, stderr, latest, target)
	default:
		fmt.Fprintf(stderr, "error: unknown install location: %s\n", target)
		fmt.Fprintln(stderr, "instalá che via brew (recommended) o usando install.sh, o buildealo con go install")
		os.Exit(3)
	}
	return nil
}

func releasesAPIURL() string {
	if v := os.Getenv("CHE_RELEASES_API_URL"); v != "" {
		return v
	}
	return defaultReleasesAPI
}

func fetchLatest(url string) (*release, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(v, "v")
}

// resolveTargetPath returns the path of the binary we should replace.
// Honours CHE_UPGRADE_TARGET_PATH for tests and for users with wrapper
// symlinks. Otherwise falls back to os.Executable.
func resolveTargetPath() (string, error) {
	if v := os.Getenv("CHE_UPGRADE_TARGET_PATH"); v != "" {
		return v, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating current executable: %w", err)
	}
	return exe, nil
}

// detectInstall classifies the install method based on the target path and
// the symlink it resolves to (if any). Anything under Caskroom/Cellar is
// brew; anything under the user's HOME with a known layout is direct;
// anything else is unknown.
func detectInstall(path string) installMethod {
	candidates := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, p := range candidates {
		if strings.Contains(p, "/Caskroom/") || strings.Contains(p, "/Cellar/") {
			return installBrew
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, p := range candidates {
			if strings.HasPrefix(p, home) {
				return installDirect
			}
		}
	}
	return installUnknown
}

func upgradeViaBrew(stdout, stderr io.Writer, tag string) error {
	fmt.Fprintln(stdout, "Detected brew install, upgrading via brew…")
	// `brew update` antes de uninstall/install: sin esto, brew usa el
	// estado local del tap (cacheado como git repo bajo
	// $(brew --repository)/Library/Taps/chichex/homebrew-tap) y puede
	// re-instalar la versión vieja. Caso real visto en v0.0.29: el
	// cask.rb ya apuntaba a 0.0.29 en el remote, pero brew bajó 0.0.28
	// porque el git del tap no había hecho fetch después de la release.
	if err := runPassthrough(stdout, stderr, "brew", "update"); err != nil {
		fmt.Fprintf(stderr, "warning: brew update failed (%v) — sigo igual, puede que la versión nueva no esté sincronizada\n", err)
	}
	if err := runPassthrough(stdout, stderr, "brew", "uninstall", "--cask", "che"); err != nil {
		fmt.Fprintf(stderr, "error: brew uninstall failed: %v\n", err)
		os.Exit(2)
	}
	if err := runPassthrough(stdout, stderr, "brew", "install", "--cask", "chichex/tap/che"); err != nil {
		fmt.Fprintf(stderr, "error: brew install failed: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(stdout, "Upgraded to %s\n", tag)
	return nil
}

func upgradeDirect(stdout, stderr io.Writer, latest *release, target string) error {
	assetName := fmt.Sprintf("che_%s_%s_%s.tar.gz", normalizeVersion(latest.TagName), runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range latest.Assets {
		if a.Name == assetName {
			downloadURL = a.DownloadURL
			break
		}
	}
	if downloadURL == "" {
		fmt.Fprintf(stderr, "error: no download URL for %s/%s\n", runtime.GOOS, runtime.GOARCH)
		os.Exit(2)
	}

	fmt.Fprintf(stdout, "Downloading %s…\n", assetName)
	body, err := httpGet(downloadURL)
	if err != nil {
		fmt.Fprintf(stderr, "error: download failed: %v\n", err)
		os.Exit(2)
	}
	binary, err := extractCheFromTarball(body)
	if err != nil {
		fmt.Fprintf(stderr, "error: extract failed: %v\n", err)
		os.Exit(2)
	}

	tmp := target + ".new"
	if err := os.WriteFile(tmp, binary, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: writing %s: %v\n", tmp, err)
		os.Exit(2)
	}
	if err := os.Rename(tmp, target); err != nil {
		fmt.Fprintf(stderr, "error: replacing %s: %v\n", target, err)
		os.Exit(2)
	}

	if runtime.GOOS == "darwin" && os.Getenv("CHE_SKIP_CODESIGN") == "" {
		_ = exec.Command("xattr", "-dr", "com.apple.quarantine", target).Run()
		_ = exec.Command("codesign", "--sign", "-", "--force", target).Run()
	}

	fmt.Fprintf(stdout, "Upgraded to %s\n", latest.TagName)
	return nil
}

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func extractCheFromTarball(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("che binary not found in tarball")
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if filepath.Base(hdr.Name) == "che" {
			return io.ReadAll(tr)
		}
	}
}

func runPassthrough(stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
