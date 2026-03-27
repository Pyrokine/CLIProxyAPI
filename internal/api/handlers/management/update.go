// Last compiled: 2026-03-17
// Author: pyro

package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

/* ---------- types ---------- */

type updateState struct {
	mu      sync.RWMutex
	status  string // idle | downloading | verifying | replacing | done | error
	message string
	version string
	percent int
}

type updateRequest struct {
	Version string `json:"version"` // e.g. "v1.3.2"
}

type githubReleaseForUpdate struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

/* ---------- singleton state ---------- */

var selfUpdate updateState

func init() {
	selfUpdate.status = "idle"
}

/* ---------- handlers ---------- */

// GetUpdateStatus returns the current self-update progress.
func (h *Handler) GetUpdateStatus(c *gin.Context) {
	selfUpdate.mu.RLock()
	defer selfUpdate.mu.RUnlock()

	c.JSON(
		http.StatusOK, gin.H{
			"status":          selfUpdate.status,
			"message":         selfUpdate.message,
			"target_version":  selfUpdate.version,
			"percent":         selfUpdate.percent,
			"current_version": buildinfo.Version,
		},
	)
}

// PostUpdate triggers a self-update to the specified version.
func (h *Handler) PostUpdate(c *gin.Context) {
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Version) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version is required"})
		return
	}

	if strings.EqualFold(strings.TrimSpace(req.Version), buildinfo.Version) {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "same_version", "message": "Target version is the same as current version"},
		)
		return
	}

	selfUpdate.mu.Lock()
	if selfUpdate.status == "downloading" || selfUpdate.status == "verifying" || selfUpdate.status == "replacing" {
		selfUpdate.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "update_in_progress", "message": "An update is already in progress"})
		return
	}
	selfUpdate.status = "downloading"
	selfUpdate.message = ""
	selfUpdate.version = req.Version
	selfUpdate.percent = 0
	selfUpdate.mu.Unlock()

	repo := h.cpaRepository()
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}

	go h.performUpdate(repo, req.Version, proxyURL)

	c.JSON(http.StatusAccepted, gin.H{"status": "downloading", "target_version": req.Version})
}

/* ---------- update logic ---------- */

func (h *Handler) performUpdate(repo, version, proxyURL string) {
	setStatus := func(status, message string, percent int) {
		selfUpdate.mu.Lock()
		selfUpdate.status = status
		selfUpdate.message = message
		selfUpdate.percent = percent
		selfUpdate.mu.Unlock()
	}

	// 1. Fetch the target release
	release, err := h.fetchRelease(repo, version, proxyURL)
	if err != nil {
		setStatus("error", fmt.Sprintf("fetch release: %v", err), 0)
		return
	}

	// 2. Find the matching binary asset
	assetName := expectedAssetName()
	var downloadURL string
	var checksumURL string
	for _, asset := range release.Assets {
		if strings.EqualFold(asset.Name, assetName) {
			downloadURL = asset.BrowserDownloadURL
		}
		if strings.EqualFold(asset.Name, "checksums.txt") || strings.EqualFold(asset.Name, assetName+".sha256") {
			checksumURL = asset.BrowserDownloadURL
		}
	}

	if downloadURL == "" {
		setStatus("error", fmt.Sprintf("asset %s not found in release %s", assetName, version), 0)
		return
	}

	if checksumURL == "" {
		setStatus(
			"error",
			fmt.Sprintf("checksum file not found in release %s, refusing to update without verification", version),
			0,
		)
		return
	}

	// Validate download URLs point to GitHub — prevent RCE via malicious repository config
	for label, u := range map[string]string{"binary": downloadURL, "checksum": checksumURL} {
		parsed, errParse := url.Parse(u)
		if errParse != nil || parsed.Scheme != "https" {
			setStatus("error", fmt.Sprintf("%s URL is not HTTPS", label), 0)
			return
		}
		host := strings.ToLower(parsed.Hostname())
		if host != "github.com" && !strings.HasSuffix(host, ".github.com") &&
			!strings.HasSuffix(host, ".githubusercontent.com") {
			setStatus("error", fmt.Sprintf("%s URL host %q is not a GitHub domain", label, host), 0)
			return
		}
	}

	setStatus("downloading", "Downloading binary...", 20)

	// 3. Download binary
	client := newUpdateHTTPClient(proxyURL)
	binaryData, err := githubGet(context.Background(), client, downloadURL)
	if err != nil {
		setStatus("error", fmt.Sprintf("download: %v", err), 20)
		return
	}

	setStatus("verifying", "Verifying checksum...", 60)

	// 4. SHA256 verification (mandatory — early return above guarantees checksumURL is non-empty)
	downloadedHash := sha256Hex(binaryData)

	expectedHash, errChecksum := fetchExpectedHash(client, checksumURL, assetName)
	if errChecksum != nil {
		setStatus("error", fmt.Sprintf("checksum verification failed: %v", errChecksum), 60)
		return
	}
	if expectedHash == "" {
		setStatus("error", fmt.Sprintf("checksum file does not contain hash for %s", assetName), 60)
		return
	}
	if !strings.EqualFold(expectedHash, downloadedHash) {
		setStatus("error", fmt.Sprintf("SHA256 mismatch: expected %s, got %s", expectedHash, downloadedHash), 60)
		return
	}

	setStatus("replacing", "Replacing binary...", 80)

	// 5. Replace current binary
	execPath, err := os.Executable()
	if err != nil {
		setStatus("error", fmt.Sprintf("resolve executable path: %v", err), 80)
		return
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		setStatus("error", fmt.Sprintf("resolve symlinks: %v", err), 80)
		return
	}

	if err = replaceBinary(execPath, binaryData); err != nil {
		setStatus("error", fmt.Sprintf("replace binary: %v", err), 80)
		return
	}

	setStatus("done", fmt.Sprintf("Updated to %s. Restart the process to apply.", version), 100)
	log.Infof("self-update: binary replaced successfully (version=%s, sha256=%s)", version, downloadedHash)
}

