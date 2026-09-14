package management

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	latestReleaseUserAgent = "CLIProxyAPI"
	maxConfigYAMLBytes     = int64(10 << 20)
)

var (
	errConfigYAMLBodyTooLarge          = errors.New("config YAML body exceeds 10 MiB")
	errConfigYAMLManagementSecretAlias = errors.New(
		"remote-management.secret-key cannot use YAML aliases or merge keys",
	)
)

// minPanelVersion is the minimum management-panel (frontend) version this server
// is compatible with. When the panel reports a lower version via __APP_VERSION__,
// the frontend renders a compatibility banner. Bump when management API changes
// require matching panel behavior.
const minPanelVersion = "1.15.3-aug.1"

type remoteManagementConfigResponse struct {
	AllowRemote            bool     `json:"allow-remote"`
	TrustedProxies         []string `json:"trusted-proxies,omitempty"`
	DisableControlPanel    bool     `json:"disable-control-panel"`
	AutoUpdatePanel        bool     `json:"auto-update-panel"`
	DisableAutoUpdatePanel bool     `json:"disable-auto-update-panel"`
	PanelGitHubRepository  string   `json:"panel-github-repository"`
	CPAGitHubRepository    string   `json:"cpa-github-repository"`
	AutoCheckUpdate        bool     `json:"auto-check-update"`
	AutoUpdateCPA          bool     `json:"auto-update-cpa"`
	CheckInterval          int      `json:"check-interval"`
}

type configValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type configValidationInternalError struct {
	err error
}

func (e *configValidationInternalError) Error() string {
	return "internal config validation failure"
}

func (e *configValidationInternalError) Unwrap() error {
	return e.err
}

var (
	configFieldPatterns = []struct {
		pattern *regexp.Regexp
		field   string
	}{
		{pattern: regexp.MustCompile(`\bapi-keys\[(\d+)]`), field: "api-keys"},
		{pattern: regexp.MustCompile(`\bport\b`), field: "port"},
		{pattern: regexp.MustCompile(`\blogs-max-total-size-mb\b`), field: "logs-max-total-size-mb"},
		{pattern: regexp.MustCompile(`\brequest-retry\b`), field: "request-retry"},
		{pattern: regexp.MustCompile(`\bmax-retry-interval\b`), field: "max-retry-interval"},
		{pattern: regexp.MustCompile(`\busage-retention\.days\b`), field: "usage-retention.days"},
		{pattern: regexp.MustCompile(`\busage-retention\.max-db-size-mb\b`), field: "usage-retention.max-db-size-mb"},
		{
			pattern: regexp.MustCompile(`\busage-retention\.warning-threshold-pct\b`),
			field:   "usage-retention.warning-threshold-pct",
		},
		{
			pattern: regexp.MustCompile(`\bpanel-github-repository\b`),
			field:   "remote-management.panel-github-repository",
		},
		{pattern: regexp.MustCompile(`\bcpa-github-repository\b`), field: "remote-management.cpa-github-repository"},
		{pattern: regexp.MustCompile(`\btls cert\b`), field: "tls.cert"},
		{pattern: regexp.MustCompile(`\btls key\b`), field: "tls.key"},
	}
	yamlLinePattern = regexp.MustCompile(`line\s+(\d+)`)
)

type configResponse struct {
	*config.Config
	RemoteManagement remoteManagementConfigResponse `json:"remote-management"`
	AuthIndexes      configAuthIndexResponse        `json:"auth-indexes"`
}

