// Package live forwards Codex realtime WebRTC session bootstrap requests.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	upstreamCallURL  = "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	defaultLiveModel = "gpt-live-1-codex"
	maxBodySize      = 16 << 20
)

var liveProtocolHeaders = []string{
	"OpenAI-Alpha",
	"X-Session-Id",
	"Session-Id",
	"Thread-Id",
	"Originator",
	"OpenAI-Safety-Identifier",
	"OpenAI-Organization",
	"OpenAI-Project",
	"X-Oai-Attestation",
}

// ConfigProvider returns the current runtime configuration snapshot.
type ConfigProvider func() *config.Config

// Handler forwards Codex live session requests through the shared auth scheduler.
type Handler struct {
	authManager          *auth.Manager
	configProvider       ConfigProvider
	sessions             *sessionStore
	clientSecrets        *clientSecretStore
	sidebandAPIBaseURL   string
	mediaRelayMu         sync.RWMutex
	mediaRelay           mediaRelayFactory
	mediaRelayErr        error
	mediaRelayConfig     config.CodexLiveMediaRelayConfig
	mediaRelayConfigured bool
	mediaLimiter         *mediaSessionLimiter
}

// NewHandler creates a Codex live session handler.
func NewHandler(authManager *auth.Manager, configProvider ConfigProvider) *Handler {
	handler := &Handler{
		authManager:        authManager,
		configProvider:     configProvider,
		sessions:           newSessionStore(),
		clientSecrets:      newClientSecretStore(),
		sidebandAPIBaseURL: defaultSidebandAPIBaseURL,
	}
	if errUpdate := handler.UpdateConfig(handler.configSnapshot()); errUpdate != nil {
		log.WithError(errUpdate).Error("failed to configure Codex Live media relay")
	}
	return handler
}

// UpdateConfig atomically applies Codex Live media relay settings to new sessions.
func (h *Handler) UpdateConfig(cfg *config.Config) error {
	if h == nil {
		return nil
	}
	var relayConfig config.CodexLiveMediaRelayConfig
	if cfg != nil {
		relayConfig = cfg.Codex.LiveMediaRelay
	}

	h.mediaRelayMu.Lock()
	previousConfig := h.mediaRelayConfig
	previouslyConfigured := h.mediaRelayConfigured
	if previouslyConfigured && reflect.DeepEqual(previousConfig, relayConfig) {
		currentErr := h.mediaRelayErr
		h.mediaRelayMu.Unlock()
		return currentErr
	}
	if h.mediaLimiter == nil {
		h.mediaLimiter = &mediaSessionLimiter{}
	}
	var relay mediaRelayFactory
	var relayErr error
	if relayConfig.Enabled {
		relay, relayErr = newPionMediaRelayWithLimiter(relayConfig, h.mediaLimiter)
	}
	h.mediaRelay = relay
	h.mediaRelayErr = relayErr
	h.mediaRelayConfig = relayConfig
	h.mediaRelayConfigured = true
	h.mediaRelayMu.Unlock()

	if relayErr == nil && (previouslyConfigured || relayConfig.Enabled) {
		message := "codex live media relay configured"
		if previouslyConfigured {
			message = "codex live media relay configuration reloaded; changes apply to new sessions"
		}
		log.WithFields(liveMediaConfigLogFields(relayConfig)).Info(message)
	}
	return relayErr
}

func liveMediaConfigLogFields(relayConfig config.CodexLiveMediaRelayConfig) log.Fields {
	publicIP := strings.TrimSpace(relayConfig.PublicIP)
	if publicIP == "" {
		publicIP = "auto"
	}
	return log.Fields{
		"enabled":                    relayConfig.Enabled,
		"max_sessions":               relayConfig.EffectiveMaxSessions(),
		"disable_private_remote_ips": relayConfig.DisablePrivateRemoteIPs,
		"public_ip":                  publicIP,
		"udp_port_min":               relayConfig.UDPPortMin,
		"udp_port_max":               relayConfig.UDPPortMax,
		"ice_server_count":           len(relayConfig.ICEServers),
	}
}

func (h *Handler) currentRuntime() (*config.Config, mediaRelayFactory, error) {
	if h == nil {
		return nil, nil, nil
	}
	h.mediaRelayMu.RLock()
	relay := h.mediaRelay
	relayErr := h.mediaRelayErr
	h.mediaRelayMu.RUnlock()
	return h.configSnapshot(), relay, relayErr
}

