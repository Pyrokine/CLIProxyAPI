package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	geminiAuth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/gemini"
	iflowauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/iflow"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/oauthcommon"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/qwen"
	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/credentialweight"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	lastRefreshKeys                  = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}
	errAuthFileChangedDuringDeletion = errors.New("auth file changed during deletion")
	errPluginVirtualAuth             = errors.New("plugin virtual auth cannot be modified directly; edit or delete the source auth file")
)

const (
	anthropicCallbackPort = 54545
	geminiCallbackPort    = 8085
	codexCallbackPort     = 1455
)

type callbackForwarder struct {
	provider string
	port     int
	server   *http.Server
	done     chan struct{}
}

var (
	callbackForwardersMu sync.Mutex
	callbackForwarders   = make(map[int]*callbackForwarder)
)

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func isWebUIRequest(c *gin.Context) bool {
	raw := strings.TrimSpace(c.Query("is_webui"))
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func startCallbackForwarder(port int, provider, targetBase string) (*callbackForwarder, error) {
	callbackForwardersMu.Lock()
	prev := callbackForwarders[port]
	if prev != nil {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	if prev != nil {
		stopForwarderInstance(port, prev)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	handler := http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			target := targetBase
			if raw := r.URL.RawQuery; raw != "" {
				if strings.Contains(target, "?") {
					target = target + "&" + raw
				} else {
					target = target + "?" + raw
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, target, http.StatusFound)
		},
	)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	done := make(chan struct{})

	go func() {
		if errServe := srv.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.WithError(errServe).Warnf("callback forwarder for %s stopped unexpectedly", provider)
		}
		close(done)
	}()

	forwarder := &callbackForwarder{
		provider: provider,
		port:     actualPort,
		server:   srv,
		done:     done,
	}

	callbackForwardersMu.Lock()
	callbackForwarders[actualPort] = forwarder
	callbackForwardersMu.Unlock()

	log.Infof("callback forwarder for %s listening on 127.0.0.1:%d", provider, actualPort)

	return forwarder, nil
}

func stopCallbackForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil {
		return
	}
	callbackForwardersMu.Lock()
	if current := callbackForwarders[port]; current == forwarder {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	stopForwarderInstance(port, forwarder)
}

func stopForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil || forwarder.server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := forwarder.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.WithError(err).Warnf("failed to shut down callback forwarder on port %d", port)
	}

	select {
	case <-forwarder.done:
	case <-time.After(2 * time.Second):
	}

	log.Infof("callback forwarder on port %d stopped", port)
}

func (h *Handler) authDirSnapshot() string {
	cfg := h.configSnapshot()
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.AuthDir)
}

func (h *Handler) authFlowConfig(c *gin.Context) (*config.Config, bool) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return nil, false
	}
	flowCfg, err := config.Clone(cfg)
	if err != nil {
		log.WithError(err).Error("failed to snapshot config for authentication flow")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to snapshot config"})
		return nil, false
	}
	if strings.TrimSpace(flowCfg.AuthDir) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return nil, false
	}
	return flowCfg, true
}

func managementCallbackURL(cfg *config.Config, path string) (string, error) {
	if cfg == nil || cfg.Port <= 0 {
		return "", fmt.Errorf("server port is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, cfg.Port, path), nil
}

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		authDir := h.authDirSnapshot()
		if authDir == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
			return
		}
		h.listAuthFilesFromDisk(c, authDir)
		return
	}
	nameFilter := strings.TrimSpace(c.Query("name"))
	authIndexFilter := strings.TrimSpace(c.Query("auth_index"))
	auths := h.authManager.List()
	observedAt := time.Now().UTC()
	cooldownsKnown := !h.authManager.HomeEnabled()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if !matchesAuthFileLookup(auth, nameFilter, authIndexFilter) {
			continue
		}
		if entry := h.buildAuthFileEntry(auth); entry != nil {
			entry["cooldowns"] = nil
			if cooldownsKnown {
				entry["cooldowns"] = coreauth.CooldownSnapshotForAuth(auth, observedAt)
			}
			files = append(files, entry)
		}
	}
	sort.Slice(
		files, func(i, j int) bool {
			nameI, _ := files[i]["name"].(string)
			nameJ, _ := files[j]["name"].(string)
			return strings.ToLower(nameI) < strings.ToLower(nameJ)
		},
	)
	c.JSON(200, gin.H{"observed_at": observedAt, "files": files})
}

func matchesAuthFileLookup(auth *coreauth.Auth, name, authIndex string) bool {
	if auth == nil {
		return false
	}
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if name != "" && strings.TrimSpace(auth.ID) != name && strings.TrimSpace(auth.FileName) != name {
		return false
	}
	return authIndex == "" || strings.TrimSpace(auth.EnsureIndex()) == authIndex
}

func (h *Handler) lookupAuthFile(name, authIndex string) (*coreauth.Auth, bool) {
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if h == nil || h.authManager == nil || name == "" && authIndex == "" {
		return nil, false
	}
	for _, auth := range h.authManager.List() {
		if matchesAuthFileLookup(auth, name, authIndex) {
			return auth, true
		}
	}
	return nil, false
}

// GetAuthFileModels returns the models supported by a specific auth file
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	// Try to find auth ID via authManager
	var authID string
	if h.authManager != nil {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name || auth.ID == name {
				authID = auth.ID
				break
			}
		}
	}

	if authID == "" {
		authID = name // fallback to filename as ID
	}

	// Get models from registry
	reg := registry.GetGlobalRegistry()
	models := reg.GetModelsForClient(authID)

	result := make([]gin.H, 0, len(models))
	for _, m := range models {
		entry := gin.H{
			"id": m.ID,
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Type != "" {
			entry["type"] = m.Type
		}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		result = append(result, entry)
	}

	c.JSON(200, gin.H{"models": result})
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context, authDir string) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	files := make([]gin.H, 0)
	nameFilter := strings.TrimSpace(c.Query("name"))
	if strings.TrimSpace(c.Query("auth_index")) != "" {
		c.JSON(200, gin.H{"files": files})
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if nameFilter != "" && name != nameFilter {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field
			full := filepath.Join(authDir, name)
			if data, errRead := os.ReadFile(full); errRead == nil {
				typeValue := gjson.GetBytes(data, "type").String()
				emailValue := gjson.GetBytes(data, "email").String()
				fileData["type"] = typeValue
				fileData["email"] = emailValue
				if websockets, ok := authFileBoolValueFromGJSON(gjson.GetBytes(data, "websockets")); ok {
					fileData["websockets"] = websockets
				}
			}

			files = append(files, fileData)
		}
	}
	c.JSON(200, gin.H{"files": files})
}

