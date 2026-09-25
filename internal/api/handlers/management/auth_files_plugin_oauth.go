package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type pluginOAuthHost interface {
	HasAuthProvider(string) bool
	StartLogin(
		context.Context,
		string,
		string,
		...map[string]any,
	) (pluginapi.AuthLoginStartResponse, bool, error)
	PollLogin(
		context.Context,
		string,
		string,
		...map[string]any,
	) (pluginapi.AuthLoginPollResponse, bool, error)
	AuthDataToCoreAuth(pluginapi.AuthData, string, string) *coreauth.Auth
}

func (h *Handler) pluginOAuthHost() pluginOAuthHost {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	host, _ := h.pluginHost.(pluginOAuthHost)
	h.mu.RUnlock()
	return host
}

func pluginAuthProviderFromPath(path string) (string, bool) {
	const prefix = "/v0/management/"
	const suffix = "-auth-url"

	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	provider, err := normalizePluginOAuthProvider(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	return provider, err == nil
}

func queryValuesToMetadata(values url.Values) map[string]any {
	if len(values) == 0 {
		return nil
	}
	metadata := make(map[string]any, len(values))
	for key, items := range values {
		switch len(items) {
		case 0:
			continue
		case 1:
			metadata[key] = items[0]
		default:
			metadata[key] = append([]string(nil), items...)
		}
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
}

// HasPluginAuthURL reports whether a GET path belongs to an active plugin auth provider.
func (h *Handler) HasPluginAuthURL(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	provider, okProvider := pluginAuthProviderFromPath(path)
	if !okProvider {
		return false
	}
	host := h.pluginOAuthHost()
	return host != nil && host.HasAuthProvider(provider)
}

// ServePluginAuthURL starts a plugin-owned login flow for /v0/management/<provider>-auth-url.
func (h *Handler) ServePluginAuthURL(c *gin.Context) bool {
	if h == nil || c == nil || c.Request == nil || c.Request.URL == nil || c.Request.Method != http.MethodGet {
		return false
	}
	provider, okProvider := pluginAuthProviderFromPath(c.Request.URL.Path)
	if !okProvider {
		return false
	}
	host := h.pluginOAuthHost()
	if host == nil || !host.HasAuthProvider(provider) {
		return false
	}
	flowCfg, okConfig := h.authFlowConfig(c)
	if !okConfig {
		return true
	}
	baseURL, errBaseURL := managementCallbackURL(flowCfg, "/v0/management/oauth-callback")
	if errBaseURL != nil {
		log.WithError(errBaseURL).WithField("provider", provider).Error("failed to compute plugin OAuth callback URL")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	ctx := populateAuthContext(c.Request.Context(), c)
	response, handled, errStart := host.StartLogin(
		ctx,
		provider,
		baseURL,
		queryValuesToMetadata(c.Request.URL.Query()),
	)
	if !handled {
		return false
	}
	if errStart != nil {
		log.WithError(errStart).WithField("provider", provider).Error("failed to start plugin OAuth login")
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	if responseProvider := strings.TrimSpace(response.Provider); responseProvider != "" {
		canonicalResponseProvider, errProvider := normalizePluginOAuthProvider(responseProvider)
		if errProvider != nil || canonicalResponseProvider != provider {
			log.WithFields(log.Fields{"provider": provider, "response_provider": responseProvider}).Error(
				"plugin OAuth login changed provider",
			)
			c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth provider"})
			return true
		}
	}
	state := strings.TrimSpace(response.State)
	if errState := validateOAuthState(state); errState != nil {
		log.WithError(errState).WithField("provider", provider).Error("plugin OAuth provider returned invalid state")
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth state"})
		return true
	}
	if errRegister := registerPluginOAuthSession(
		state,
		provider,
		strings.TrimSpace(c.Query("nonce")),
		flowCfg.AuthDir,
		response.Metadata,
	); errRegister != nil {
		log.WithError(errRegister).WithField("provider", provider).Error("failed to register plugin OAuth session")
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
		return true
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": response.URL, "state": state})
	return true
}

func (h *Handler) pollPluginOAuthSession(c *gin.Context, state string, session oauthSession) {
	host := h.pluginOAuthHost()
	if host == nil || !host.HasAuthProvider(session.Provider) {
		const message = "Authentication provider unavailable"
		setPendingOAuthSessionError(state, session.Provider, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": message})
		return
	}
	ctx := populateAuthContext(c.Request.Context(), c)
	response, handled, errPoll := host.PollLogin(ctx, session.Provider, state, session.Metadata)
	if !handled {
		const message = "Authentication provider unavailable"
		setPendingOAuthSessionError(state, session.Provider, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": message})
		return
	}
	if errPoll != nil {
		message := pluginOAuthErrorMessage(errPoll.Error())
		setPendingOAuthSessionError(state, session.Provider, message)
		log.WithError(errPoll).WithField("provider", session.Provider).Warn("plugin OAuth poll failed")
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": message})
		return
	}

	switch response.Status {
	case "", pluginapi.AuthLoginStatusPending:
		payload := gin.H{"status": "wait", "phase": oauthSessionPhase(session)}
		if message := pluginOAuthErrorMessage(response.Message); message != "Authentication failed" {
			payload["message"] = message
		}
		c.JSON(http.StatusOK, payload)
	case pluginapi.AuthLoginStatusError:
		message := pluginOAuthErrorMessage(response.Message)
		setPendingOAuthSessionError(state, session.Provider, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": message})
	case pluginapi.AuthLoginStatusSuccess:
		if errClaim := guardOAuthSessionPendingForSave(state, session.Provider); errClaim != nil {
			c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": "oauth flow is not pending"})
			return
		}
		records, errRecords := pluginLoginPollRecords(host, session.Provider, response)
		if errRecords != nil {
			setOAuthSessionError(state, "Authentication failed")
			log.WithError(errRecords).WithField("provider", session.Provider).Warn("plugin OAuth returned invalid auth data")
			c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": "Authentication failed"})
			return
		}
		if errSave := h.saveClaimedPluginLoginRecords(ctx, session.AuthDir, records); errSave != nil {
			setOAuthSessionError(state, "Failed to save authentication tokens")
			log.WithError(errSave).WithField("provider", session.Provider).Error("failed to save plugin OAuth credentials")
			c.JSON(
				http.StatusOK,
				gin.H{"status": "error", "phase": "error", "error": "Failed to save authentication tokens"},
			)
			return
		}
		completeOAuthSession(state)
		c.JSON(http.StatusOK, gin.H{"status": "ok", "phase": "success"})
	default:
		const message = "Authentication provider returned invalid status"
		setOAuthSessionError(state, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "phase": "error", "error": message})
	}
}

func pluginOAuthErrorMessage(message string) string {
	message = strings.TrimSpace(string(util.MaskSensitivePayload([]byte(message))))
	if message == "" {
		return "Authentication failed"
	}
	return message
}

func pluginLoginPollRecords(
	host pluginOAuthHost,
	provider string,
	response pluginapi.AuthLoginPollResponse,
) ([]*coreauth.Auth, error) {
	if host == nil {
		return nil, fmt.Errorf("plugin OAuth host is unavailable")
	}
	authData := response.Auths
	if authData == nil {
		authData = []pluginapi.AuthData{response.Auth}
	}
	if len(authData) == 0 {
		return nil, fmt.Errorf("plugin OAuth returned no auth records")
	}
	records := make([]*coreauth.Auth, 0, len(authData))
	for index := range authData {
		data := authData[index]
		if strings.TrimSpace(data.Provider) == "" {
			data.Provider = provider
		}
		canonicalProvider, errProvider := normalizePluginOAuthProvider(data.Provider)
		if errProvider != nil || canonicalProvider != provider {
			return nil, fmt.Errorf("plugin OAuth auth provider changed at record %d", index)
		}
		data.Provider = canonicalProvider
		record := host.AuthDataToCoreAuth(data, "", data.FileName)
		if record == nil {
			return nil, fmt.Errorf("plugin OAuth auth record %d is invalid", index)
		}
		records = append(records, record)
	}
	return records, nil
}

type pluginLoginTargetSnapshot struct {
	path      string
	existed   bool
	persisted []byte
	runtime   []*coreauth.Auth
}

type pluginLoginPersistedStorage []byte

func (payload pluginLoginPersistedStorage) SaveTokenToFile(path string) error {
	return authfile.WriteTokenFileAtomic(path, payload)
}

func (payload pluginLoginPersistedStorage) RawJSON() []byte {
	return payload
}

func (h *Handler) saveClaimedPluginLoginRecords(
	ctx context.Context,
	authDir string,
	records []*coreauth.Auth,
) error {
	if len(records) == 0 {
		return fmt.Errorf("plugin OAuth returned no auth records")
	}
	snapshots := make([]pluginLoginTargetSnapshot, 0, len(records))
	seenPaths := make(map[string]struct{}, len(records))
	for index, record := range records {
		path, errPath := preparePluginLoginRecordPath(authDir, record)
		if errPath != nil {
			return fmt.Errorf("prepare plugin OAuth auth record %d: %w", index, errPath)
		}
		canonicalPath := filepath.Clean(path)
		if _, exists := seenPaths[canonicalPath]; exists {
			return fmt.Errorf("plugin OAuth returned duplicate auth path %q", filepath.Base(path))
		}
		seenPaths[canonicalPath] = struct{}{}
		snapshot, errSnapshot := h.capturePluginLoginTarget(path)
		if errSnapshot != nil {
			return fmt.Errorf("capture plugin OAuth auth record %d: %w", index, errSnapshot)
		}
		snapshots = append(snapshots, snapshot)
	}

	for index, record := range records {
		if _, errSave := h.saveTokenRecord(ctx, authDir, record); errSave != nil {
			errRollback := h.rollbackPluginLoginTargets(ctx, authDir, snapshots[:index+1])
			return errors.Join(errSave, errRollback)
		}
	}
	return nil
}

func preparePluginLoginRecordPath(authDir string, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("auth record is nil")
	}
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return "", fmt.Errorf("auth directory not configured")
	}
	fileName := strings.TrimSpace(record.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(record.ID)
	}
	if fileName == "" || fileName == "." || strings.ContainsAny(fileName, "/\\") || strings.Contains(fileName, "..") {
		return "", fmt.Errorf("invalid auth file name")
	}
	fileName = filepath.Base(fileName)
	path := filepath.Join(authDir, fileName)
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	record.FileName = fileName
	record.Attributes[coreauth.AttributePath] = path
	record.Attributes[coreauth.AttributeSource] = path
	return path, nil
}

func (h *Handler) capturePluginLoginTarget(path string) (pluginLoginTargetSnapshot, error) {
	snapshot := pluginLoginTargetSnapshot{path: path}
	persisted, errRead := os.ReadFile(path)
	switch {
	case errRead == nil:
		snapshot.existed = true
		snapshot.persisted = persisted
	case errors.Is(errRead, os.ErrNotExist):
	case errRead != nil:
		return pluginLoginTargetSnapshot{}, errRead
	}
	if h != nil && h.authManager != nil {
		for _, auth := range findAuthsByPath(h.authManager.List(), path) {
			snapshot.runtime = append(snapshot.runtime, auth.Clone())
		}
	}
	return snapshot, nil
}

func (h *Handler) restorePluginLoginPersisted(
	ctx context.Context,
	authDir string,
	snapshot pluginLoginTargetSnapshot,
) error {
	store := h.tokenStoreSnapshot()
	var persistError error
	if store == nil {
		persistError = fmt.Errorf("token store unavailable")
	} else {
		record := &coreauth.Auth{
			ID:       authIDForPath(snapshot.path, authDir),
			FileName: filepath.Base(snapshot.path),
			Storage:  pluginLoginPersistedStorage(snapshot.persisted),
			Attributes: map[string]string{
				coreauth.AttributePath:   snapshot.path,
				coreauth.AttributeSource: snapshot.path,
			},
		}
		restoreCtx := context.WithoutCancel(ctx)
		if scopedStore, ok := store.(interface {
			SaveWithBaseDir(context.Context, string, *coreauth.Auth) (string, error)
		}); ok {
			_, persistError = scopedStore.SaveWithBaseDir(restoreCtx, authDir, record)
		} else {
			_, persistError = store.Save(restoreCtx, record)
		}
	}
	localError := authfile.WriteTokenFileAtomic(snapshot.path, snapshot.persisted)
	return errors.Join(persistError, localError)
}

func (h *Handler) rollbackPluginLoginTargets(
	ctx context.Context,
	authDir string,
	snapshots []pluginLoginTargetSnapshot,
) error {
	var rollbackError error
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		if snapshot.existed {
			if errRestore := h.restorePluginLoginPersisted(ctx, authDir, snapshot); errRestore != nil {
				rollbackError = errors.Join(rollbackError, fmt.Errorf("restore %s: %w", filepath.Base(snapshot.path), errRestore))
			}
		} else if errDelete := h.deleteTokenRecord(context.WithoutCancel(ctx), snapshot.path); errDelete != nil {
			rollbackError = errors.Join(rollbackError, fmt.Errorf("remove %s: %w", filepath.Base(snapshot.path), errDelete))
		}
		if errRuntime := h.restorePluginLoginRuntime(ctx, snapshot); errRuntime != nil {
			rollbackError = errors.Join(rollbackError, errRuntime)
		}
	}
	return rollbackError
}

func (h *Handler) restorePluginLoginRuntime(
	ctx context.Context,
	snapshot pluginLoginTargetSnapshot,
) error {
	if h == nil || h.authManager == nil {
		return nil
	}
	runtimeCtx := coreauth.WithSkipPersist(context.WithoutCancel(ctx))
	previousByID := make(map[string]*coreauth.Auth, len(snapshot.runtime))
	for _, previous := range snapshot.runtime {
		previousByID[previous.ID] = previous
	}
	var restoreError error
	for _, current := range findAuthsByPath(h.authManager.List(), snapshot.path) {
		if _, existed := previousByID[current.ID]; existed {
			continue
		}
		removed, errRemove := h.authManager.RemoveIfGeneration(
			coreauth.WithRuntimeOnlyRemove(runtimeCtx), current.ID, current.Generation,
		)
		if errRemove != nil {
			restoreError = errors.Join(restoreError, errRemove)
			continue
		}
		if !removed {
			restoreError = errors.Join(restoreError, fmt.Errorf("runtime auth %s changed during rollback", current.ID))
		}
	}
	for _, previous := range snapshot.runtime {
		current, exists := h.authManager.GetByID(previous.ID)
		if !exists || current == nil || !authMatchesPath(current, snapshot.path) {
			restoreError = errors.Join(restoreError, fmt.Errorf("runtime auth %s changed during rollback", previous.ID))
			continue
		}
		restored, errUpdate := h.authManager.Update(runtimeCtx, previous.Clone())
		if errUpdate != nil {
			restoreError = errors.Join(restoreError, errUpdate)
			continue
		}
		if restored == nil {
			restoreError = errors.Join(restoreError, fmt.Errorf("runtime auth %s was not restored", previous.ID))
			continue
		}
		h.syncQuotaSchedulerForAuth(restored)
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(runtimeCtx, restored); errHook != nil {
				restoreError = errors.Join(restoreError, errHook)
			}
		}
	}
	return restoreError
}

var _ pluginOAuthHost = (*pluginhost.Host)(nil)
