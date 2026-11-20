package managementasset

import (
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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	defaultManagementReleaseURL = "https://api.github.com/repos/Pyrokine/Cli-Proxy-API-Management-Center/" +
		"releases/" +
		"latest"
	managementAssetName        = "management.html"
	httpUserAgent              = "CLIProxyAPI-management-updater"
	managementSyncMinInterval  = 30 * time.Second
	defaultUpdateCheckInterval = 3 * time.Hour
	minCheckIntervalMinutes    = 30
	maxManagementAssetBytes    = 10 << 20
)

// managementFileName exposes the control panel asset filename.
const managementFileName = managementAssetName

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateChecks    = make(map[string]time.Time)
	currentConfigPtr    atomic.Pointer[config.Config]
	schedulerOnce       sync.Once
	schedulerConfigPath atomic.Value
	sfGroup             singleflight.Group
)

// SetCurrentConfig stores the latest configuration snapshot for management asset decisions.
func SetCurrentConfig(cfg *config.Config) {
	if cfg == nil {
		currentConfigPtr.Store(nil)
		return
	}
	currentConfigPtr.Store(cfg)
}

// StartAutoUpdater launches a background goroutine that periodically ensures the management asset is up to date.
// It respects the disable-control-panel flag on every iteration and supports hot-reloaded configurations.
func StartAutoUpdater(ctx context.Context, configFilePath string) {
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		log.Debug("management asset auto-updater skipped: empty config path")
		return
	}

	schedulerConfigPath.Store(configFilePath)

	schedulerOnce.Do(
		func() {
			go runAutoUpdater(ctx)
		},
	)
}

func runAutoUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	currentInterval := time.Duration(0)
	var ticker *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	runOnce := func() {
		cfg := currentConfigPtr.Load()
		if reason, skip := autoUpdateSkipReason(cfg); skip {
			log.Debugf("management asset auto-updater skipped: %s", reason)
			return
		}

		if !cfg.RemoteManagement.IsAutoUpdatePanel() {
			releaseURL := resolveReleaseURL(cfg.RemoteManagement.PanelGitHubRepository, "")
			if _, _, err := fetchReleaseAsset(ctx, newHTTPClient(cfg.ProxyURL), releaseURL); err != nil {
				log.WithError(err).Warn("management asset update check failed")
			}
			return
		}

		configPath, _ := schedulerConfigPath.Load().(string)
		staticDir := StaticDir(configPath)
		EnsureManagementHTML(ctx, staticDir, cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository, "")
	}

	for {
		cfg := currentConfigPtr.Load()
		interval := defaultUpdateCheckInterval
		if cfg != nil && cfg.RemoteManagement.CheckInterval >= minCheckIntervalMinutes {
			interval = time.Duration(cfg.RemoteManagement.CheckInterval) * time.Minute
		}
		if interval != currentInterval {
			if ticker != nil {
				ticker.Stop()
			}
			ticker = time.NewTicker(interval)
			tick = ticker.C
			currentInterval = interval
		}

		runOnce()

		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

func autoUpdateSkipReason(cfg *config.Config) (string, bool) {
	if cfg == nil {
		return "config not yet available", true
	}
	if cfg.Home.Enabled {
		return "cluster mode enabled", true
	}
	if cfg.RemoteManagement.DisableControlPanel {
		return "control panel disabled", true
	}
	if !cfg.RemoteManagement.AutoCheckUpdate {
		return "auto-check-update is false", true
	}
	return "", false
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}

	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)

	return client
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type releaseResponse struct {
	Assets []releaseAsset `json:"assets"`
}

// StaticDir resolves the directory that stores the management control panel asset.
func StaticDir(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return filepath.Dir(cleaned)
		}
		return cleaned
	}

	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "static")
	}

	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}

	base := filepath.Dir(configFilePath)
	fileInfo, err := os.Stat(configFilePath)
	if err == nil {
		if fileInfo.IsDir() {
			base = configFilePath
		}
	}

	return filepath.Join(base, "static")
}