func (h *Handler) currentMediaRelay() (mediaRelayFactory, error) {
	if h == nil {
		return nil, nil
	}
	h.mediaRelayMu.RLock()
	relay := h.mediaRelay
	relayErr := h.mediaRelayErr
	h.mediaRelayMu.RUnlock()
	return relay, relayErr
}

// Close releases all active Codex live sessions and client secrets.
func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	var errClose error
	if h.sessions != nil {
		errClose = h.sessions.closeAll("server_stopped")
	}
	if h.clientSecrets != nil {
		h.clientSecrets.close()
	}
	return errClose
}

// Handle forwards a WebRTC SDP bootstrap request to the Codex realtime calls endpoint.
func (h *Handler) Handle(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeLiveError(c, http.StatusServiceUnavailable, "Codex auth manager unavailable")
		return
	}

	body, errRead := readBody(c.Request.Body)
	if errRead != nil {
		status := clienterror.HTTPStatusFromErrorOr(errRead, http.StatusBadRequest)
		if errors.Is(errRead, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeLiveError(c, status, errRead.Error())
		return
	}
	upstreamBody, upstreamContentType, model, errPayload := prepareCallRequest(body, c.GetHeader("Content-Type"))
	requestedModel := model
	if errPayload == nil {
		upstreamBody, upstreamContentType, model, errPayload = applyClientSecretCallSession(
			upstreamBody,
			upstreamContentType,
			model,
			clientSecretSession(c),
		)
	}
	if errPayload == nil {
		upstreamBody, model, errPayload = rewriteCallRequestModel(upstreamBody, upstreamContentType, model)
	}
	if errPayload != nil {
		writeLiveError(c, http.StatusBadRequest, errPayload.Error())
		return
	}
	if !h.authorizeRealtimeModel(c, requestedModel) || !h.authorizeRealtimeModel(c, model) {
		return
	}
	runtimeConfig, mediaRelay, mediaRelayErr := h.currentRuntime()
	if mediaRelayErr != nil {
		log.WithError(mediaRelayErr).Warn("codex live media relay unavailable")
		writeLiveError(c, http.StatusServiceUnavailable, "Codex live media relay unavailable")
		return
	}
	var mediaSession mediaRelaySession
	mediaRetained := false

	ctx := util.WithGinContext(c.Request.Context(), c)
	selectionHeaders := liveSelectionHeaders(c)
	selectionMetadata := liveSelectionMetadata(c, nil)
	ctx = enrichLiveSessionContext(ctx, selectionHeaders, body, selectionMetadata)
	selectionOpts := coreexecutor.Options{
		Headers:         selectionHeaders,
		OriginalRequest: body,
		Metadata:        selectionMetadata,
	}
	selection, selected, errSelect := h.selectOAuth(ctx, model, selectionOpts)
	if errSelect != nil {
		writeSelectionError(c, errSelect)
		return
	}
	if selected == nil {
		if selection != nil {
			selection.End("missing_auth")
		}
		writeLiveError(c, http.StatusServiceUnavailable, "Codex auth unavailable")
		return
	}

	if selection != nil {
		attemptCtx, releaseAttempt, errAttempt := selection.AttemptContext(ctx)
		if errAttempt != nil {
			selection.End("attempt_bind_failed")
			writeLiveError(c, http.StatusServiceUnavailable, errAttempt.Error())
			return
		}
		ctx = attemptCtx
		defer releaseAttempt()
		defer func() {
			if selection.Active() && !selection.Retained() {
				selection.End("request_closed")
			}
		}()
	}
	selectedIndex := selected.EnsureIndex()
	logging.SetGinCPATraceID(c, selectedIndex)

	if mediaRelay != nil {
		clientOffer, errSDP := callRequestSDP(upstreamBody, upstreamContentType)
		if errSDP != nil {
			writeLiveError(c, http.StatusBadRequest, errSDP.Error())
			return
		}
		var upstreamOffer string
		mediaSession, upstreamOffer, errSDP = mediaRelay.NewSession(
			ctx,
			clientOffer,
			mediaSessionRoute{
				proxyURL:   proxyURLForAuth(runtimeConfig, selected),
				credential: mediaCredentialName(selected, selectedIndex),
				authIndex:  selectedIndex,
			},
		)
		if errSDP != nil {
			log.WithError(errSDP).Warn("codex live media session creation failed")
			writeLiveError(
				c,
				clienterror.HTTPStatusFromErrorOr(errSDP, http.StatusBadGateway),
				"Failed to create Codex live media session",
			)
			return
		}
		defer func() {
			if !mediaRetained {
				if errClose := mediaSession.CloseWithReason("request_not_retained"); errClose != nil {
					log.WithError(errClose).Debug("codex live media: close unretained session")
				}
			}
		}()
		upstreamBody, upstreamContentType, errSDP = replaceCallRequestSDP(
			upstreamBody,
			upstreamContentType,
			upstreamOffer,
		)
		if errSDP != nil {
			writeLiveError(c, http.StatusBadRequest, errSDP.Error())
			return
		}
	}

	headers := protocolHeaders(c.Request.Header)
	headers.Set("Content-Type", upstreamContentType)
	setAccountHeader(headers, selected)
	req, errRequest := h.authManager.NewHttpRequest(
		ctx, selected, http.MethodPost, upstreamCallURL, upstreamBody, headers,
	)
	if errRequest != nil {
		if selection != nil {
			selection.End("request_build_failed")
		}
		writeLiveError(c, clienterror.HTTPStatusFromErrorOr(errRequest, http.StatusBadGateway), errRequest.Error())
		return
	}

	runtimeexecutor.RecordAPIRequest(
		ctx,
		runtimeConfig,
		upstreamCallURL,
		http.MethodPost,
		headersForLogging(req.Header),
		upstreamBody,
		"codex",
		selected,
	)

	if errContext := ctx.Err(); errContext != nil {
		if selection != nil {
			selection.End("attempt_canceled")
		}
		writeLiveError(
			c,
			clienterror.HTTPStatusFromErrorOr(errContext, http.StatusRequestTimeout),
			errContext.Error(),
		)
		return
	}
	resp, errRequest := h.authManager.HttpRequest(ctx, selected, req)
	if errRequest != nil {
		if selection != nil {
			selection.End("request_failed")
		}
		runtimeexecutor.RecordAPIResponseError(ctx, runtimeConfig, errRequest)
		writeLiveError(c, clienterror.HTTPStatusFromErrorOr(errRequest, http.StatusBadGateway), errRequest.Error())
		return
	}

	var closeResponseOnce sync.Once
	var closeResponseErr error
	closeResponseBody := func() error {
		closeResponseOnce.Do(
			func() {
				closeResponseErr = resp.Body.Close()
				if closeResponseErr != nil {
					log.Errorf("codex live: close response body error: %v", closeResponseErr)
				}
			},
		)
		return closeResponseErr
	}
	defer func() { _ = closeResponseBody() }()
	if selection != nil {
		if errBind := selection.Bind(closeResponseBody); errBind != nil {
			if resp.StatusCode == http.StatusUnauthorized {
				h.authManager.ReportHomeUnauthorized(ctx, selected, "codex", model)
			}
			selection.End("response_bind_failed")
			writeLiveError(c, http.StatusServiceUnavailable, errBind.Error())
			return
		}
	}

	responseHeaders := callResponseHeaders(resp.Header)
	runtimeexecutor.RecordAPIResponseMetadata(ctx, runtimeConfig, resp.StatusCode, responseHeaders)
	responseBody, errResponse := readLimitedBody(resp.Body)
	runtimeexecutor.AppendAPIResponseChunk(ctx, runtimeConfig, responseBody)
	if selection != nil && resp.StatusCode == http.StatusUnauthorized {
		h.authManager.ReportHomeUnauthorized(ctx, selected, "codex", model, responseBody)
	}
	if errResponse != nil {
		runtimeexecutor.RecordAPIResponseError(ctx, runtimeConfig, errResponse)
		message := "Failed to read Codex live response"
		status := clienterror.HTTPStatusFromErrorOr(errResponse, http.StatusBadGateway)
		if errors.Is(errResponse, errBodyTooLarge) {
			message = "Codex live response body too large"
			status = http.StatusBadGateway
		}
		writeLiveError(c, status, message)
		return
	}
	if selection != nil && resp.StatusCode == http.StatusUnauthorized {
		log.WithField("status", resp.StatusCode).Warnf("codex live upstream request failed: %s", logging.SafeDiagnosticForLog(string(responseBody)))
	}
	responseBodyToWrite := responseBody
	success := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	callID := ""
	if success {
		callID = callIDFromLocation(resp.Header.Get("Location"))
		if callID == "" && mediaSession != nil {
			writeLiveError(c, http.StatusBadGateway, "Codex live response is missing a valid call ID")
			return
		}
		if mediaSession != nil {
			mediaSession.SetCallID(callID)
		}
		if callID != "" && c.Request != nil && c.Request.URL != nil &&
			strings.HasPrefix(c.Request.URL.Path, "/v1/realtime") {
			responseHeaders.Set("Location", "/v1/realtime/calls/"+callID)
		}
	}
	if success && mediaSession != nil {
		upstreamAnswer, errSDP := callResponseSDP(responseBody, resp.Header.Get("Content-Type"))
		if errSDP != nil {
			writeLiveError(c, http.StatusBadGateway, errSDP.Error())
			return
		}
		downstreamAnswer, errAnswer := mediaSession.AcceptUpstreamAnswer(ctx, upstreamAnswer)
		if errAnswer != nil {
			log.WithError(errAnswer).Warn("codex live media answer failed")
			writeLiveError(
				c,
				clienterror.HTTPStatusFromErrorOr(errAnswer, http.StatusBadGateway),
				"Failed to establish Codex live media session",
			)
			return
		}
		responseBodyToWrite = []byte(downstreamAnswer)
		responseHeaders.Set("Content-Type", "application/sdp")
	}

	var storedSession liveSession
	sessionStored := false
	if success && h.sessions != nil {
		if callID != "" {
			clientMetadata := logging.GetClientRequestMetadata(ctx)
			session := liveSession{
				authID:          selected.ID,
				model:           model,
				media:           mediaSession,
				sessionID:       clientMetadata.SessionID,
				parentSessionID: clientMetadata.ParentSessionID,
			}
			session.ownerPrincipal, session.ownerProvider = requestOwner(c)
			if principal, ok := c.Get(ClientSecretPrincipalContextKey); ok {
				session.clientSecretPrincipal, _ = principal.(string)
			}
			if selection != nil {
				if mediaSession != nil {
					if errBind := selection.Bind(
						func() error {
							return mediaSession.CloseWithReason("home_selection_closed")
						},
					); errBind != nil {
						selection.End("media_bind_failed")
						writeLiveError(c, http.StatusServiceUnavailable, errBind.Error())
						return
					}
				}
				if errBind := selection.Bind(
					func() error {
						go selection.End("session_drained")
						return nil
					},
				); errBind != nil {
					selection.End("session_drain_bind_failed")
					writeLiveError(c, http.StatusServiceUnavailable, errBind.Error())
					return
				}
				selection.Retain()
				session.homeSelection = selection
			}
			storedSession = h.sessions.put(callID, session)
			sessionStored = storedSession.callID != ""
			if mediaSession != nil && sessionStored {
				mediaSession.SetCloseHandler(
					func(reason string) {
						h.sessions.complete(storedSession, reason)
					},
				)
				mediaRetained = true
			}
		}
	}
	writeResponseHeaders(c.Writer.Header(), responseHeaders)
	c.Status(resp.StatusCode)
	if _, errWrite := c.Writer.Write(responseBodyToWrite); errWrite != nil {
		if sessionStored {
			h.sessions.complete(storedSession, "response_write_failed")
		}
		runtimeexecutor.RecordAPIResponseError(ctx, runtimeConfig, errWrite)
		log.WithError(errWrite).Warn("codex live: write response body failed")
	}
}