func (h *Handler) GetConfig(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{})
		return
	}

	checkInterval := cfg.RemoteManagement.CheckInterval
	if checkInterval < minBackendUpdateCheckMinutes {
		checkInterval = int(defaultBackendUpdateCheckInterval / time.Minute)
	}

	authIndexes := h.buildConfigAuthIndexViews(cfg).response()
	writeRedactedJSON(
		c, 200, configResponse{
			Config:      cfg,
			AuthIndexes: authIndexes,
			RemoteManagement: remoteManagementConfigResponse{
				AllowRemote:            cfg.RemoteManagement.AllowRemote,
				TrustedProxies:         append([]string(nil), cfg.RemoteManagement.TrustedProxies...),
				DisableControlPanel:    cfg.RemoteManagement.DisableControlPanel,
				AutoUpdatePanel:        cfg.RemoteManagement.IsAutoUpdatePanel(),
				DisableAutoUpdatePanel: !cfg.RemoteManagement.IsAutoUpdatePanel(),
				PanelGitHubRepository:  cfg.RemoteManagement.PanelGitHubRepository,
				CPAGitHubRepository:    cfg.RemoteManagement.CPAGitHubRepository,
				AutoCheckUpdate:        cfg.RemoteManagement.AutoCheckUpdate,
				AutoUpdateCPA:          cfg.RemoteManagement.AutoUpdateCPA,
				CheckInterval:          checkInterval,
			},
		},
	)
}

type releaseInfo struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at"`
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	cfg := h.configSnapshot()
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", cpaRepositoryFromConfig(cfg))
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, releaseURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.JSON(
			http.StatusBadGateway, gin.H{
				"error":   "unexpected_status",
				"message": fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
			},
		)
		return
	}

	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "decode_failed", "message": errDecode.Error()})
		return
	}

	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_response", "message": "missing release version"})
		return
	}

	c.JSON(
		http.StatusOK, gin.H{
			"latest-version": version,
			"published-at":   strings.TrimSpace(info.PublishedAt),
		},
	)
}

// GetVersion returns the build metadata of the currently running server binary
// plus the minimum panel version required for compatibility. The management
// panel calls this on mount to decide whether to render a version mismatch banner.
func (h *Handler) GetVersion(c *gin.Context) {
	c.JSON(
		http.StatusOK, gin.H{
			"version":           buildinfo.Version,
			"build_time":        buildinfo.BuildDate,
			"commit":            buildinfo.Commit,
			"min_panel_version": minPanelVersion,
			"features": gin.H{
				"plugins": true,
			},
		},
	)
}

func readConfigYAMLBody(c *gin.Context) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxConfigYAMLBytes+1))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, errConfigYAMLBodyTooLarge
		}
		return nil, err
	}
	if int64(len(body)) > maxConfigYAMLBytes {
		return nil, errConfigYAMLBodyTooLarge
	}
	return body, nil
}

func (h *Handler) configYAMLRedactionKey() ([32]byte, error) {
	if h == nil || strings.TrimSpace(h.configFilePath) == "" {
		return configYAMLPlaceholderKey, nil
	}
	h.configYAMLKeyMu.Lock()
	defer h.configYAMLKeyMu.Unlock()
	if h.configYAMLKeyLoaded {
		return h.configYAMLKey, nil
	}
	key, err := loadOrCreateConfigYAMLRedactionKey(h.configFilePath)
	if err != nil {
		return [32]byte{}, err
	}
	h.configYAMLKey = key
	h.configYAMLKeyLoaded = true
	return key, nil
}

