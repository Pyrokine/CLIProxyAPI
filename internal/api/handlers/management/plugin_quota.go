package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const maxQuotaProbeResponseBytes = 10 << 20

type quotaPluginHost interface {
	QuotaProviders(context.Context) []pluginhost.RegisteredQuotaProviderInfo
	HasQuotaProviderContext(context.Context, string) bool
	HasQuotaProviderForPlugin(string) bool
	DescribeQuota(context.Context, string) (pluginapi.QuotaDescribeResponse, bool, error)
	FetchQuota(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, bool, error)
	FetchQuotaByPlugin(
		context.Context,
		string,
		pluginapi.QuotaFetchRequest,
	) (pluginapi.QuotaFetchResponse, bool, error)
	ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, bool, error)
	ResetQuotaByPlugin(
		context.Context,
		string,
		pluginapi.QuotaResetRequest,
	) (pluginapi.QuotaResetResponse, bool, error)
}

type credentialQuotaRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
	PluginID        string  `json:"plugin_id"`
	Provider        string  `json:"provider"`
}

func (r credentialQuotaRequest) resolveAuthIndex() string {
	for _, value := range []*string{r.AuthIndexSnake, r.AuthIndexCamel, r.AuthIndexPascal} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

func (h *Handler) quotaPluginHost() quotaPluginHost {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	host, _ := h.pluginHost.(quotaPluginHost)
	h.mu.RUnlock()
	return host
}

// GetQuotaProviders returns active plugin quota providers and their reset capability.
func (h *Handler) GetQuotaProviders(c *gin.Context) {
	host := h.quotaPluginHost()
	if host == nil {
		c.JSON(http.StatusOK, gin.H{"providers": []pluginhost.RegisteredQuotaProviderInfo{}})
		return
	}
	providers := host.QuotaProviders(c.Request.Context())
	if providers == nil {
		providers = []pluginhost.RegisteredQuotaProviderInfo{}
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// FetchCredentialQuota asks a matching plugin first, then a declarative read-only probe.
func (h *Handler) FetchCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request_body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index_required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_not_found"})
		return
	}

	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}
	pluginID, okPluginID := canonicalQuotaPluginID(body.PluginID)
	if !okPluginID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_plugin_id"})
		return
	}

	if host := h.quotaPluginHost(); host != nil {
		req := quotaFetchRequest(auth, provider)
		var (
			response pluginapi.QuotaFetchResponse
			handled  bool
			errFetch error
		)
		if pluginID != "" {
			response, handled, errFetch = host.FetchQuotaByPlugin(c.Request.Context(), pluginID, req)
		} else {
			response, handled, errFetch = host.FetchQuota(c.Request.Context(), req)
		}
		if handled {
			if errFetch != nil {
				log.WithError(errFetch).WithFields(log.Fields{
					"auth_index": auth.Index,
					"plugin_id":  pluginID,
					"provider":   provider,
				}).Warn("management quota plugin fetch failed")
				c.JSON(http.StatusBadGateway, gin.H{"error": "quota_fetch_failed"})
				return
			}
			if errValidate := validateQuotaFetchResponse(response); errValidate != nil {
				log.WithError(errValidate).WithFields(log.Fields{
					"auth_index": auth.Index,
					"plugin_id":  pluginID,
					"provider":   provider,
				}).Warn("management quota plugin returned invalid response")
				c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_quota_response"})
				return
			}
			c.JSON(http.StatusOK, response)
			return
		}
	}

	probe, okProbe := quotaProbeFromAuth(auth)
	if !okProbe {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "quota_provider_unavailable"})
		return
	}
	response, handled, errProbe := h.executeQuotaProbe(c.Request.Context(), auth, probe)
	if !handled {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "quota_provider_unavailable"})
		return
	}
	if errProbe != nil {
		log.WithError(errProbe).WithFields(log.Fields{
			"auth_index": auth.Index,
			"provider":   provider,
		}).Warn("management declarative quota probe failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota_probe_failed"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// ResetCredentialQuota invokes an explicitly reset-capable plugin and then clears local routing quota state.
func (h *Handler) ResetCredentialQuota(c *gin.Context) {
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request_body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index_required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_not_found"})
		return
	}
	pluginID, okPluginID := canonicalQuotaPluginID(body.PluginID)
	if !okPluginID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_plugin_id"})
		return
	}
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = auth.Provider
	}
	h.resetCredentialQuota(c, auth, pluginID, provider)
}