// FilePath resolves the absolute path to the management control panel asset.
func FilePath(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return cleaned
		}
		return filepath.Join(cleaned, managementFileName)
	}

	dir := StaticDir(configFilePath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, managementFileName)
}

// EnsureManagementHTML fetches a specific version (empty version means latest)
// of the management.html asset and updates the local copy when needed. It coalesces
// concurrent sync attempts and returns whether the asset exists after the sync
// attempt. Explicit version requests bypass the hash-equality short-circuit so
// downgrades work, but they still respect the digest verification.
func EnsureManagementHTML(
	ctx context.Context,
	staticDir string,
	proxyURL string,
	panelRepository string,
	version string,
) bool {
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return false
	}
	if _, err := syncManagementHTML(ctx, staticDir, proxyURL, panelRepository, version, false); err != nil {
		log.WithError(err).Warn("failed to synchronize management asset")
	}
	info, err := os.Stat(filepath.Join(staticDir, managementAssetName))
	return err == nil && info.Mode().IsRegular()
}

// updateManagementHTML 返回本次下载、校验和发布的错误，手动更新绕过自动检查节流
func updateManagementHTML(ctx context.Context, staticDir, proxyURL, panelRepository, version string) error {
	_, err := UpdateManagementHTMLWithResult(ctx, staticDir, proxyURL, panelRepository, version)
	return err
}

// UpdateManagementHTMLWithResult 返回文件是否已发布；latest 内容相同时返回 false 和 nil
func UpdateManagementHTMLWithResult(ctx context.Context, staticDir, proxyURL, panelRepository, version string) (
	bool,
	error,
) {
	return syncManagementHTML(ctx, staticDir, proxyURL, panelRepository, version, true)
}

func syncManagementHTML(ctx context.Context, staticDir, proxyURL, panelRepository, version string, force bool) (
	bool,
	error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		return false, errors.New("management asset static directory is empty")
	}
	localPath := filepath.Join(staticDir, managementAssetName)
	version = strings.TrimSpace(version)
	releaseURL := resolveReleaseURL(panelRepository, version)
	key := fmt.Sprintf("%q|%q|%q", localPath, releaseURL, strings.TrimSpace(proxyURL))
	result := sfGroup.DoChan(
		fmt.Sprintf("%s|%t", key, force), func() (any, error) {
			lastUpdateCheckMu.Lock()
			now := time.Now()
			for target, checkedAt := range lastUpdateChecks {
				if now.Sub(checkedAt) >= managementSyncMinInterval {
					delete(lastUpdateChecks, target)
				}
			}
			lastAttempt := lastUpdateChecks[key]
			if !force && version == "" && !lastAttempt.IsZero() &&
				now.Sub(lastAttempt) < managementSyncMinInterval {
				lastUpdateCheckMu.Unlock()
				log.Debug("management asset automatic sync skipped by throttle")
				return false, nil
			}
			lastUpdateChecks[key] = now
			lastUpdateCheckMu.Unlock()

			if err := os.MkdirAll(staticDir, 0o755); err != nil {
				return false, fmt.Errorf("prepare management asset directory: %w", err)
			}
			client := newHTTPClient(proxyURL)
			localHash, err := fileSHA256(localPath)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					log.WithError(err).Debug("failed to read local management asset hash")
				}
				localHash = ""
			}
			asset, remoteHash, err := fetchReleaseAsset(ctx, client, releaseURL)
			if err != nil {
				return false, fmt.Errorf("fetch management release: %w", err)
			}
			if remoteHash == "" {
				return false, errors.New("management asset release has no digest")
			}
			if version == "" && localHash != "" && strings.EqualFold(remoteHash, localHash) {
				log.Debug("management asset is already up to date")
				return false, nil
			}
			data, downloadedHash, err := downloadAsset(ctx, client, asset.BrowserDownloadURL)
			if err != nil {
				return false, fmt.Errorf("download management asset: %w", err)
			}
			if !strings.EqualFold(remoteHash, downloadedHash) {
				return false, fmt.Errorf(
					"management asset digest mismatch: expected %s, got %s", remoteHash, downloadedHash,
				)
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if err := atomicWriteFile(localPath, data); err != nil {
				return false, fmt.Errorf("publish management asset: %w", err)
			}
			log.Infof("management asset updated successfully (hash=%s)", downloadedHash)
			return true, nil
		},
	)
	select {
	case completed := <-result:
		return completed.Val.(bool), completed.Err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// resolveReleaseURL turns a repository reference into the GitHub releases API URL.
// An empty version returns the "/releases/latest" endpoint; a non-empty version
// returns "/releases/tags/{version}".
func resolveReleaseURL(repo, version string) string {
	repo = strings.TrimSpace(repo)
	version = strings.TrimSpace(version)

	suffix := "/releases/latest"
	if version != "" {
		suffix = "/releases/tags/" + url.PathEscape(version)
	}

	if repo == "" {
		if version == "" {
			return defaultManagementReleaseURL
		}
		// Derive host-specific form from the default latest URL.
		return strings.TrimSuffix(defaultManagementReleaseURL, "/releases/latest") + suffix
	}

	parsed, err := url.Parse(repo)
	if err != nil || parsed.Host == "" {
		return strings.TrimSuffix(defaultManagementReleaseURL, "/releases/latest") + suffix
	}

	host := strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")

	if host == "api.github.com" {
		// Strip any trailing /releases/... so we always land on the right endpoint.
		parsed.Path = strings.TrimSuffix(parsed.Path, "/releases/latest")
		parsed.Path = strings.TrimPrefix(parsed.Path, "/")
		parsed.Path = "/" + parsed.Path + suffix
		return parsed.String()
	}

	if host == "github.com" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			repoName := strings.TrimSuffix(parts[1], ".git")
			return fmt.Sprintf("https://api.github.com/repos/%s/%s%s", parts[0], repoName, suffix)
		}
	}

	return strings.TrimSuffix(defaultManagementReleaseURL, "/releases/latest") + suffix
}

