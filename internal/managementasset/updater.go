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
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	managementAssetName        = "management.html"
	httpUserAgent              = "CLIProxyAPI-management-updater"
	managementSyncMinInterval  = 30 * time.Second
	defaultUpdateCheckInterval = 3 * time.Hour
	minCheckIntervalMinutes    = 30
	maxManagementAssetBytes    = 10 << 20
	panelRequestTimeout        = 15 * time.Second
	githubAPIHost              = "api.github.com"
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
			if _, _, err := getReleaseAsset(
				ctx, cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository, "",
			); err != nil {
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

// NewReleaseHTTPClient limits metadata redirects to the configured release API origin.
func NewReleaseHTTPClient(proxyURL string, source *url.URL) *http.Client {
	client := &http.Client{
		Timeout: panelRequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || !AllowsReleaseOrigin(source, req.URL) {
				return errors.New("management release redirect rejected")
			}
			return nil
		},
	}
	return util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}, client)
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type releaseResponse struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
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

// PrepareManagementHTML installs a missing panel when automatic installation is enabled.
// It runs independently of the periodic update check and never replaces an existing file.
func PrepareManagementHTML(ctx context.Context, configFilePath string, cfg *config.Config) {
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel ||
		!cfg.RemoteManagement.IsAutoUpdatePanel() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	localPath := FilePath(configFilePath)
	if localPath == "" {
		return
	}
	if _, err := os.Lstat(localPath); err == nil {
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		log.WithError(err).Warn("failed to check for an existing management asset")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*panelRequestTimeout)
	defer cancel()
	source, err := ResolveReleasesURL(cfg.RemoteManagement.PanelGitHubRepository)
	if err == nil {
		_, err = installManagementHTML(ctx, localPath, cfg.ProxyURL, "", source, true)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.WithError(err).Warn("failed to prepare management asset")
	}
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

func syncManagementHTML(
	ctx context.Context, staticDir, proxyURL, panelRepository, version string, force bool,
) (bool, error) {
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
	source, err := ResolveReleasesURL(panelRepository)
	if err != nil {
		return false, err
	}
	releaseURL := releaseURLForVersion(source, version)
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

			return installManagementHTML(ctx, localPath, proxyURL, version, source, false)
		},
	)
	select {
	case completed := <-result:
		return completed.Val.(bool), completed.Err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func installManagementHTML(
	ctx context.Context, localPath, proxyURL, version string, source *url.URL, onlyWhenMissing bool,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return false, fmt.Errorf("prepare management asset directory: %w", err)
	}
	client := NewReleaseHTTPClient(proxyURL, source)
	asset, remoteHash, err := fetchReleaseAsset(ctx, client, releaseURLForVersion(source, version), version)
	if err != nil {
		return false, fmt.Errorf("fetch management release: %w", err)
	}
	if version == "" && !onlyWhenMissing {
		localHash, err := fileSHA256(localPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.WithError(err).Debug("failed to read local management asset hash")
		}
		if localHash == remoteHash {
			log.Debug("management asset is already up to date")
			return false, nil
		}
	}
	data, downloadedHash, err := downloadAsset(ctx, client, source, asset.BrowserDownloadURL)
	if err != nil {
		return false, fmt.Errorf("download management asset: %w", err)
	}
	if remoteHash != downloadedHash {
		return false, fmt.Errorf("management asset digest mismatch: expected %s, got %s", remoteHash, downloadedHash)
	}
	if err := atomicWriteFile(ctx, localPath, data, onlyWhenMissing); err != nil {
		if onlyWhenMissing && errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publish management asset: %w", err)
	}
	log.Infof("management asset updated successfully (hash=%s)", downloadedHash)
	return true, nil
}