// GetPluginQuota handles GET /plugins/:id/quota?auth_index=...
func (h *Handler) GetPluginQuota(c *gin.Context) {
	pluginID, okPluginID := pluginIDFromRequest(c)
	if !okPluginID {
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index_required"})
		return
	}
	h.fetchQuotaForPlugin(c, pluginID, authIndex)
}

// FetchPluginQuota handles POST /plugins/:id/quota.
func (h *Handler) FetchPluginQuota(c *gin.Context) {
	pluginID, okPluginID := pluginIDFromRequest(c)
	if !okPluginID {
		return
	}
	var body credentialQuotaRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request_body"})
		return
	}
	authIndex := body.resolveAuthIndex()
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index_required"})
		return
	}
	h.fetchQuotaForPlugin(c, pluginID, authIndex)
}

func (h *Handler) fetchQuotaForPlugin(c *gin.Context, pluginID, authIndex string) {
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_not_found"})
		return
	}
	host := h.quotaPluginHost()
	if host == nil || !host.HasQuotaProviderForPlugin(pluginID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota_provider_not_found"})
		return
	}
	response, handled, errFetch := host.FetchQuotaByPlugin(
		c.Request.Context(), pluginID, quotaFetchRequest(auth, auth.Provider),
	)
	if !handled {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota_provider_not_found"})
		return
	}
	if errFetch != nil {
		log.WithError(errFetch).WithFields(log.Fields{
			"auth_index": auth.Index,
			"plugin_id":  pluginID,
			"provider":   auth.Provider,
		}).Warn("management plugin quota fetch failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota_fetch_failed"})
		return
	}
	if errValidate := validateQuotaFetchResponse(response); errValidate != nil {
		log.WithError(errValidate).WithFields(log.Fields{
			"auth_index": auth.Index,
			"plugin_id":  pluginID,
			"provider":   auth.Provider,
		}).Warn("management plugin returned invalid quota response")
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_quota_response"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// ResetPluginQuota handles DELETE /plugins/:id/quota and POST /plugins/:id/quota/reset.
func (h *Handler) ResetPluginQuota(c *gin.Context) {
	pluginID, okPluginID := pluginIDFromRequest(c)
	if !okPluginID {
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		var body credentialQuotaRequest
		if errBind := c.ShouldBindJSON(&body); errBind == nil {
			authIndex = body.resolveAuthIndex()
		}
	}
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index_required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth_not_found"})
		return
	}
	h.resetCredentialQuota(c, auth, pluginID, auth.Provider)
}