func loadOrCreateConfigYAMLRedactionKey(configFilePath string) ([32]byte, error) {
	root, keyName, err := openConfigYAMLRedactionKeyRoot(configFilePath)
	if err != nil {
		return [32]byte{}, err
	}
	defer func() { _ = root.Close() }()
	key, err := readConfigYAMLRedactionKey(root, keyName)
	if err == nil {
		return key, nil
	}
	if !os.IsNotExist(err) {
		return [32]byte{}, err
	}

	return withConfigYAMLRedactionKeyLock(
		root,
		keyName,
		func() ([32]byte, error) {
			key, err = readConfigYAMLRedactionKey(root, keyName)
			if err == nil {
				return key, nil
			}
			if !os.IsNotExist(err) {
				return [32]byte{}, err
			}
			if _, err = rand.Read(key[:]); err != nil {
				return [32]byte{}, fmt.Errorf("generate config YAML redaction key: %w", err)
			}
			file, tempName, errCreate := createConfigYAMLRedactionKeyCandidate(root, keyName)
			if errCreate != nil {
				return [32]byte{}, errCreate
			}
			fileOpen := true
			defer func() {
				if fileOpen {
					_ = file.Close()
				}
				if tempName != "" {
					_ = root.Remove(tempName)
				}
			}()
			if err = secureConfigYAMLRedactionKeyFile(file, false); err != nil {
				return [32]byte{}, fmt.Errorf("secure config YAML redaction key candidate: %w", err)
			}
			written, errWrite := file.Write(key[:])
			if errWrite != nil {
				return [32]byte{}, fmt.Errorf("write config YAML redaction key candidate: %w", errWrite)
			}
			if written != len(key) {
				return [32]byte{}, io.ErrShortWrite
			}
			if err = file.Sync(); err != nil {
				return [32]byte{}, fmt.Errorf("sync config YAML redaction key candidate: %w", err)
			}
			candidateInfo, errStat := file.Stat()
			if errStat != nil {
				return [32]byte{}, fmt.Errorf("inspect config YAML redaction key candidate: %w", errStat)
			}
			if err = file.Close(); err != nil {
				fileOpen = false
				return [32]byte{}, fmt.Errorf("close config YAML redaction key candidate: %w", err)
			}
			fileOpen = false
			pathInfo, errStat := root.Lstat(tempName)
			if errStat != nil {
				return [32]byte{}, fmt.Errorf("verify config YAML redaction key candidate: %w", errStat)
			}
			if !pathInfo.Mode().IsRegular() || !os.SameFile(candidateInfo, pathInfo) {
				return [32]byte{}, fmt.Errorf("config YAML redaction key candidate changed before publication")
			}
			if err = root.Rename(tempName, keyName); err != nil {
				return [32]byte{}, fmt.Errorf("publish config YAML redaction key: %w", err)
			}
			tempName = ""
			if err = syncConfigRoot(root); err != nil {
				return [32]byte{}, fmt.Errorf("sync config YAML redaction key directory: %w", err)
			}
			return key, nil
		},
	)
}

func DeriveArtifactAccessSigningKey(configFilePath string, namespace string) ([32]byte, error) {
	namespace = strings.TrimSpace(namespace)
	decodedNamespace, err := hex.DecodeString(namespace)
	if err != nil || len(decodedNamespace) != sha256.Size || namespace != strings.ToLower(namespace) {
		return [32]byte{}, fmt.Errorf("artifact instance namespace is invalid")
	}
	masterKey, err := loadOrCreateConfigYAMLRedactionKey(configFilePath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("load config YAML redaction key for artifact access: %w", err)
	}
	mac := hmac.New(sha256.New, masterKey[:])
	_, _ = mac.Write([]byte("CLIProxyAPI/artifact-access-signing/v1"))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(namespace))
	return [sha256.Size]byte(mac.Sum(nil)), nil
}

func openConfigYAMLRedactionKeyRoot(configFilePath string) (*os.Root, string, error) {
	if writablePath := util.WritablePath(); writablePath != "" {
		root, err := util.OpenOrCreateDirectoryRoot(
			filepath.Join(writablePath, "management-state"), 0o700,
		)
		if err != nil {
			return nil, "", fmt.Errorf("open config YAML redaction key root: %w", err)
		}
		if err = secureConfigYAMLRedactionKeyRoot(root); err != nil {
			_ = root.Close()
			return nil, "", err
		}
		return root, "config-redaction.key", nil
	}

	keyPath, err := configYAMLRedactionKeyPath(configFilePath)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(keyPath)
	root, err := util.OpenOrCreateDirectoryRoot(dir, 0o700)
	if err != nil {
		return nil, "", fmt.Errorf("open config YAML redaction key root: %w", err)
	}
	if err = secureConfigYAMLRedactionKeyRoot(root); err != nil {
		_ = root.Close()
		return nil, "", err
	}
	return root, filepath.Base(keyPath), nil
}

func secureConfigYAMLRedactionKeyRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open config YAML redaction key root directory: %w", err)
	}
	if err = secureConfigYAMLRedactionKeyFile(directory, true); err != nil {
		_ = directory.Close()
		return fmt.Errorf("secure config YAML redaction key root: %w", err)
	}
	if err = directory.Close(); err != nil {
		return fmt.Errorf("close config YAML redaction key root directory: %w", err)
	}
	return nil
}