var repositoryPart = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ResolveReleasesURL returns the releases collection shared by listing and installation.
func ResolveReleasesURL(repository string) (*url.URL, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		repository = config.DefaultPanelGitHubRepository
	}
	invalid := errors.New("invalid management panel repository")
	parsed, err := url.Parse(repository)
	if err != nil {
		return nil, invalid
	}
	endpoint := &url.URL{Scheme: "https", Host: githubAPIHost}
	prefix := "/repos/"
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if parsed.Host == "" {
		if parsed.Scheme != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
			parsed.RawPath != "" || len(parts) != 2 {
			return nil, invalid
		}
	} else {
		if err := proxyutil.ValidateHTTPURL(repository); err != nil || parsed.Hostname() == "" {
			return nil, invalid
		}
		host := strings.ToLower(parsed.Hostname())
		web := host == "github.com" || host == "www.github.com"
		if (web || host == githubAPIHost) && !defaultHTTPSPort(parsed) &&
			!(parsed.Scheme == "http" && parsed.Port() == "80") {
			return nil, invalid
		}
		if web && len(parts) >= 2 {
			parts[1] = strings.TrimSuffix(parts[1], ".git")
		}
		if !web {
			if parsed.Fragment != "" {
				return nil, invalid
			}
			if host != githubAPIHost {
				// A configured GitHub Enterprise or compatible API may use a private origin.
				if !isHTTPSReleaseURL(parsed) {
					return nil, invalid
				}
				endpoint.Host = parsed.Host
				if strings.HasPrefix(parsed.Path, "/api/v3/repos/") {
					prefix = "/api/v3/repos/"
				}
			}
			repoPath, ok := strings.CutPrefix(parsed.Path, prefix)
			if !ok {
				return nil, invalid
			}
			parts = strings.Split(strings.TrimSuffix(repoPath, "/"), "/")
			if len(parts) > 2 && parts[2] != "releases" {
				return nil, invalid
			}
			if _, err := url.ParseQuery(parsed.RawQuery); err != nil {
				return nil, invalid
			}
			endpoint.User, endpoint.RawQuery = parsed.User, parsed.RawQuery
		}
	}
	if len(parts) < 2 {
		return nil, invalid
	}
	owner, repo := parts[0], parts[1]
	if !validRepositoryPart(owner) || !validRepositoryPart(repo) {
		return nil, invalid
	}
	endpoint.Path = prefix + owner + "/" + repo + "/releases"
	return endpoint, nil
}

func validRepositoryPart(part string) bool {
	return part != "." && part != ".." && repositoryPart.MatchString(part)
}

func releaseURLForVersion(source *url.URL, version string) string {
	target := *source
	if version == "" {
		target.Path += "/latest"
	} else {
		target.Path += "/tags/" + version
		target.RawPath = source.EscapedPath() + "/tags/" + url.PathEscape(version)
	}
	return target.String()
}

func isHTTPSReleaseURL(target *url.URL) bool {
	return target.Scheme == "https" && target.Opaque == "" && target.Hostname() != "" && target.Fragment == ""
}

func defaultHTTPSPort(target *url.URL) bool {
	return target.Port() == "" || target.Port() == "443"
}

func githubAPIOrigin(target *url.URL) bool {
	return isHTTPSReleaseURL(target) && strings.EqualFold(target.Hostname(), githubAPIHost) && defaultHTTPSPort(target)
}

// IsGitHubAPIRequestURL reports whether a URL may receive the GitHub API token.
func IsGitHubAPIRequestURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && parsed.User == nil && githubAPIOrigin(parsed)
}

// AllowsReleaseOrigin checks the configured HTTPS origin for metadata redirects and continuations.
func AllowsReleaseOrigin(source, target *url.URL) bool {
	return isHTTPSReleaseURL(target) && strings.EqualFold(source.Hostname(), target.Hostname()) &&
		(source.Port() == target.Port() || defaultHTTPSPort(source) && defaultHTTPSPort(target)) &&
		(target.User == nil || source.User != nil && source.User.String() == target.User.String())
}