func (h *Handler) resetCredentialQuota(c *gin.Context, auth *coreauth.Auth, pluginID, provider string) {
	if auth != nil {
		setManagementMutationTarget(c, "auth_index", auth.Index)
	}
	host := h.quotaPluginHost()
	if host == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "quota_provider_unavailable"})
		return
	}
	if pluginID != "" {
		if !host.HasQuotaProviderForPlugin(pluginID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "quota_provider_not_found"})
			return
		}
	} else if !host.HasQuotaProviderContext(c.Request.Context(), provider) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "quota_provider_unavailable"})
		return
	}
	if !quotaResetSupported(c.Request.Context(), host, pluginID, provider) {
		log.WithFields(log.Fields{
			"auth_index": auth.Index,
			"plugin_id":  pluginID,
			"provider":   provider,
		}).Warn("management quota reset rejected because provider did not declare reset support")
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "quota_reset_not_supported"})
		return
	}

	req := pluginapi.QuotaResetRequest{
		AuthIndex:   auth.Index,
		AuthID:      auth.ID,
		Provider:    provider,
		StorageJSON: quotaStorageJSONFromAuth(auth),
		Metadata:    auth.Metadata,
		Attributes:  auth.Attributes,
	}
	var (
		response pluginapi.QuotaResetResponse
		handled  bool
		errReset error
	)
	if pluginID != "" {
		response, handled, errReset = host.ResetQuotaByPlugin(c.Request.Context(), pluginID, req)
	} else {
		response, handled, errReset = host.ResetQuota(c.Request.Context(), req)
	}
	fields := log.Fields{
		"auth_index": auth.Index,
		"plugin_id":  pluginID,
		"provider":   provider,
	}
	if !handled {
		log.WithFields(fields).Warn("management quota reset was not handled")
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota_reset_not_handled"})
		return
	}
	if errReset != nil {
		log.WithError(errReset).WithFields(fields).Warn("management quota reset failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota_reset_failed"})
		return
	}
	if !response.Success {
		log.WithFields(fields).Warn("management quota reset was rejected by provider")
		c.JSON(http.StatusBadGateway, gin.H{"error": "quota_reset_rejected"})
		return
	}

	if h.authManager != nil {
		updated, _, errCore := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
		if errCore != nil {
			log.WithError(errCore).WithFields(fields).Error("management quota reset succeeded upstream but local routing reset failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "routing_quota_reset_failed"})
			return
		}
		if updated != nil {
			updated.EnsureIndex()
		}
	}
	log.WithFields(fields).Info("management quota reset completed")
	payload := gin.H{"status": "ok", "auth_index": auth.Index}
	if message := strings.TrimSpace(response.Message); message != "" {
		payload["message"] = message
	}
	c.JSON(http.StatusOK, payload)
}

func canonicalQuotaPluginID(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "", true
	}
	pluginID := pluginstore.CanonicalPluginID(raw)
	return pluginID, pluginhost.ValidatePluginID(pluginID)
}

func quotaResetSupported(ctx context.Context, host quotaPluginHost, pluginID, provider string) bool {
	if host == nil {
		return false
	}
	if pluginID != "" {
		description, handled, errDescribe := host.DescribeQuota(ctx, pluginID)
		return errDescribe == nil && handled && description.SupportsReset
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, info := range host.QuotaProviders(ctx) {
		if strings.EqualFold(info.Provider, provider) {
			return info.SupportsReset
		}
		for _, supported := range info.SupportedProviders {
			if strings.EqualFold(supported, provider) {
				return info.SupportsReset
			}
		}
	}
	return false
}

func quotaFetchRequest(auth *coreauth.Auth, provider string) pluginapi.QuotaFetchRequest {
	return pluginapi.QuotaFetchRequest{
		AuthIndex:   auth.Index,
		AuthID:      auth.ID,
		Provider:    provider,
		StorageJSON: quotaStorageJSONFromAuth(auth),
		Metadata:    auth.Metadata,
		Attributes:  auth.Attributes,
	}
}

func quotaStorageJSONFromAuth(auth *coreauth.Auth) []byte {
	if auth == nil {
		return nil
	}
	if rawProvider, okRaw := auth.Storage.(interface{ RawJSON() []byte }); okRaw {
		return bytes.Clone(rawProvider.RawJSON())
	}
	if len(auth.Metadata) == 0 {
		return nil
	}
	data, errMarshal := json.Marshal(auth.Metadata)
	if errMarshal != nil {
		return nil
	}
	return data
}

func quotaProbeFromAuth(auth *coreauth.Auth) (map[string]any, bool) {
	if auth == nil || auth.Metadata == nil {
		return nil, false
	}
	raw := auth.Metadata[pluginapi.QuotaProbeMetadataKey]
	if raw == nil {
		return nil, false
	}
	if probe, okProbe := raw.(map[string]any); okProbe {
		return probe, true
	}
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil, false
	}
	var probe map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &probe); errUnmarshal != nil || probe == nil {
		return nil, false
	}
	return probe, true
}

type quotaProbeDoFunc func(*http.Request) (*http.Response, error)

func (h *Handler) executeQuotaProbe(
	ctx context.Context,
	auth *coreauth.Auth,
	probe map[string]any,
) (pluginapi.QuotaFetchResponse, bool, error) {
	return h.executeQuotaProbeWithHTTPDo(ctx, auth, probe, nil)
}