func createConfigYAMLRedactionKeyCandidate(root *os.Root, keyName string) (*os.File, string, error) {
	for range 100 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("generate config YAML redaction key candidate name: %w", err)
		}
		name := fmt.Sprintf(".%s.tmp-%x", keyName, suffix)
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("create config YAML redaction key candidate: %w", err)
		}
	}
	return nil, "", fmt.Errorf("create config YAML redaction key candidate: name collision limit exceeded")
}

func syncConfigRoot(root *os.Root) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func syncConfigDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func configYAMLRedactionKeyPath(configFilePath string) (string, error) {
	if writablePath := util.WritablePath(); writablePath != "" {
		return filepath.Join(writablePath, "management-state", "config-redaction.key"), nil
	}
	targetPath, err := resolveConfigTargetPath(configFilePath)
	if err != nil {
		return "", err
	}
	absoluteTargetPath, err := filepath.Abs(targetPath)
	if err != nil {
		return "", fmt.Errorf("resolve config YAML redaction key target: %w", err)
	}
	targetIdentity, err := configYAMLRedactionKeyIdentity(absoluteTargetPath)
	if err != nil {
		return "", err
	}
	targetHash := sha256.Sum256([]byte(targetIdentity))
	return filepath.Join(
		filepath.Dir(absoluteTargetPath),
		fmt.Sprintf(".cliproxy-management-%x", targetHash),
		"config-redaction.key",
	), nil
}