func mediaCredentialName(selected *auth.Auth, authIndex string) string {
	if selected == nil {
		return strings.TrimSpace(authIndex)
	}
	if label := strings.TrimSpace(selected.Label); label != "" {
		return label
	}
	if fileName := strings.TrimSpace(selected.FileName); fileName != "" {
		if baseName := strings.TrimSpace(filepath.Base(fileName)); baseName != "" && baseName != "." {
			return baseName
		}
	}
	return strings.TrimSpace(authIndex)
}

func (h *Handler) configSnapshot() *config.Config {
	if h == nil || h.configProvider == nil {
		return nil
	}
	return h.configProvider()
}

// RealtimeModelAllowed applies the issuer's current model and provider restrictions.
func RealtimeModelAllowed(cfg *config.Config, principal, requestedModel string) bool {
	if cfg == nil {
		return true
	}
	rule, hasRule := cfg.APIKeyRule(principal)
	if !hasRule {
		return true
	}
	canonicalModel := codexRealtimeModel(requestedModel)
	mappedModel, mapped, err := cfg.ResolveAPIKeyModel(principal, requestedModel)
	if err != nil || (mapped && codexRealtimeModel(mappedModel) != canonicalModel) || len(rule.Limits) > 0 {
		return false
	}
	return !cfg.APIKeyBlocksModel(principal, requestedModel) &&
		!cfg.APIKeyBlocksModel(principal, canonicalModel) &&
		cfg.APIKeyAllowsCanonicalModel(principal, canonicalModel) &&
		len(cfg.FilterAPIKeyProviders(principal, []string{"codex"})) > 0
}