func gitHubReleaseURL(releaseURL string) bool {
	parsed, errParse := url.Parse(strings.TrimSpace(releaseURL))
	return errParse == nil && parsed.User == nil && strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Hostname(), "api.github.com") &&
		(parsed.Port() == "" || parsed.Port() == "443")
}

func fetchReleaseAsset(
	ctx context.Context,
	client *http.Client,
	releaseURL string,
) (*releaseAsset, string, error) {
	if strings.TrimSpace(releaseURL) == "" {
		releaseURL = defaultManagementReleaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", httpUserAgent)
	if gitHubReleaseURL(releaseURL) {
		if token := util.GitHubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute release request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf(
			"unexpected release status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	var release releaseResponse
	if err = json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, "", fmt.Errorf("decode release response: %w", err)
	}

	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, managementAssetName) {
			remoteHash := parseDigest(asset.Digest)
			return asset, remoteHash, nil
		}
	}

	return nil, "", fmt.Errorf("management asset %s not found in latest release", managementAssetName)
}

func downloadAsset(ctx context.Context, client *http.Client, downloadURL string) ([]byte, string, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, "", fmt.Errorf("empty download url")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", httpUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute download request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf(
			"unexpected download status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManagementAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read download body: %w", err)
	}
	if len(data) > maxManagementAssetBytes {
		return nil, "", fmt.Errorf("management asset download exceeds %d bytes", maxManagementAssetBytes)
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()

	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func atomicWriteFile(path string, data []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if err != nil {
		return err
	}

	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err = tmpFile.Write(data); err != nil {
		return err
	}

	if err = tmpFile.Chmod(0o644); err != nil {
		return err
	}

	if err = tmpFile.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, path); err != nil {
		return err
	}

	return nil
}

func parseDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return ""
	}

	if idx := strings.Index(digest, ":"); idx >= 0 {
		digest = digest[idx+1:]
	}

	return strings.ToLower(strings.TrimSpace(digest))
}