func (h *Handler) executeQuotaProbeWithHTTPDo(
	ctx context.Context,
	auth *coreauth.Auth,
	probe map[string]any,
	do quotaProbeDoFunc,
) (pluginapi.QuotaFetchResponse, bool, error) {
	urlString := strings.TrimSpace(stringFromAny(probe["url"]))
	if urlString == "" {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	placeholder := "$" + pluginapi.QuotaProbeTokenPlaceholder + "$"
	if strings.Contains(urlString, placeholder) {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("token placeholder is not allowed in quota probe URL")
	}
	parsedURL, errParseURL := url.Parse(urlString)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("invalid quota probe URL")
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe only supports HTTP and HTTPS")
	}
	if _, private := resolveAndCheckHost(parsedURL.Hostname()); private {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe target is private or internal")
	}

	method := strings.ToUpper(strings.TrimSpace(stringFromAny(probe["method"])))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe method must be GET or POST")
	}
	headers, errHeaders := quotaProbeHeaders(probe)
	if errHeaders != nil {
		return pluginapi.QuotaFetchResponse{}, true, errHeaders
	}
	if errValidateHeaders := validateAPICallHeaders(headers); errValidateHeaders != nil {
		return pluginapi.QuotaFetchResponse{}, true, errValidateHeaders
	}
	data, errData := quotaProbeData(probe["data"])
	if errData != nil {
		return pluginapi.QuotaFetchResponse{}, true, errData
	}
	headerHasToken := false
	for _, value := range headers {
		if strings.Contains(value, placeholder) {
			headerHasToken = true
			break
		}
	}
	bodyHasToken := strings.Contains(data, placeholder)
	hasToken := headerHasToken || bodyHasToken
	if hasToken {
		if errTransport := validateTokenTransportURL(parsedURL); errTransport != nil {
			return pluginapi.QuotaFetchResponse{}, true, errTransport
		}
		if !isAllowedTokenTargetURL(parsedURL) {
			return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe token target is not allowed")
		}
	}

	cfg := h.configSnapshot()
	token, replacedData, errToken := substituteAPICallToken(headers, data, func() (string, error) {
		return h.resolveTokenForAuth(ctx, cfg, auth)
	})
	if errToken != nil {
		return pluginapi.QuotaFetchResponse{}, true, errToken
	}
	var body io.Reader
	if replacedData != "" {
		body = strings.NewReader(replacedData)
	}
	request, errRequest := http.NewRequestWithContext(ctx, method, urlString, body)
	if errRequest != nil {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("build quota probe request: %w", errRequest)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	if do == nil {
		client := &http.Client{Timeout: defaultAPICallTimeout}
		client.Transport = publicHostTransport(h.apiCallTransport(cfg, auth, ""))
		client.CheckRedirect = func(redirectRequest *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if isPrivateHost(redirectRequest.URL.Hostname()) {
				return fmt.Errorf("redirect to private address blocked")
			}
			if !hasToken {
				return nil
			}
			if errTransport := validateTokenTransportURL(redirectRequest.URL); errTransport != nil {
				return errTransport
			}
			if bodyHasToken && !strings.EqualFold(parsedURL.Hostname(), redirectRequest.URL.Hostname()) {
				return fmt.Errorf("cross-host redirect blocked for token-bearing request body")
			}
			if !isAllowedTokenRedirectTarget(parsedURL, redirectRequest.URL) {
				return fmt.Errorf("redirect to untrusted domain blocked")
			}
			if shouldStripTokenRedirectHeaders(parsedURL.Hostname(), redirectRequest.URL.Hostname()) {
				for name := range redirectRequest.Header {
					if token != "" && strings.Contains(redirectRequest.Header.Get(name), token) {
						redirectRequest.Header.Del(name)
					}
				}
			}
			return nil
		}
		do = client.Do
	}

	observedAt := time.Now().UTC()
	response, errDo := do(request)
	if errDo != nil {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe request failed: %w", errDo)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("close quota probe response body")
		}
	}()
	responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, maxQuotaProbeResponseBytes+1))
	if errRead != nil {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("read quota probe response: %w", errRead)
	}
	if len(responseBody) > maxQuotaProbeResponseBytes {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe response exceeds size limit")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe returned status %d", response.StatusCode)
	}
	if !json.Valid(responseBody) {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe response is not valid JSON")
	}

	rawMapping, hasMapping := probe["mapping"]
	mapping, okMapping := quotaProbeMapping(rawMapping)
	if hasMapping && rawMapping != nil && !okMapping {
		return pluginapi.QuotaFetchResponse{}, true, fmt.Errorf("quota probe mapping must be an object")
	}
	var normalized pluginapi.QuotaFetchResponse
	var errNormalize error
	if okMapping {
		normalized, errNormalize = mapQuotaProbeResponse(responseBody, mapping)
	} else {
		normalized, errNormalize = normalizeQuotaProbeResponse(responseBody)
	}
	if errNormalize != nil {
		return pluginapi.QuotaFetchResponse{}, true, errNormalize
	}
	if normalized.ServerTimeOffsetMs == 0 {
		if offset, okOffset := quotaProbeServerTimeOffsetMilliseconds(response.Header, observedAt); okOffset {
			normalized.ServerTimeOffsetMs = offset
		}
	}
	return normalized, true, nil
}