func readConfigYAMLRedactionKey(root *os.Root, name string) ([32]byte, error) {
	var key [32]byte
	pathInfo, err := root.Lstat(name)
	if err != nil {
		return key, err
	}
	if !pathInfo.Mode().IsRegular() {
		return key, fmt.Errorf("config YAML redaction key is not a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return key, err
	}
	defer func() { _ = file.Close() }()
	fileInfo, err := file.Stat()
	if err != nil {
		return key, err
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(fileInfo, pathInfo) {
		return key, fmt.Errorf("config YAML redaction key changed during validation")
	}
	if err = secureConfigYAMLRedactionKeyFile(file, false); err != nil {
		return key, fmt.Errorf("secure config YAML redaction key: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(key)+1)))
	if err != nil {
		return key, err
	}
	if len(data) != len(key) {
		return key, fmt.Errorf("config YAML redaction key has invalid length")
	}
	copy(key[:], data)
	return key, nil
}

func resolveConfigTargetPath(path string) (string, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolvedPath, errResolve := filepath.EvalSymlinks(path)
		if errResolve != nil {
			return "", errResolve
		}
		return resolvedPath, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

func writeConfigFileAtomically(path string, data []byte) error {
	targetPath, err := resolveConfigTargetPath(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(targetPath)
	file, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = file.Close()
		}
		if tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()

	if err = file.Chmod(0o600); err != nil {
		return err
	}
	written, errWrite := file.Write(data)
	if errWrite != nil {
		return errWrite
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		fileOpen = false
		return err
	}
	fileOpen = false
	if err = os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	tempPath = ""
	return syncConfigDirectory(dir)
}

func configYAMLETag(data []byte, key [32]byte) string {
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(data)
	return `"` + fmt.Sprintf("%x", mac.Sum(nil)) + `"`
}

func configYAMLMatchesIfMatch(header string, current []byte, key [32]byte) bool {
	expected := configYAMLETag(current, key)
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == expected {
			return true
		}
	}
	return false
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	warnings := beginConfigCommitWarnings(c)
	body, err := readConfigYAMLBody(c)
	if err != nil {
		if errors.Is(err, errConfigYAMLBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body_too_large", "message": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	containsPlaceholder, errPlaceholder := configYAMLContainsPlaceholder(body)
	if errPlaceholder != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": errPlaceholder.Error()})
		return
	}

	h.commitMu.Lock()
	h.mu.Lock()
	commit, callback, ok := h.commitConfigYAMLLocked(c, body, containsPlaceholder)
	h.mu.Unlock()
	h.commitMu.Unlock()
	if !ok {
		return
	}
	if !h.notifyConfigCommitted(c, callback, commit) {
		return
	}
	response := gin.H{"ok": true, "changed": []string{"config"}}
	if cleanupError := warnings.cleanupErrorMessage(); cleanupError != "" {
		response["cleanup_error"] = cleanupError
	}
	c.JSON(http.StatusOK, response)
}

// commitConfigYAMLLocked validates, persists and publishes a complete YAML config.
// Caller must hold h.mu and h.commitMu.
func (h *Handler) commitConfigYAMLLocked(
	c *gin.Context,
	body []byte,
	containsPlaceholder bool,
) (ConfigCommit, ConfigCommittedFunc, bool) {
	current, errRead := os.ReadFile(h.configFilePath)
	currentExists := errRead == nil
	ifMatch := strings.TrimSpace(c.GetHeader("If-Match"))
	var placeholderKey [32]byte
	if currentExists && (containsPlaceholder || ifMatch != "") {
		var errKey error
		placeholderKey, errKey = h.configYAMLRedactionKey()
		if errKey != nil {
			log.WithError(errKey).Error("failed to load config YAML redaction key")
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": "redaction_key_failed", "message": "failed to load config redaction state"},
			)
			return ConfigCommit{}, nil, false
		}
	}
	if ifMatch != "" && (!currentExists || !configYAMLMatchesIfMatch(ifMatch, current, placeholderKey)) {
		c.JSON(
			http.StatusPreconditionFailed,
			gin.H{
				"error":   "config_changed",
				"message": "config changed since it was loaded; review the latest version before saving again",
			},
		)
		return ConfigCommit{}, nil, false
	}
	switch {
	case currentExists && containsPlaceholder:
		var err error
		body, err = restoreConfigYAMLPlaceholdersWithKey(body, current, placeholderKey)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
			return ConfigCommit{}, nil, false
		}
	case currentExists:
	case os.IsNotExist(errRead) && containsPlaceholder:
		c.JSON(
			http.StatusUnprocessableEntity,
			gin.H{"error": "invalid_config", "message": "redacted values require the current config file"},
		)
		return ConfigCommit{}, nil, false
	case os.IsNotExist(errRead):
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": errRead.Error()})
		return ConfigCommit{}, nil, false
	}

	newCfg, _, errValidate := h.validateConfigBody(body)
	if errValidate != nil {
		if internalError, ok := errors.AsType[*configValidationInternalError](errValidate); ok {
			log.WithError(internalError.Unwrap()).Error("failed to validate config YAML")
			c.JSON(
				http.StatusInternalServerError, gin.H{"error": "validation_failed", "message": internalError.Error()},
			)
			return ConfigCommit{}, nil, false
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": errValidate.Error()})
		return ConfigCommit{}, nil, false
	}

	if currentExists {
		latest, errLatest := os.ReadFile(h.configFilePath)
		if errLatest != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": errLatest.Error()})
			return ConfigCommit{}, nil, false
		}
		if !bytes.Equal(latest, current) {
			c.JSON(
				http.StatusPreconditionFailed,
				gin.H{
					"error":   "config_changed",
					"message": "config changed since it was loaded; review the latest version before saving again",
				},
			)
			return ConfigCommit{}, nil, false
		}
		if err := config.SaveLastGood(h.configFilePath); err != nil {
			log.Errorf("failed to save last-good config: %v", err)
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": "backup_failed", "message": "failed to save config backup"},
			)
			return ConfigCommit{}, nil, false
		}
	}

	persistedData, errPersisted := configYAMLWithManagementSecret(
		body,
		newCfg.RemoteManagement.SecretKey,
	)
	if errPersisted != nil {
		if errors.Is(errPersisted, errConfigYAMLManagementSecretAlias) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": errPersisted.Error()})
			return ConfigCommit{}, nil, false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to secure config"})
		return ConfigCommit{}, nil, false
	}
	persistedData = config.NormalizeCommentIndentation(persistedData)
	if err := writeConfigFileAtomically(h.configFilePath, persistedData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return ConfigCommit{}, nil, false
	}
	digest := sha256.Sum256(persistedData)
	commit, callback := h.publishConfigLocked(newCfg, fmt.Sprintf("%x", digest[:]))
	return commit, callback, true
}

func configYAMLWithManagementSecret(body []byte, secret string) ([]byte, error) {
	document, errDecode := decodeConfigYAML(body)
	if errDecode != nil {
		return nil, errDecode
	}
	if len(document.Content) == 0 || document.Content[0] == nil {
		return nil, fmt.Errorf("config YAML has no document root")
	}
	root := document.Content[0]
	remoteManagement, merged, errLookup := yamlEffectiveMappingValue(root, "remote-management", nil)
	if errLookup != nil {
		return nil, errLookup
	}
	if remoteManagement == nil {
		return body, nil
	}
	if merged || remoteManagement.Kind == yaml.AliasNode {
		return nil, errConfigYAMLManagementSecretAlias
	}
	secretNode, merged, errLookup := yamlEffectiveMappingValue(remoteManagement, "secret-key", nil)
	if errLookup != nil {
		return nil, errLookup
	}
	if secretNode == nil {
		return body, nil
	}
	if merged || secretNode.Kind == yaml.AliasNode {
		return nil, errConfigYAMLManagementSecretAlias
	}
	secretNode.Kind = yaml.ScalarNode
	secretNode.Tag = "!!str"
	secretNode.Value = secret
	secretNode.Content = nil
	secretNode.Alias = nil
	return encodeConfigYAML(document)
}

func yamlEffectiveMappingValue(
	mapping *yaml.Node,
	key string,
	seen map[*yaml.Node]struct{},
) (*yaml.Node, bool, error) {
	if mapping == nil {
		return nil, false, nil
	}
	if seen == nil {
		seen = make(map[*yaml.Node]struct{})
	}
	if _, duplicate := seen[mapping]; duplicate {
		return nil, false, fmt.Errorf("config YAML contains a cyclic alias")
	}
	seen[mapping] = struct{}{}
	defer delete(seen, mapping)
	if mapping.Kind == yaml.AliasNode {
		if mapping.Alias == nil {
			return nil, false, fmt.Errorf("config YAML contains an unresolved alias")
		}
		value, _, errLookup := yamlEffectiveMappingValue(mapping.Alias, key, seen)
		return value, true, errLookup
	}
	if mapping.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keyValue, aliased, errKey := yamlScalarValue(mapping.Content[index], nil)
		if errKey != nil {
			return nil, false, errKey
		}
		if keyValue == key && keyValue != "<<" {
			return mapping.Content[index+1], aliased, nil
		}
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keyValue, _, errKey := yamlScalarValue(mapping.Content[index], nil)
		if errKey != nil {
			return nil, false, errKey
		}
		if keyValue != "<<" {
			continue
		}
		value, found, errLookup := yamlMergedMappingValue(mapping.Content[index+1], key, seen)
		if errLookup != nil {
			return nil, false, errLookup
		}
		if found {
			return value, true, nil
		}
	}
	return nil, false, nil
}

func yamlScalarValue(
	node *yaml.Node,
	seen map[*yaml.Node]struct{},
) (string, bool, error) {
	if node == nil {
		return "", false, nil
	}
	if seen == nil {
		seen = make(map[*yaml.Node]struct{})
	}
	if _, duplicate := seen[node]; duplicate {
		return "", false, fmt.Errorf("config YAML contains a cyclic alias")
	}
	seen[node] = struct{}{}
	defer delete(seen, node)
	if node.Kind == yaml.AliasNode {
		if node.Alias == nil {
			return "", false, fmt.Errorf("config YAML contains an unresolved alias")
		}
		value, _, errResolve := yamlScalarValue(node.Alias, seen)
		return value, true, errResolve
	}
	if node.Kind != yaml.ScalarNode {
		return "", false, nil
	}
	return node.Value, false, nil
}

func yamlMergedMappingValue(
	node *yaml.Node,
	key string,
	seen map[*yaml.Node]struct{},
) (*yaml.Node, bool, error) {
	if node == nil {
		return nil, false, nil
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			value, _, errLookup := yamlEffectiveMappingValue(child, key, seen)
			if errLookup != nil {
				return nil, false, errLookup
			}
			if value != nil {
				return value, true, nil
			}
		}
		return nil, false, nil
	}
	value, _, errLookup := yamlEffectiveMappingValue(node, key, seen)
	return value, value != nil, errLookup
}