type authFileRuntimeError struct {
	Code            string     `json:"code,omitempty"`
	Message         string     `json:"message"`
	HTTPStatus      int        `json:"http_status,omitempty"`
	Model           string     `json:"model,omitempty"`
	EffectScope     string     `json:"effect_scope,omitempty"`
	EffectKey       string     `json:"effect_key,omitempty"`
	NextRetryAfter  *time.Time `json:"next_retry_after,omitempty"`
	StateGeneration uint64     `json:"state_generation,omitempty"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
}

type authFileRuntimeErrorCandidate struct {
	err             *coreauth.Error
	statusMessage   string
	model           string
	effectScope     string
	effectKey       string
	nextRetryAfter  time.Time
	nextRecoverAt   time.Time
	stateGeneration uint64
	observedAt      time.Time
}

func buildAuthFileRuntimeError(auth *coreauth.Auth) *authFileRuntimeError {
	if auth == nil {
		return nil
	}

	selected := authFileRuntimeErrorCandidate{
		err:             auth.LastError,
		statusMessage:   auth.StatusMessage,
		effectScope:     string(auth.StateEffectScope),
		effectKey:       auth.StateEffectKey,
		nextRetryAfter:  auth.NextRetryAfter,
		nextRecoverAt:   auth.Quota.NextRecoverAt,
		stateGeneration: auth.StateGeneration,
		observedAt:      auth.UpdatedAt,
	}
	hasSelected := auth.LastError != nil ||
		auth.Status == coreauth.StatusError && strings.TrimSpace(auth.StatusMessage) != ""

	models := make([]string, 0, len(auth.ModelStates))
	for model := range auth.ModelStates {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := auth.ModelStates[model]
		if state == nil || state.LastError == nil && state.Status != coreauth.StatusError {
			continue
		}
		selectedScope := selected.effectScope
		if selected.err != nil && selected.err.EffectScope != "" {
			selectedScope = string(selected.err.EffectScope)
		}
		selectedIsModelAggregate := selected.model == "" &&
			(selectedScope == "exact_model" || selectedScope == "quota_family")
		if hasSelected && !selectedIsModelAggregate && !state.UpdatedAt.After(selected.observedAt) {
			continue
		}
		selected = authFileRuntimeErrorCandidate{
			err:             state.LastError,
			statusMessage:   state.StatusMessage,
			model:           model,
			effectScope:     string(state.EffectScope),
			effectKey:       state.EffectKey,
			nextRetryAfter:  state.NextRetryAfter,
			nextRecoverAt:   state.Quota.NextRecoverAt,
			stateGeneration: state.Generation,
			observedAt:      state.UpdatedAt,
		}
		hasSelected = true
	}
	if !hasSelected {
		return nil
	}

	result := &authFileRuntimeError{
		Message:         selected.statusMessage,
		Model:           strings.TrimSpace(selected.model),
		EffectScope:     strings.TrimSpace(selected.effectScope),
		EffectKey:       strings.TrimSpace(selected.effectKey),
		StateGeneration: selected.stateGeneration,
	}
	if selected.err != nil {
		result.Code = strings.TrimSpace(selected.err.Code)
		if message := strings.TrimSpace(selected.err.Message); message != "" {
			result.Message = message
		}
		result.HTTPStatus = selected.err.HTTPStatus
		if selected.err.EffectScope != "" {
			result.EffectScope = string(selected.err.EffectScope)
		}
		if effectKey := strings.TrimSpace(selected.err.EffectKey); effectKey != "" {
			result.EffectKey = effectKey
		}
		if selected.err.StateGeneration != 0 {
			result.StateGeneration = selected.err.StateGeneration
		}
	}
	result.Message = strings.TrimSpace(string(util.MaskSensitivePayload([]byte(result.Message))))
	if result.Message == "" {
		result.Message = result.Code
	}
	if result.Message == "" {
		result.Message = "request failed"
	}
	if result.Model == "" && result.EffectScope == "exact_model" {
		result.Model = result.EffectKey
	}
	if selected.nextRetryAfter.IsZero() {
		selected.nextRetryAfter = selected.nextRecoverAt
	}
	if !selected.nextRetryAfter.IsZero() {
		nextRetryAfter := selected.nextRetryAfter.UTC()
		result.NextRetryAfter = &nextRetryAfter
	}
	if !selected.observedAt.IsZero() {
		observedAt := selected.observedAt.UTC()
		result.ObservedAt = &observedAt
	}
	return result
}

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth) gin.H {
	if auth == nil {
		return nil
	}
	auth.EnsureIndex()
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := gin.H{
		"id":              auth.ID,
		"auth_index":      auth.Index,
		"name":            name,
		"type":            strings.TrimSpace(auth.Provider),
		"provider":        strings.TrimSpace(auth.Provider),
		"label":           auth.Label,
		"status":          auth.Status,
		"status_message":  strings.TrimSpace(string(util.MaskSensitivePayload([]byte(auth.StatusMessage)))),
		"disabled":        auth.Disabled,
		"unavailable":     auth.Unavailable,
		"runtime_only":    runtimeOnly,
		"source":          "memory",
		"size":            int64(0),
		"success":         auth.Success,
		"failed":          auth.Failed,
		"recent_requests": auth.RecentRequestsSnapshot(time.Now()),
	}
	if runtimeError := buildAuthFileRuntimeError(auth); runtimeError != nil {
		entry["runtime_error"] = runtimeError
	}
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if accountType, account := authFileAccountInfo(auth); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if info, err := os.Stat(path); err == nil {
			entry["size"] = info.Size()
			entry["modtime"] = info.ModTime()
		} else if os.IsNotExist(err) {
			// Hide credentials removed from disk but still lingering in memory.
			if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(
				strings.TrimSpace(auth.StatusMessage), "removed via management api",
			)) {
				return nil
			}
			entry["source"] = "memory"
		} else {
			log.WithError(err).Warnf("failed to stat auth file %s", path)
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	if priority, ok := authPriorityValue(auth); ok {
		entry["priority"] = priority
	}
	if weight, ok := authWeightValue(auth); ok {
		entry["weight"] = weight
	}
	if note := authNoteValue(auth); note != "" {
		entry["note"] = note
	}
	if websockets, ok := authWebsocketsValue(auth); ok {
		entry["websockets"] = websockets
	}
	if disableCooling, ok := authMetadataBool(auth, "disable_cooling", "disable-cooling"); ok {
		entry["disable_cooling"] = disableCooling
	}
	if retry, ok := auth.RequestRetryOverride(); ok {
		entry["request_retry"] = retry
	}
	if usingAPI, ok := authMetadataBool(auth, "using_api"); ok {
		entry["using_api"] = usingAPI
	}
	if excludedModels := authFileExcludedModels(auth); len(excludedModels) > 0 {
		entry["excluded_models"] = excludedModels
	}
	if projectID := authProjectID(auth); projectID != "" {
		entry["project_id"] = projectID
	}
	return entry
}

func authPriorityValue(auth *coreauth.Auth) (int, bool) {
	if auth == nil {
		return 0, false
	}
	if raw, ok := auth.Metadata["priority"]; ok {
		switch v := raw.(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		case string:
			priority := strings.TrimSpace(v)
			if parsed, err := strconv.Atoi(priority); err == nil {
				return parsed, true
			}
		}
	}
	if auth.Attributes != nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(auth.Attributes["priority"])); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func authWeightValue(auth *coreauth.Auth) (int64, bool) {
	if auth == nil {
		return 0, false
	}
	if raw, ok := auth.Metadata[coreauth.AttributeWeight]; ok {
		weight, errParse := credentialweight.ParseValue(raw)
		if errParse == nil {
			return weight, true
		}
	}
	if auth.Attributes != nil {
		raw, ok := auth.Attributes[coreauth.AttributeWeight]
		if ok {
			weight, errParse := credentialweight.ParseString(raw)
			if errParse == nil {
				return weight, true
			}
		}
	}
	return 0, false
}

func authNoteValue(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if raw, ok := auth.Metadata["note"].(string); ok {
		return strings.TrimSpace(raw)
	}
	if auth.Attributes != nil {
		return strings.TrimSpace(auth.Attributes["note"])
	}
	return ""
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return nil
	}

	result := gin.H{}
	if v := strings.TrimSpace(claims.AuthInfo.ChatgptAccountID); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(claims.AuthInfo.ChatgptPlanType); v != "" {
		result["plan_type"] = v
	}
	if v := claims.AuthInfo.ChatgptSubscriptionActiveStart; v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v := claims.AuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
		result["chatgpt_subscription_active_until"] = v
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authWebsocketsValue(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
				return parsed, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	return authFileBoolValue(auth.Metadata["websockets"])
}

func authFileBoolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

func authMetadataBool(auth *coreauth.Auth, keys ...string) (bool, bool) {
	if auth == nil || auth.Metadata == nil {
		return false, false
	}
	for _, key := range keys {
		if value, ok := auth.Metadata[key]; ok {
			if parsed, valid := authFileBoolValue(value); valid {
				return parsed, true
			}
		}
	}
	return false, false
}

func authFileExcludedModels(auth *coreauth.Auth) []string {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw, ok := auth.Metadata["excluded_models"]
	if !ok {
		raw = auth.Metadata["excluded-models"]
	}
	var values []string
	switch typed := raw.(type) {
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			if value, okString := item.(string); okString {
				values = append(values, value)
			}
		}
	}
	return normalizeAuthFileExcludedModels(values)
}

func normalizeAuthFileExcludedModels(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		model := strings.ToLower(strings.TrimSpace(value))
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	sort.Strings(normalized)
	return normalized
}

func syncAuthFileExcludedModelsAttribute(auth *coreauth.Auth, cfg *config.Config) {
	if auth == nil {
		return
	}
	combined := authFileExcludedModels(auth)
	if !strings.EqualFold(strings.TrimSpace(authAttribute(auth, "auth_kind")), "apikey") && cfg != nil {
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		combined = normalizeAuthFileExcludedModels(append(combined, cfg.OAuthExcludedModels[provider]...))
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if len(combined) == 0 {
		delete(auth.Attributes, "excluded_models")
		delete(auth.Attributes, "excluded_models_hash")
		return
	}
	auth.Attributes["excluded_models"] = strings.Join(combined, ",")
	encoded, _ := json.Marshal(combined)
	hash := sha256.Sum256(encoded)
	auth.Attributes["excluded_models_hash"] = hex.EncodeToString(hash[:])
}

func authProjectID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["project_id"].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return strings.TrimSpace(authAttribute(auth, "project_id"))
}

func authFileBoolValueFromGJSON(value gjson.Result) (bool, bool) {
	if !value.Exists() {
		return false, false
	}
	switch value.Type {
	case gjson.True:
		return true, true
	case gjson.False:
		return false, true
	case gjson.String:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value.String()))
		if errParse == nil {
			return parsed, true
		}
	case gjson.Null, gjson.Number, gjson.JSON:
		return false, false
	}
	return false, false
}

func syncAuthFileWebsocketsAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	websockets, ok := authFileBoolValue(auth.Metadata["websockets"])
	if !ok {
		delete(auth.Attributes, "websockets")
		return
	}
	auth.Attributes["websockets"] = strconv.FormatBool(websockets)
}

func authFileAccountInfo(auth *coreauth.Auth) (string, string) {
	if auth == nil {
		return "", ""
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "devin") {
		if email := authEmail(auth); email != "" {
			return "oauth", email
		}
		return "", ""
	}
	return auth.AccountInfo()
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

// validateAuthFileName checks that name is a safe, non-traversal .json filename
// and returns the resolved absolute path under authDir.
func validateAuthFileName(authDir, name string) (string, error) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return "", fmt.Errorf("auth directory not configured")
	}
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid name")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return "", fmt.Errorf("name must end with .json")
	}
	base := filepath.Base(name)
	full, err := filepath.Abs(filepath.Join(authDir, base))
	if err != nil {
		return "", fmt.Errorf("failed to resolve path")
	}
	authAbs, err := filepath.Abs(authDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve auth dir")
	}
	if !strings.HasPrefix(full, authAbs+string(os.PathSeparator)) && full != authAbs {
		return "", fmt.Errorf("path escapes auth directory")
	}
	return full, nil
}

// DownloadAuthFile downloads a single auth file by name.
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	authDir := h.authDirSnapshot()
	if authDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}
	full, err := validateAuthFileName(authDir, c.Query("name"))
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		}
		return
	}
	safeName := strings.ReplaceAll(filepath.Base(full), "\"", "_")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", safeName))
	c.Data(200, "application/json", util.MaskSensitivePayload(data))
}

type uploadedAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error)
}

type uploadedAuthPersister interface {
	PersistAuthFiles(context.Context, string, ...string) error
}

func applyPersistedAuthDigest(auth *coreauth.Auth, data []byte) error {
	if auth == nil {
		return fmt.Errorf("auth is nil")
	}
	digest, err := coreauth.DigestPersistedJSON(data)
	if err != nil {
		return fmt.Errorf("digest persisted auth: %w", err)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[coreauth.PersistedSHA256Attribute] = hex.EncodeToString(digest[:])
	return nil
}

func authsFromUploadedFile(
	ctx context.Context,
	parser uploadedAuthParser,
	authDir,
	path string,
	data []byte,
) ([]*coreauth.Auth, bool, bool, error) {
	if parser != nil {
		metadata := make(map[string]any)
		if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
			return nil, false, false, fmt.Errorf("invalid auth file: %w", errUnmarshal)
		}
		coreauth.NormalizeCredentialMetadata(metadata)
		if _, _, errProfile := coreauth.ClaudeFingerprintProfile(&coreauth.Auth{Metadata: metadata}); errProfile != nil {
			return nil, false, false, errProfile
		}
		provider, _ := metadata["type"].(string)
		parseCtx := sdkAuth.WithPluginAuthParser(ctx, parser)
		auths, handled, errParse := sdkAuth.ParsePluginAuthFiles(
			parseCtx,
			strings.TrimSpace(provider),
			path,
			filepath.Base(path),
			data,
		)
		if errParse != nil {
			return nil, false, false, fmt.Errorf("parse plugin auth: %w", errParse)
		}
		if handled {
			for _, auth := range auths {
				if errDigest := applyPersistedAuthDigest(auth, data); errDigest != nil {
					return nil, false, true, errDigest
				}
			}
			return auths, false, true, nil
		}
	}
	auth, hasLastRefresh, errAuth := authFromFile(authDir, path, data)
	if errAuth != nil || auth == nil {
		return nil, hasLastRefresh, false, errAuth
	}
	return []*coreauth.Auth{auth}, hasLastRefresh, false, nil
}

func authFromUploadedFile(
	ctx context.Context,
	parser uploadedAuthParser,
	authDir,
	path string,
	data []byte,
) (*coreauth.Auth, bool, error) {
	auths, hasLastRefresh, _, errParse := authsFromUploadedFile(ctx, parser, authDir, path, data)
	if errParse != nil || len(auths) == 0 {
		return nil, hasLastRefresh, errParse
	}
	return auths[0], hasLastRefresh, nil
}

func persistUploadedAuthFile(
	ctx context.Context,
	store coreauth.Store,
	path string,
	data []byte,
) error {
	unlockFile := authfile.LockFileMutation(path)
	defer unlockFile()
	previous, errRead := os.ReadFile(path)
	existed := errRead == nil
	if errRead != nil && !errors.Is(errRead, os.ErrNotExist) {
		return fmt.Errorf("read existing auth file: %w", errRead)
	}
	if errWrite := authfile.WriteTokenFileAtomic(path, data); errWrite != nil {
		return fmt.Errorf("failed to write auth file: %w", errWrite)
	}
	persister, ok := store.(uploadedAuthPersister)
	if !ok {
		return nil
	}
	if errPersist := persister.PersistAuthFiles(ctx, "Upload auth "+filepath.Base(path), path); errPersist != nil {
		var errRestore error
		if existed {
			errRestore = authfile.WriteTokenFileAtomic(path, previous)
		} else if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			errRestore = errRemove
		}
		return errors.Join(fmt.Errorf("persist uploaded auth: %w", errPersist), errRestore)
	}
	return nil
}

func (h *Handler) disableSupersededFileAuths(
	ctx context.Context,
	path string,
	updated *coreauth.Auth,
) error {
	if h == nil || h.authManager == nil || updated == nil {
		return nil
	}
	syncCtx := coreauth.WithSkipPersist(context.WithoutCancel(ctx))
	for _, candidate := range h.authManager.List() {
		if candidate == nil || candidate.ID == updated.ID || !authMatchesPath(candidate, path) {
			continue
		}
		candidate.Disabled = true
		candidate.Status = coreauth.StatusDisabled
		candidate.StatusMessage = "superseded by file-backed auth sync"
		candidate.UpdatedAt = updated.UpdatedAt
		disabledAuth, errUpdate := h.authManager.Update(syncCtx, candidate)
		if errUpdate != nil {
			return fmt.Errorf("disable superseded auth %s: %w", candidate.ID, errUpdate)
		}
		if disabledAuth == nil {
			return fmt.Errorf("disable superseded auth %s: update was superseded", candidate.ID)
		}
		h.syncQuotaSchedulerForAuth(disabledAuth)
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(syncCtx, disabledAuth); errHook != nil {
				return fmt.Errorf("synchronize superseded auth %s: %w", candidate.ID, errHook)
			}
		}
	}
	return nil
}

func (h *Handler) publishUploadedPluginAuths(
	ctx context.Context,
	path string,
	data []byte,
	auths []*coreauth.Auth,
) error {
	if h == nil || h.authManager == nil {
		return fmt.Errorf("core auth manager unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	publicationCtx := coreauth.WithAllowAuthRecreate(
		coreauth.WithSkipPersist(context.WithoutCancel(ctx)),
	)
	updatedAuths := make([]*coreauth.Auth, 0, len(auths))
	if len(auths) == 0 {
		if errPersist := persistUploadedAuthFile(ctx, h.tokenStore, path, data); errPersist != nil {
			return errPersist
		}
	} else {
		first := auths[0]
		updated, errMutate := h.authManager.MutatePersistentAuth(
			coreauth.WithAllowAuthRecreate(ctx),
			first.ID,
			func(persistCtx context.Context) (*coreauth.Auth, error) {
				if errPersist := persistUploadedAuthFile(persistCtx, h.tokenStore, path, data); errPersist != nil {
					return nil, errPersist
				}
				return first, nil
			},
		)
		if errMutate != nil {
			return errMutate
		}
		if updated == nil {
			return fmt.Errorf("persisted plugin auth mutation was superseded")
		}
		updatedAuths = append(updatedAuths, updated)

		for _, auth := range auths[1:] {
			var (
				published  *coreauth.Auth
				errPublish error
			)
			if _, exists := h.authManager.GetByID(auth.ID); exists {
				published, errPublish = h.authManager.Update(publicationCtx, auth)
			} else {
				published, errPublish = h.authManager.Register(publicationCtx, auth)
			}
			if errPublish != nil {
				return fmt.Errorf("publish plugin auth %s: %w", auth.ID, errPublish)
			}
			if published == nil {
				return fmt.Errorf("publish plugin auth %s: update was superseded", auth.ID)
			}
			updatedAuths = append(updatedAuths, published)
		}
	}

	currentIDs := make(map[string]struct{}, len(updatedAuths))
	for _, auth := range updatedAuths {
		currentIDs[auth.ID] = struct{}{}
	}
	removeCtx := coreauth.WithRuntimeOnlyRemove(publicationCtx)
	for _, candidate := range findAuthsByPath(h.authManager.List(), path) {
		if _, current := currentIDs[candidate.ID]; current {
			continue
		}
		removed, errRemove := h.authManager.RemoveIfGeneration(
			removeCtx,
			candidate.ID,
			candidate.Generation,
		)
		if errRemove != nil {
			return fmt.Errorf("remove stale plugin auth %s: %w", candidate.ID, errRemove)
		}
		if !removed {
			return fmt.Errorf("remove stale plugin auth %s: update was superseded", candidate.ID)
		}
	}

	h.syncQuotaSchedulerRemoval(path)
	for _, auth := range updatedAuths {
		h.syncQuotaSchedulerForAuth(auth)
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(publicationCtx, auth); errHook != nil {
				return fmt.Errorf("failed to synchronize plugin auth %s: %w", auth.ID, errHook)
			}
		}
	}
	return nil
}

func (h *Handler) saveUploadedAuth(ctx context.Context, authDir, path string, data []byte) error {
	if h == nil || h.authManager == nil {
		return fmt.Errorf("core auth manager unavailable")
	}
	var parser uploadedAuthParser
	if candidate, ok := h.pluginHost.(uploadedAuthParser); ok {
		parser = candidate
	}
	auths, _, pluginHandled, errValidate := authsFromUploadedFile(ctx, parser, authDir, path, data)
	if errValidate != nil {
		return errValidate
	}
	if pluginHandled {
		return h.publishUploadedPluginAuths(ctx, path, data, auths)
	}
	if len(auths) != 1 || auths[0] == nil {
		return fmt.Errorf("uploaded auth file did not produce an auth")
	}
	auth := auths[0]
	mutationCtx := coreauth.WithAllowAuthRecreate(ctx)
	updated, errMutate := h.authManager.MutatePersistentAuth(
		mutationCtx,
		auth.ID,
		func(persistCtx context.Context) (*coreauth.Auth, error) {
			if errPersist := persistUploadedAuthFile(persistCtx, h.tokenStore, path, data); errPersist != nil {
				return nil, errPersist
			}
			return auth, nil
		},
	)
	if errMutate != nil {
		return errMutate
	}
	if updated == nil {
		return fmt.Errorf("persisted auth mutation was superseded")
	}

	if errSync := h.disableSupersededFileAuths(mutationCtx, path, updated); errSync != nil {
		return errSync
	}
	h.syncQuotaSchedulerForAuth(updated)
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updated); errHook != nil {
			return fmt.Errorf("failed to synchronize auth runtime: %w", errHook)
		}
	}
	return nil
}

// UploadAuthFile uploads auth files via multipart or raw JSON with ?name= parameter.
func (h *Handler) UploadAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	authDir := h.authDirSnapshot()
	if authDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}
	ctx := c.Request.Context()
	form, err := c.MultipartForm()
	if err == nil && form != nil && len(form.File["file"]) > 0 {
		uploadedFiles := make([]string, 0, len(form.File["file"]))
		failed := make([]gin.H, 0)
		for _, file := range form.File["file"] {
			dst, errV := validateAuthFileName(authDir, filepath.Base(file.Filename))
			if errV != nil {
				failed = append(failed, gin.H{"name": file.Filename, "error": errV.Error()})
				continue
			}
			source, errOpen := file.Open()
			if errOpen != nil {
				failed = append(
					failed, gin.H{"name": file.Filename, "error": fmt.Sprintf("failed to open upload: %v", errOpen)},
				)
				continue
			}
			data, errRead := io.ReadAll(io.LimitReader(source, 10<<20))
			errClose := source.Close()
			if errRead != nil || errClose != nil {
				failed = append(failed, gin.H{"name": file.Filename, "error": "failed to read upload"})
				continue
			}
			if errSave := h.saveUploadedAuth(ctx, authDir, dst, data); errSave != nil {
				failed = append(failed, gin.H{"name": file.Filename, "error": errSave.Error()})
				continue
			}
			uploadedFiles = append(uploadedFiles, filepath.Base(dst))
		}
		status := "ok"
		statusCode := http.StatusOK
		if len(failed) > 0 {
			status = "partial"
			statusCode = http.StatusMultiStatus
		}
		c.JSON(statusCode, gin.H{"status": status, "uploaded": len(uploadedFiles), "files": uploadedFiles, "failed": failed})
		return
	}
	dst, errV := validateAuthFileName(authDir, c.Query("name"))
	if errV != nil {
		c.JSON(400, gin.H{"error": errV.Error()})
		return
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err = h.saveUploadedAuth(ctx, authDir, dst, data); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok", "uploaded": 1, "files": []string{filepath.Base(dst)}, "failed": []string{}})
}

// DeleteAuthFile deletes auth files: single by name, batch by body, or all.
func (h *Handler) DeleteAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	authDir := h.authDirSnapshot()
	if authDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}
	ctx := c.Request.Context()
	if all := c.Query("all"); all == "true" || all == "1" || all == "*" {
		if c.GetHeader("X-Confirm-Action") != "delete-all-auth-files" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "delete-all confirmation required"})
			return
		}
		entries, err := os.ReadDir(authDir)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
			return
		}
		deleted := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				continue
			}
			full := filepath.Join(authDir, name)
			if !filepath.IsAbs(full) {
				if abs, errAbs := filepath.Abs(full); errAbs == nil {
					full = abs
				}
			}
			if errDelete := h.deleteAuthFile(ctx, authDir, full); errDelete != nil {
				status := http.StatusInternalServerError
				if errors.Is(errDelete, errAuthFileChangedDuringDeletion) {
					status = http.StatusConflict
				}
				c.JSON(status, gin.H{"error": errDelete.Error()})
				return
			}
			deleted++
			h.syncQuotaSchedulerRemoval(full)
		}
		c.JSON(200, gin.H{"status": "ok", "deleted": deleted})
		return
	}
	var body struct {
		Names []string `json:"names"`
	}
	if err := c.ShouldBindJSON(&body); err == nil && len(body.Names) > 0 {
		deletedFiles := make([]string, 0, len(body.Names))
		failed := make([]gin.H, 0)
		for _, rawName := range body.Names {
			name := strings.TrimSpace(rawName)
			if _, childIdentity := h.lookupPluginVirtualChildIdentity(name, ""); childIdentity {
				failed = append(failed, gin.H{"name": name, "error": errPluginVirtualAuth.Error()})
				continue
			}
			full, errV := validateAuthFileName(authDir, name)
			if errV != nil {
				failed = append(failed, gin.H{"name": name, "error": errV.Error()})
				continue
			}
			if _, errStat := os.Stat(full); errStat != nil {
				if os.IsNotExist(errStat) {
					failed = append(failed, gin.H{"name": name, "error": "file not found"})
				} else {
					failed = append(
						failed, gin.H{"name": name, "error": fmt.Sprintf("failed to stat file: %v", errStat)},
					)
				}
				continue
			}
			if errDelete := h.deleteAuthFile(ctx, authDir, full); errDelete != nil {
				failed = append(failed, gin.H{"name": name, "error": errDelete.Error()})
				continue
			}
			h.syncQuotaSchedulerRemoval(full)
			deletedFiles = append(deletedFiles, filepath.Base(full))
		}
		status := "ok"
		statusCode := http.StatusOK
		if len(failed) > 0 {
			status = "partial"
			statusCode = http.StatusMultiStatus
		}
		c.JSON(statusCode, gin.H{"status": status, "deleted": len(deletedFiles), "files": deletedFiles, "failed": failed})
		return
	}

	name := strings.TrimSpace(c.Query("name"))
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if _, childIdentity := h.lookupPluginVirtualChildIdentity(name, authIndex); childIdentity {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}
	full, errV := validateAuthFileName(authDir, name)
	if errV != nil {
		c.JSON(400, gin.H{"error": errV.Error()})
		return
	}
	if _, errStat := os.Stat(full); errStat != nil {
		if os.IsNotExist(errStat) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to stat file: %v", errStat)})
		}
		return
	}
	if errDelete := h.deleteAuthFile(ctx, authDir, full); errDelete != nil {
		status := http.StatusInternalServerError
		if errors.Is(errDelete, errAuthFileChangedDuringDeletion) {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": errDelete.Error()})
		return
	}
	h.syncQuotaSchedulerRemoval(full)
	c.JSON(200, gin.H{"status": "ok", "deleted": 1, "files": []string{filepath.Base(full)}})
}

func authIDForPath(path, authDir string) string {
	return authfile.FileAuthID(path, authDir)
}

func normalizeAuthProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "gemini" {
		return string(quota.TypeGeminiCli)
	}
	if provider == "anthropic" {
		return string(quota.TypeClaude)
	}
	if provider == "cognition" {
		return string(quota.TypeDevin)
	}
	return provider
}

func quotaCredentialTypeForProvider(provider string) (quota.CredentialType, bool) {
	switch normalizeAuthProvider(provider) {
	case string(quota.TypeClaude):
		return quota.TypeClaude, true
	case string(quota.TypeCodex):
		return quota.TypeCodex, true
	case string(quota.TypeGeminiCli):
		return quota.TypeGeminiCli, true
	case string(quota.TypeAntigravity):
		return quota.TypeAntigravity, true
	case string(quota.TypeKimi):
		return quota.TypeKimi, true
	case string(quota.TypeXAI), "x-ai", "grok":
		return quota.TypeXAI, true
	case string(quota.TypeDevin), "cognition":
		return quota.TypeDevin, true
	default:
		return "", false
	}
}

func authFilePathsEqual(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if absolute, errAbsolute := filepath.Abs(left); errAbsolute == nil {
		left = absolute
	}
	if absolute, errAbsolute := filepath.Abs(right); errAbsolute == nil {
		right = absolute
	}
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func authMatchesPath(auth *coreauth.Auth, path string) bool {
	if auth == nil {
		return false
	}
	if authFilePathsEqual(authAttribute(auth, "path"), path) {
		return true
	}
	return authFilePathsEqual(authAttribute(auth, "source"), path)
}

func pluginVirtualSourcePath(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	if source := strings.TrimSpace(auth.Attributes[coreauth.AttributeVirtualSource]); source != "" {
		return source
	}
	if source := strings.TrimSpace(auth.Attributes[coreauth.AttributeSource]); source != "" {
		return source
	}
	return strings.TrimSpace(auth.Attributes[coreauth.AttributePath])
}

func pluginVirtualChildIdentity(auth *coreauth.Auth, name, authIndex string) bool {
	if !coreauth.IsPluginVirtualAuth(auth) {
		return false
	}
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	if authIndex != "" {
		return true
	}
	return name != "" && name != strings.TrimSpace(auth.FileName)
}

func (h *Handler) lookupPluginVirtualChildIdentity(name, authIndex string) (*coreauth.Auth, bool) {
	if h == nil || h.authManager == nil {
		return nil, false
	}
	name = strings.TrimSpace(name)
	authIndex = strings.TrimSpace(authIndex)
	for _, auth := range h.authManager.List() {
		if !coreauth.IsPluginVirtualAuth(auth) {
			continue
		}
		index := strings.TrimSpace(auth.EnsureIndex())
		if authIndex != "" && authIndex == index || name != "" &&
			(name == strings.TrimSpace(auth.ID) || name == index) {
			return auth, true
		}
	}
	return nil, false
}

func (h *Handler) updatePluginVirtualAuthStatus(
	ctx context.Context,
	target *coreauth.Auth,
	disabled bool,
) error {
	if h == nil || h.authManager == nil || target == nil {
		return fmt.Errorf("core auth manager unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sourcePath := pluginVirtualSourcePath(target)
	if sourcePath == "" {
		return fmt.Errorf("plugin virtual auth source path is empty")
	}
	data, errRead := os.ReadFile(sourcePath)
	if errRead != nil {
		return fmt.Errorf("read plugin auth source: %w", errRead)
	}
	metadata := make(map[string]any)
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return fmt.Errorf("parse plugin auth source: %w", errUnmarshal)
	}
	coreauth.NormalizeCredentialMetadata(metadata)
	metadata["disabled"] = disabled
	updatedRaw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return fmt.Errorf("encode plugin auth source: %w", errMarshal)
	}

	h.mu.RLock()
	pluginHost := h.pluginHost
	h.mu.RUnlock()
	parser, ok := pluginHost.(sdkAuth.PluginAuthParser)
	if !ok || parser == nil {
		return fmt.Errorf("plugin auth parser unavailable")
	}
	provider, _ := metadata["type"].(string)
	parseCtx := sdkAuth.WithPluginAuthParser(ctx, parser)
	parsed, handled, errParse := sdkAuth.ParsePluginAuthFiles(
		parseCtx,
		provider,
		sourcePath,
		filepath.Base(sourcePath),
		updatedRaw,
	)
	if errParse != nil {
		return fmt.Errorf("parse updated plugin auth source: %w", errParse)
	}
	if !handled || len(parsed) == 0 {
		return fmt.Errorf("plugin auth parser no longer owns source %s", filepath.Base(sourcePath))
	}

	currentByID := make(map[string]*coreauth.Auth)
	for _, auth := range findAuthsByPath(h.authManager.List(), sourcePath) {
		if coreauth.IsPluginVirtualAuth(auth) {
			currentByID[auth.ID] = auth
		}
	}
	if len(currentByID) != len(parsed) {
		return fmt.Errorf("plugin auth identities changed while updating source status")
	}
	snapshot := pluginLoginTargetSnapshot{path: sourcePath, existed: true, persisted: data}
	for _, auth := range parsed {
		current := currentByID[auth.ID]
		if current == nil || !coreauth.IsPluginVirtualAuth(auth) || current.EnsureIndex() != auth.EnsureIndex() {
			return fmt.Errorf("plugin auth identities changed while updating source status")
		}
		snapshot.runtime = append(snapshot.runtime, current.Clone())
	}

	store := h.tokenStoreSnapshot()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	sourceRecord := &coreauth.Auth{
		ID:       authIDForPath(sourcePath, h.authDirSnapshot()),
		Provider: strings.TrimSpace(provider),
		FileName: filepath.Base(sourcePath),
		Disabled: disabled,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributePath:          sourcePath,
			coreauth.AttributeSource:        sourcePath,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
		},
		Metadata: metadata,
	}
	if disabled {
		sourceRecord.Status = coreauth.StatusDisabled
	}
	if _, errSave := store.Save(ctx, sourceRecord); errSave != nil {
		if errors.Is(errSave, coreauth.ErrAuthDurableStateUncertain) {
			return errors.Join(fmt.Errorf("persist plugin auth source status: %w", errSave),
				h.restorePluginLoginPersisted(ctx, h.authDirSnapshot(), snapshot))
		}
		return fmt.Errorf("persist plugin auth source status: %w", errSave)
	}
	restoreOnFailure := func(failure error) error {
		persistError := h.restorePluginLoginPersisted(ctx, h.authDirSnapshot(), snapshot)
		restoreCtx := coreauth.WithSkipPersist(context.WithoutCancel(ctx))
		var runtimeError error
		for _, previous := range snapshot.runtime {
			restored, errUpdate := h.authManager.Update(restoreCtx, previous.Clone())
			if errUpdate != nil {
				runtimeError = errors.Join(runtimeError, fmt.Errorf("restore plugin auth %s: %w", previous.ID, errUpdate))
				continue
			}
			if restored == nil {
				runtimeError = errors.Join(runtimeError, fmt.Errorf("plugin auth %s changed during rollback", previous.ID))
				continue
			}
			h.syncQuotaSchedulerForAuth(restored)
			if h.postAuthPersistHook != nil {
				if errHook := h.postAuthPersistHook(restoreCtx, restored); errHook != nil {
					runtimeError = errors.Join(runtimeError, fmt.Errorf("restore plugin auth runtime %s: %w", previous.ID, errHook))
				}
			}
		}
		return errors.Join(failure, persistError, runtimeError)
	}

	digest, errDigest := coreauth.DigestPersistedJSON(updatedRaw)
	if errDigest != nil {
		return restoreOnFailure(fmt.Errorf("digest plugin auth source: %w", errDigest))
	}
	now := time.Now().UTC()
	digestText := hex.EncodeToString(digest[:])
	publicationCtx := coreauth.WithSkipPersist(context.WithoutCancel(ctx))
	for _, parsedAuth := range parsed {
		current := currentByID[parsedAuth.ID]
		next := current.Clone()
		next.Disabled = parsedAuth.Disabled
		next.Status = parsedAuth.Status
		next.StatusMessage = parsedAuth.StatusMessage
		if next.Metadata == nil {
			next.Metadata = make(map[string]any)
		}
		if parsedAuth.Metadata != nil {
			if value, exists := parsedAuth.Metadata["disabled"]; exists {
				next.Metadata["disabled"] = value
			} else {
				delete(next.Metadata, "disabled")
			}
		} else {
			delete(next.Metadata, "disabled")
		}
		if next.Attributes == nil {
			next.Attributes = make(map[string]string)
		}
		next.Attributes[coreauth.PersistedSHA256Attribute] = digestText
		next.Attributes[coreauth.PersistedReadAtAttribute] = now.Format(time.RFC3339Nano)
		next.UpdatedAt = now
		updated, errUpdate := h.authManager.Update(publicationCtx, next)
		if errUpdate != nil {
			return restoreOnFailure(fmt.Errorf("publish plugin auth source status: %w", errUpdate))
		}
		if updated == nil {
			return restoreOnFailure(fmt.Errorf("plugin auth source status update was superseded"))
		}
		h.syncQuotaSchedulerForAuth(updated)
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(publicationCtx, updated); errHook != nil {
				return restoreOnFailure(fmt.Errorf("failed to synchronize auth runtime: %w", errHook))
			}
		}
	}
	return nil
}

func findAuthsByPath(auths []*coreauth.Auth, path string) []*coreauth.Auth {
	matched := make([]*coreauth.Auth, 0, 2)
	for _, auth := range auths {
		if authMatchesPath(auth, path) {
			matched = append(matched, auth)
		}
	}
	return matched
}

func mergeAuthRuntimeState(target *coreauth.Auth, existing *coreauth.Auth, hasLastRefresh bool) {
	if target == nil || existing == nil {
		return
	}
	target.CreatedAt = existing.CreatedAt
	if !hasLastRefresh {
		target.LastRefreshedAt = existing.LastRefreshedAt
	}
	target.NextRefreshAfter = existing.NextRefreshAfter
	target.NextRetryAfter = existing.NextRetryAfter
	target.Runtime = existing.Runtime
	target.Quota = existing.Quota
	target.LastError = existing.LastError
	target.Unavailable = existing.Unavailable
	target.ModelStates = existing.ModelStates
	target.StateGeneration = existing.StateGeneration
	target.StateEffectScope = existing.StateEffectScope
	target.StateEffectKey = existing.StateEffectKey
	target.ResetAuthority = existing.ResetAuthority
	if target.StatusMessage == "" {
		target.StatusMessage = existing.StatusMessage
	}
	if target.Status == coreauth.StatusActive && existing.Status != "" && existing.Status != coreauth.StatusActive {
		target.Status = existing.Status
	}
}

func (h *Handler) syncQuotaSchedulerForAuth(auth *coreauth.Auth) {
	if h == nil || h.quotaScheduler == nil || auth == nil {
		return
	}
	fileName := strings.TrimSpace(auth.FileName)
	if fileName == "" {
		return
	}
	credType, ok := quotaCredentialTypeForProvider(auth.Provider)
	if !ok {
		return
	}
	auth.EnsureIndex()
	credentialRevision := auth.Index
	if auth.Attributes != nil {
		if digest := strings.ToLower(strings.TrimSpace(auth.Attributes[coreauth.PersistedSHA256Attribute])); digest != "" {
			credentialRevision = digest
		}
	}
	h.quotaScheduler.RegisterBinding(fileName, credType, auth.Index, credentialRevision)
	h.quotaScheduler.SetBindingDisabled(fileName, credType, auth.Index, auth.Disabled)
}

func (h *Handler) syncQuotaSchedulerRemoval(path string) {
	if h == nil || h.quotaScheduler == nil {
		return
	}
	fileName := strings.TrimSpace(filepath.Base(path))
	if fileName == "" {
		return
	}
	h.quotaScheduler.Unregister(fileName)
}

func authFromFile(authDir, path string, data []byte) (*coreauth.Auth, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false, fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var errRead error
		data, errRead = os.ReadFile(path)
		if errRead != nil {
			return nil, false, fmt.Errorf("failed to read auth file: %w", errRead)
		}
	}
	metadata := make(map[string]any)
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil, false, fmt.Errorf("invalid auth file: %w", errUnmarshal)
	}
	coreauth.NormalizeCredentialMetadata(metadata)
	if errWeight := coreauth.ValidateAuthWeight(&coreauth.Auth{Metadata: metadata}); errWeight != nil {
		return nil, false, errWeight
	}
	provider, _ := metadata["type"].(string)
	provider = normalizeAuthProvider(provider)
	if provider == "" {
		provider = "unknown"
	}
	label := provider
	if email, ok := metadata["email"].(string); ok && email != "" {
		label = email
	}
	lastRefresh, hasLastRefresh := extractLastRefreshTimestamp(metadata)
	authID := authIDForPath(path, authDir)
	if authID == "" {
		authID = path
	}
	attr := map[string]string{
		"path":   path,
		"source": path,
	}
	if rawPrefix, ok := metadata["prefix"].(string); ok {
		trimmed := strings.Trim(strings.TrimSpace(rawPrefix), "/")
		if trimmed != "" && !strings.Contains(trimmed, "/") {
			attr["prefix"] = trimmed
		}
	}
	if rawPriority, ok := metadata["priority"]; ok {
		switch value := rawPriority.(type) {
		case float64:
			attr["priority"] = strconv.Itoa(int(value))
		case string:
			priority := strings.TrimSpace(value)
			if _, errPriority := strconv.Atoi(priority); errPriority == nil {
				attr["priority"] = priority
			}
		}
	}
	if websockets, ok := authFileBoolValue(metadata["websockets"]); ok {
		attr["websockets"] = strconv.FormatBool(websockets)
	}
	proxyURL := ""
	if rawProxyURL, ok := metadata["proxy_url"].(string); ok {
		proxyURL = strings.TrimSpace(rawProxyURL)
		if proxyURL != "" {
			if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
				return nil, false, fmt.Errorf("proxy URL rejected: %w", errValidate)
			}
		}
	}
	disabled, _ := metadata["disabled"].(bool)
	status := coreauth.StatusActive
	if disabled {
		status = coreauth.StatusDisabled
	}
	if rawStatus, ok := metadata["status"].(string); ok {
		switch candidate := coreauth.Status(strings.TrimSpace(rawStatus)); candidate {
		case coreauth.StatusUnknown, coreauth.StatusActive, coreauth.StatusPending, coreauth.StatusRefreshing, coreauth.StatusError, coreauth.StatusDisabled:
			status = candidate
		}
	}
	statusMessage, _ := metadata["status_message"].(string)
	auth := &coreauth.Auth{
		ID:            authID,
		Provider:      provider,
		FileName:      filepath.Base(path),
		Label:         label,
		Prefix:        attr["prefix"],
		Status:        status,
		StatusMessage: strings.TrimSpace(statusMessage),
		Disabled:      disabled,
		Attributes:    attr,
		ProxyURL:      proxyURL,
		Metadata:      metadata,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	if errHeaders := coreauth.ApplyCustomHeadersFromMetadata(auth); errHeaders != nil {
		return nil, false, errHeaders
	}
	if errWeight := coreauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
		return nil, false, errWeight
	}
	if errProfile := coreauth.ApplyClaudeFingerprintProfileMetadata(auth, metadata); errProfile != nil {
		return nil, false, errProfile
	}
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if errDigest := applyPersistedAuthDigest(auth, data); errDigest != nil {
		return nil, false, errDigest
	}
	return auth, hasLastRefresh, nil
}

func (h *Handler) registerAuthFromFile(ctx context.Context, authDir, path string, data []byte) error {
	if h.authManager == nil {
		return nil
	}
	auth, hasLastRefresh, errLoad := authFromFile(authDir, path, data)
	if errLoad != nil {
		return errLoad
	}
	ctx = coreauth.WithAllowAuthRecreate(ctx)
	allAuths := h.authManager.List()
	existingByID, hasExistingByID := h.authManager.GetByID(auth.ID)
	var existingByPath *coreauth.Auth
	for _, candidate := range findAuthsByPath(allAuths, path) {
		if candidate != nil && !strings.EqualFold(candidate.ID, auth.ID) {
			existingByPath = candidate
			break
		}
	}
	if hasExistingByID {
		mergeAuthRuntimeState(auth, existingByID, hasLastRefresh)
	}
	if existingByPath != nil {
		mergeAuthRuntimeState(auth, existingByPath, hasLastRefresh)
	}
	if hasExistingByID {
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			return errUpdate
		}
	} else {
		if _, errRegister := h.authManager.Register(ctx, auth); errRegister != nil {
			return errRegister
		}
	}
	for _, candidate := range allAuths {
		if candidate == nil || candidate.ID == auth.ID || !authMatchesPath(candidate, path) {
			continue
		}
		candidate.Disabled = true
		candidate.Status = coreauth.StatusDisabled
		candidate.StatusMessage = "superseded by file-backed auth sync"
		candidate.UpdatedAt = auth.UpdatedAt
		if _, errUpdate := h.authManager.Update(ctx, candidate); errUpdate != nil {
			return errUpdate
		}
	}
	h.syncQuotaSchedulerForAuth(auth)
	return nil
}

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	authIndex := strings.TrimSpace(req.AuthIndex)
	if name == "" && authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or auth_index is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	h.authStatusMu.Lock()
	defer h.authStatusMu.Unlock()

	ctx := c.Request.Context()

	targetAuth, _ := h.lookupAuthFile(name, authIndex)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		if pluginVirtualChildIdentity(targetAuth, name, authIndex) {
			c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
			return
		}
		if errUpdate := h.updatePluginVirtualAuthStatus(ctx, targetAuth, *req.Disabled); errUpdate != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update plugin auth source: %v", errUpdate)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
		return
	}

	// Update disabled state
	targetAuth.Disabled = *req.Disabled
	if *req.Disabled {
		targetAuth.Status = coreauth.StatusDisabled
		targetAuth.StatusMessage = "disabled via management API"
	} else {
		targetAuth.Status = coreauth.StatusActive
		targetAuth.StatusMessage = ""
	}
	targetAuth.UpdatedAt = time.Now()

	updatedAuth, errUpdate := h.authManager.Update(coreauth.WithDurableAuthPublication(ctx), targetAuth)
	if errUpdate != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errUpdate)})
		return
	}
	if updatedAuth == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "auth update was superseded"})
		return
	}

	h.syncQuotaSchedulerForAuth(updatedAuth)
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updatedAuth); errHook != nil {
			log.Errorf("post-auth persist hook failed for status update on %s: %v", targetAuth.ID, errHook)
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": fmt.Sprintf("failed to synchronize auth runtime: %v", errHook)},
			)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// PatchAuthFileFields updates editable fields of an auth file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name                 string            `json:"name"`
		AuthIndex            string            `json:"auth-index"`
		Prefix               *string           `json:"prefix"`
		ProxyURL             *string           `json:"proxy_url"`
		Priority             *int              `json:"priority"`
		Weight               json.RawMessage   `json:"weight"`
		Headers              map[string]string `json:"headers"`
		Note                 *string           `json:"note"`
		DisableCooling       json.RawMessage   `json:"disable_cooling"`
		DisableCoolingLegacy json.RawMessage   `json:"disable-cooling"`
		RequestRetry         json.RawMessage   `json:"request_retry"`
		RequestRetryLegacy   json.RawMessage   `json:"request-retry"`
		Websockets           *bool             `json:"websockets"`
		UsingAPI             *bool             `json:"using_api"`
		ExcludedModels       *[]string         `json:"excluded_models"`
		ExcludedModelsLegacy *[]string         `json:"excluded-models"`
		FingerprintProfile   json.RawMessage   `json:"fingerprint_profile"`
		FingerprintLegacy    json.RawMessage   `json:"fingerprint-profile"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	authIndex := strings.TrimSpace(req.AuthIndex)
	if name == "" && authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or auth-index is required"})
		return
	}

	h.authStatusMu.Lock()
	defer h.authStatusMu.Unlock()

	ctx := c.Request.Context()

	var targetAuth *coreauth.Auth
	if authIndex != "" {
		if auth, ok := h.authManager.GetByID(authIndex); ok {
			targetAuth = auth
		} else {
			for _, auth := range h.authManager.List() {
				if auth != nil && auth.Index == authIndex {
					targetAuth = auth
					break
				}
			}
		}
	}
	if targetAuth == nil && name != "" {
		if auth, ok := h.authManager.GetByID(name); ok {
			targetAuth = auth
		} else {
			for _, auth := range h.authManager.List() {
				if auth != nil && auth.FileName == name {
					targetAuth = auth
					break
				}
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if (authIndex != "" && authIndex != targetAuth.ID && authIndex != targetAuth.Index) ||
		(name != "" && name != targetAuth.ID && name != targetAuth.FileName) {
		c.JSON(http.StatusConflict, gin.H{"error": "auth file identity mismatch"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}

	changed := false
	profileRaw := req.FingerprintProfile
	if len(profileRaw) == 0 {
		profileRaw = req.FingerprintLegacy
	}
	if len(profileRaw) > 0 {
		var value any
		if err := json.Unmarshal(profileRaw, &value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid fingerprint-profile"})
			return
		}
		if err := coreauth.ApplyClaudeFingerprintProfileMetadata(targetAuth, map[string]any{
			coreauth.AttributeClaudeFingerprintProfile: value,
		}); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		changed = true
	}
	if req.Prefix != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		prefix := strings.Trim(strings.TrimSpace(*req.Prefix), "/")
		targetAuth.Prefix = prefix
		if prefix == "" {
			delete(targetAuth.Metadata, "prefix")
		} else {
			targetAuth.Metadata["prefix"] = prefix
		}
		changed = true
	}
	if req.ProxyURL != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		proxyURL := strings.TrimSpace(*req.ProxyURL)
		if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		targetAuth.ProxyURL = proxyURL
		if proxyURL == "" {
			delete(targetAuth.Metadata, "proxy_url")
		} else {
			targetAuth.Metadata["proxy_url"] = proxyURL
		}
		changed = true
	}
	if req.Priority != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}
		if *req.Priority == 0 {
			delete(targetAuth.Metadata, "priority")
			delete(targetAuth.Attributes, "priority")
		} else {
			targetAuth.Metadata["priority"] = *req.Priority
			targetAuth.Attributes["priority"] = strconv.Itoa(*req.Priority)
		}
		changed = true
	}
	if len(req.Weight) > 0 {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}
		if strings.TrimSpace(string(req.Weight)) == "null" {
			delete(targetAuth.Metadata, coreauth.AttributeWeight)
			delete(targetAuth.Attributes, coreauth.AttributeWeight)
		} else {
			var rawWeight int
			if errUnmarshal := json.Unmarshal(req.Weight, &rawWeight); errUnmarshal != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "weight must be an integer"})
				return
			}
			weight, errWeight := credentialweight.Normalize(int64(rawWeight))
			if errWeight != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": errWeight.Error()})
				return
			}
			targetAuth.Metadata[coreauth.AttributeWeight] = weight
			targetAuth.Attributes[coreauth.AttributeWeight] = strconv.FormatInt(weight, 10)
		}
		changed = true
	}
	if req.Headers != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		merged := coreauth.ExtractCustomHeadersFromMetadata(targetAuth.Metadata)
		if merged == nil {
			merged = make(map[string]string)
		}
		for name, value := range req.Headers {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			value = strings.TrimSpace(value)
			if value == "" {
				delete(merged, name)
				continue
			}
			merged[name] = value
		}
		if err := coreauth.ValidateCustomHeaders(merged); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(merged) == 0 {
			delete(targetAuth.Metadata, "headers")
		} else {
			targetAuth.Metadata["headers"] = merged
		}
		if targetAuth.Attributes != nil {
			for key := range targetAuth.Attributes {
				if strings.HasPrefix(key, "header:") {
					delete(targetAuth.Attributes, key)
				}
			}
		}
		if err := coreauth.ApplyCustomHeadersFromMetadata(targetAuth); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		changed = true
	}
	if req.Note != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		note := strings.TrimSpace(*req.Note)
		if note == "" {
			delete(targetAuth.Metadata, "note")
		} else {
			targetAuth.Metadata["note"] = note
		}
		changed = true
	}
	coolingRaw, retryRaw := req.DisableCooling, req.RequestRetry
	if len(coolingRaw) == 0 {
		coolingRaw = req.DisableCoolingLegacy
	}
	if len(retryRaw) == 0 {
		retryRaw = req.RequestRetryLegacy
	}
	var cooling *bool
	var retry *int
	if err := (credentialOverridesPatch{DisableCooling: coolingRaw, RequestRetry: retryRaw}).apply(&cooling, &retry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(coolingRaw) > 0 || len(retryRaw) > 0 {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if len(coolingRaw) > 0 {
			delete(targetAuth.Metadata, "disable_cooling")
			delete(targetAuth.Metadata, "disable-cooling")
			if cooling != nil {
				targetAuth.Metadata["disable_cooling"] = *cooling
			}
		}
		if len(retryRaw) > 0 {
			delete(targetAuth.Metadata, "request_retry")
			delete(targetAuth.Metadata, "request-retry")
			if retry != nil && *retry >= 0 {
				targetAuth.Metadata["request_retry"] = *retry
			}
		}
		changed = true
	}
	if req.UsingAPI != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}
		targetAuth.Metadata["using_api"] = *req.UsingAPI
		targetAuth.Attributes["using_api"] = strconv.FormatBool(*req.UsingAPI)
		changed = true
	}
	excludedModels := req.ExcludedModels
	if excludedModels == nil {
		excludedModels = req.ExcludedModelsLegacy
	}
	if excludedModels != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		normalized := normalizeAuthFileExcludedModels(*excludedModels)
		targetAuth.Metadata["excluded_models"] = normalized
		delete(targetAuth.Metadata, "excluded-models")
		syncAuthFileExcludedModelsAttribute(targetAuth, h.configSnapshot())
		changed = true
	}
	if req.Websockets != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		targetAuth.Metadata["websockets"] = *req.Websockets
		syncAuthFileWebsocketsAttribute(targetAuth)
		changed = true
	}

	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	targetAuth.UpdatedAt = time.Now()

	updatedAuth, errUpdate := h.authManager.Update(coreauth.WithDurableAuthPublication(ctx), targetAuth)
	if errUpdate != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errUpdate)})
		return
	}
	if updatedAuth == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "auth update was superseded"})
		return
	}
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updatedAuth); errHook != nil {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{"error": fmt.Sprintf("failed to synchronize auth runtime: %v", errHook)},
			)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func authFileIDsEqual(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func authsForDeletion(auths []*coreauth.Auth, authDir, path string) []*coreauth.Auth {
	authID := authIDForPath(path, authDir)
	matched := make([]*coreauth.Auth, 0, 2)
	seen := make(map[string]struct{})
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if !authMatchesPath(auth, path) &&
			!authFileIDsEqual(auth.ID, path) &&
			!authFileIDsEqual(auth.ID, authID) {
			continue
		}
		if _, ok := seen[auth.ID]; ok {
			continue
		}
		seen[auth.ID] = struct{}{}
		matched = append(matched, auth)
	}
	sort.SliceStable(
		matched, func(i, j int) bool {
			iExact := authFileIDsEqual(matched[i].ID, authID)
			jExact := authFileIDsEqual(matched[j].ID, authID)
			if iExact != jExact {
				return iExact
			}
			if runtime.GOOS == "windows" {
				return strings.ToLower(matched[i].ID) < strings.ToLower(matched[j].ID)
			}
			return matched[i].ID < matched[j].ID
		},
	)
	return matched
}

