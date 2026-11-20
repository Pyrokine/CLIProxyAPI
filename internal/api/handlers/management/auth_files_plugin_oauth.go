package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const pluginOAuthMutationRoute = "/v0/management/:plugin_provider-auth-url"

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
	served := true
	h.AuditMutation(
		c, pluginOAuthMutationRoute, func() {
			setManagementMutationTarget(c, "provider", provider)
			flowCfg, okConfig := h.authFlowConfig(c)
			if !okConfig {
				return
			}
			baseURL, errBaseURL := managementCallbackURL(flowCfg, "/v0/management/oauth-callback")
			if errBaseURL != nil {
				log.WithError(errBaseURL).WithField(
					"provider", provider,
				).Error("failed to compute plugin OAuth callback URL")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
				return
			}
			response, handled, errStart := host.StartLogin(
				populateAuthContext(c.Request.Context(), c), provider, baseURL,
				queryValuesToMetadata(c.Request.URL.Query()),
			)
			if !handled {
				served = false
				return
			}
			if errStart != nil {
				log.WithError(errStart).WithField("provider", provider).Error("failed to start plugin OAuth login")
				c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
				return
			}
			if responseProvider := strings.TrimSpace(response.Provider); responseProvider != "" {
				canonicalResponseProvider, errProvider := normalizePluginOAuthProvider(responseProvider)
				if errProvider != nil || canonicalResponseProvider != provider {
					log.WithFields(
						log.Fields{
							"provider": provider, "response_provider": responseProvider,
						},
					).Error("plugin OAuth login changed provider")
					c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth provider"})
					return
				}
			}
			state := strings.TrimSpace(response.State)
			if errState := validateOAuthState(state); errState != nil {
				log.WithError(errState).WithField(
					"provider", provider,
				).Error("plugin OAuth provider returned invalid state")
				c.JSON(http.StatusBadGateway, gin.H{"error": "invalid oauth state"})
				return
			}
			var session oauthSession
			if errRegister := registerPluginOAuthSession(
				state, provider, strings.TrimSpace(c.Query("nonce")), flowCfg.AuthDir, response.Metadata, &session,
			); errRegister != nil {
				log.WithError(errRegister).WithField(
					"provider", provider,
				).Error("failed to register plugin OAuth session")
				c.JSON(http.StatusBadGateway, gin.H{"error": "failed to generate authorization url"})
				return
			}
			sessions := oauthSessions
			// The creation receipt confirms the task, not its later credential writes.
			workerCtx := coreauth.WithRequestInfo(
				context.Background(), &coreauth.RequestInfo{
					Query: c.Request.URL.Query(), Headers: c.Request.Header.Clone(),
				},
			)
			workerCtx = withOAuthSessionContext(workerCtx, state, provider)
			workerCtx, cancel := context.WithDeadline(workerCtx, session.ExpiresAt)
			if sessions.attachCancel(state, provider, cancel, session.CreatedAt) {
				go func() {
					defer cancel()
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for h.pollPluginOAuthSession(workerCtx, state, session, sessions) {
						select {
						case <-workerCtx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
			}
			RecordMutationEffect(c.Request.Context(), "task.created", "task")
			CompleteMutation(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"status": "ok", "url": response.URL, "state": state})
		},
	)
	return served
}

func (h *Handler) pollPluginOAuthSession(
	ctx context.Context,
	state string,
	session oauthSession,
	sessions *oauthSessionStore,
) bool {
	if ctx.Err() != nil || !sessions.claimPoll(state, session.Provider, session.CreatedAt) {
		return false
	}
	message := ""
	defer func() { sessions.releasePoll(state, session.Provider, message, session.CreatedAt) }()
	host := h.pluginOAuthHost()
	if host == nil || !host.HasAuthProvider(session.Provider) {
		sessions.setErrorIfPending(state, session.Provider, "Authentication provider unavailable", session.CreatedAt)
		return false
	}
	response, handled, errPoll := host.PollLogin(ctx, session.Provider, state, session.Metadata)
	if ctx.Err() != nil {
		return false
	}
	if !handled {
		sessions.setErrorIfPending(state, session.Provider, "Authentication provider unavailable", session.CreatedAt)
		return false
	}
	if errPoll != nil {
		sessions.setErrorIfPending(state, session.Provider, pluginOAuthErrorMessage(errPoll.Error()), session.CreatedAt)
		log.WithError(errPoll).WithField("provider", session.Provider).Warn("plugin OAuth poll failed")
		return false
	}
	switch response.Status {
	case "", pluginapi.AuthLoginStatusPending:
		if pending := pluginOAuthErrorMessage(response.Message); pending != "Authentication failed" {
			message = pending
		}
		return true
	case pluginapi.AuthLoginStatusError:
		sessions.setErrorIfPending(
			state, session.Provider, pluginOAuthErrorMessage(response.Message), session.CreatedAt,
		)
	case pluginapi.AuthLoginStatusSuccess:
		if !sessions.claimForSave(state, session.Provider, session.CreatedAt) {
			return false
		}
		records, errRecords := pluginLoginPollRecords(host, session.Provider, response)
		if errRecords != nil {
			sessions.setError(state, "Authentication failed", session.CreatedAt)
			log.WithError(errRecords).WithField(
				"provider", session.Provider,
			).Warn("plugin OAuth returned invalid auth data")
			return false
		}
		saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancelSave()
		if errSave := h.saveClaimedPluginLoginRecords(saveCtx, session.AuthDir, records); errSave != nil {
			sessions.setError(state, "Failed to save authentication tokens", session.CreatedAt)
			log.WithError(errSave).WithField(
				"provider", session.Provider,
			).Error("failed to save plugin OAuth credentials")
			return false
		}
		sessions.complete(state, session.CreatedAt)
	default:
		sessions.setError(state, "Authentication provider returned invalid status", session.CreatedAt)
	}
	return false
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
	path         string
	existed      bool
	persisted    []byte
	runtime      []*coreauth.Auth
	runtimeAfter []*coreauth.Auth
	written      *coreauth.Auth
	version      *coreauth.PersistedVersion
}

type pluginLoginPersistedStorage []byte

func (payload pluginLoginPersistedStorage) SaveTokenToFile(path string) error {
	return authfile.WriteTokenFileAtomic(path, payload)
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
		_, errSave := h.saveTokenRecord(ctx, authDir, record, &snapshots[index].written)
		if h.authManager != nil {
			snapshots[index].runtimeAfter = findAuthsByPath(h.authManager.List(), snapshots[index].path)
		}
		version, errVersion := coreauth.PersistedVersionForAuth(snapshots[index].path, record)
		if errVersion == nil {
			snapshots[index].version = &version
		}
		if errSave != nil || errVersion != nil {
			errRollback := h.rollbackPluginLoginTargets(ctx, authDir, snapshots[:index+1])
			return errors.Join(errSave, errVersion, errRollback)
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
	for _, snapshot := range slices.Backward(snapshots) {

		if snapshot.version == nil {
			rollbackError = errors.Join(
				rollbackError, fmt.Errorf("rollback %s has no saved file version", filepath.Base(snapshot.path)),
			)
			continue
		}
		id := authIDForPath(snapshot.path, authDir)
		var previous *coreauth.Auth
		for _, auth := range snapshot.runtime {
			if auth.ID == id {
				previous = auth
				break
			}
		}
		expectedGeneration := uint64(0)
		if previous != nil {
			expectedGeneration = previous.Generation
		}
		if snapshot.written != nil {
			expectedGeneration = snapshot.written.Generation
		}
		restore := func(context.Context) (*coreauth.Auth, error) {
			unlockFile := authfile.LockFileMutation(snapshot.path)
			defer unlockFile()
			captured, matched, errCapture := coreauth.CaptureFileIfVersion(snapshot.path, *snapshot.version)
			if errCapture != nil {
				return nil, errCapture
			}
			if !matched {
				return nil, fmt.Errorf(
					"%w: credential file changed during plugin rollback", coreauth.ErrAuthGenerationConflict,
				)
			}
			var errRestore error
			if snapshot.existed {
				errRestore = h.restorePluginLoginPersisted(ctx, authDir, snapshot)
			} else {
				errRestore = h.deleteTokenRecord(context.WithoutCancel(ctx), snapshot.path)
			}
			if errRestore != nil {
				return nil, errors.Join(errRestore, captured.Rollback())
			}
			if errCommit := captured.Commit(); errCommit != nil {
				return nil, errCommit
			}
			if previous == nil {
				return nil, errPluginLoginRuntimeAbsent
			}
			return previous.Clone(), nil
		}
		runtimeCtx := coreauth.WithExpectedAuthGeneration(
			coreauth.WithSkipPersist(context.WithoutCancel(ctx)), expectedGeneration,
		)
		var restored *coreauth.Auth
		var errRestore error
		if h.authManager == nil {
			restored, errRestore = restore(runtimeCtx)
		} else {
			restored, errRestore = h.authManager.MutatePersistentAuth(runtimeCtx, id, restore)
		}
		if errors.Is(errRestore, errPluginLoginRuntimeAbsent) {
			errRestore = nil
			if h.authManager != nil && snapshot.written != nil {
				removed, errRemove := h.authManager.RemoveIfGeneration(
					coreauth.WithRuntimeOnlyRemove(runtimeCtx), id, expectedGeneration,
				)
				if errRemove != nil {
					errRestore = errRemove
				} else if !removed {
					errRestore = coreauth.ErrAuthGenerationConflict
				}
			}
		}
		if errRestore != nil {
			rollbackError = errors.Join(
				rollbackError, fmt.Errorf("rollback %s: %w", filepath.Base(snapshot.path), errRestore),
			)
			continue
		}
		rollbackError = errors.Join(rollbackError, h.restorePluginLoginRuntime(runtimeCtx, snapshot))
		if restored != nil {
			h.syncQuotaSchedulerForAuth(restored)
			if h.postAuthPersistHook != nil {
				rollbackError = errors.Join(rollbackError, h.postAuthPersistHook(runtimeCtx, restored))
			}
		}
	}
	return rollbackError
}

var errPluginLoginRuntimeAbsent = errors.New("plugin OAuth rollback restores an absent runtime credential")

func (h *Handler) restorePluginLoginRuntime(
	ctx context.Context,
	snapshot pluginLoginTargetSnapshot,
) error {
	if h == nil || h.authManager == nil {
		return nil
	}
	runtimeCtx := coreauth.WithSkipPersist(context.WithoutCancel(ctx))
	var restoreError error
	for _, previous := range snapshot.runtime {
		if previous.ID == filepath.Base(snapshot.path) {
			continue
		}
		var expected *coreauth.Auth
		for _, current := range snapshot.runtimeAfter {
			if current.ID == previous.ID {
				expected = current
				break
			}
		}
		if expected == nil {
			restoreError = errors.Join(restoreError, coreauth.ErrAuthGenerationConflict)
			continue
		}
		restored, errUpdate := h.authManager.MutatePersistentAuth(
			coreauth.WithExpectedAuthGeneration(runtimeCtx, expected.Generation), previous.ID,
			func(context.Context) (*coreauth.Auth, error) {
				currentVersion, errRead := coreauth.ReadPersistedVersion(snapshot.path)
				if !snapshot.existed && errors.Is(errRead, os.ErrNotExist) {
					return previous.Clone(), nil
				}
				if errRead != nil {
					return nil, errRead
				}
				oldDigest, errDigest := coreauth.DigestPersistedJSON(snapshot.persisted)
				if errDigest != nil {
					return nil, errDigest
				}
				if currentVersion.SHA256 != oldDigest {
					return nil, coreauth.ErrAuthGenerationConflict
				}
				return previous.Clone(), nil
			},
		)
		if errUpdate != nil {
			restoreError = errors.Join(restoreError, errUpdate)
			continue
		}
		if restored != nil {
			h.syncQuotaSchedulerForAuth(restored)
			if h.postAuthPersistHook != nil {
				restoreError = errors.Join(restoreError, h.postAuthPersistHook(runtimeCtx, restored))
			}
		}
	}
	return restoreError
}

var _ pluginOAuthHost = (*pluginhost.Host)(nil)
