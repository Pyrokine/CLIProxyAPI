// Last compiled: 2026-05-09
// Author: pyro

package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const latestReleaseUserAgent = "CLIProxyAPI"

// minPanelVersion is the minimum management-panel (frontend) version this server
// is compatible with. When the panel reports a lower version via __APP_VERSION__,
// the frontend renders a compatibility banner. Bump when introducing breaking
// changes to management API response shapes.
const minPanelVersion = "1.7.16-aug.1"

type remoteManagementConfigResponse struct {
	AllowRemote           bool   `json:"allow-remote"`
	DisableControlPanel   bool   `json:"disable-control-panel"`
	AutoUpdatePanel       bool   `json:"auto-update-panel"`
	PanelGitHubRepository string `json:"panel-github-repository"`
	CPAGitHubRepository   string `json:"cpa-github-repository"`
	AutoCheckUpdate       bool   `json:"auto-check-update"`
	AutoUpdateCPA         bool   `json:"auto-update-cpa"`
	CheckInterval         int    `json:"check-interval"`
}

type configValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

var (
	configFieldPatterns = []struct {
		pattern *regexp.Regexp
		field   string
	}{
		{pattern: regexp.MustCompile(`\bapi-keys\[(\d+)\]`), field: "api-keys"},
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
}

func (h *Handler) GetConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{})
		return
	}

	checkInterval := h.cfg.RemoteManagement.CheckInterval
	if checkInterval < minBackendUpdateCheckMinutes {
		checkInterval = int(defaultBackendUpdateCheckInterval / time.Minute)
	}

	c.JSON(
		200, configResponse{
			Config: h.cfg,
			RemoteManagement: remoteManagementConfigResponse{
				AllowRemote:           h.cfg.RemoteManagement.AllowRemote,
				DisableControlPanel:   h.cfg.RemoteManagement.DisableControlPanel,
				AutoUpdatePanel:       h.cfg.RemoteManagement.IsAutoUpdatePanel(),
				PanelGitHubRepository: h.cfg.RemoteManagement.PanelGitHubRepository,
				CPAGitHubRepository:   h.cfg.RemoteManagement.CPAGitHubRepository,
				AutoCheckUpdate:       h.cfg.RemoteManagement.AutoCheckUpdate,
				AutoUpdateCPA:         h.cfg.RemoteManagement.AutoUpdateCPA,
				CheckInterval:         checkInterval,
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
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	releaseURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", h.cpaRepository())
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
		},
	)
}

func writeConfig(path string, data []byte) error {
	data = config.NormalizeCommentIndentation(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	_, _, errValidate := h.validateConfigBody(body)
	if errValidate != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": errValidate.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// Save current config as last-good before overwriting
	if err := config.SaveLastGood(h.configFilePath); err != nil {
		log.Errorf("failed to save last-good config: %v", err)
		c.JSON(
			http.StatusInternalServerError, gin.H{"error": "backup_failed", "message": "failed to save config backup"},
		)
		return
	}

	if writeConfig(h.configFilePath, body) != nil {
		h.rollbackLastGood()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		h.rollbackLastGood()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		return
	}
	h.cfg = newCfg
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
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
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	_, warnings, errValidate := h.validateConfigBody(body)
	if errValidate != nil {
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
		return nil, nil, fmt.Errorf("internal error: %w", err)
	}
	tempFile := tmpFile.Name()
	defer func() {
		_ = os.Remove(tempFile)
	}()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		return nil, nil, fmt.Errorf("internal error: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		return nil, nil, fmt.Errorf("internal error: %w", errClose)
	}

	loaded, err := config.LoadConfigOptional(tempFile, false)
	if err != nil {
		return nil, nil, err
	}

	warnings := config.ValidateConfig(loaded)
	return loaded, warnings, nil
}

// GetConfigYAML returns the raw config.yaml file bytes without re-encoding.
// It preserves comments and original formatting/styles.
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
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// Write raw bytes as-is
	_, _ = c.Writer.Write(data)
}

// GetDebug returns the debug flag.
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// GetUsageStatisticsEnabled returns the usage statistics enabled flag.
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	c.JSON(200, gin.H{"usage-statistics-enabled": h.cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.UsageStatisticsEnabled = v })
}

// GetLoggingToFile returns the logging-to-file flag.
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	c.JSON(200, gin.H{"logging-to-file": h.cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.LoggingToFile = v })
}

// GetLogsMaxTotalSizeMB returns the logs max total size in MB.
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	c.JSON(200, gin.H{"logs-max-total-size-mb": h.cfg.LogsMaxTotalSizeMB})
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
	h.cfg.LogsMaxTotalSizeMB = value
	h.persist(c)
}

// GetErrorLogsMaxFiles returns the error logs max files count.
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	c.JSON(200, gin.H{"error-logs-max-files": h.cfg.ErrorLogsMaxFiles})
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
	h.cfg.ErrorLogsMaxFiles = value
	h.persist(c)
}

// GetRequestLog returns the request log flag.
func (h *Handler) GetRequestLog(c *gin.Context) { c.JSON(200, gin.H{"request-log": h.cfg.RequestLog}) }
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.RequestLog = v })
}

// GetWebsocketAuth returns the websocket auth flag.
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// GetRequestRetry returns the request retry count.
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// GetMaxRetryInterval returns the max retry interval.
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// GetForceModelPrefix returns the force model prefix flag.
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// GetRoutingStrategy returns the routing strategy.
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(h.cfg.Routing.Strategy)})
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
	h.cfg.Routing.Strategy = normalized
	h.persist(c)
}

// GetAutoRefreshInterval returns the auto refresh interval.
func (h *Handler) GetAutoRefreshInterval(c *gin.Context) {
	c.JSON(200, gin.H{"auto-refresh-interval": h.cfg.AutoRefreshInterval})
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
	h.cfg.AutoRefreshInterval = value
	h.persist(c)
}

// GetProxyURL returns the proxy URL.
func (h *Handler) GetProxyURL(c *gin.Context) { c.JSON(200, gin.H{"proxy-url": h.cfg.ProxyURL}) }
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.ProxyURL = v })
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	h.cfg.ProxyURL = ""
	h.persist(c)
}