func quotaProbeServerTimeOffsetMilliseconds(header http.Header, observedAt time.Time) (int64, bool) {
	for name, values := range header {
		if !strings.EqualFold(strings.TrimSpace(name), "Date") {
			continue
		}
		for _, value := range values {
			serverTime, errParse := http.ParseTime(strings.TrimSpace(value))
			if errParse == nil {
				return serverTime.Sub(observedAt).Milliseconds(), true
			}
		}
	}
	return 0, false
}

func quotaProbeMapping(raw any) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	if mapping, okMapping := raw.(map[string]any); okMapping {
		return mapping, true
	}
	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil, false
	}
	var mapping map[string]any
	if errUnmarshal := json.Unmarshal(encoded, &mapping); errUnmarshal != nil || mapping == nil {
		return nil, false
	}
	return mapping, true
}

func quotaProbeHeaders(probe map[string]any) (map[string]string, error) {
	raw := probe["headers"]
	if raw == nil {
		raw = probe["header"]
	}
	if raw == nil {
		return map[string]string{}, nil
	}
	switch values := raw.(type) {
	case map[string]string:
		out := make(map[string]string, len(values))
		for name, value := range values {
			out[name] = value
		}
		return out, nil
	case map[string]any:
		out := make(map[string]string, len(values))
		for name, value := range values {
			text, okText := value.(string)
			if !okText {
				return nil, fmt.Errorf("quota probe header %q must be a string", name)
			}
			out[name] = text
		}
		return out, nil
	default:
		return nil, fmt.Errorf("quota probe headers must be an object")
	}
}

func quotaProbeData(raw any) (string, error) {
	switch value := raw.(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	default:
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return "", fmt.Errorf("encode quota probe body: %w", errMarshal)
		}
		return string(encoded), nil
	}
}

func stringFromAny(value any) string {
	text, _ := value.(string)
	return text
}

func normalizeQuotaProbeResponse(data []byte) (pluginapi.QuotaFetchResponse, error) {
	if !json.Valid(data) {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("normalized quota response is not valid JSON")
	}
	response := pluginapi.QuotaFetchResponse{}
	subscription := gjson.GetBytes(data, "subscription")
	if subscription.IsObject() {
		item := &pluginapi.QuotaSubscription{
			Plan:     subscription.Get("plan").String(),
			TierName: firstGJSONValue(subscription, "tierName", "tier_name"),
			TierID:   firstGJSONValue(subscription, "tierId", "tier_id"),
		}
		if item.Plan != "" || item.TierName != "" || item.TierID != "" {
			response.Subscription = item
		}
	}
	serverOffset := gjson.GetBytes(data, "serverTimeOffsetMs")
	if !serverOffset.Exists() {
		serverOffset = gjson.GetBytes(data, "server_time_offset_ms")
	}
	if serverOffset.Type == gjson.Number {
		response.ServerTimeOffsetMs = serverOffset.Int()
	}
	groups := gjson.GetBytes(data, "groups")
	if groups.IsArray() {
		for _, rawGroup := range groups.Array() {
			rawBuckets := rawGroup.Get("buckets")
			if !rawBuckets.IsArray() {
				continue
			}
			group := pluginapi.QuotaGroup{DisplayName: firstGJSONValue(rawGroup, "displayName", "display_name")}
			for _, rawBucket := range rawBuckets.Array() {
				remaining := rawBucket.Get("remainingFraction")
				if !remaining.Exists() {
					remaining = rawBucket.Get("remaining_fraction")
				}
				fraction, okFraction := parseQuotaFraction(remaining)
				if !okFraction {
					continue
				}
				group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
					Window:            rawBucket.Get("window").String(),
					RemainingFraction: fraction,
					ResetTime:         firstGJSONValue(rawBucket, "resetTime", "reset_time"),
					Description:       rawBucket.Get("description").String(),
				})
			}
			if len(group.Buckets) > 0 {
				response.Groups = append(response.Groups, group)
			}
		}
	}
	if errValidate := validateQuotaFetchResponse(response); errValidate != nil {
		return pluginapi.QuotaFetchResponse{}, errValidate
	}
	return response, nil
}