func (h *Handler) authorizeRealtimeModel(c *gin.Context, requestedModel string) bool {
	principal, _ := requestOwner(c)
	if RealtimeModelAllowed(h.configSnapshot(), principal, requestedModel) {
		return true
	}
	writeRealtimeError(c, http.StatusForbidden, "Realtime request is not permitted by this API key policy", "permission_error", "realtime_api_key_restricted")
	return false
}

func liveSelectionMetadata(c *gin.Context, metadata map[string]any) map[string]any {
	principal, _ := requestOwner(c)
	callerScope := coresession.CallerScope(principal)
	if callerScope == "" {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[coreexecutor.CallerScopeMetadataKey] = callerScope
	return metadata
}

func enrichLiveSessionContext(
	ctx context.Context,
	headers http.Header,
	payload []byte,
	metadata map[string]any,
) context.Context {
	clientMetadata := logging.GetClientRequestMetadata(ctx)
	if info, ok := coresession.ExtractSessionInfo(headers, payload, metadata); ok {
		clientMetadata.SessionID = info.SessionID
		clientMetadata.ParentSessionID = info.ParentSessionID
		if clientMetadata.SessionID == clientMetadata.ParentSessionID {
			clientMetadata.ParentSessionID = ""
		}
		ctx = logging.WithClientRequestMetadata(ctx, clientMetadata)
		return util.WithSessionID(ctx, clientMetadata.SessionID)
	}
	if clientMetadata.SessionID != "" || clientMetadata.ParentSessionID != "" || util.SessionIDFromContext(ctx) != "" {
		clientMetadata.SessionID = ""
		clientMetadata.ParentSessionID = ""
		ctx = logging.WithClientRequestMetadata(ctx, clientMetadata)
		return util.WithSessionID(ctx, "")
	}
	return ctx
}

func (h *Handler) selectOAuth(
	ctx context.Context,
	model string,
	opts coreexecutor.Options,
) (*auth.HomeDispatchSelection, *auth.Auth, error) {
	var selection *auth.HomeDispatchSelection
	var selected *auth.Auth
	var errSelect error
	if h.authManager.HomeEnabled() {
		selection, errSelect = h.authManager.SelectHomeAuthByKind(ctx, "codex", model, auth.AuthKindOAuth, opts)
		if selection != nil {
			selected = selection.CloneAuth()
		}
	} else {
		selected, errSelect = h.authManager.SelectAuthByKind(ctx, "codex", "", auth.AuthKindOAuth, opts)
	}
	if errSelect != nil && selection != nil {
		selection.End("selection_failed")
	}
	return selection, selected, errSelect
}

var errBodyTooLarge = errors.New("codex live request body too large")

func readBody(body io.Reader) ([]byte, error) {
	payload, errRead := readLimitedBody(body)
	if errRead != nil {
		if errors.Is(errRead, errBodyTooLarge) {
			return nil, errRead
		}
		return nil, fmt.Errorf("failed to read Codex live request: %w", errRead)
	}
	return payload, nil
}

func readLimitedBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	payload, errRead := io.ReadAll(io.LimitReader(body, maxBodySize+1))
	if len(payload) > maxBodySize {
		payload = payload[:maxBodySize]
		if errRead == nil {
			errRead = errBodyTooLarge
		}
	}
	return payload, errRead
}

