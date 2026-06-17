package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"git-compose/internal/ui"
)

// githubRepo is set at build time via -ldflags "-X main.githubRepo=owner/repo".
var githubRepo string

// buildTime is the RFC3339 build timestamp baked in at build time via -ldflags "-X main.buildTime=<time>".
// Used to detect whether the running binary is already up to date.
var buildTime string

// updateCmd implements the self-update command.
type updateCmd struct {
	Check bool   `name:"check" short:"c" help:"Only check for a newer version; do not download."`
	Tag   string `name:"tag"   short:"t" default:"" help:"Download a specific release tag instead of the latest."`
}

func (c *updateCmd) Run() error {
	return runUpdate(c.Check, c.Tag)
}

// ghRelease is a minimal GitHub Releases API response.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	PublishedAt string    `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func runUpdate(checkOnly bool, tag string) error {
	ui.Section("Self-update")

	if githubRepo == "" {
		return fmt.Errorf("githubRepo was not set at build time (rebuild with -ldflags \"-X main.githubRepo=owner/repo\")")
	}

	// Determine which release to fetch.
	var apiURL string
	if tag != "" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", githubRepo, tag)
	} else {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	}

	ui.Step("Fetching release info from GitHub")
	release, err := fetchRelease(apiURL)
	if err != nil {
		return fmt.Errorf("fetch release: %w", err)
	}
	ui.Info("Latest release: %s", release.TagName)

	// Find the asset that matches this OS/arch.
	assetName := fmt.Sprintf("git-compose-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range release.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no asset %q found in release %s", assetName, release.TagName)
	}
	ui.Info("Asset: %s", assetName)

	if checkOnly {
		ui.OK("Update available: %s (%s)", release.TagName, assetName)
		return nil
	}

	// Locate the running binary.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	// Download to a temp file next to the binary so the rename is atomic
	// (same filesystem as the target).
	tmp, err := os.CreateTemp(filepath.Dir(self), "git-compose-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op if rename succeeded
	}()

	ui.Step("Downloading %s", downloadURL)
	if err := downloadTo(downloadURL, tmp); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	tmp.Close()

	// Make it executable.
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Atomically replace the running binary.
	ui.Step("Replacing %s", self)
	if err := os.Rename(tmpName, self); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	ui.OK("Updated to %s", release.TagName)
	return nil
}

func fetchRelease(url string) (*ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, body)
	}

	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &r, nil
}

func downloadTo(url string, w io.Writer) error {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

// fetchTagCommit returns the commit SHA that a git tag ref points to.
func fetchTagCommit(refURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, refURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ref API returned %d", resp.StatusCode)
	}

	var result struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Object.SHA, nil
}

// selfUpdate checks for a newer release, downloads it if available, replaces
// the running binary, and re-execs the new binary with the same arguments so
// that the calling reconcile loop runs under the updated version.
// If githubRepo is unset (dev build) or no update is available, it returns
// without error and without re-execing.
func selfUpdate() error {
	if githubRepo == "" {
		ui.Warn("skipping auto-update: binary was not built with -X main.githubRepo")
		return nil
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	release, err := fetchRelease(apiURL)
	if err != nil {
		return fmt.Errorf("auto-update: fetch release: %w", err)
	}

	// Skip if the running binary was built after the release was published.
	if buildTime != "" && release.PublishedAt != "" {
		bt, err1 := time.Parse(time.RFC3339, buildTime)
		rt, err2 := time.Parse(time.RFC3339, release.PublishedAt)
		if err1 == nil && err2 == nil && !bt.Before(rt) {
			return nil
		}
	}

	assetName := fmt.Sprintf("git-compose-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range release.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		// No asset for this platform — not an error, just skip.
		ui.Info("auto-update: no asset %q in latest release (%s), skipping", assetName, release.TagName)
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("auto-update: resolve executable: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(self), "git-compose-update-*")
	if err != nil {
		return fmt.Errorf("auto-update: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if err := downloadTo(downloadURL, tmp); err != nil {
		return fmt.Errorf("auto-update: download: %w", err)
	}
	tmp.Close()

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("auto-update: chmod: %w", err)
	}

	if err := os.Rename(tmpName, self); err != nil {
		return fmt.Errorf("auto-update: replace binary: %w", err)
	}

	ui.OK("Updated to %s; re-execing...", release.TagName)

	// Re-exec the new binary with the same args so the reconcile runs on the
	// updated version. syscall.Exec replaces the current process image.
	return syscall.Exec(self, os.Args, os.Environ())
}