// ValidateConfigYAML validates a YAML config body without saving.
func inferConfigValidationField(message string) string {
	for _, candidate := range configFieldPatterns {
		if candidate.pattern.MatchString(message) {
			return candidate.field
		}
	}
	if matches := yamlLinePattern.FindStringSubmatch(message); len(matches) == 2 {
		return "line " + matches[1]
	}
	if strings.Contains(message, "failed to parse config file") || strings.Contains(
		message, "YAML parse error",
	) || strings.Contains(message, "yaml:") {
		return "$yaml"
	}
	return ""
}

func (h *Handler) ValidateConfigYAML(c *gin.Context) {
	body, err := readConfigYAMLBody(c)
	if err != nil {
		if errors.Is(err, errConfigYAMLBodyTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body_too_large", "message": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	_, warnings, errValidate := h.validateConfigBody(body)
	if errValidate != nil {
		if internalError, ok := errors.AsType[*configValidationInternalError](errValidate); ok {
			log.WithError(internalError.Unwrap()).Error("failed to validate config YAML")
			c.JSON(
				http.StatusInternalServerError, gin.H{"error": "validation_failed", "message": internalError.Error()},
			)
			return
		}
		message := errValidate.Error()
		c.JSON(
			http.StatusOK, gin.H{
				"valid": false,
				"errors": []configValidationError{
					{
						Field:   inferConfigValidationField(message),
						Message: message,
					},
				},
				"warnings": warnings,
			},
		)
		return
	}
	c.JSON(
		http.StatusOK, gin.H{
			"valid":    true,
			"errors":   []configValidationError{},
			"warnings": warnings,
		},
	)
}

// validateConfigBody validates a YAML config body and returns the parsed config, warnings, and any error.
func (h *Handler) validateConfigBody(body []byte) (*config.Config, []string, error) {
	var cfg config.Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return nil, nil, fmt.Errorf("YAML parse error: %w", err)
	}

	// Write to temp file for LoadConfigOptional validation
	tmpDir := filepath.Dir(h.configFilePath)
	tmpFile, err := os.CreateTemp(tmpDir, "config-validate-*.yaml")
	if err != nil {
		return nil, nil, &configValidationInternalError{err: err}
	}
	tempFile := tmpFile.Name()
	defer func() {
		_ = os.Remove(tempFile)
	}()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		return nil, nil, &configValidationInternalError{err: errWrite}
	}
	if errClose := tmpFile.Close(); errClose != nil {
		return nil, nil, &configValidationInternalError{err: errClose}
	}

	loaded, err := config.LoadConfigOptional(tempFile, false)
	if err != nil {
		return nil, nil, err
	}

	warnings := config.ValidateConfig(loaded)
	return loaded, warnings, nil
}

