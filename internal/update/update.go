// internal/update/update.go
// GitHub release checking and binary download for auto-update
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
)

const (
	// GitHubRepo is the repository for releases
	GitHubRepo = "aceteam-ai/citadel-cli"

	// GitHubAPIBase is the GitHub API endpoint
	GitHubAPIBase = "https://api.github.com"

	// GitHubDownloadBase is the download endpoint
	GitHubDownloadBase = "https://github.com"
)

// Release represents a GitHub release
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
	HTMLURL    string  `json:"html_url"`
}

// Asset represents a release asset
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Client handles update operations
type Client struct {
	CurrentVersion string
	Channel        string // "stable" or "rc"
	httpClient     *http.Client
}

// NewClient creates a new update client
func NewClient(currentVersion string) *Client {
	return &Client{
		CurrentVersion: currentVersion,
		Channel:        "stable",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WithChannel sets the release channel
func (c *Client) WithChannel(channel string) *Client {
	c.Channel = channel
	return c
}

// CheckForUpdate checks if a new version is available
// Returns nil if already on latest version
func (c *Client) CheckForUpdate() (*Release, error) {
	release, err := c.fetchLatestRelease()
	if err != nil {
		return nil, err
	}

	// Compare versions
	hasUpdate, err := c.isNewerVersion(release.TagName)
	if err != nil {
		return nil, err
	}

	if !hasUpdate {
		return nil, nil
	}

	return release, nil
}

// GetLatestRelease fetches the latest release info without version comparison
func (c *Client) GetLatestRelease() (*Release, error) {
	return c.fetchLatestRelease()
}

// Download downloads the binary for the current OS/ARCH
func (c *Client) Download(release *Release, destPath string) error {
	downloadURL := c.getDownloadURL(release)

	if err := EnsureUpdateDir(); err != nil {
		return fmt.Errorf("failed to create update directory: %w", err)
	}

	resp, err := c.httpClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status: %s", resp.Status)
	}

	// Download to temp file first
	archivePath := destPath + ".archive"
	outFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	_, err = io.Copy(outFile, resp.Body)
	outFile.Close()
	if err != nil {
		os.Remove(archivePath)
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Extract binary from archive
	if err := c.extractBinary(archivePath, destPath); err != nil {
		os.Remove(archivePath)
		return fmt.Errorf("failed to extract binary: %w", err)
	}

	// Clean up archive
	os.Remove(archivePath)

	// Set executable permissions on Unix
	if runtime.GOOS != "windows" {
		if err := os.Chmod(destPath, 0755); err != nil {
			return fmt.Errorf("failed to set permissions: %w", err)
		}
	}

	return nil
}

// VerifyChecksum verifies the SHA256 checksum of a file
func (c *Client) VerifyChecksum(filePath string, release *Release) error {
	checksumURL := c.getChecksumURL(release)

	resp, err := c.httpClient.Get(checksumURL)
	if err != nil {
		return fmt.Errorf("failed to fetch checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum fetch failed with status: %s", resp.Status)
	}

	checksumData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read checksums: %w", err)
	}

	// Find expected checksum for our binary
	binaryName := c.getBinaryArchiveName(release)
	expectedChecksum := ""
	lines := strings.Split(string(checksumData), "\n")
	for _, line := range lines {
		if strings.Contains(line, binaryName) {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				expectedChecksum = parts[0]
				break
			}
		}
	}

	if expectedChecksum == "" {
		return fmt.Errorf("checksum not found for %s", binaryName)
	}

	// Calculate actual checksum
	actualChecksum, err := calculateSHA256(filePath + ".archive")
	if err != nil {
		return fmt.Errorf("failed to calculate checksum: %w", err)
	}

	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}

	return nil
}

// DownloadAndVerify downloads and verifies the binary in one step
func (c *Client) DownloadAndVerify(release *Release, destPath string) error {
	downloadURL := c.getDownloadURL(release)

	if err := EnsureUpdateDir(); err != nil {
		return fmt.Errorf("failed to create update directory: %w", err)
	}

	// Download archive
	archivePath := destPath + ".archive"
	resp, err := c.httpClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status: %s", resp.Status)
	}

	outFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	_, err = io.Copy(outFile, resp.Body)
	outFile.Close()
	if err != nil {
		os.Remove(archivePath)
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Verify checksum before extracting
	if err := c.verifyArchiveChecksum(archivePath, release); err != nil {
		os.Remove(archivePath)
		return err
	}

	// Extract binary
	if err := c.extractBinary(archivePath, destPath); err != nil {
		os.Remove(archivePath)
		return fmt.Errorf("failed to extract binary: %w", err)
	}

	// Clean up archive
	os.Remove(archivePath)

	// Set executable permissions on Unix
	if runtime.GOOS != "windows" {
		if err := os.Chmod(destPath, 0755); err != nil {
			return fmt.Errorf("failed to set permissions: %w", err)
		}
	}

	return nil
}