func prepareCallRequest(body []byte, contentType string) ([]byte, string, string, error) {
	mediaType, params, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		return multipartCallRequest(body, strings.TrimSpace(params["boundary"]))
	}

	model := modelFromJSON(body)
	if model == "" {
		model = defaultLiveModel
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	return body, contentType, model, nil
}

func applyClientSecretCallSession(
	body []byte,
	contentType, model string,
	session json.RawMessage,
) ([]byte, string, string, error) {
	if len(session) == 0 {
		return body, contentType, model, nil
	}
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType == nil && (strings.EqualFold(mediaType, "application/sdp") ||
		strings.EqualFold(mediaType, "text/plain")) {
		encoded, errEncode := encodeCallRequest(string(body), session)
		if errEncode != nil {
			return nil, "", "", errEncode
		}
		return encoded, "application/json", modelFromJSON(session), nil
	}
	if errMediaType != nil || !strings.EqualFold(mediaType, "application/json") {
		return nil, "", "", errors.New("realtime client secrets require an SDP or JSON call request")
	}
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, "", "", fmt.Errorf("failed to decode Realtime call request: %w", errUnmarshal)
	}
	if payload == nil {
		return nil, "", "", errors.New("realtime call request must be a JSON object")
	}
	payload["session"] = append(json.RawMessage(nil), session...)
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, "", "", fmt.Errorf("failed to encode Realtime call request: %w", errMarshal)
	}
	return encoded, "application/json", modelFromJSON(session), nil
}