func (h *Handler) deleteAuthFile(ctx context.Context, authDir, path string) error {
	if h == nil || h.authManager == nil {
		return fmt.Errorf("core auth manager unavailable")
	}
	matched := authsForDeletion(h.authManager.List(), authDir, path)
	if len(matched) == 0 {
		return h.deleteTokenRecord(ctx, path)
	}
	pluginVirtual := false
	for _, auth := range matched {
		if coreauth.IsPluginVirtualAuth(auth) {
			pluginVirtual = true
			break
		}
	}
	removeCtx := ctx
	if pluginVirtual {
		if errDelete := h.deleteTokenRecord(ctx, path); errDelete != nil {
			return errDelete
		}
		removeCtx = coreauth.WithRuntimeOnlyRemove(coreauth.WithSkipPersist(context.WithoutCancel(ctx)))
	}
	for _, auth := range matched {
		removed, errRemove := h.authManager.RemoveIfGeneration(removeCtx, auth.ID, auth.Generation)
		if errRemove != nil {
			return errRemove
		}
		if !removed {
			return fmt.Errorf("%w: %s", errAuthFileChangedDuringDeletion, filepath.Base(path))
		}
	}
	return nil
}

func (h *Handler) removeAuth(ctx context.Context, authDir, id string) error {
	if h == nil || h.authManager == nil {
		return nil
	}
	trimmedID := strings.TrimSpace(id)
	if trimmedID == "" {
		return nil
	}
	var errRemove error
	removed := make(map[string]struct{})
	for _, auth := range h.authManager.List() {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if authMatchesPath(auth, trimmedID) || authFileIDsEqual(auth.ID, trimmedID) {
			errRemove = errors.Join(errRemove, h.authManager.Remove(ctx, auth.ID))
			removed[auth.ID] = struct{}{}
		}
	}
	authID := authIDForPath(trimmedID, authDir)
	if authID == "" {
		authID = trimmedID
	}
	if _, ok := removed[authID]; !ok {
		errRemove = errors.Join(errRemove, h.authManager.Remove(ctx, authID))
	}
	return errRemove
}