// fetchLatestRelease fetches the latest release from GitHub
func (c *Client) fetchLatestRelease() (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", GitHubAPIBase, GitHubRepo)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "citadel-cli")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status: %s", resp.Status)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	return &release, nil
}

// isNewerVersion compares versions
func (c *Client) isNewerVersion(newVersion string) (bool, error) {
	// Strip leading 'v' if present
	currentStr := strings.TrimPrefix(c.CurrentVersion, "v")
	newStr := strings.TrimPrefix(newVersion, "v")

	// Handle "dev" version
	if currentStr == "dev" || currentStr == "" {
		return true, nil
	}

	current, err := version.NewVersion(currentStr)
	if err != nil {
		return false, fmt.Errorf("invalid current version %s: %w", c.CurrentVersion, err)
	}

	latest, err := version.NewVersion(newStr)
	if err != nil {
		return false, fmt.Errorf("invalid new version %s: %w", newVersion, err)
	}

	return latest.GreaterThan(current), nil
}

// getDownloadURL constructs the download URL for the current platform
func (c *Client) getDownloadURL(release *Release) string {
	binaryName := c.getBinaryArchiveName(release)
	return fmt.Sprintf("%s/%s/releases/download/%s/%s",
		GitHubDownloadBase, GitHubRepo, release.TagName, binaryName)
}

// getChecksumURL returns the URL to the checksums.txt file
func (c *Client) getChecksumURL(release *Release) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/checksums.txt",
		GitHubDownloadBase, GitHubRepo, release.TagName)
}

// getBinaryArchiveName returns the archive filename for the current platform
func (c *Client) getBinaryArchiveName(release *Release) string {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	ext := ".tar.gz"
	if osName == "windows" {
		ext = ".zip"
	}

	return fmt.Sprintf("citadel_%s_%s_%s%s", release.TagName, osName, arch, ext)
}

// extractBinary extracts the binary from the archive
func (c *Client) extractBinary(archivePath, destPath string) error {
	if runtime.GOOS == "windows" {
		return c.extractZip(archivePath, destPath)
	}
	return c.extractTarGz(archivePath, destPath)
}

// extractTarGz extracts a binary from a .tar.gz archive
func (c *Client) extractTarGz(archivePath, destPath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Look for the citadel binary
		if header.Typeflag == tar.TypeReg && (header.Name == "citadel" || filepath.Base(header.Name) == "citadel") {
			outFile, err := os.Create(destPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
			return nil
		}
	}

	return fmt.Errorf("citadel binary not found in archive")
}

// extractZip extracts a binary from a .zip archive
func (c *Client) extractZip(archivePath, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		// Look for the citadel.exe binary
		if f.Name == "citadel.exe" || filepath.Base(f.Name) == "citadel.exe" {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			outFile, err := os.Create(destPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, rc); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
			return nil
		}
	}

	return fmt.Errorf("citadel.exe not found in archive")
}

// verifyArchiveChecksum verifies the checksum of an archive file
func (c *Client) verifyArchiveChecksum(archivePath string, release *Release) error {
	checksumURL := c.getChecksumURL(release)

	resp, err := c.httpClient.Get(checksumURL)
	if err != nil {
		return fmt.Errorf("failed to fetch checksums: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum fetch failed with status: %s", resp.Status)
	}

	checksumData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read checksums: %w", err)
	}

	// Find expected checksum for our binary
	binaryName := c.getBinaryArchiveName(release)
	expectedChecksum := ""
	lines := strings.Split(string(checksumData), "\n")
	for _, line := range lines {
		if strings.Contains(line, binaryName) {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				expectedChecksum = parts[0]
				break
			}
		}
	}

	if expectedChecksum == "" {
		return fmt.Errorf("checksum not found for %s", binaryName)
	}

	// Calculate actual checksum
	actualChecksum, err := calculateSHA256(archivePath)
	if err != nil {
		return fmt.Errorf("failed to calculate checksum: %w", err)
	}

	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}

	return nil
}

// calculateSHA256 calculates the SHA256 hash of a file
func calculateSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