func rewriteCallRequestModel(body []byte, contentType, model string) ([]byte, string, error) {
	upstreamModel := codexRealtimeModel(model)
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType != nil || !strings.EqualFold(mediaType, "application/json") || len(bytes.TrimSpace(body)) == 0 {
		return body, upstreamModel, nil
	}
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, "", fmt.Errorf("failed to decode Realtime call request: %w", errUnmarshal)
	}
	if payload == nil {
		return nil, "", errors.New("realtime call request must be a JSON object")
	}
	encodedModel, errMarshal := json.Marshal(upstreamModel)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("failed to encode Realtime model: %w", errMarshal)
	}
	if sessionJSON, ok := payload["session"]; ok && len(sessionJSON) > 0 {
		var session map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(sessionJSON, &session); errUnmarshal != nil {
			return nil, "", fmt.Errorf("failed to decode Realtime session: %w", errUnmarshal)
		}
		if session == nil {
			return nil, "", errors.New("realtime session must be a JSON object")
		}
		session["model"] = encodedModel
		encodedSession, errEncode := json.Marshal(session)
		if errEncode != nil {
			return nil, "", fmt.Errorf("failed to encode Realtime session: %w", errEncode)
		}
		payload["session"] = encodedSession
	} else if _, ok := payload["model"]; ok {
		payload["model"] = encodedModel
	} else {
		return body, upstreamModel, nil
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("failed to encode Realtime call request: %w", errMarshal)
	}
	return encoded, upstreamModel, nil
}

func multipartCallRequest(body []byte, boundary string) ([]byte, string, string, error) {
	if boundary == "" {
		return nil, "", "", errors.New("codex live multipart boundary is missing")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var sdp *string
	var session json.RawMessage
	model := ""
	for {
		part, errPart := reader.NextPart()
		if errors.Is(errPart, io.EOF) {
			break
		}
		if errPart != nil {
			return nil, "", "", fmt.Errorf("failed to parse Codex live multipart body: %w", errPart)
		}
		partBody, errRead := io.ReadAll(part)
		errClose := part.Close()
		if errRead != nil {
			return nil, "", "", fmt.Errorf("failed to read Codex live multipart field: %w", errRead)
		}
		if errClose != nil {
			return nil, "", "", fmt.Errorf("failed to close Codex live multipart field: %w", errClose)
		}

		switch part.FormName() {
		case "sdp":
			value := string(partBody)
			sdp = &value
		case "session":
			if !json.Valid(partBody) {
				return nil, "", "", errors.New("codex live session field must contain valid JSON")
			}
			session = append(json.RawMessage(nil), partBody...)
			model = modelFromJSON(partBody)
		}
	}
	if sdp == nil {
		return nil, "", "", errors.New("codex live multipart body requires an sdp field")
	}
	if model == "" {
		model = defaultLiveModel
	}

	encoded, errEncode := encodeCallRequest(*sdp, session)
	if errEncode != nil {
		return nil, "", "", errEncode
	}
	return encoded, "application/json", model, nil
}

func encodeCallRequest(sdp string, session json.RawMessage) ([]byte, error) {
	payload := struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session,omitempty"`
	}{
		SDP:     sdp,
		Session: session,
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("failed to encode Codex live request: %w", errMarshal)
	}
	return encoded, nil
}