func (h *Handler) deleteTokenRecord(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	store := h.tokenStoreSnapshot()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	versionedStore, versioned := store.(coreauth.VersionedStore)
	if !versioned {
		return store.Delete(ctx, path)
	}
	version, errVersion := coreauth.ReadPersistedVersion(path)
	if errVersion != nil {
		if errors.Is(errVersion, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read auth version before deletion: %w", errVersion)
	}
	deleted, errDelete := versionedStore.DeleteIfVersion(ctx, path, version)
	if errDelete != nil {
		return errDelete
	}
	if !deleted {
		return fmt.Errorf("%w: %s", errAuthFileChangedDuringDeletion, filepath.Base(path))
	}
	return nil
}

func (h *Handler) tokenStoreSnapshot() coreauth.Store {
	if h == nil {
		return nil
	}
	if h.tokenStore != nil {
		return h.tokenStore
	}
	return sdkAuth.GetTokenStore()
}

func (h *Handler) saveTokenRecord(
	ctx context.Context,
	authDir string,
	record *coreauth.Auth,
) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return "", fmt.Errorf("auth directory not configured")
	}
	store := h.tokenStoreSnapshot()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	if strings.TrimSpace(record.Attributes["path"]) == "" {
		fileName := filepath.Base(strings.TrimSpace(record.FileName))
		if fileName == "." || fileName == "" {
			fileName = filepath.Base(strings.TrimSpace(record.ID))
		}
		if fileName == "." || fileName == "" {
			return "", fmt.Errorf("token record file name is empty")
		}
		record.Attributes["path"] = filepath.Join(authDir, fileName)
	}
	targetPath := strings.TrimSpace(record.Attributes["path"])
	authID := authIDForPath(targetPath, authDir)
	if authID == "" {
		authID = strings.TrimSpace(record.ID)
	}
	if authID == "" {
		return "", fmt.Errorf("token record auth id is empty")
	}

	if h.postAuthHook != nil {
		if errHook := h.postAuthHook(ctx, record); errHook != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", errHook)
		}
	}
	save := func(saveCtx context.Context) (string, error) {
		if scopedStore, ok := store.(interface {
			SaveWithBaseDir(context.Context, string, *coreauth.Auth) (string, error)
		}); ok {
			return scopedStore.SaveWithBaseDir(saveCtx, authDir, record)
		}
		return store.Save(saveCtx, record)
	}

	if h.authManager == nil {
		savedPath, errSave := save(ctx)
		if errSave != nil {
			return savedPath, errSave
		}
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(ctx, record); errHook != nil {
				return savedPath, fmt.Errorf("failed to synchronize auth runtime: %w", errHook)
			}
		}
		return savedPath, nil
	}

	var savedPath string
	mutationCtx := coreauth.WithAllowAuthRecreate(ctx)
	updated, errMutate := h.authManager.MutatePersistentAuth(
		mutationCtx,
		authID,
		func(saveCtx context.Context) (*coreauth.Auth, error) {
			var errSave error
			savedPath, errSave = save(saveCtx)
			if errSave != nil {
				return nil, errSave
			}
			parsePath := strings.TrimSpace(savedPath)
			if parsePath == "" || !filepath.IsAbs(parsePath) {
				parsePath = targetPath
			}
			loaded, _, errLoad := authFromFile(authDir, parsePath, nil)
			if errLoad == nil {
				return loaded, nil
			}
			raw, errMarshal := json.Marshal(record.Metadata)
			if errMarshal != nil {
				return nil, errors.Join(errLoad, errMarshal)
			}
			loaded, _, errLoad = authFromFile(authDir, targetPath, raw)
			return loaded, errLoad
		},
	)
	if errMutate != nil {
		return "", errMutate
	}
	if updated == nil {
		return "", fmt.Errorf("persisted auth mutation was superseded")
	}

	if errSync := h.disableSupersededFileAuths(mutationCtx, targetPath, updated); errSync != nil {
		return "", errSync
	}
	h.syncQuotaSchedulerForAuth(updated)
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updated); errHook != nil {
			return savedPath, fmt.Errorf("failed to synchronize auth runtime: %w", errHook)
		}
	}
	return savedPath, nil
}

