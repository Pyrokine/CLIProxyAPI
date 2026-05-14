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
	defaultManagementReleaseURL = "https://api.github.com/repos/Pyrokine/Cli-Proxy-API-Management-Center/releases/latest"
	managementAssetName         = "management.html"
	httpUserAgent               = "CLIProxyAPI-management-updater"
	managementSyncMinInterval   = 30 * time.Second
	defaultUpdateCheckInterval  = 3 * time.Hour
	minCheckIntervalMinutes     = 30
)

// managementFileName exposes the control panel asset filename.
const managementFileName = managementAssetName

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateCheckTime time.Time
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
		if cfg == nil {
			log.Debug("management asset auto-updater skipped: config not yet available")
			return
		}
		if cfg.RemoteManagement.DisableControlPanel {
			log.Debug("management asset auto-updater skipped: control panel disabled")
			return
		}
		if !cfg.RemoteManagement.AutoCheckUpdate {
			log.Debug("management asset auto-updater skipped: auto-check-update is false")
			return
		}
		if !cfg.RemoteManagement.IsAutoUpdatePanel() {
			log.Debug("management asset auto-updater skipped: auto-update-panel is false")
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
	if ctx == nil {
		ctx = context.Background()
	}

	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return false
	}
	localPath := filepath.Join(staticDir, managementAssetName)
	version = strings.TrimSpace(version)

	_, _, _ = sfGroup.Do(
		localPath, func() (any, error) {
			lastUpdateCheckMu.Lock()
			now := time.Now()
			timeSinceLastAttempt := now.Sub(lastUpdateCheckTime)
			if version == "" && !lastUpdateCheckTime.IsZero() && timeSinceLastAttempt < managementSyncMinInterval {
				lastUpdateCheckMu.Unlock()
				log.Debugf(
					"management asset sync skipped by throttle: last attempt %v ago (interval %v)",
					timeSinceLastAttempt.Round(time.Second),
					managementSyncMinInterval,
				)
				return nil, nil
			}
			lastUpdateCheckTime = now
			lastUpdateCheckMu.Unlock()

			localFileMissing := false
			if _, errStat := os.Stat(localPath); errStat != nil {
				if errors.Is(errStat, os.ErrNotExist) {
					localFileMissing = true
				} else {
					log.WithError(errStat).Debug("failed to stat local management asset")
				}
			}

			if errMkdirAll := os.MkdirAll(staticDir, 0o755); errMkdirAll != nil {
				log.WithError(errMkdirAll).Warn("failed to prepare static directory for management asset")
				return nil, nil
			}

			releaseURL := resolveReleaseURL(panelRepository, version)
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
				if localFileMissing {
					log.WithError(err).
						Warn("failed to fetch management release information; fallback download is disabled for security (no digest verification)")
				} else {
					log.WithError(err).Warn("failed to fetch management release information")
				}
				return nil, nil
			}

			// Only short-circuit when caller wants "latest" and hashes match. Explicit
			// version requests (downgrade/sidegrade) must always replace the file.
			if version == "" && remoteHash != "" && localHash != "" && strings.EqualFold(remoteHash, localHash) {
				log.Debug("management asset is already up to date")
				return nil, nil
			}

			data, downloadedHash, err := downloadAsset(ctx, client, asset.BrowserDownloadURL)
			if err != nil {
				if localFileMissing {
					log.WithError(err).Warn("failed to download management asset; fallback download is disabled for security (no digest verification)")
				} else {
					log.WithError(err).Warn("failed to download management asset")
				}
				return nil, nil
			}

			// Fail-closed: reject update if remote digest is missing or mismatched
			if remoteHash == "" {
				log.Warn("management asset release has no digest; rejecting update (fail-closed)")
				return nil, nil
			}
			if !strings.EqualFold(remoteHash, downloadedHash) {
				log.Errorf(
					"remote digest mismatch for management asset: expected %s got %s (rejecting update)", remoteHash,
					downloadedHash,
				)
				return nil, nil
			}

			if err = atomicWriteFile(localPath, data); err != nil {
				log.WithError(err).Warn("failed to update management asset on disk")
				return nil, nil
			}

			log.Infof("management asset updated successfully (hash=%s)", downloadedHash)
			return nil, nil
		},
	)

	_, err := os.Stat(localPath)
	return err == nil
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

func fetchReleaseAsset(ctx context.Context, client *http.Client, releaseURL string) (*releaseAsset, string, error) {
	if strings.TrimSpace(releaseURL) == "" {
		releaseURL = defaultManagementReleaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", httpUserAgent)
	gitURL := strings.ToLower(strings.TrimSpace(os.Getenv("GITSTORE_GIT_URL")))
	if tok := strings.TrimSpace(os.Getenv("GITSTORE_GIT_TOKEN")); tok != "" && strings.Contains(gitURL, "github.com") {
		req.Header.Set("Authorization", "Bearer "+tok)
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
		return nil, "", fmt.Errorf("unexpected release status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return nil, "", fmt.Errorf("read download body: %w", err)
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