func allowsAssetTarget(source, target *url.URL) bool {
	if !isHTTPSReleaseURL(target) || target.User != nil {
		return false
	}
	if !githubAPIOrigin(source) {
		return AllowsReleaseOrigin(source, target)
	}
	if !defaultHTTPSPort(target) {
		return false
	}
	switch strings.ToLower(target.Hostname()) {
	case "github.com":
		parts := strings.Split(strings.TrimPrefix(target.EscapedPath(), "/"), "/")
		return len(parts) == 6 && path.Clean(target.Path) == target.Path &&
			validRepositoryPart(parts[0]) && validRepositoryPart(parts[1]) &&
			parts[2] == "releases" && parts[3] == "download" && parts[4] != "" &&
			strings.EqualFold(parts[5], managementAssetName)
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	default:
		return false
	}
}

func getReleaseAsset(ctx context.Context, proxyURL, repository, version string) (*releaseAsset, string, error) {
	source, err := ResolveReleasesURL(repository)
	if err != nil {
		return nil, "", err
	}
	return fetchReleaseAsset(
		ctx, NewReleaseHTTPClient(proxyURL, source), releaseURLForVersion(source, version), version,
	)
}

func fetchReleaseAsset(
	ctx context.Context,
	client *http.Client,
	releaseURL, version string,
) (*releaseAsset, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, "", errors.New("invalid management release request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", httpUserAgent)
	if IsGitHubAPIRequestURL(releaseURL) {
		if token := util.GitHubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute release request: %w", withoutRequestURL(err))
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected release status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManagementAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read release response: %w", err)
	}
	if len(body) > maxManagementAssetBytes {
		return nil, "", errors.New("management release response is too large")
	}
	var release releaseResponse
	if err = json.Unmarshal(body, &release); err != nil {
		return nil, "", errors.New("invalid management release response")
	}
	if version != "" && release.TagName != version {
		return nil, "", errors.New("management release tag does not match the requested version")
	}

	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, managementAssetName) {
			remoteHash, err := parseDigest(asset.Digest)
			return asset, remoteHash, err
		}
	}

	return nil, "", fmt.Errorf("management asset %s not found in release", managementAssetName)
}

func downloadAsset(ctx context.Context, client *http.Client, source *url.URL, downloadURL string) (
	[]byte,
	string,
	error,
) {
	target, err := url.Parse(strings.TrimSpace(downloadURL))
	if err != nil || !allowsAssetTarget(source, target) {
		return nil, "", errors.New("management asset download target rejected")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", errors.New("invalid management asset download request")
	}
	req.Header.Set("User-Agent", httpUserAgent)

	assetClient := *client
	assetClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowsAssetTarget(source, req.URL) {
			return errors.New("management asset redirect rejected")
		}
		return nil
	}
	resp, err := assetClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute download request: %w", withoutRequestURL(err))
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected download status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManagementAssetBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read download body: %w", err)
	}
	if len(data) > maxManagementAssetBytes {
		return nil, "", fmt.Errorf("management asset download exceeds %d bytes", maxManagementAssetBytes)
	}
	if len(data) == 0 {
		return nil, "", errors.New("management asset download is empty")
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func withoutRequestURL(err error) error {
	if requestErr, ok := errors.AsType[*url.Error](err); ok {
		return requestErr.Err
	}
	return err
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

func atomicWriteFile(ctx context.Context, path string, data []byte, onlyWhenMissing bool) error {
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

	if err := ctx.Err(); err != nil {
		return err
	}
	if onlyWhenMissing {
		return os.Link(tmpName, path)
	}
	return os.Rename(tmpName, path)
}

func parseDigest(digest string) (string, error) {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return "", errors.New("management asset release has no digest")
	}
	if algorithm, hash, ok := strings.Cut(digest, ":"); ok {
		if !strings.EqualFold(algorithm, "sha256") {
			return "", errors.New("management asset release digest is not SHA-256")
		}
		digest = strings.TrimSpace(hash)
	}
	if len(digest) != sha256.Size*2 {
		return "", errors.New("invalid management asset SHA-256 digest length")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("invalid management asset SHA-256 digest")
	}
	return strings.ToLower(digest), nil
}