func (h *Handler) saveOAuthTokenRecord(
	ctx context.Context,
	authDir string,
	state string,
	provider string,
	record *coreauth.Auth,
) (string, error) {
	if err := guardOAuthSessionPendingForSave(state, provider); err != nil {
		return "", err
	}
	savedPath, errSave := h.saveTokenRecord(ctx, authDir, record)
	if errSave != nil {
		return "", errSave
	}
	fields := log.Fields{
		"provider": provider,
		"file":     filepath.Base(savedPath),
	}
	log.WithFields(fields).Info("OAuth credential file published")
	if h.authManager != nil {
		log.WithFields(fields).Info("OAuth credential published to runtime auth manager")
	}
	return savedPath, nil
}

// generatePKCEAndState generates PKCE codes and a random state parameter for OAuth flows.
// Returns false on failure after writing the error response to c.
func generatePKCEAndState(c *gin.Context) (*misc.PKCECodes, string, bool) {
	pkceCodes, err := misc.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return nil, "", false
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return nil, "", false
	}
	return pkceCodes, state, true
}

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing Claude authentication...")

	pkceCodes, state, ok := generatePKCEAndState(c)
	if !ok {
		return
	}

	// Initialize Claude auth service
	anthropicAuth := claude.NewAuth(flowCfg)

	// Generate authorization URL (then override redirect_uri to reuse server port)
	authURL, state, err := anthropicAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	registerOAuthSessionForAuthDir(state, "anthropic", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	forwarder, ok := h.setupOAuthForwarder(c, flowCfg, "anthropic", anthropicCallbackPort, "/anthropic/callback")
	if !ok {
		return
	}

	go func() {
		if forwarder != nil {
			defer stopCallbackForwarderInstance(anthropicCallbackPort, forwarder)
		}

		log.Info("Waiting for authentication callback...")
		resultMap, errWait := waitForOAuthCallback(flowCfg.AuthDir, "anthropic", state, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionCancelled) {
				return
			}
			authErr := oauthcommon.NewAuthenticationError(oauthcommon.ErrCallbackTimeout, errWait)
			log.Error(oauthcommon.GetUserFriendlyMessage(authErr))
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := oauthcommon.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(oauthcommon.GetUserFriendlyMessage(oauthErr))
			setOAuthSessionError(state, "Bad request")
			return
		}
		if resultMap["state"] != state {
			authErr := oauthcommon.NewAuthenticationError(
				oauthcommon.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]),
			)
			log.Error(oauthcommon.GetUserFriendlyMessage(authErr))
			setOAuthSessionError(state, "State code error")
			return
		}

		// Parse code (Claude may append state after '#')
		rawCode := resultMap["code"]
		code, _, _ := strings.Cut(rawCode, "#")

		// Exchange code for tokens using internal auth service
		bundle, errExchange := anthropicAuth.ExchangeCodeForTokens(ctx, code, state, pkceCodes)
		if errExchange != nil {
			authErr := oauthcommon.NewAuthenticationError(oauthcommon.ErrCodeExchangeFailed, errExchange)
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			setOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			return
		}

		// Create token storage
		tokenStorage := anthropicAuth.CreateTokenStorage(bundle)
		metadata := map[string]any{"email": tokenStorage.Email}
		if tokenStorage.AccountUUID != "" {
			metadata["account_uuid"] = tokenStorage.AccountUUID
		}
		if tokenStorage.OrganizationUUID != "" {
			metadata["organization_uuid"] = tokenStorage.OrganizationUUID
		}
		if tokenStorage.OrganizationName != "" {
			metadata["organization_name"] = tokenStorage.OrganizationName
		}
		if len(tokenStorage.DeviceIDs) > 0 {
			metadata[claude.DeviceIDsMetadataKey] = append([]string(nil), tokenStorage.DeviceIDs...)
		}
		record := &coreauth.Auth{
			ID:       fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Provider: "claude",
			FileName: fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "anthropic", record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			setOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		log.Infof("Authentication successful! Token saved to %s", savedPath)
		if bundle.APIKey != "" {
			log.Info("API key obtained and saved")
		}
		log.Info("You can now use Claude services through this CLI")
		completeOAuthSession(state)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}
	proxyHTTPClient := util.SetProxy(&flowCfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	// Optional project ID from query
	projectID := c.Query("project_id")

	log.Info("Initializing Google authentication...")

	// OAuth2 configuration using exported constants from internal/auth/gemini
	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	// Build authorization URL and return it immediately
	randomState, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(500, gin.H{"error": "failed to generate state"})
		return
	}
	state := "gem-" + randomState
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	registerOAuthSessionForAuthDir(state, "gemini", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := managementCallbackURL(flowCfg, "/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		log.Info("Waiting for authentication callback...")
		resultMap, errWait := waitForOAuthCallback(flowCfg.AuthDir, "gemini", state, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionCancelled) {
				return
			}
			log.Errorf("Gemini OAuth callback failed: %v", errWait)
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			log.Errorf("Authentication failed: %s", errStr)
			setOAuthSessionError(state, "Authentication failed")
			return
		}
		authCode := resultMap["code"]
		if authCode == "" {
			log.Error("Authentication failed: code not found")
			setOAuthSessionError(state, "Authentication failed: code not found")
			return
		}

		// Exchange authorization code for token
		token, err := conf.Exchange(ctx, authCode)
		if err != nil {
			log.Errorf("Failed to exchange token: %v", err)
			setOAuthSessionError(state, "Failed to exchange token")
			return
		}
		log.Info("Gemini OAuth token exchanged")

		requestedProjectID := strings.TrimSpace(projectID)

		// Create token storage (mirrors internal/auth/gemini createTokenStorage)
		authHTTPClient := conf.Client(ctx, token)
		req, errNewRequest := http.NewRequestWithContext(
			ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil,
		)
		if errNewRequest != nil {
			log.Errorf("Could not get user info: %v", errNewRequest)
			setOAuthSessionError(state, "Could not get user info")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

		resp, errDo := authHTTPClient.Do(req)
		if errDo != nil {
			log.Errorf("Failed to execute request: %v", errDo)
			setOAuthSessionError(state, "Failed to execute request")
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Printf("warn: failed to close response body: %v", errClose)
			}
		}()

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Errorf("Get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			setOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode))
			return
		}

		email := gjson.GetBytes(bodyBytes, "email").String()
		if email != "" {
			log.Infof("Authenticated user email: %s", email)
		} else {
			log.Warn("Failed to get user email from token")
		}

		// Marshal/unmarshal oauth2.Token to generic map and enrich fields
		var ifToken map[string]any
		jsonData, _ := json.Marshal(token)
		if errUnmarshal := json.Unmarshal(jsonData, &ifToken); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal token: %v", errUnmarshal)
			setOAuthSessionError(state, "Failed to unmarshal token")
			return
		}

		ifToken["token_uri"] = "https://oauth2.googleapis.com/token"
		ifToken["client_id"] = geminiAuth.ClientID
		ifToken["client_secret"] = geminiAuth.ClientSecret
		ifToken["scopes"] = geminiAuth.Scopes
		ifToken["universe_domain"] = "googleapis.com"

		ts := geminiAuth.TokenStorage{
			Token:     ifToken,
			ProjectID: requestedProjectID,
			Email:     email,
			Auto:      requestedProjectID == "",
		}

		// Initialize authenticated HTTP client via Auth to honor proxy settings
		gemAuth := geminiAuth.NewAuth()
		gemClient, errGetClient := gemAuth.GetAuthenticatedClient(
			ctx, &ts, flowCfg, &geminiAuth.WebLoginOptions{
				NoBrowser: true,
			},
		)
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			setOAuthSessionError(state, "Failed to get authenticated client")
			return
		}
		log.Info("Authentication successful.")

		if strings.EqualFold(requestedProjectID, "ALL") {
			ts.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &ts)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				setOAuthSessionError(
					state, oauthSessionErrorWithCause("Failed to complete Gemini CLI onboarding", errAll),
				)
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				setOAuthSessionError(
					state, oauthSessionErrorWithCause("Failed to verify Cloud AI API status", errVerify),
				)
				return
			}
			ts.ProjectID = strings.Join(projects, ",")
			ts.Checked = true
		} else if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
			ts.Auto = false
			if errSetup := geminiAuth.PerformCLISetup(ctx, gemClient, &ts, "", nil); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				setOAuthSessionError(state, oauthSessionErrorWithCause("Google One auto-discovery failed", errSetup))
				return
			}
			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Google One auto-discovery returned empty project ID")
				setOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := geminiAuth.CheckCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				setOAuthSessionError(
					state, oauthSessionErrorWithCause("Failed to verify Cloud AI API status", errCheck),
				)
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the auto-discovered project")
				setOAuthSessionError(state, "Cloud AI API not enabled")
				return
			}
		} else {
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &ts, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				setOAuthSessionError(
					state, oauthSessionErrorWithCause("Failed to complete Gemini CLI onboarding", errEnsure),
				)
				return
			}

			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Onboarding did not return a project ID")
				setOAuthSessionError(state, "Failed to resolve project ID")
				return
			}

			isChecked, errCheck := geminiAuth.CheckCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				setOAuthSessionError(
					state, oauthSessionErrorWithCause("Failed to verify Cloud AI API status", errCheck),
				)
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the selected project")
				setOAuthSessionError(state, "Cloud AI API not enabled")
				return
			}
		}

		recordMetadata := map[string]any{
			"email":      ts.Email,
			"project_id": ts.ProjectID,
			"auto":       ts.Auto,
			"checked":    ts.Checked,
		}

		fileName := geminiAuth.CredentialFileName(ts.Email, ts.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &ts,
			Metadata: recordMetadata,
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "gemini", record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			setOAuthSessionError(state, "Failed to save token to file")
			return
		}

		completeOAuthSession(state)
		log.Infof("You can now use Gemini CLI services through this CLI; token saved to %s", savedPath)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing Codex authentication...")

	pkceCodes, state, ok := generatePKCEAndState(c)
	if !ok {
		return
	}

	// Initialize Codex auth service
	openaiAuth := codex.NewAuth(flowCfg)

	// Generate authorization URL
	authURL, err := openaiAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	registerOAuthSessionForAuthDir(state, "codex", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	forwarder, ok := h.setupOAuthForwarder(c, flowCfg, "codex", codexCallbackPort, "/codex/callback")
	if !ok {
		return
	}

	go func() {
		if forwarder != nil {
			defer stopCallbackForwarderInstance(codexCallbackPort, forwarder)
		}

		resultMap, errWait := waitForOAuthCallback(flowCfg.AuthDir, "codex", state, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionCancelled) {
				return
			}
			authErr := oauthcommon.NewAuthenticationError(oauthcommon.ErrCallbackTimeout, errWait)
			log.Error(oauthcommon.GetUserFriendlyMessage(authErr))
			setOAuthSessionError(state, "Timeout waiting for OAuth callback")
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := oauthcommon.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(oauthcommon.GetUserFriendlyMessage(oauthErr))
			setOAuthSessionError(state, "Bad Request")
			return
		}
		if resultMap["state"] != state {
			authErr := oauthcommon.NewAuthenticationError(
				oauthcommon.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]),
			)
			setOAuthSessionError(state, "State code error")
			log.Error(oauthcommon.GetUserFriendlyMessage(authErr))
			return
		}
		code := resultMap["code"]

		log.Debug("Authorization code received, exchanging for tokens...")
		// Exchange code for tokens using internal auth service
		bundle, errExchange := openaiAuth.ExchangeCodeForTokens(ctx, code, pkceCodes)
		if errExchange != nil {
			authErr := oauthcommon.NewAuthenticationError(oauthcommon.ErrCodeExchangeFailed, errExchange)
			setOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			return
		}

		// Extract additional info for filename generation
		claims, _ := codex.ParseJWTToken(bundle.TokenData.IDToken)
		planType := ""
		hashAccountID := ""
		if claims != nil {
			planType = strings.TrimSpace(claims.AuthInfo.ChatgptPlanType)
			if accountID := claims.GetAccountID(); accountID != "" {
				digest := sha256.Sum256([]byte(accountID))
				hashAccountID = hex.EncodeToString(digest[:])[:8]
			}
		}

		// Create token storage and persist
		tokenStorage := openaiAuth.CreateTokenStorage(bundle)
		fileName := codex.CredentialFileName(tokenStorage.Email, planType, hashAccountID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "codex",
			FileName: fileName,
			Storage:  tokenStorage,
			Metadata: map[string]any{
				"email":      tokenStorage.Email,
				"account_id": tokenStorage.AccountID,
			},
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "codex", record)
		if errSave != nil {
			setOAuthSessionError(state, "Failed to save authentication tokens")
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			return
		}
		log.Infof("Authentication successful! Token saved to %s", savedPath)
		if bundle.APIKey != "" {
			log.Info("API key obtained and saved")
		}
		log.Info("You can now use Codex services through this CLI")
		completeOAuthSession(state)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing Antigravity authentication...")

	authSvc := antigravity.NewAuth(flowCfg, nil)

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", antigravity.CallbackPort)
	authURL := authSvc.BuildAuthURL(state, redirectURI)

	registerOAuthSessionForAuthDir(state, "antigravity", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	forwarder, ok := h.setupOAuthForwarder(c, flowCfg, "antigravity", antigravity.CallbackPort, "/antigravity/callback")
	if !ok {
		return
	}

	go func() {
		if forwarder != nil {
			defer stopCallbackForwarderInstance(antigravity.CallbackPort, forwarder)
		}

		resultMap, errWait := waitForOAuthCallback(flowCfg.AuthDir, "antigravity", state, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionCancelled) {
				return
			}
			log.Error("oauth flow timed out")
			return
		}
		if errStr := strings.TrimSpace(resultMap["error"]); errStr != "" {
			log.Errorf("Authentication failed: %s", errStr)
			setOAuthSessionError(state, "Authentication failed")
			return
		}
		if payloadState := strings.TrimSpace(resultMap["state"]); payloadState != "" && payloadState != state {
			log.Errorf("Authentication failed: state mismatch")
			setOAuthSessionError(state, "Authentication failed: state mismatch")
			return
		}
		authCode := strings.TrimSpace(resultMap["code"])
		if authCode == "" {
			log.Error("Authentication failed: code not found")
			setOAuthSessionError(state, "Authentication failed: code not found")
			return
		}

		tokenResp, errToken := authSvc.ExchangeCodeForTokens(ctx, authCode, redirectURI)
		if errToken != nil {
			log.Errorf("Failed to exchange token: %v", errToken)
			setOAuthSessionError(state, "Failed to exchange token")
			return
		}

		accessToken := strings.TrimSpace(tokenResp.AccessToken)
		if accessToken == "" {
			log.Error("antigravity: token exchange returned empty access token")
			setOAuthSessionError(state, "Failed to exchange token")
			return
		}

		email, errInfo := authSvc.FetchUserInfo(ctx, accessToken)
		if errInfo != nil {
			log.Errorf("Failed to fetch user info: %v", errInfo)
			setOAuthSessionError(state, "Failed to fetch user info")
			return
		}
		email = strings.TrimSpace(email)
		if email == "" {
			log.Error("antigravity: user info returned empty email")
			setOAuthSessionError(state, "Failed to fetch user info")
			return
		}

		projectID := ""
		if accessToken != "" {
			fetchedProjectID, errProject := authSvc.FetchProjectID(ctx, accessToken)
			if errProject != nil {
				log.Errorf("antigravity: failed to fetch project ID: %v", errProject)
				setOAuthSessionError(state, "Failed to fetch project ID")
				return
			}
			projectID = fetchedProjectID
			log.Infof("antigravity: obtained project ID %s", util.HideAPIKey(projectID))
		}
		if strings.TrimSpace(projectID) == "" {
			log.Error("antigravity: project ID discovery returned empty project")
			setOAuthSessionError(state, "Failed to fetch project ID")
			return
		}

		metadata := antigravity.BuildMetadata(tokenResp, email, projectID)

		fileName := antigravity.CredentialFileName(email)
		label := strings.TrimSpace(email)
		if label == "" {
			label = "antigravity"
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "antigravity",
			FileName: fileName,
			Label:    label,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "antigravity", record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			setOAuthSessionError(state, "Failed to save token to file")
			return
		}

		completeOAuthSession(state)
		log.Infof("Authentication successful! Token saved to %s", savedPath)
		if projectID != "" {
			log.Infof("Using GCP project: %s", util.HideAPIKey(projectID))
		}
		log.Info("You can now use Antigravity services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestQwenToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing Qwen authentication...")

	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	// Initialize Qwen auth service
	qwenAuth := qwen.NewAuth(flowCfg)

	// Generate authorization URL
	deviceFlow, err := qwenAuth.InitiateDeviceFlow(ctx)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := deviceFlow.VerificationURIComplete

	registerOAuthSessionForAuthDir(state, "qwen", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)
	pollCtx, cancelPoll := context.WithCancel(ctx)
	if !setOAuthSessionCancel(state, "qwen", cancelPoll) {
		c.JSON(http.StatusConflict, gin.H{"error": "oauth session is no longer pending"})
		return
	}

	go func() {
		defer cancelPoll()
		log.Info("Waiting for authentication...")
		tokenData, errPollForToken := qwenAuth.PollForToken(
			pollCtx, deviceFlow.DeviceCode, deviceFlow.CodeVerifier,
		)
		if handleQwenPollError(state, errPollForToken) {
			return
		}

		// Create token storage
		tokenStorage := qwenAuth.CreateTokenStorage(tokenData)

		tokenStorage.Email = fmt.Sprintf("%d", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fmt.Sprintf("qwen-%s.json", tokenStorage.Email),
			Provider: "qwen",
			FileName: fmt.Sprintf("qwen-%s.json", tokenStorage.Email),
			Storage:  tokenStorage,
			Metadata: map[string]any{"email": tokenStorage.Email},
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "qwen", record)
		if handleQwenSaveError(state, errSave) {
			return
		}

		log.Infof("Authentication successful! Token saved to %s", savedPath)
		log.Info("You can now use Qwen services through this CLI")
		completeOAuthSession(state)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func handleQwenPollError(state string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if !setPendingOAuthSessionError(state, "qwen", "Authentication failed") {
		return true
	}
	log.Errorf("Authentication failed: %v", err)
	return true
}

func handleQwenSaveError(state string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errOAuthSessionNotPending) {
		return true
	}
	log.Errorf("Failed to save authentication tokens: %v", err)
	setOAuthSessionError(state, "Failed to save authentication tokens")
	return true
}

func (h *Handler) RequestKimiToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing Kimi authentication...")

	state := fmt.Sprintf("kmi-%d", time.Now().UnixNano())
	// Initialize Kimi auth service
	kimiAuth := kimi.NewAuth(flowCfg)

	// Generate authorization URL
	deviceFlow, errStartDeviceFlow := kimiAuth.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to generate authorization URL: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := deviceFlow.VerificationURIComplete
	if authURL == "" {
		authURL = deviceFlow.VerificationURI
	}

	registerOAuthSessionForAuthDir(state, "kimi", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		if !setOAuthSessionCancel(state, "kimi", cancelPoll) {
			return
		}

		log.Info("Waiting for authentication...")
		authBundle, errWaitForAuthorization := kimiAuth.WaitForAuthorization(pollCtx, deviceFlow)
		if errWaitForAuthorization != nil {
			if !isOAuthSessionPending(state, "kimi") {
				return
			}
			setOAuthSessionError(
				state,
				oauthSessionErrorWithCause("Authentication failed", errWaitForAuthorization),
			)
			log.Errorf("Authentication failed: %v", errWaitForAuthorization)
			return
		}

		// Create token storage
		tokenStorage := kimiAuth.CreateTokenStorage(authBundle)

		metadata := kimi.BuildMetadata(authBundle)

		fileName := fmt.Sprintf("kimi-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kimi",
			FileName: fileName,
			Label:    "Kimi User",
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "kimi", record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			setOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		log.Infof("Authentication successful! Token saved to %s", savedPath)
		log.Info("You can now use Kimi services through this CLI")
		completeOAuthSession(state)
	}()

	c.JSON(
		http.StatusOK, gin.H{
			"status":                    "ok",
			"url":                       strings.TrimSpace(authURL),
			"state":                     state,
			"flow":                      "device",
			"user_code":                 strings.TrimSpace(deviceFlow.UserCode),
			"verification_uri":          strings.TrimSpace(deviceFlow.VerificationURI),
			"verification_uri_complete": strings.TrimSpace(deviceFlow.VerificationURIComplete),
			"expires_in":                deviceFlow.ExpiresIn,
			"interval":                  deviceFlow.Interval,
		},
	)
}

func (h *Handler) RequestIFlowToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing iFlow authentication...")

	randomState, errState := misc.GenerateRandomState()
	if errState != nil {
		c.JSON(500, gin.H{"error": "failed to generate state"})
		return
	}
	state := "ifl-" + randomState
	authSvc := iflowauth.NewAuth(flowCfg)
	authURL, redirectURI := authSvc.AuthorizationURL(state, iflowauth.CallbackPort)

	registerOAuthSessionForAuthDir(state, "iflow", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	forwarder, ok := h.setupOAuthForwarder(c, flowCfg, "iflow", iflowauth.CallbackPort, "/iflow/callback")
	if !ok {
		return
	}

	go func() {
		if forwarder != nil {
			defer stopCallbackForwarderInstance(iflowauth.CallbackPort, forwarder)
		}
		log.Info("Waiting for authentication...")

		resultMap, errWait := waitForOAuthCallback(flowCfg.AuthDir, "iflow", state, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionCancelled) {
				return
			}
			setOAuthSessionError(state, "Authentication failed")
			log.Error("Authentication failed: timeout waiting for callback")
			return
		}

		if errStr := strings.TrimSpace(resultMap["error"]); errStr != "" {
			setOAuthSessionError(state, "Authentication failed")
			log.Errorf("Authentication failed: %s", errStr)
			return
		}
		if resultState := strings.TrimSpace(resultMap["state"]); resultState != state {
			setOAuthSessionError(state, "Authentication failed")
			log.Error("Authentication failed: state mismatch")
			return
		}

		code := strings.TrimSpace(resultMap["code"])
		if code == "" {
			setOAuthSessionError(state, "Authentication failed")
			log.Error("Authentication failed: code missing")
			return
		}

		tokenData, errExchange := authSvc.ExchangeCodeForTokens(ctx, code, redirectURI)
		if errExchange != nil {
			setOAuthSessionError(state, "Authentication failed")
			log.Errorf("Authentication failed: %v", errExchange)
			return
		}

		tokenStorage := authSvc.CreateTokenStorage(tokenData)
		identifier := strings.TrimSpace(tokenStorage.Email)
		if identifier == "" {
			identifier = fmt.Sprintf("%d", time.Now().UnixMilli())
			tokenStorage.Email = identifier
		}
		record := &coreauth.Auth{
			ID:         fmt.Sprintf("iflow-%s.json", identifier),
			Provider:   "iflow",
			FileName:   fmt.Sprintf("iflow-%s.json", identifier),
			Storage:    tokenStorage,
			Metadata:   map[string]any{"email": identifier, "api_key": tokenStorage.APIKey},
			Attributes: map[string]string{"api_key": tokenStorage.APIKey},
		}

		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "iflow", record)
		if errSave != nil {
			setOAuthSessionError(state, "Failed to save authentication tokens")
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			return
		}

		log.Infof("Authentication successful! Token saved to %s", savedPath)
		if tokenStorage.APIKey != "" {
			log.Info("API key obtained and saved")
		}
		log.Info("You can now use iFlow services through this CLI")
		completeOAuthSession(state)
	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestIFlowCookieToken(c *gin.Context) {
	ctx := context.Background()
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	var payload struct {
		Cookie string `json:"cookie"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		return
	}

	cookieValue := strings.TrimSpace(payload.Cookie)

	if cookieValue == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		return
	}

	cookieValue, errNormalize := iflowauth.NormalizeCookie(cookieValue)
	if errNormalize != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errNormalize.Error()})
		return
	}

	// Check for duplicate BXAuth before authentication
	bxAuth := iflowauth.ExtractBXAuth(cookieValue)
	if existingFile, err := iflowauth.CheckDuplicateBXAuth(flowCfg.AuthDir, bxAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to check duplicate"})
		return
	} else if existingFile != "" {
		existingFileName := filepath.Base(existingFile)
		c.JSON(
			http.StatusConflict,
			gin.H{"status": "error", "error": "duplicate BXAuth found", "existing_file": existingFileName},
		)
		return
	}

	authSvc := iflowauth.NewAuth(flowCfg)
	tokenData, errAuth := authSvc.AuthenticateWithCookie(ctx, cookieValue)
	if errAuth != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errAuth.Error()})
		return
	}

	tokenData.Cookie = cookieValue

	tokenStorage := authSvc.CreateCookieTokenStorage(tokenData)
	email := strings.TrimSpace(tokenStorage.Email)
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "failed to extract email from token"})
		return
	}

	fileName := iflowauth.SanitizeIFlowFileName(email)
	if fileName == "" {
		fileName = fmt.Sprintf("iflow-%d", time.Now().UnixMilli())
	} else {
		fileName = fmt.Sprintf("iflow-%s", fileName)
	}

	tokenStorage.Email = email
	timestamp := time.Now().Unix()

	record := &coreauth.Auth{
		ID:       fmt.Sprintf("%s-%d.json", fileName, timestamp),
		Provider: "iflow",
		FileName: fmt.Sprintf("%s-%d.json", fileName, timestamp),
		Storage:  tokenStorage,
		Metadata: map[string]any{
			"email":        email,
			"api_key":      tokenStorage.APIKey,
			"expired":      tokenStorage.Expire,
			"cookie":       tokenStorage.Cookie,
			"type":         tokenStorage.Type,
			"last_refresh": tokenStorage.LastRefresh,
		},
		Attributes: map[string]string{
			"api_key": tokenStorage.APIKey,
		},
	}

	savedPath, errSave := h.saveTokenRecord(ctx, flowCfg.AuthDir, record)
	if errSave != nil {
		c.JSON(
			http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save authentication tokens"},
		)
		return
	}

	log.Infof("iFlow cookie authentication successful. Token saved to %s", savedPath)
	c.JSON(
		http.StatusOK, gin.H{
			"status":     "ok",
			"saved_path": savedPath,
			"email":      email,
			"expired":    tokenStorage.Expire,
			"type":       tokenStorage.Type,
		},
	)
}

func ensureGeminiProjectAndOnboard(
	ctx context.Context,
	httpClient *http.Client,
	storage *geminiAuth.TokenStorage,
	requestedProject string,
) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := geminiAuth.FetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := geminiAuth.PerformCLISetup(ctx, httpClient, storage, trimmedRequest, nil); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func onboardAllGeminiProjects(
	ctx context.Context,
	httpClient *http.Client,
	storage *geminiAuth.TokenStorage,
) ([]string, error) {
	projects, errProjects := geminiAuth.FetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := geminiAuth.PerformCLISetup(ctx, httpClient, storage, candidate, nil); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := geminiAuth.CheckCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if err := validateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	h.authStatusMu.Lock()
	defer h.authStatusMu.Unlock()

	session, ok := getOAuthSessionIntegrationDetails(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "expired", "error": "unknown or expired state"})
		return
	}
	if !verifyOAuthNonce(state, c.Query("nonce")) {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "phase": "error", "error": "nonce mismatch"})
		return
	}
	if session.Completed {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "phase": "success"})
		return
	}
	if session.Status != "" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": session.Status})
		return
	}
	if session.Source == oauthSessionSourcePlugin {
		h.pollPluginOAuthSession(c, state, session)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait", "phase": oauthSessionPhase(session)})
}

func (h *Handler) CancelAuthSession(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "state is required"})
		return
	}
	if err := validateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}
	if _, _, _, ok := getOAuthSessionDetails(state); ok && !verifyOAuthNonce(state, c.Query("nonce")) {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "error", "error": "nonce mismatch"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "cancelled": cancelOAuthSession(state)})
}

func (h *Handler) RequestXAIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = populateAuthContext(ctx, c)
	flowCfg, ok := h.authFlowConfig(c)
	if !ok {
		return
	}

	log.Info("Initializing xAI authentication...")

	randomState, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}
	state := "xai-" + randomState

	authSvc := xaiauth.NewXAIAuth(flowCfg)
	deviceFlow, errStartDeviceFlow := authSvc.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to start xAI device flow: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start device authorization flow"})
		return
	}
	authURL := strings.TrimSpace(deviceFlow.VerificationURIComplete)
	if authURL == "" {
		authURL = strings.TrimSpace(deviceFlow.VerificationURI)
	}

	registerOAuthSessionForAuthDir(state, "xai", strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir)

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		if !setOAuthSessionCancel(state, "xai", cancelPoll) {
			return
		}

		log.Info("Waiting for xAI authentication...")
		bundle, errWaitForAuthorization := authSvc.WaitForAuthorization(pollCtx, deviceFlow)
		if errWaitForAuthorization != nil {
			if !isOAuthSessionPending(state, "xai") {
				return
			}
			log.Errorf("xAI authentication failed: %v", errWaitForAuthorization)
			setOAuthSessionError(
				state,
				oauthSessionErrorWithCause("Authentication failed", errWaitForAuthorization),
			)
			return
		}

		tokenStorage := authSvc.CreateTokenStorage(bundle)
		if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
			log.Error("xAI token exchange returned empty access token")
			setOAuthSessionError(state, "Failed to exchange token")
			return
		}

		fileName := xaiauth.CredentialFileName(tokenStorage.Email, tokenStorage.Subject)
		label := strings.TrimSpace(tokenStorage.Email)
		if label == "" {
			label = "xAI"
		}

		metadata := map[string]any{
			"type":           "xai",
			"access_token":   tokenStorage.AccessToken,
			"refresh_token":  tokenStorage.RefreshToken,
			"id_token":       tokenStorage.IDToken,
			"token_type":     tokenStorage.TokenType,
			"expires_in":     tokenStorage.ExpiresIn,
			"expired":        tokenStorage.Expire,
			"last_refresh":   tokenStorage.LastRefresh,
			"base_url":       tokenStorage.BaseURL,
			"token_endpoint": tokenStorage.TokenEndpoint,
			"auth_kind":      "oauth",
		}
		if tokenStorage.Email != "" {
			metadata["email"] = tokenStorage.Email
		}
		if tokenStorage.Subject != "" {
			metadata["sub"] = tokenStorage.Subject
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "xai",
			FileName: fileName,
			Label:    label,
			Storage:  tokenStorage,
			Metadata: metadata,
			Attributes: map[string]string{
				"auth_kind": "oauth",
				"base_url":  tokenStorage.BaseURL,
			},
		}
		savedPath, errSave := h.saveOAuthTokenRecord(ctx, flowCfg.AuthDir, state, "xai", record)
		if errSave != nil {
			if errors.Is(errSave, errOAuthSessionNotPending) {
				return
			}
			log.Errorf("Failed to save xAI token to file: %v", errSave)
			setOAuthSessionError(state, "Failed to save token to file")
			return
		}

		completeOAuthSession(state)
		log.Infof("Authentication successful! Token saved to %s", savedPath)
		log.Info("You can now use xAI services through this CLI")
	}()

	expiresIn := deviceFlow.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = int(xaiauth.MaxPollDuration / time.Second)
	}
	c.JSON(
		http.StatusOK, gin.H{
			"status":                    "ok",
			"url":                       authURL,
			"state":                     state,
			"flow":                      "device",
			"user_code":                 strings.TrimSpace(deviceFlow.UserCode),
			"verification_uri":          strings.TrimSpace(deviceFlow.VerificationURI),
			"verification_uri_complete": strings.TrimSpace(deviceFlow.VerificationURIComplete),
			"expires_in":                expiresIn,
			"interval":                  deviceFlow.Interval,
		},
	)
}

// populateAuthContext extracts request info and adds it to the context
func populateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	info := &coreauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	}
	return coreauth.WithRequestInfo(ctx, info)
}
