// Last compiled: 2026-05-14
// Author: pyro

package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
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

type updateCompatibilityResponse struct {
	CurrentVersion  string                   `json:"current_version"`
	TargetVersion   string                   `json:"target_version"`
	MinPanelVersion string                   `json:"min_panel_version"`
	RequiresRestart bool                     `json:"requires_restart"`
	Compatible      bool                     `json:"compatible"`
	Warnings        []string                 `json:"warnings,omitempty"`
	Usage           updateUsageCompatibility `json:"usage"`
}

type updateUsageCompatibility struct {
	ConfiguredDataDir string     `json:"configured_data_dir,omitempty"`
	ResolvedDataDir   string     `json:"resolved_data_dir,omitempty"`
	DBPath            string     `json:"db_path,omitempty"`
	DBExists          bool       `json:"db_exists"`
	PersisterReady    bool       `json:"persister_ready"`
	DBSizeBytes       int64      `json:"db_size_bytes"`
	SchemaVersion     string     `json:"schema_version,omitempty"`
	MigratedFrom      string     `json:"migrated_from,omitempty"`
	MigratedAt        *time.Time `json:"migrated_at,omitempty"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubReleaseForUpdate struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

const (
	defaultBackendUpdateCheckInterval = 3 * time.Hour
	minBackendUpdateCheckMinutes      = 30
)

/* ---------- singleton state ---------- */

var (
	selfUpdate         updateState
	cpaAutoUpdaterMu   sync.Mutex
	cpaAutoUpdaterDone context.CancelFunc
)

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

// GetUpdateCompatibility returns real backend upgrade compatibility signals.
func (h *Handler) GetUpdateCompatibility(c *gin.Context) {
	targetVersion := strings.TrimSpace(c.Query("version"))
	if targetVersion == "" {
		targetVersion = strings.TrimSpace(c.Query("target_version"))
	}
	compat := h.buildUpdateCompatibility(c.Request.Context(), targetVersion)
	c.JSON(http.StatusOK, compat)
}

// PostUpdate triggers a self-update to the specified version.
func (h *Handler) PostUpdate(c *gin.Context) {
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Version) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "version is required"})
		return
	}

	targetVersion := strings.TrimSpace(req.Version)
	if strings.EqualFold(targetVersion, buildinfo.Version) {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "same_version", "message": "Target version is the same as current version"},
		)
		return
	}

	compat := h.buildUpdateCompatibility(c.Request.Context(), targetVersion)
	if !compat.Compatible {
		c.JSON(
			http.StatusConflict,
			gin.H{
				"error":         "update_incompatible",
				"message":       "Target version requires manual migration review before update",
				"compatibility": compat,
			},
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
	selfUpdate.version = targetVersion
	selfUpdate.percent = 0
	selfUpdate.mu.Unlock()

	repo := h.cpaRepository()
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}

	go h.performUpdate(repo, targetVersion, proxyURL)

	c.JSON(
		http.StatusAccepted, gin.H{"status": "downloading", "target_version": targetVersion, "compatibility": compat},
	)
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	release, err := h.fetchRelease(repo, version, proxyURL)
	if err != nil {
		setStatus("error", fmt.Sprintf("fetch release: %v", err), 0)
		return
	}

	assetNames := expectedArchiveAssetNames(version)
	asset, checksumAsset := findReleaseAssets(release.Assets, assetNames)
	if asset == nil {
		setStatus("error", fmt.Sprintf("asset %s not found in release %s", strings.Join(assetNames, " or "), version), 0)
		return
	}
	if checksumAsset == nil {
		setStatus("error", fmt.Sprintf("checksums.txt not found in release %s", version), 0)
		return
	}

	for label, u := range map[string]string{
		"archive": asset.BrowserDownloadURL, "checksum": checksumAsset.BrowserDownloadURL,
	} {
		if err := validateGitHubDownloadURL(label, u); err != nil {
			setStatus("error", err.Error(), 0)
			return
		}
	}

	setStatus("downloading", "Downloading release archive...", 20)
	client := newUpdateHTTPClient(proxyURL)
	archiveData, err := githubGet(ctx, client, asset.BrowserDownloadURL)
	if err != nil {
		setStatus("error", fmt.Sprintf("download archive: %v", err), 20)
		return
	}

	setStatus("verifying", "Verifying checksum...", 55)
	downloadedHash := sha256Hex(archiveData)
	expectedHash, err := fetchExpectedHash(ctx, client, checksumAsset.BrowserDownloadURL, asset.Name)
	if err != nil {
		setStatus("error", fmt.Sprintf("checksum verification failed: %v", err), 55)
		return
	}
	if expectedHash == "" {
		setStatus("error", fmt.Sprintf("checksum file does not contain hash for %s", asset.Name), 55)
		return
	}
	if !strings.EqualFold(expectedHash, downloadedHash) {
		setStatus("error", fmt.Sprintf("SHA256 mismatch: expected %s, got %s", expectedHash, downloadedHash), 55)
		return
	}

	setStatus("replacing", "Extracting archive...", 75)
	binaryData, err := extractBinaryFromArchive(asset.Name, archiveData)
	if err != nil {
		setStatus("error", fmt.Sprintf("extract binary: %v", err), 75)
		return
	}

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

	setStatus("replacing", "Replacing binary...", 90)
	if err = replaceBinary(execPath, binaryData); err != nil {
		setStatus("error", fmt.Sprintf("replace binary: %v", err), 90)
		return
	}

	setStatus("replacing", fmt.Sprintf("Updated to %s, restarting service...", version), 100)
	log.Infof(
		"self-update: binary replaced successfully (version=%s, archive=%s, sha256=%s)", version, asset.Name,
		downloadedHash,
	)
	if err = restartCurrentProcess(); err != nil {
		setStatus("error", fmt.Sprintf("restart process: %v", err), 100)
		return
	}
}

func restartCurrentProcess() error {
	if os.Getenv("INVOCATION_ID") != "" {
		cmd := exec.Command("systemctl", "restart", "cli-proxy-api.service")
		return cmd.Run()
	}
	return fmt.Errorf("service restart is not supported outside systemd")
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

func (h *Handler) StartCPAAutoUpdater(ctx context.Context) {
	if h == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cpaAutoUpdaterMu.Lock()
	if cpaAutoUpdaterDone != nil {
		cpaAutoUpdaterDone()
	}
	runCtx, cancel := context.WithCancel(ctx)
	cpaAutoUpdaterDone = cancel
	cpaAutoUpdaterMu.Unlock()

	go h.runCPAAutoUpdater(runCtx)
}

func (h *Handler) StopCPAAutoUpdater() {
	cpaAutoUpdaterMu.Lock()
	cancel := cpaAutoUpdaterDone
	cpaAutoUpdaterDone = nil
	cpaAutoUpdaterMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Handler) runCPAAutoUpdater(ctx context.Context) {
	if h == nil {
		return
	}
	interval := defaultBackendUpdateCheckInterval
	var ticker *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	for {
		cfg := h.cfg
		if cfg != nil && cfg.RemoteManagement.CheckInterval >= minBackendUpdateCheckMinutes {
			interval = time.Duration(cfg.RemoteManagement.CheckInterval) * time.Minute
		}
		if ticker == nil {
			ticker = time.NewTicker(interval)
			tick = ticker.C
		}

		h.tryAutoUpdateCPA(ctx)

		select {
		case <-ctx.Done():
			return
		case <-tick:
			if cfg := h.cfg; cfg != nil {
				next := defaultBackendUpdateCheckInterval
				if cfg.RemoteManagement.CheckInterval >= minBackendUpdateCheckMinutes {
					next = time.Duration(cfg.RemoteManagement.CheckInterval) * time.Minute
				}
				if next != interval {
					interval = next
					ticker.Stop()
					ticker = time.NewTicker(interval)
					tick = ticker.C
				}
			}
		}
	}
}

func (h *Handler) tryAutoUpdateCPA(ctx context.Context) {
	if h == nil || h.cfg == nil {
		return
	}
	if !h.cfg.RemoteManagement.AutoCheckUpdate || !h.cfg.RemoteManagement.AutoUpdateCPA {
		return
	}
	if selfUpdate.status == "downloading" || selfUpdate.status == "verifying" || selfUpdate.status == "replacing" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	latest, err := h.fetchLatestReleaseVersion(ctx)
	if err != nil {
		log.Warnf("cpa auto-update: fetch latest release failed: %v", err)
		return
	}
	if latest == "" || strings.EqualFold(strings.TrimSpace(latest), buildinfo.Version) {
		return
	}
	compat := h.buildUpdateCompatibility(ctx, latest)
	if !compat.Compatible {
		log.Warnf("cpa auto-update: blocked by compatibility checks for %s: %v", latest, compat.Warnings)
		return
	}

	selfUpdate.mu.Lock()
	busy := selfUpdate.status == "downloading" || selfUpdate.status == "verifying" || selfUpdate.status == "replacing"
	if !busy {
		selfUpdate.status = "downloading"
		selfUpdate.message = ""
		selfUpdate.version = latest
		selfUpdate.percent = 0
	}
	selfUpdate.mu.Unlock()
	if busy {
		return
	}

	go h.performUpdate(h.cpaRepository(), latest, strings.TrimSpace(h.cfg.ProxyURL))
}

func (h *Handler) fetchLatestReleaseVersion(ctx context.Context) (string, error) {
	client := newUpdateHTTPClient(strings.TrimSpace(h.cfg.ProxyURL))
	body, err := githubGet(
		ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", h.cpaRepository()),
	)
	if err != nil {
		return "", err
	}
	var info releaseInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		return "", fmt.Errorf("missing latest release version")
	}
	return version, nil
}

func (h *Handler) buildUpdateCompatibility(ctx context.Context, targetVersion string) updateCompatibilityResponse {
	compat := updateCompatibilityResponse{
		CurrentVersion:  buildinfo.Version,
		TargetVersion:   strings.TrimSpace(targetVersion),
		MinPanelVersion: minPanelVersion,
		RequiresRestart: true,
		Compatible:      true,
	}

	if ctx == nil {
		ctx = context.Background()
	}
	cfg := h.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	compat.Usage.ConfiguredDataDir = strings.TrimSpace(cfg.UsageDataDir)
	compat.Usage.ResolvedDataDir = usage.ResolveDataDir(cfg.UsageDataDir, h.configFilePath)
	if compat.Usage.ResolvedDataDir != "" {
		compat.Usage.DBPath = filepath.Join(compat.Usage.ResolvedDataDir, "events.db")
		if info, err := os.Stat(compat.Usage.DBPath); err == nil && !info.IsDir() {
			compat.Usage.DBExists = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to stat usage db: %v", err))
		}
	}
	if h == nil || h.usagePersister == nil {
		if compat.Usage.DBExists {
			compat.Compatible = false
			compat.Warnings = append(compat.Warnings, "usage db exists but persister is unavailable in this process")
		}
		return compat
	}

	compat.Usage.PersisterReady = h.usagePersister.IsReady()
	compat.Usage.DBSizeBytes = h.usagePersister.DBSize()
	if !compat.Usage.PersisterReady {
		if compat.Usage.DBExists {
			compat.Compatible = false
			compat.Warnings = append(
				compat.Warnings, "usage persister is not ready; update would hide schema compatibility failures",
			)
		}
		return compat
	}

	schemaVersion, err := h.usagePersister.SchemaVersion(ctx)
	if err != nil {
		compat.Compatible = false
		compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to read usage schema version: %v", err))
	} else {
		compat.Usage.SchemaVersion = schemaVersion
		if schemaVersion != "" && schemaVersion != "2" {
			compat.Compatible = false
			compat.Warnings = append(compat.Warnings, fmt.Sprintf("usage schema_version=%s, expected 2", schemaVersion))
		}
	}

	migrationStatus, err := h.usagePersister.MigrationStatus(ctx)
	if err != nil {
		compat.Warnings = append(compat.Warnings, fmt.Sprintf("failed to read usage migration marker: %v", err))
	} else {
		compat.Usage.MigratedFrom = migrationStatus.From
		if !migrationStatus.At.IsZero() {
			migratedAt := migrationStatus.At
			compat.Usage.MigratedAt = &migratedAt
		}
	}

	return compat
}

/* ---------- helpers ---------- */

func expectedArchiveAssetNames(version string) []string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	format := "tar.gz"
	if goos == "windows" {
		format = "zip"
	}
	baseName := fmt.Sprintf("CLIProxyAPI_%s_%s.%s", goos, goarch, format)
	normalizedVersion := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if normalizedVersion == "" {
		return []string{baseName}
	}
	return []string{fmt.Sprintf("CLIProxyAPI_%s_%s_%s.%s", normalizedVersion, goos, goarch, format), baseName}
}

func findReleaseAssets(assets []githubReleaseAsset, archiveNames []string) (*githubReleaseAsset, *githubReleaseAsset) {
	var archiveAsset *githubReleaseAsset
	var checksumAsset *githubReleaseAsset
	for i := range assets {
		asset := &assets[i]
		for _, archiveName := range archiveNames {
			if strings.EqualFold(asset.Name, archiveName) {
				archiveAsset = asset
				break
			}
		}
		if strings.EqualFold(asset.Name, "checksums.txt") {
			checksumAsset = asset
		}
	}
	return archiveAsset, checksumAsset
}

func validateGitHubDownloadURL(label, rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return fmt.Errorf("%s URL is not HTTPS", label)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".github.com") && !strings.HasSuffix(
		host, ".githubusercontent.com",
	) {
		return fmt.Errorf("%s URL host %q is not a GitHub domain", label, host)
	}
	return nil
}

func extractBinaryFromArchive(assetName string, archiveData []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(assetName), ".tar.gz"):
		return extractBinaryFromTarGz(archiveData)
	case strings.HasSuffix(strings.ToLower(assetName), ".zip"):
		return extractBinaryFromZip(archiveData)
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", assetName)
	}
}

func extractBinaryFromTarGz(archiveData []byte) ([]byte, error) {
	gzReader, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, fmt.Errorf("open tar.gz: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if header == nil || header.Typeflag != tar.TypeReg {
			continue
		}
		if !isArchiveBinaryPath(header.Name) {
			continue
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("read binary entry: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("binary entry not found in tar.gz archive")
}

func extractBinaryFromZip(archiveData []byte) ([]byte, error) {
	zipReader, err := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range zipReader.File {
		if file == nil || file.FileInfo().IsDir() || !isArchiveBinaryPath(file.Name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry: %w", err)
		}
		data, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read zip entry: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close zip entry: %w", closeErr)
		}
		return data, nil
	}
	return nil, fmt.Errorf("binary entry not found in zip archive")
}

func isArchiveBinaryPath(name string) bool {
	base := strings.ToLower(path.Base(strings.TrimSpace(name)))
	return base == "cli-proxy-api" || base == "cli-proxy-api.exe"
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

func fetchExpectedHash(ctx context.Context, client *http.Client, checksumURL, assetName string) (string, error) {
	body, err := githubGet(ctx, client, checksumURL)
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

// PostPanelUpdate triggers a panel HTML asset refresh. An optional `version`
// field in the request body pins the update to a specific release tag; when
// empty the latest release is used.
func (h *Handler) PostPanelUpdate(c *gin.Context) {
	if h.cfg != nil && h.cfg.RemoteManagement.DisableControlPanel {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "control_panel_disabled", "message": "Control panel is disabled in configuration"},
		)
		return
	}

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

	var req struct {
		Version string `json:"version"`
	}
	_ = c.ShouldBindJSON(&req)
	version := strings.TrimSpace(req.Version)

	ok := managementasset.EnsureManagementHTML(c.Request.Context(), staticDir, proxyURL, panelRepo, version)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_failed", "message": "Failed to update management panel"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "Management panel updated"})
}