// GetConfigYAML returns config.yaml with sensitive scalar values redacted.
// YAML comments and mapping order are preserved.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	placeholderKey, err := h.configYAMLRedactionKey()
	if err != nil {
		log.WithError(err).Error("failed to load config YAML redaction key")
		c.JSON(
			http.StatusInternalServerError,
			gin.H{"error": "redaction_key_failed", "message": "failed to load config redaction state"},
		)
		return
	}
	redacted, err := redactConfigYAMLWithKey(data, placeholderKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "redaction_failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("ETag", configYAMLETag(data, placeholderKey))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if _, err = c.Writer.Write(redacted); err != nil {
		log.WithError(err).Warn("failed to write redacted config YAML response")
	}
}

// GetDebug returns the debug flag.
func (h *Handler) GetDebug(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"debug": cfg != nil && cfg.Debug})
}
func (h *Handler) PutDebug(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.Debug = v })
}

// GetUsageStatisticsEnabled returns the usage statistics enabled flag.
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"usage-statistics-enabled": cfg != nil && cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.UsageStatisticsEnabled = v })
}

// GetLoggingToFile returns the logging-to-file flag.
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"logging-to-file": cfg != nil && cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.LoggingToFile = v })
}

// GetLogsMaxTotalSizeMB returns the logs max total size in MB.
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	cfg := h.configSnapshot()
	value := 0
	if cfg != nil {
		value = cfg.LogsMaxTotalSizeMB
	}
	c.JSON(200, gin.H{"logs-max-total-size-mb": value})
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := max(*body.Value, 0)
	if _, ok := h.mutateConfig(
		c, func(cfg *config.Config) error {
			cfg.LogsMaxTotalSizeMB = value
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// GetErrorLogsMaxFiles returns the error logs max files count.
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	cfg := h.configSnapshot()
	value := 0
	if cfg != nil {
		value = cfg.ErrorLogsMaxFiles
	}
	c.JSON(200, gin.H{"error-logs-max-files": value})
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	if _, ok := h.mutateConfig(
		c, func(cfg *config.Config) error {
			cfg.ErrorLogsMaxFiles = value
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// GetRequestLog returns the request log flag.
func (h *Handler) GetRequestLog(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"request-log": cfg != nil && cfg.RequestLog})
}
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.RequestLog = v })
}