func firstGJSONValue(parent gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := parent.Get(path); value.Exists() {
			return value.String()
		}
	}
	return ""
}

func parseQuotaFraction(result gjson.Result) (float64, bool) {
	if !result.Exists() {
		return 0, false
	}
	var value float64
	switch result.Type {
	case gjson.Number:
		value = result.Float()
	case gjson.String:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(result.String()), 64)
		if errParse != nil {
			return 0, false
		}
		value = parsed
	default:
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0, false
	}
	return value, true
}

func validateQuotaFetchResponse(response pluginapi.QuotaFetchResponse) error {
	hasSubscription := response.Subscription != nil && (strings.TrimSpace(response.Subscription.Plan) != "" ||
		strings.TrimSpace(response.Subscription.TierName) != "" ||
		strings.TrimSpace(response.Subscription.TierID) != "")
	hasBucket := false
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			if math.IsNaN(bucket.RemainingFraction) || math.IsInf(bucket.RemainingFraction, 0) ||
				bucket.RemainingFraction < 0 || bucket.RemainingFraction > 1 {
				return fmt.Errorf("quota remaining fraction is outside [0,1]")
			}
			hasBucket = true
		}
	}
	if !hasSubscription && !hasBucket {
		return fmt.Errorf("quota response contains no subscription or numeric bucket")
	}
	return nil
}

func mapQuotaProbeResponse(data []byte, mapping map[string]any) (pluginapi.QuotaFetchResponse, error) {
	response := pluginapi.QuotaFetchResponse{}
	setSubscriptionField := func(path string, assign func(*pluginapi.QuotaSubscription, string)) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		value := gjson.GetBytes(data, path)
		if !value.Exists() || strings.TrimSpace(value.String()) == "" {
			return
		}
		if response.Subscription == nil {
			response.Subscription = &pluginapi.QuotaSubscription{}
		}
		assign(response.Subscription, value.String())
	}
	setSubscriptionField(mappingString(mapping, "plan"), func(item *pluginapi.QuotaSubscription, value string) {
		item.Plan = value
	})
	setSubscriptionField(firstMappingString(mapping, "tier_name", "tierName"), func(item *pluginapi.QuotaSubscription, value string) {
		item.TierName = value
	})
	setSubscriptionField(firstMappingString(mapping, "tier_id", "tierId"), func(item *pluginapi.QuotaSubscription, value string) {
		item.TierID = value
	})

	rawGroups, _ := mapping["groups"].([]any)
	for _, rawGroup := range rawGroups {
		groupMapping, okGroup := rawGroup.(map[string]any)
		if !okGroup {
			continue
		}
		group := pluginapi.QuotaGroup{}
		group.DisplayName = mappedStringOrLiteral(
			data,
			firstMappingString(groupMapping, "display_name", "displayName"),
		)
		if bucketsPath := mappingString(groupMapping, "buckets_path"); bucketsPath != "" {
			for _, item := range gjson.GetBytes(data, bucketsPath).Array() {
				bucket, okBucket := quotaBucketFromMappedItem(item, groupMapping)
				if okBucket {
					group.Buckets = append(group.Buckets, bucket)
				}
			}
		}
		if rawBuckets, okBuckets := groupMapping["buckets"].([]any); okBuckets {
			for _, rawBucket := range rawBuckets {
				bucketMapping, okBucketMapping := rawBucket.(map[string]any)
				if !okBucketMapping {
					continue
				}
				bucket, okBucket := quotaBucketFromResponse(data, bucketMapping)
				if okBucket {
					group.Buckets = append(group.Buckets, bucket)
				}
			}
		}
		if len(group.Buckets) > 0 {
			response.Groups = append(response.Groups, group)
		}
	}
	if errValidate := validateQuotaFetchResponse(response); errValidate != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota probe mapping did not produce a usable response: %w", errValidate)
	}
	return response, nil
}