var validVersion = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func (h *Handler) fetchRelease(repo, version, proxyURL string) (*githubReleaseForUpdate, error) {
	if !validVersion.MatchString(version) {
		return nil, fmt.Errorf("invalid version format: %q", version)
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, version)
	client := newUpdateHTTPClient(proxyURL)
	body, err := githubGet(context.Background(), client, url)
	if err != nil {
		return nil, err
	}
	var release githubReleaseForUpdate
	if err = json.Unmarshal(body, &release); err != nil {
		return nil, err
	}
	return &release, nil
}

/* ---------- helpers ---------- */

func expectedAssetName() string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("CLIProxyAPI_%s_%s%s", goos, goarch, ext)
}

func newUpdateHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			host := strings.ToLower(req.URL.Hostname())
			if host != "github.com" && !strings.HasSuffix(host, ".github.com") &&
				!strings.HasSuffix(host, ".githubusercontent.com") {
				return fmt.Errorf("redirect to non-GitHub domain %q blocked", host)
			}
			return nil
		},
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}
	return client
}

func fetchExpectedHash(client *http.Client, checksumURL, assetName string) (string, error) {
	body, err := githubGet(context.Background(), client, checksumURL)
	if err != nil {
		return "", err
	}

	// Parse checksum file: each line is "hash  filename" or "hash filename"
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && strings.EqualFold(parts[1], assetName) {
			return strings.ToLower(parts[0]), nil
		}
	}

	// If the file contains only a single hash (no filename)
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) == 64 && !strings.ContainsAny(trimmed, " \t\n") {
		return strings.ToLower(trimmed), nil
	}

	return "", nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func replaceBinary(execPath string, newBinary []byte) error {
	dir := filepath.Dir(execPath)

	// Write new binary to a temp file
	tmpFile, err := os.CreateTemp(dir, "cpa-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath) // clean up on failure
	}()

	if _, err = tmpFile.Write(newBinary); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmpFile.Chmod(0o755); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Backup old binary
	backupPath := execPath + ".bak"
	_ = os.Remove(backupPath)
	if err = os.Rename(execPath, backupPath); err != nil {
		return fmt.Errorf("backup old binary: %w", err)
	}

	// Move new binary into place
	if err = os.Rename(tmpPath, execPath); err != nil {
		// Attempt to restore backup
		_ = os.Rename(backupPath, execPath)
		return fmt.Errorf("replace binary: %w", err)
	}

	return nil
}

// PostPanelUpdate triggers an immediate refresh of the management panel HTML asset.
func (h *Handler) PostPanelUpdate(c *gin.Context) {
	staticDir := managementasset.StaticDir(h.configFilePath)
	if staticDir == "" {
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "static_dir_unknown", "message": "Cannot resolve static directory"},
		)
		return
	}

	proxyURL := ""
	panelRepo := ""
	if h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
		panelRepo = strings.TrimSpace(h.cfg.RemoteManagement.PanelGitHubRepository)
	}

	ok := managementasset.EnsureLatestManagementHTML(c.Request.Context(), staticDir, proxyURL, panelRepo)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_failed", "message": "Failed to update management panel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Management panel updated"})
}