// GetWebsocketAuth returns the websocket auth flag.
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"ws-auth": cfg != nil && cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.WebsocketAuth = v })
}

// GetRequestRetry returns the request retry count.
func (h *Handler) GetRequestRetry(c *gin.Context) {
	cfg := h.configSnapshot()
	value := 0
	if cfg != nil {
		value = cfg.RequestRetry
	}
	c.JSON(200, gin.H{"request-retry": value})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(cfg *config.Config, v int) { cfg.RequestRetry = v })
}

// GetMaxRetryInterval returns the max retry interval.
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	cfg := h.configSnapshot()
	value := 0
	if cfg != nil {
		value = cfg.MaxRetryInterval
	}
	c.JSON(200, gin.H{"max-retry-interval": value})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(cfg *config.Config, v int) { cfg.MaxRetryInterval = v })
}

// GetForceModelPrefix returns the force model prefix flag.
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	cfg := h.configSnapshot()
	// configSnapshot may be nil for a handler created without runtime configuration.
	// noinspection GoDfaConstantCondition
	c.JSON(200, gin.H{"force-model-prefix": cfg != nil && cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(cfg *config.Config, v bool) { cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "quota-aware", "quotaaware", "qa":
		return "quota-aware", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// GetRoutingStrategy returns the routing strategy.
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(200, gin.H{"strategy": ""})
		return
	}
	strategy, ok := normalizeRoutingStrategy(cfg.Routing.Strategy)
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(cfg.Routing.Strategy)})
		return
	}
	c.JSON(200, gin.H{"strategy": strategy})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, ok := normalizeRoutingStrategy(*body.Value)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	if _, ok = h.mutateConfig(
		c, func(cfg *config.Config) error {
			cfg.Routing.Strategy = normalized
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// GetAutoRefreshInterval returns the auto refresh interval.
func (h *Handler) GetAutoRefreshInterval(c *gin.Context) {
	cfg := h.configSnapshot()
	value := 0
	if cfg != nil {
		value = cfg.AutoRefreshInterval
	}
	c.JSON(200, gin.H{"auto-refresh-interval": value})
}
func (h *Handler) PutAutoRefreshInterval(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := max(*body.Value, 0)
	if _, ok := h.mutateConfig(
		c, func(cfg *config.Config) error {
			cfg.AutoRefreshInterval = value
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// GetProxyURL returns the proxy URL.
func (h *Handler) GetProxyURL(c *gin.Context) {
	cfg := h.configSnapshot()
	value := ""
	if cfg != nil {
		value = cfg.ProxyURL
	}
	c.JSON(200, gin.H{"proxy-url": value})
}
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringFieldValidated(
		c, func(v string) error {
			if v == "" {
				return nil
			}
			if errValidate := proxyutil.ValidateProxyURL(v); errValidate != nil {
				return fmt.Errorf("invalid proxy-url: %w", errValidate)
			}
			return nil
		}, func(cfg *config.Config, v string) { cfg.ProxyURL = v },
	)
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	if _, ok := h.mutateConfig(
		c, func(cfg *config.Config) error {
			cfg.ProxyURL = ""
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
