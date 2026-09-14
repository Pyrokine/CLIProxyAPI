package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/antigravity"
	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	geminiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/gemini"
	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/geminicli"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

const (
	defaultAPICallTimeout    = 60 * time.Second
	maxAPICallDraftTokenSize = 64 << 10
)

var antigravityOAuthTokenURL = antigravity.TokenEndpoint

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	ProxyURLSnake   *string           `json:"proxy_url"`
	ProxyURLCamel   *string           `json:"proxyUrl"`
	ProxyURLPascal  *string           `json:"ProxyUrl"`
	DraftTokenSnake *string           `json:"draft_token"`
	DraftTokenCamel *string           `json:"draftToken"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

// APICall makes a generic HTTP request on behalf of the management API caller.
// It is protected by the management middleware.
//
// Endpoint:
//
//	POST /v0/management/api-call
//
// Authentication:
//
//	Same as other management APIs (requires a management key and remote-management rules).
//	You can provide the key via:
//	- Authorization: Bearer <key>
//	- X-Management-Key: <key>
//
// Request JSON:
//   - auth_index / authIndex / AuthIndex (optional):
//     The credential "auth_index" from GET /v0/management/auth-files (or other endpoints returning it).
//     If omitted or not found, credential-specific proxy/token substitution is skipped.
//   - method (required): HTTP method, e.g. GET, POST, PUT, PATCH, DELETE.
//   - url (required): Absolute URL including scheme and host, e.g. "https://api.example.com/v1/ping".
//   - header (optional): Request headers map.
//     Supports magic variable "$TOKEN$" which is replaced using the selected credential:
//     1) metadata.access_token
//     2) attributes.api_key
//     3) metadata.token / metadata.id_token / metadata.cookie
//     Example: {"Authorization":"Bearer $TOKEN$"}.
//     Literal restricted headers are rejected.
//   - draft_token / draftToken (optional):
//     An unsaved credential used only for $TOKEN$ replacement in this request.
//     It is never persisted and takes precedence over auth_index token resolution.
//   - data (optional): Raw request body as string (useful for POST/PUT/PATCH).
//
// Proxy selection (highest priority first):
//  1. Request proxy_url / proxyUrl / ProxyUrl
//  2. Selected credential proxy_url
//  3. Global config proxy-url
//  4. Direct connect (environment proxies are not used)
//
// Response JSON (returned with HTTP 200 when the APICall itself succeeds):
//   - status_code: Upstream HTTP status code.
//   - header: Upstream response headers.
//   - body: Upstream response body as string.
//
// Example:
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"GET",
//	       "url":"https://api.example.com/v1/ping",
//	       "header":{"Authorization":"Bearer $TOKEN$"}}'
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer 831227" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"POST",
//	       "url":"https://api.example.com/v1/fetchAvailableModels",
//	       "header":{"Authorization":"Bearer $TOKEN$",
//	                  "Content-Type":"application/json",
//	                  "User-Agent":"cliproxyapi"},
//	       "data":"{}"}'
func (h *Handler) APICall(c *gin.Context) {
	cfg := h.configSnapshot()
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only http and https schemes are allowed"})
		return
	}
	_, private := resolveAndCheckHost(parsedURL.Hostname())
	if private {
		c.JSON(http.StatusForbidden, gin.H{"error": "requests to private/internal addresses are not allowed"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	proxyURL := firstNonEmptyString(body.ProxyURLSnake, body.ProxyURLCamel, body.ProxyURLPascal)
	draftToken := firstNonEmptyString(body.DraftTokenSnake, body.DraftTokenCamel)
	if len(draftToken) > maxAPICallDraftTokenSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "draft token is too large"})
		return
	}
	auth := h.authByIndex(authIndex)

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}
	if err := validateAPICallHeaders(reqHeaders); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Check if any header uses $TOKEN$ — if so, restrict target URL to known provider domains
	// to prevent token exfiltration to arbitrary URLs (e.g., httpbin.org reflection).
	hasTokenPlaceholder := false
	for _, value := range reqHeaders {
		if strings.Contains(value, "$TOKEN$") {
			hasTokenPlaceholder = true
			break
		}
	}
	if draftToken != "" && !hasTokenPlaceholder {
		c.JSON(http.StatusBadRequest, gin.H{"error": "draft token requires a $TOKEN$ header placeholder"})
		return
	}
	if hasTokenPlaceholder {
		if errTransport := validateTokenTransportURL(parsedURL); errTransport != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errTransport.Error()})
			return
		}
		if !isAllowedTokenTargetURL(parsedURL) {
			c.JSON(http.StatusForbidden, gin.H{"error": "$TOKEN$ can only be used with known provider domains"})
			return
		}
	}

	var token string
	var tokenErr error
	if hasTokenPlaceholder {
		token, tokenErr = h.resolveAPICallToken(c.Request.Context(), cfg, auth, draftToken)
	}
	if hasTokenPlaceholder && token == "" {
		if tokenErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token refresh failed"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth token not found"})
		return
	}
	for key, value := range reqHeaders {
		if strings.Contains(value, "$TOKEN$") {
			reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
		}
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	for key, value := range reqHeaders {
		req.Header.Set(key, value)
	}

	httpClient := &http.Client{
		Timeout: defaultAPICallTimeout,
	}
	httpClient.CheckRedirect = func(redirectReq *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if isPrivateHost(redirectReq.URL.Hostname()) {
			return fmt.Errorf("redirect to private address blocked")
		}
		// When $TOKEN$ was used, block redirect to non-whitelisted domains
		// and strip token-bearing headers to prevent credential exfiltration.
		if hasTokenPlaceholder {
			if errTransport := validateTokenTransportURL(redirectReq.URL); errTransport != nil {
				return errTransport
			}
			if !isAllowedTokenRedirectTarget(parsedURL, redirectReq.URL) {
				return fmt.Errorf("redirect to untrusted domain blocked (token protection)")
			}
			if shouldStripTokenRedirectHeaders(parsedURL.Hostname(), redirectReq.URL.Hostname()) {
				for key := range redirectReq.Header {
					if strings.Contains(redirectReq.Header.Get(key), token) && token != "" {
						redirectReq.Header.Del(key)
					}
				}
			}
		}
		return nil
	}
	httpClient.Transport = publicHostTransport(h.apiCallTransport(cfg, auth, proxyURL))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if errReadAll != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	c.JSON(
		http.StatusOK, apiCallResponse{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       string(respBody),
		},
	)
}

func validateTokenTransportURL(target *url.URL) error {
	if target == nil || !strings.EqualFold(target.Scheme, "https") {
		return errors.New("$TOKEN$ requires HTTPS and cannot follow a downgrade redirect")
	}
	return nil
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if out := strings.TrimSpace(*v); out != "" {
			return out
		}
	}
	return ""
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := tokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		if v := tokenValueFromMetadata(shared.MetadataSnapshot()); v != "" {
			return v
		}
	}
	return ""
}

func validateAPICallHeaders(headers map[string]string) error {
	for name, value := range headers {
		trimmedName := strings.TrimSpace(name)
		if trimmedName == "" || strings.TrimSpace(value) == "" {
			continue
		}
		if strings.EqualFold(trimmedName, "Host") {
			return fmt.Errorf("host header override is not allowed")
		}
		if proxyutil.IsRestrictedCustomHeader(trimmedName) && !strings.Contains(value, "$TOKEN$") {
			return fmt.Errorf("literal %s header is not allowed; use $TOKEN$ with a supported provider", trimmedName)
		}
	}
	return nil
}

func (h *Handler) resolveAPICallToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
	draftToken string,
) (string, error) {
	if draftToken != "" {
		return draftToken, nil
	}
	return h.resolveTokenForAuth(ctx, cfg, auth)
}

func (h *Handler) resolveTokenForAuth(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	if auth == nil {
		return "", nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "claude", "anthropic":
		token, errToken := h.refreshClaudeOAuthAccessToken(ctx, cfg, auth)
		return token, errToken
	case "codex":
		token, errToken := h.refreshCodexOAuthAccessToken(ctx, cfg, auth)
		return token, errToken
	case "gemini", "gemini-cli":
		token, errToken := h.refreshGeminiOAuthAccessToken(ctx, cfg, auth)
		return token, errToken
	case "antigravity":
		token, errToken := h.refreshAntigravityOAuthAccessToken(ctx, cfg, auth)
		return token, errToken
	case "xai", "x-ai", "grok":
		token, errToken := h.refreshXAIOAuthAccessToken(ctx, cfg, auth)
		return token, errToken
	default:
		return tokenValueForAuth(auth), nil
	}
}

func tokenExpired(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	for _, key := range []string{"expired", "expiry", "expires_at", "expiresAt"} {
		raw := stringValue(metadata, key)
		if raw == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			return !time.Now().Add(time.Minute).Before(ts)
		}
	}
	return false
}

func refreshTokenValue(metadata map[string]any) string {
	if refreshToken := stringValue(metadata, "refresh_token"); refreshToken != "" {
		return refreshToken
	}
	if token, ok := metadata["token"].(map[string]any); ok {
		return stringValue(token, "refresh_token")
	}
	return ""
}

func refreshOwnerMatches(current, before *coreauth.Auth) bool {
	if current == nil || before == nil || current.ID != before.ID ||
		!strings.EqualFold(strings.TrimSpace(current.Provider), strings.TrimSpace(before.Provider)) {
		return false
	}
	refreshToken := refreshTokenValue(before.Metadata)
	return refreshToken != "" && refreshTokenValue(current.Metadata) == refreshToken
}

func mergeRefreshedAuth(current, before, refreshed *coreauth.Auth) *coreauth.Auth {
	next := current.Clone()
	if next.Metadata == nil {
		next.Metadata = make(map[string]any)
	}
	refreshedSnapshot := refreshed.Clone()
	for key, value := range refreshedSnapshot.Metadata {
		if previous, existed := before.Metadata[key]; !existed || !reflect.DeepEqual(previous, value) {
			next.Metadata[key] = value
		}
	}
	for key, previous := range before.Metadata {
		if _, exists := refreshedSnapshot.Metadata[key]; !exists {
			if currentValue, currentExists := next.Metadata[key]; currentExists && reflect.DeepEqual(
				currentValue, previous,
			) {
				delete(next.Metadata, key)
			}
		}
	}
	if !refreshed.LastRefreshedAt.Equal(before.LastRefreshedAt) {
		next.LastRefreshedAt = refreshed.LastRefreshedAt
	}
	return next
}

func (h *Handler) persistRefreshedAuth(ctx context.Context, before, refreshed *coreauth.Auth) error {
	if h == nil || h.authManager == nil || before == nil || refreshed == nil || before.ID != refreshed.ID {
		return fmt.Errorf("refreshed auth persistence requires matching registered auth")
	}
	persistenceBase := context.Background()
	if ctx != nil {
		persistenceBase = context.WithoutCancel(ctx)
	}
	persistenceCtx, cancelPersistence := context.WithTimeout(persistenceBase, defaultAPICallTimeout)
	defer cancelPersistence()
	for range 8 {
		current, ok := h.authManager.GetByID(refreshed.ID)
		if !ok || current == nil {
			return fmt.Errorf("refreshed auth %s is no longer registered", refreshed.ID)
		}
		if !refreshOwnerMatches(current, before) {
			return fmt.Errorf("persist refreshed auth %s: credential owner changed", refreshed.ID)
		}
		next := mergeRefreshedAuth(current, before, refreshed)
		updated, errUpdate := h.authManager.MergePersistentUpdate(persistenceCtx, current, next)
		if errUpdate != nil {
			return fmt.Errorf("persist refreshed auth %s: %w", refreshed.ID, errUpdate)
		}
		if updated != nil {
			return nil
		}
		if errContext := persistenceCtx.Err(); errContext != nil {
			return errContext
		}
	}
	return fmt.Errorf("persist refreshed auth %s: credential changed concurrently", refreshed.ID)
}

func (h *Handler) refreshClaudeOAuthAccessToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	current := tokenValueForAuth(auth)
	if current != "" && !tokenExpired(auth.Metadata) {
		return current, nil
	}
	refreshToken := stringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return current, nil
	}
	before := auth.Clone()
	svc := claudeauth.NewAuth(cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.Email != "" {
		auth.Metadata["email"] = td.Email
	}
	if td.AccountUUID != "" {
		auth.Metadata["account_uuid"] = td.AccountUUID
	}
	if td.OrganizationUUID != "" {
		auth.Metadata["organization_uuid"] = td.OrganizationUUID
	}
	if td.OrganizationName != "" {
		auth.Metadata["organization_name"] = td.OrganizationName
	}
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	if errPersist := h.persistRefreshedAuth(ctx, before, auth); errPersist != nil {
		return "", errPersist
	}
	return strings.TrimSpace(td.AccessToken), nil
}

func (h *Handler) refreshCodexOAuthAccessToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	current := tokenValueForAuth(auth)
	if current != "" && !tokenExpired(auth.Metadata) {
		return current, nil
	}
	refreshToken := stringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return current, nil
	}
	before := auth.Clone()
	svc := codexauth.NewAuth(cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return "", err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	if errPersist := h.persistRefreshedAuth(ctx, before, auth); errPersist != nil {
		return "", errPersist
	}
	return strings.TrimSpace(td.AccessToken), nil
}

func (h *Handler) refreshGeminiOAuthAccessToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	before := auth.Clone()
	sharedCredential := geminicli.ResolveSharedCredential(auth.Runtime) != nil
	metadata, updater := geminiOAuthMetadata(auth)
	if len(metadata) == 0 {
		return "", fmt.Errorf("gemini oauth metadata missing")
	}

	base, token, conf := geminiauth.TokenAndConfigFromMetadata(metadata)

	ctxToken := ctx
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(cfg, auth, ""),
	}
	ctxToken = context.WithValue(ctxToken, oauth2.HTTPClient, httpClient)

	src := conf.TokenSource(ctxToken, token)
	currentToken, errToken := src.Token()
	if errToken != nil {
		return "", errToken
	}

	merged := buildOAuthTokenMap(base, currentToken)
	fields := buildOAuthTokenFields(currentToken, merged)
	if updater != nil {
		updater(fields)
	}
	if !sharedCredential {
		if errPersist := h.persistRefreshedAuth(ctx, before, auth); errPersist != nil {
			return "", errPersist
		}
	}
	return strings.TrimSpace(currentToken.AccessToken), nil
}

func (h *Handler) refreshAntigravityOAuthAccessToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("antigravity oauth metadata missing")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(metadata))
	if current != "" && !antigravityTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("antigravity refresh token missing")
	}
	before := auth.Clone()

	tokenURL := strings.TrimSpace(antigravityOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	form.Set("client_id", antigravity.ClientID)
	form.Set("client_secret", antigravity.ClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(cfg, auth, ""),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if errRead != nil {
		return "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"antigravity oauth token refresh failed: status %d: %s", resp.StatusCode,
			strings.TrimSpace(string(bodyBytes)),
		)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return "", errUnmarshal
	}

	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return "", fmt.Errorf("antigravity oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
	if strings.TrimSpace(tokenResp.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenResp.RefreshToken)
	}
	if tokenResp.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = tokenResp.ExpiresIn
		auth.Metadata["timestamp"] = now.UnixMilli()
		auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	auth.Metadata["type"] = "antigravity"

	auth.LastRefreshedAt = now
	auth.UpdatedAt = now
	if errPersist := h.persistRefreshedAuth(ctx, before, auth); errPersist != nil {
		return "", errPersist
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func (h *Handler) refreshXAIOAuthAccessToken(
	ctx context.Context,
	cfg *config.Config,
	auth *coreauth.Auth,
) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}
	current := tokenValueForAuth(auth)
	if current != "" && !xaiTokenNeedsRefresh(auth.Metadata) {
		return current, nil
	}
	refreshToken := stringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return current, nil
	}
	before := auth.Clone()
	tokenEndpoint := stringValue(auth.Metadata, "token_endpoint")
	svc := xaiauth.NewXAIAuthWithProxyURL(cfg, auth.ProxyURL)
	td, err := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if err != nil {
		return "", err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now().UTC()
	auth.Metadata["access_token"] = strings.TrimSpace(td.AccessToken)
	if strings.TrimSpace(td.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(td.RefreshToken)
	}
	if strings.TrimSpace(td.IDToken) != "" {
		auth.Metadata["id_token"] = strings.TrimSpace(td.IDToken)
	}
	if strings.TrimSpace(td.TokenType) != "" {
		auth.Metadata["token_type"] = strings.TrimSpace(td.TokenType)
	}
	if td.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = td.ExpiresIn
		auth.Metadata["timestamp"] = now.UnixMilli()
	}
	if strings.TrimSpace(td.Expire) != "" {
		auth.Metadata["expired"] = strings.TrimSpace(td.Expire)
	}
	if strings.TrimSpace(td.Email) != "" {
		auth.Metadata["email"] = strings.TrimSpace(td.Email)
	}
	if strings.TrimSpace(td.Subject) != "" {
		auth.Metadata["sub"] = strings.TrimSpace(td.Subject)
	}
	auth.Metadata["type"] = "xai"
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)
	if errPersist := h.persistRefreshedAuth(ctx, before, auth); errPersist != nil {
		return "", errPersist
	}
	return strings.TrimSpace(td.AccessToken), nil
}

func xaiTokenNeedsRefresh(metadata map[string]any) bool {
	if metadata == nil {
		return true
	}
	if expStr := stringValue(metadata, "expired"); expStr != "" {
		if ts, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
			return !ts.After(time.Now().Add(xaiauth.RefreshLead()))
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(xaiauth.RefreshLead()))
	}
	return true
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
	// Refresh a bit early to avoid requests racing token expiry.
	const skew = 30 * time.Second

	if metadata == nil {
		return true
	}
	if expStr, ok := metadata["expired"].(string); ok {
		if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
			return !ts.After(time.Now().Add(skew))
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(skew))
	}
	return true
}

func int64Value(raw any) int64 {
	switch typed := raw.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > ^uint64(0)>>1 {
			return 0
		}
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i
		}
	case string:
		if s := strings.TrimSpace(typed); s != "" {
			if i, errParse := json.Number(s).Int64(); errParse == nil {
				return i
			}
		}
	}
	return 0
}

func geminiOAuthMetadata(auth *coreauth.Auth) (map[string]any, func(map[string]any)) {
	if auth == nil {
		return nil, nil
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		snapshot := shared.MetadataSnapshot()
		return snapshot, func(fields map[string]any) { shared.MergeMetadata(fields) }
	}
	return auth.Metadata, func(fields map[string]any) {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		maps.Copy(auth.Metadata, fields)
	}
}

func stringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 || key == "" {
		return ""
	}
	if v, ok := metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func buildOAuthTokenMap(base map[string]any, tok *oauth2.Token) map[string]any {
	merged := cloneMap(base)
	if merged == nil {
		merged = make(map[string]any)
	}
	if tok == nil {
		return merged
	}
	if raw, errMarshal := json.Marshal(tok); errMarshal == nil {
		var tokenMap map[string]any
		if errUnmarshal := json.Unmarshal(raw, &tokenMap); errUnmarshal == nil {
			maps.Copy(merged, tokenMap)
		}
	}
	return merged
}

func buildOAuthTokenFields(tok *oauth2.Token, merged map[string]any) map[string]any {
	fields := make(map[string]any, 5)
	if tok != nil && tok.AccessToken != "" {
		fields["access_token"] = tok.AccessToken
	}
	if tok != nil && tok.TokenType != "" {
		fields["token_type"] = tok.TokenType
	}
	if tok != nil && tok.RefreshToken != "" {
		fields["refresh_token"] = tok.RefreshToken
	}
	if tok != nil && !tok.Expiry.IsZero() {
		fields["expiry"] = tok.Expiry.Format(time.RFC3339)
	}
	if len(merged) > 0 {
		fields["token"] = cloneMap(merged)
	}
	return fields
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok && tokenRaw != nil {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, ok := typed["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]string:
			if v := strings.TrimSpace(typed["access_token"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(typed["accessToken"]); v != "" {
				return v
			}
		}
	}
	if v, ok := metadata["token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["id_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["cookie"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func (h *Handler) apiCallTransport(
	cfg *config.Config,
	auth *coreauth.Auth,
	proxyURL string,
) http.RoundTripper {
	var proxyCandidates []string
	if proxyStr := strings.TrimSpace(proxyURL); proxyStr != "" {
		proxyCandidates = append(proxyCandidates, proxyStr)
	}
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}
	if cfg != nil {
		if proxyStr := strings.TrimSpace(cfg.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}

	for _, proxyStr := range proxyCandidates {
		if transport := buildProxyTransport(proxyStr); transport != nil {
			return transport
		}
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return &http.Transport{Proxy: nil}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return clone
}

func buildProxyTransport(proxyStr string) *http.Transport {
	setting, errParse := proxyutil.Parse(proxyStr)
	if errParse != nil {
		log.WithError(errParse).Debug("parse proxy URL failed")
		return nil
	}
	if setting.Mode != proxyutil.ModeProxy || setting.URL == nil {
		return nil
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		return nil
	}
	return transport
}

func isPrivateHost(hostname string) bool {
	return proxyutil.IsPrivateHost(hostname)
}

func resolveAndCheckHost(hostname string) (pinnedAddr string, private bool) {
	return proxyutil.ResolvePublicHost(hostname)
}

func publicHostTransport(base http.RoundTripper) http.RoundTripper {
	return proxyutil.PublicHostTransport(base)
}

func pinnedTransport(base http.RoundTripper, hostname string, pinnedAddr string) http.RoundTripper {
	return proxyutil.PinnedTransport(base, hostname, pinnedAddr)
}

func shouldStripTokenRedirectHeaders(originalHostname string, redirectHostname string) bool {
	return !strings.EqualFold(strings.TrimSpace(originalHostname), strings.TrimSpace(redirectHostname))
}

func isAllowedTokenRedirectTarget(originalURL *url.URL, redirectURL *url.URL) bool {
	if originalURL == nil || redirectURL == nil {
		return false
	}
	originalHostname := strings.ToLower(strings.TrimSpace(originalURL.Hostname()))
	redirectHostname := strings.ToLower(strings.TrimSpace(redirectURL.Hostname()))
	if originalHostname == "api.deepseek.com" {
		return redirectHostname == originalHostname && isDefaultHTTPSPort(redirectURL)
	}
	return isAllowedTokenTargetURL(redirectURL)
}

func isAllowedTokenTargetURL(target *url.URL) bool {
	if target == nil {
		return false
	}
	hostname := strings.ToLower(strings.TrimSpace(target.Hostname()))
	if hostname == "api.deepseek.com" && !isDefaultHTTPSPort(target) {
		return false
	}
	return isAllowedTokenTarget(hostname)
}

func isDefaultHTTPSPort(target *url.URL) bool {
	port := target.Port()
	return port == "" || port == "443"
}

// isAllowedTokenTarget returns true if the hostname belongs to a known AI provider,
// preventing $TOKEN$ credential exfiltration to arbitrary domains.
func isAllowedTokenTarget(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "api.deepseek.com" {
		return true
	}
	allowed := []string{
		"api.anthropic.com",
		"claude.ai",
		"generativelanguage.googleapis.com",
		"aiplatform.googleapis.com",
		"cloudcode-pa.googleapis.com",
		"api.openai.com",
		"api.fenno.ai",
		"api.qnaigc.com",
		"api.modelink.ai",
		"chatgpt.com",
		"api.githubcopilot.com",
		"api.individual.githubcopilot.com",
		"api.business.githubcopilot.com",
		"api.enterprise.githubcopilot.com",
		"antigravity.l7.tech",
		"iflow.cn",
		"platform.iflow.cn",
		"apis.iflow.cn",
		"kimi.moonshot.cn",
		"dashscope.aliyuncs.com",
		"x.ai",
		"cli-chat-proxy.grok.com",
	}
	for _, domain := range allowed {
		if hostname == domain || strings.HasSuffix(hostname, "."+domain) {
			return true
		}
	}
	return false
}