func quotaBucketFromMappedItem(item gjson.Result, mapping map[string]any) (pluginapi.QuotaBucket, bool) {
	fractionKey := mappingString(mapping, "remaining_fraction_key")
	if fractionKey == "" {
		fractionKey = "remaining_fraction"
	}
	fraction, okFraction := parseQuotaFraction(item.Get(fractionKey))
	if !okFraction {
		remainingKey := mappingString(mapping, "remaining_amount_key")
		totalKey := mappingString(mapping, "total_amount_key")
		remaining, okRemaining := parseNonNegativeNumber(item.Get(remainingKey))
		total, okTotal := parseNonNegativeNumber(item.Get(totalKey))
		if !okRemaining || !okTotal || total <= 0 || remaining > total {
			return pluginapi.QuotaBucket{}, false
		}
		fraction = remaining / total
	}
	windowKey := mappingString(mapping, "window_key")
	if windowKey == "" {
		windowKey = "window"
	}
	resetKey := mappingString(mapping, "reset_time_key")
	if resetKey == "" {
		resetKey = "reset_time"
	}
	descriptionKey := mappingString(mapping, "description_key")
	if descriptionKey == "" {
		descriptionKey = "description"
	}
	return pluginapi.QuotaBucket{
		Window:            item.Get(windowKey).String(),
		RemainingFraction: fraction,
		ResetTime:         item.Get(resetKey).String(),
		Description:       item.Get(descriptionKey).String(),
	}, true
}

func quotaBucketFromResponse(data []byte, mapping map[string]any) (pluginapi.QuotaBucket, bool) {
	fractionPath := mappingString(mapping, "remaining_fraction")
	fraction, okFraction := parseQuotaFraction(gjson.GetBytes(data, fractionPath))
	if !okFraction {
		remainingPath := mappingString(mapping, "remaining_amount")
		totalPath := mappingString(mapping, "total_amount")
		remaining, okRemaining := parseNonNegativeNumber(gjson.GetBytes(data, remainingPath))
		total, okTotal := parseNonNegativeNumber(gjson.GetBytes(data, totalPath))
		if !okRemaining || !okTotal || total <= 0 || remaining > total {
			return pluginapi.QuotaBucket{}, false
		}
		fraction = remaining / total
	}
	return pluginapi.QuotaBucket{
		Window:            mappedStringOrLiteral(data, mappingString(mapping, "window")),
		RemainingFraction: fraction,
		ResetTime:         mappedStringOrLiteral(data, mappingString(mapping, "reset_time")),
		Description:       mappedStringOrLiteral(data, mappingString(mapping, "description")),
	}, true
}

func parseNonNegativeNumber(result gjson.Result) (float64, bool) {
	if !result.Exists() {
		return 0, false
	}
	var value float64
	switch result.Type {
	case gjson.Number:
		value = result.Float()
	case gjson.String:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(result.String()), 64)
		if errParse != nil {
			return 0, false
		}
		value = parsed
	default:
		return 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}

func mappingString(mapping map[string]any, key string) string {
	value, _ := mapping[key].(string)
	return strings.TrimSpace(value)
}

func firstMappingString(mapping map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := mappingString(mapping, key); value != "" {
			return value
		}
	}
	return ""
}

func mappedStringOrLiteral(data []byte, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if value := gjson.GetBytes(data, path); value.Exists() {
		return value.String()
	}
	return path
}