func callRequestSDP(body []byte, contentType string) (string, error) {
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType == nil && (strings.EqualFold(mediaType, "application/sdp") || strings.EqualFold(
		mediaType, "text/plain",
	)) {
		if strings.TrimSpace(string(body)) == "" {
			return "", errors.New("codex live call request requires an SDP offer")
		}
		return string(body), nil
	}
	if errMediaType != nil || !strings.EqualFold(mediaType, "application/json") {
		return "", errors.New("codex live media relay requires an SDP or JSON call request")
	}
	var payload struct {
		SDP string `json:"sdp"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("failed to decode Codex live call request: %w", errUnmarshal)
	}
	if strings.TrimSpace(payload.SDP) == "" {
		return "", errors.New("codex live call request requires an SDP offer")
	}
	return payload.SDP, nil
}

func replaceCallRequestSDP(body []byte, contentType, sdp string) ([]byte, string, error) {
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType == nil && (strings.EqualFold(mediaType, "application/sdp") || strings.EqualFold(
		mediaType, "text/plain",
	)) {
		encoded, errEncode := encodeCallRequest(sdp, nil)
		if errEncode != nil {
			return nil, "", errEncode
		}
		return encoded, "application/json", nil
	}
	if errMediaType != nil || !strings.EqualFold(mediaType, "application/json") {
		return nil, "", errors.New("codex live media relay requires an SDP or JSON call request")
	}
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, "", fmt.Errorf("failed to decode Codex live call request: %w", errUnmarshal)
	}
	encodedSDP, errMarshal := json.Marshal(sdp)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("failed to encode Codex live SDP offer: %w", errMarshal)
	}
	payload["sdp"] = encodedSDP
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("failed to encode Codex live call request: %w", errMarshal)
	}
	return encoded, "application/json", nil
}

func callResponseSDP(body []byte, contentType string) (string, error) {
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if errMediaType == nil && strings.EqualFold(mediaType, "application/json") {
		var payload struct {
			SDP string `json:"sdp"`
		}
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return "", fmt.Errorf("failed to decode Codex live response: %w", errUnmarshal)
		}
		if strings.TrimSpace(payload.SDP) == "" {
			return "", errors.New("codex live response requires an SDP answer")
		}
		return payload.SDP, nil
	}
	if strings.TrimSpace(string(body)) == "" {
		return "", errors.New("codex live response requires an SDP answer")
	}
	return string(body), nil
}

func modelFromJSON(body []byte) string {
	var payload struct {
		Model   string `json:"model"`
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return ""
	}
	if model := strings.TrimSpace(payload.Session.Model); model != "" {
		return model
	}
	return strings.TrimSpace(payload.Model)
}

func protocolHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	for _, name := range liveProtocolHeaders {
		for _, value := range source.Values(name) {
			headers.Add(name, value)
		}
	}
	return headers
}

func setAccountHeader(headers http.Header, selected *auth.Auth) {
	if selected == nil {
		return
	}
	if accountID, ok := selected.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
}

func headersForLogging(source http.Header) http.Header {
	headers := source.Clone()
	if headers.Get("X-Oai-Attestation") != "" {
		headers.Set("X-Oai-Attestation", "[REDACTED]")
	}
	return headers
}

func callResponseHeaders(source http.Header) http.Header {
	headers := make(http.Header)
	for _, name := range []string{"Content-Type", "Location", "Retry-After", "X-Request-Id", "OpenAI-Request-Id"} {
		for _, value := range source.Values(name) {
			headers.Add(name, value)
		}
	}
	return headers
}

func writeResponseHeaders(destination, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func writeLiveError(c *gin.Context, status int, message string) {
	if c != nil && c.Request != nil && c.Request.URL != nil &&
		strings.HasPrefix(c.Request.URL.Path, "/v1/realtime") {
		errorType := "api_error"
		if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
			errorType = "invalid_request_error"
		}
		if status == http.StatusUnauthorized {
			errorType = "authentication_error"
		}
		writeRealtimeError(c, status, message, errorType, "realtime_request_failed")
		return
	}
	c.JSON(status, gin.H{"error": message})
}

func writeSelectionError(c *gin.Context, err error) {
	status := clienterror.HTTPStatusFromErrorOr(err, http.StatusServiceUnavailable)
	for _, value := range auth.SafeResponseHeaders(err).Values("Retry-After") {
		c.Writer.Header().Add("Retry-After", value)
	}
	writeLiveError(c, status, err.Error())
}
