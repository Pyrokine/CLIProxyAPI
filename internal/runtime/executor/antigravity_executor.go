// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Antigravity executor that proxies requests to the antigravity
// upstream using OAuth credentials.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	antigravityclaude "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityBaseURLDaily               = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySandboxBaseURLDaily        = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	antigravityCountTokensPath            = "/v1internal:countTokens"
	antigravityStreamPath                 = "/v1internal:streamGenerateContent"
	antigravityGeneratePath               = "/v1internal:generateContent"
	antigravityModelsPath                 = "/v1internal:fetchAvailableModels"
	antigravityLoadCodeAssistPath         = "/v1internal:loadCodeAssist"
	antigravityAuthType                   = "antigravity"
	antigravityLoadCodeAssistAPIClient    = "gl-node/22.21.1"
	antigravityLoadCodeAssistNodeClient   = "google-api-nodejs-client/10.3.0"
	antigravityCreditsHintRefreshInterval = 10 * time.Minute
	antigravityCreditsHintRefreshTimeout  = 5 * time.Second
	refreshSkew                           = 3000 * time.Second
)

type modelDiscoveryOutcome string

const (
	ModelDiscoveryComplete     modelDiscoveryOutcome = "complete"
	ModelDiscoveryInconclusive modelDiscoveryOutcome = "inconclusive"
	ModelDiscoveryFailed       modelDiscoveryOutcome = "failed"
)

type ModelDiscoveryResult struct {
	Outcome modelDiscoveryOutcome
	Models  []*registry.ModelInfo
	Auth    *cliproxyauth.Auth
	Err     error
}

var (
	randSource                        = rand.New(rand.NewSource(time.Now().UnixNano()))
	randSourceMutex                   sync.Mutex
	antigravityCreditsHintRefreshByID sync.Map
	antigravityRefreshGroup           singleflight.Group
)

type antigravityCreditsHintRefreshState struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

// AntigravityExecutor proxies requests to the antigravity upstream.
type AntigravityExecutor struct {
	cfg *config.Config
}

// NewAntigravityExecutor creates a new Antigravity executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *AntigravityExecutor: A new Antigravity executor instance
func NewAntigravityExecutor(cfg *config.Config) *AntigravityExecutor {
	return &AntigravityExecutor{cfg: cfg}
}

func (e *AntigravityExecutor) obfuscateSensitiveWords(payload []byte) []byte {
	if e == nil || e.cfg == nil || len(e.cfg.Antigravity.SensitiveWords) == 0 {
		return payload
	}
	return obfuscateAntigravitySystemInstruction(
		payload,
		buildSensitiveWordMatcher(e.cfg.Antigravity.SensitiveWords),
	)
}

// translateAntigravityRequestPair reuses one context-aware translation only when
// both inputs share the same backing slice and no plugin hook can observe each call.
func (e *AntigravityExecutor) translateAntigravityRequestPair(
	ctx context.Context,
	req cliproxyexecutor.Request,
	headers http.Header,
	from, to sdktranslator.Format,
	model string,
	originalPayload, payload []byte,
	stream bool,
) ([]byte, []byte) {
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx, headers, e.cfg, from, to, model, originalPayload, stream, isCompat,
	)
	if sameByteSlice(originalPayload, payload) && !sdktranslator.HasPluginHooks(ctx) {
		return originalTranslated, bytes.Clone(originalTranslated)
	}
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx, headers, e.cfg, from, to, model, payload, stream, isCompat,
	)
	return originalTranslated, bytes.Clone(translated)
}

func sameByteSlice(first, second []byte) bool {
	if len(first) != len(second) {
		return false
	}
	if len(first) == 0 {
		return true
	}
	return &first[0] == &second[0]
}

// Each Antigravity credential gets its own bounded HTTP/1.1 transport cache.
var (
	antigravityBaseTransport = defaultAntigravityBaseTransport()
	antigravityTransports    = helps.NewTransportCache[antigravityTransportKey](antigravityTransportCacheCapacity)
)

const (
	antigravityTransportCacheCapacity     = 8192
	antigravityAnonymousTransportScope    = "anonymous"
	antigravityDefaultIdleConnTimeout     = 30 * time.Second
	antigravityMaxAllowedIdleConnTimeout  = 210 * time.Second
	antigravityDefaultMaxIdleConnsPerHost = 2
	antigravityMaxAllowedIdleConnsPerHost = 100
)

type antigravityPoolSettings struct {
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

func resolveAntigravityPoolSettings(cfg *config.Config) antigravityPoolSettings {
	shortMode := antigravityPoolSettings{
		shortMode:           true,
		maxIdleConnsPerHost: -1,
	}
	if cfg == nil || cfg.Antigravity.ConnectionPool.Enabled == nil ||
		!*cfg.Antigravity.ConnectionPool.Enabled {
		return shortMode
	}

	pool := cfg.Antigravity.ConnectionPool
	idleConnTimeout := antigravityDefaultIdleConnTimeout
	if rawTimeout := strings.TrimSpace(pool.IdleConnTimeout); rawTimeout != "" {
		parsedTimeout, errParse := time.ParseDuration(rawTimeout)
		if errParse != nil {
			log.Warnf(
				"antigravity executor: invalid connection pool idle timeout %q; using %s: %v",
				rawTimeout,
				antigravityDefaultIdleConnTimeout,
				errParse,
			)
		} else {
			idleConnTimeout = parsedTimeout
		}
	}

	maxIdleConnsPerHost := antigravityDefaultMaxIdleConnsPerHost
	if pool.MaxIdleConnsPerHost != nil {
		maxIdleConnsPerHost = *pool.MaxIdleConnsPerHost
	}
	if idleConnTimeout <= 0 || maxIdleConnsPerHost < 0 {
		return shortMode
	}
	if idleConnTimeout > antigravityMaxAllowedIdleConnTimeout {
		idleConnTimeout = antigravityMaxAllowedIdleConnTimeout
	}
	if maxIdleConnsPerHost > antigravityMaxAllowedIdleConnsPerHost {
		maxIdleConnsPerHost = antigravityMaxAllowedIdleConnsPerHost
	}
	return antigravityPoolSettings{
		idleConnTimeout:     idleConnTimeout,
		maxIdleConnsPerHost: maxIdleConnsPerHost,
	}
}

type antigravityTransportKey struct {
	credential          string
	proxy               string
	base                *http.Transport
	shortMode           bool
	idleConnTimeout     time.Duration
	maxIdleConnsPerHost int
}

// ResetAntigravityTransports closes all cached idle connections.
func ResetAntigravityTransports() {
	antigravityTransports.Purge()
}

// AntigravityTransportsLen reports the current cache size.
func AntigravityTransportsLen() int {
	return antigravityTransports.Len()
}

func CloseAntigravityAuthIdleTransports(auth *cliproxyauth.Auth) {
	if auth == nil {
		return
	}
	scope := antigravityTransportScope(auth)
	if scope == "" || scope == antigravityAnonymousTransportScope {
		return
	}
	antigravityTransports.CloseMatching(func(key antigravityTransportKey) bool {
		return key.credential == scope
	})
}

func defaultAntigravityBaseTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport
	}
	return &http.Transport{}
}

func cloneTransportWithHTTP11(base *http.Transport, cfgs ...*config.Config) *http.Transport {
	if base == nil {
		return nil
	}
	clone := base.Clone()
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	// Native Antigravity sends no ALPN extension and therefore uses HTTP/1.1.
	clone.TLSClientConfig.NextProtos = nil
	applyAntigravityPoolLimits(clone, cfgs...)
	return clone
}

func applyAntigravityPoolLimits(transport *http.Transport, cfgs ...*config.Config) {
	if transport == nil {
		return
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	if settings.shortMode {
		transport.MaxIdleConnsPerHost = -1
		transport.DisableKeepAlives = false
		transport.IdleConnTimeout = 0
		return
	}

	// A negative operator-provided limit explicitly disables idle pooling.
	if transport.MaxIdleConnsPerHost < 0 {
		return
	}
	if transport.MaxIdleConnsPerHost < settings.maxIdleConnsPerHost {
		transport.MaxIdleConnsPerHost = settings.maxIdleConnsPerHost
	}
	if transport.MaxIdleConns > 0 && transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	}

	rawTimeout := ""
	if cfg != nil {
		rawTimeout = strings.TrimSpace(cfg.Antigravity.ConnectionPool.IdleConnTimeout)
	}
	switch {
	case rawTimeout != "":
		transport.IdleConnTimeout = settings.idleConnTimeout
	case transport.IdleConnTimeout == 0 || transport.IdleConnTimeout == 90*time.Second:
		transport.IdleConnTimeout = settings.idleConnTimeout
	case transport.IdleConnTimeout > antigravityMaxAllowedIdleConnTimeout:
		transport.IdleConnTimeout = antigravityMaxAllowedIdleConnTimeout
	}
}

func antigravityHTTP11Transport(
	auth *cliproxyauth.Auth,
	base *http.Transport,
	cfgs ...*config.Config,
) *http.Transport {
	if base == nil {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		base:                base,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		log.Debugf("antigravity executor: cache HTTP/1.1 transport failed: %v", errGet)
		return cloneTransportWithHTTP11(base, cfgs...)
	}
	return transport
}

func antigravityProxiedHTTP11Transport(
	auth *cliproxyauth.Auth,
	proxyURL string,
	cfgs ...*config.Config,
) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	settings := resolveAntigravityPoolSettings(cfg)
	key := antigravityTransportKey{
		credential:          antigravityTransportScope(auth),
		proxy:               proxyURL,
		shortMode:           settings.shortMode,
		idleConnTimeout:     settings.idleConnTimeout,
		maxIdleConnsPerHost: settings.maxIdleConnsPerHost,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		base, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil {
			return nil, errBuild
		}
		if base == nil {
			return nil, fmt.Errorf("antigravity executor: proxy setting produced no transport")
		}
		return cloneTransportWithHTTP11(base, cfgs...), nil
	})
	if errGet != nil {
		return nil
	}
	return transport
}

func antigravityTransportScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return antigravityAnonymousTransportScope
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); path != "" {
			return "path:" + path
		}
		if source := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); source != "" {
			return "source:" + source
		}
	}
	if refresh := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")); refresh != "" {
		return antigravityCredentialScope("refresh:", refresh)
	}
	if access := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token")); access != "" {
		return antigravityCredentialScope("token:", access)
	}
	return antigravityAnonymousTransportScope
}

func antigravityCredentialScope(prefix, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(digest[:8])
}

func (e *AntigravityExecutor) compactionScope(
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	baseModel string,
) (helps.AntigravityCompactionScope, error) {
	owner := antigravityTransportScope(auth)
	if owner == "" || owner == antigravityAnonymousTransportScope {
		return helps.AntigravityCompactionScope{}, fmt.Errorf("compaction requires a selected credential owner")
	}
	sessionID := antigravityCompactionSessionID(req, opts)
	if sessionID == "" {
		return helps.AntigravityCompactionScope{}, fmt.Errorf("compaction requires a stable session identity")
	}
	var keyMaterial []byte
	if auth != nil {
		if refreshToken := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")); refreshToken != "" {
			keyMaterial = []byte(refreshToken)
		}
	}
	if len(keyMaterial) == 0 && auth != nil {
		if accessToken := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token")); accessToken != "" {
			keyMaterial = []byte(accessToken)
		}
	}
	if len(keyMaterial) == 0 && e != nil && e.cfg != nil {
		if secret := strings.TrimSpace(e.cfg.RemoteManagement.SecretKey); secret != "" {
			keyMaterial = []byte(secret)
		}
	}
	return helps.AntigravityCompactionScope{
		Provider:    antigravityAuthType,
		Model:       strings.TrimSpace(baseModel),
		Session:     sessionID,
		Owner:       owner,
		KeyMaterial: keyMaterial,
	}, nil
}

func antigravityCompactionSessionID(
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) string {
	keys := []struct {
		name   string
		prefix string
	}{
		{name: cliproxyexecutor.CanonicalSessionIDMetadataKey},
		{name: cliproxyexecutor.LCPAffinitySessionIDMetadataKey},
		{name: cliproxyexecutor.ExecutionSessionMetadataKey, prefix: "execution:"},
		{name: cliproxyexecutor.DerivedSessionIDMetadataKey, prefix: "derived:"},
	}
	for _, key := range keys {
		for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
			if value := metadataString(metadata, key.name); value != "" {
				return cliproxysession.BoundSessionIdentity(key.prefix + value)
			}
		}
	}

	for _, headerName := range []string{"Session-Id", "Session_id"} {
		if value := cliproxysession.NormalizeExplicitID(opts.Headers.Get(headerName)); value != "" {
			return cliproxysession.BoundSessionIdentity("codex:" + value)
		}
	}

	payload := opts.OriginalRequest
	if len(payload) == 0 {
		payload = req.Payload
	}
	metadata := make(map[string]any, len(req.Metadata)+len(opts.Metadata))
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	for key, value := range opts.Metadata {
		metadata[key] = value
	}
	if info, ok := cliproxysession.ExtractSessionInfo(opts.Headers, payload, metadata); ok {
		return cliproxysession.BoundSessionIdentity(info.SessionID)
	}
	return ""
}

func antigravityProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func newAntigravityHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	if proxyURL := antigravityProxyURL(cfg, auth); proxyURL != "" {
		if transport := antigravityProxiedHTTP11Transport(auth, proxyURL, cfg); transport != nil {
			return &http.Client{Transport: transport, Timeout: timeout}
		}
	}

	client := newProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	if client.Transport == nil {
		client.Transport = antigravityHTTP11Transport(auth, antigravityBaseTransport, cfg)
		return client
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return client
	}
	if transport == nil {
		transport = antigravityBaseTransport
	}
	client.Transport = antigravityHTTP11Transport(auth, transport, cfg)
	return client
}

func validateAntigravityRequestSignatures(
	modelName string,
	from sdktranslator.Format,
	rawJSON []byte,
) ([]byte, error) {
	if from.String() != "claude" {
		return rawJSON, nil
	}
	if antigravityUsesReasoningReplayCache(modelName) {
		return antigravityclaude.StripInvalidGeminiSignatureThinkingBlocks(rawJSON), nil
	}
	return antigravityclaude.StripEmptySignatureThinkingBlocks(rawJSON), nil
}

func hasAntigravityClaudeTypedWebSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "web_search_20250305", "web_search_20260209":
			return true
		}
	}
	return false
}

func hasAntigravityGoogleSearchTool(payload []byte) bool {
	tools := util.GetGJSONBytesNoCopy(payload, "request.tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if tool.Get("googleSearch").Exists() {
			return true
		}
	}
	return false
}

func shouldResolveAntigravityWebSearchGroundingURLs(
	from sdktranslator.Format,
	originalRequestRawJSON, requestRawJSON []byte,
) bool {
	return from.String() == "claude" &&
		hasAntigravityClaudeTypedWebSearchTool(originalRequestRawJSON) &&
		hasAntigravityGoogleSearchTool(requestRawJSON)
}

func (e *AntigravityExecutor) resolveWebSearchGroundingURLs(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	originalRequestRawJSON, requestRawJSON, responseRawJSON []byte,
) []byte {
	if !shouldResolveAntigravityWebSearchGroundingURLs(from, originalRequestRawJSON, requestRawJSON) {
		return responseRawJSON
	}
	return resolveAntigravityGroundingURLs(ctx, e.cfg, auth, responseRawJSON)
}

// Identifier returns the executor identifier.
func (e *AntigravityExecutor) Identifier() string { return antigravityAuthType }

// PrepareRequest injects Antigravity credentials into the outgoing HTTP request.
func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, errToken := e.ensureAccessToken(req.Context(), auth)
	if errToken != nil {
		return errToken
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	util.ApplyCredentialHeader(req, "Authorization", "Bearer "+token)
	return nil
}

// HttpRequest injects Antigravity credentials into the request and executes it.
// It uses a whitelist approach: all incoming headers are stripped and only
// the minimum set required by the Antigravity protocol is explicitly set.
func (e *AntigravityExecutor) HttpRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)

	// --- Whitelist: save only the headers we need from the original request ---
	contentType := httpReq.Header.Get("Content-Type")

	// Wipe ALL incoming headers
	for k := range httpReq.Header {
		delete(httpReq.Header, k)
	}

	// --- Set only the headers Antigravity actually sends ---
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// Content-Length is managed automatically by Go's http.Client from the Body
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))

	// Inject Authorization: Bearer <token>
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, antigravityRequestError(ctx, errDo)
	}
	if httpResp.StatusCode == http.StatusTooManyRequests {
		CloseAntigravityAuthIdleTransports(auth)
	}
	return httpResp, nil
}

func injectEnabledCreditTypes(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	if !gjson.ValidBytes(payload) {
		return nil
	}
	updated, err := sjson.SetRawBytes(payload, "enabledCreditTypes", []byte(`["GOOGLE_ONE_AI"]`))
	if err != nil {
		return nil
	}
	return updated
}

func antigravityCreditsRetryEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.QuotaExceeded.AntigravityCredits
}

func antigravityHasExplicitCreditsBalanceExhaustedReason(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return false
	}
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		reason := strings.TrimSpace(detail.Get("reason").String())
		if strings.EqualFold(reason, "INSUFFICIENT_G1_CREDITS_BALANCE") {
			return true
		}
	}
	return false
}

func markAntigravityCreditsUnavailable(auth *cliproxyauth.Auth) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	cliproxyauth.SetAntigravityCreditsHint(
		strings.TrimSpace(auth.ID), cliproxyauth.AntigravityCreditsHint{
			Known:           true,
			Available:       false,
			CreditAmount:    0,
			MinCreditAmount: 1,
			UpdatedAt:       time.Now(),
		},
	)
}

// antigravityRetryOutcome signals how the retry loop should proceed after an error.
type antigravityRetryOutcome int

const (
	antigravityRetryNextURL     antigravityRetryOutcome = iota // try next baseURL
	antigravityRetryNextAttempt                                // sleep then retry from the first baseURL
	antigravityRetryAbort                                      // stop and return the error
)

// sendWithRetry executes an HTTP request across multiple base URLs with retry support
// for rate limiting and no-capacity errors. On a 2xx response it returns the open
// *http.Response (body unread) for the caller to consume. On error, it handles all
// retry/fallback logic and returns a terminal error.
func (e *AntigravityExecutor) sendWithRetry(
	ctx context.Context,
	httpClient *http.Client,
	baseURLs []string,
	attempts int,
	baseModel string,
	auth *cliproxyauth.Auth,
	buildReq func(baseURL string) (*http.Request, error),
) (*http.Response, error) {
	for attempt := range attempts {
		resp, err := e.sendAcrossBaseURLs(ctx, httpClient, baseURLs, attempts, attempt, baseModel, auth, buildReq)
		if err == nil {
			return resp, nil
		}
		_, ok := errors.AsType[antigravityRetryError](err)
		if !ok {
			return nil, err
		}
		// antigravityRetryNextAttempt: delay was already applied by sendAcrossBaseURLs
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
}

// antigravityRetryError wraps a terminal error with a retry signal; it is only
// used internally by sendAcrossBaseURLs to tell sendWithRetry to continue.
type antigravityRetryError struct{ err error }

func (r antigravityRetryError) Error() string { return r.err.Error() }
func (r antigravityRetryError) Unwrap() error { return r.err }

// sendAcrossBaseURLs tries each base URL once and returns the first 2xx response.
// If all base URLs fail, it returns either a terminal error or an
// antigravityRetryError signaling that the outer loop should try again.
func (e *AntigravityExecutor) sendAcrossBaseURLs(
	ctx context.Context,
	httpClient *http.Client,
	baseURLs []string,
	attempts, attempt int,
	baseModel string,
	auth *cliproxyauth.Auth,
	buildReq func(baseURL string) (*http.Request, error),
) (*http.Response, error) {
	for idx, baseURL := range baseURLs {
		hasNextURL := idx+1 < len(baseURLs)

		httpReq, errReq := buildReq(baseURL)
		if errReq != nil {
			return nil, errReq
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			errDo = antigravityRequestError(ctx, errDo)
			recordAPIResponseError(ctx, e.cfg, errDo)
			return nil, errDo
		}

		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			return httpResp, nil
		}

		// Error status: read body and apply retry logic
		bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			errRead = antigravityResponseReadError(ctx, errRead)
			recordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}

		appendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		if httpResp.StatusCode == http.StatusTooManyRequests {
			CloseAntigravityAuthIdleTransports(auth)
		}

		log.Debugf(
			"antigravity executor: upstream error status: %d, body: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes),
		)

		if httpResp.StatusCode == http.StatusTooManyRequests && cliproxyauth.AntigravityCreditsRequested(ctx) &&
			antigravityCreditsRetryEnabled(e.cfg) && antigravityHasExplicitCreditsBalanceExhaustedReason(bodyBytes) {
			markAntigravityCreditsUnavailable(auth)
		}

		outcome := e.classifyErrorForRetry(
			ctx, httpResp.StatusCode, bodyBytes, hasNextURL, attempts, attempt, baseModel, baseURL, baseURLs, idx,
		)
		if ctx != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch outcome {
		case antigravityRetryNextURL:
			continue
		case antigravityRetryNextAttempt:
			return nil, antigravityRetryError{err: buildStatusError(httpResp.StatusCode, bodyBytes, httpResp.Header)}
		case antigravityRetryAbort:
			// fall through to return error below
		}

		return nil, buildStatusError(httpResp.StatusCode, bodyBytes, httpResp.Header)
	}

	return nil, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity executor: no base url available"}
}

// classifyErrorForRetry decides how to handle an error status code in the retry loop.
func (e *AntigravityExecutor) classifyErrorForRetry(
	ctx context.Context,
	statusCode int,
	bodyBytes []byte,
	hasNextURL bool,
	attempts, attempt int,
	baseModel, baseURL string,
	baseURLs []string,
	idx int,
) antigravityRetryOutcome {
	if statusCode == http.StatusTooManyRequests && hasNextURL {
		log.Debugf(
			"antigravity executor: rate limited on base url %s, retrying with fallback base url: %s",
			baseURL, baseURLs[idx+1],
		)
		return antigravityRetryNextURL
	}

	if antigravityShouldRetryNoCapacity(statusCode, bodyBytes) {
		if hasNextURL {
			log.Debugf(
				"antigravity executor: no capacity on base url %s, retrying with fallback base url: %s",
				baseURL, baseURLs[idx+1],
			)
			return antigravityRetryNextURL
		}
		if attempt+1 < attempts {
			delay := antigravityNoCapacityRetryDelay(attempt)
			log.Debugf(
				"antigravity executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel,
				delay, attempt+1, attempts,
			)
			if errWait := antigravityWait(ctx, delay); errWait == nil {
				return antigravityRetryNextAttempt
			}
		}
	}

	return antigravityRetryAbort
}

type antigravityNoCapacityError struct {
	statusErr
}

func (e antigravityNoCapacityError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNextProvider,
		EffectScope:    cliproxyexecutor.StateEffectScopeProvider,
	}
}

// buildStatusError creates a statusErr, including retryAfter for 429 responses.
func buildStatusError(statusCode int, body []byte, headers ...http.Header) error {
	sErr := statusErr{code: statusCode, msg: string(body)}
	if len(headers) > 0 {
		sErr = sErr.withUpstreamResponse(headers[0])
	}
	if statusCode == http.StatusTooManyRequests {
		if retryAfter, parseErr := parseRetryDelay(body); parseErr == nil && retryAfter != nil {
			sErr.retryAfter = retryAfter
		}
	}
	if antigravityShouldRetryNoCapacity(statusCode, body) {
		return antigravityNoCapacityError{statusErr: sErr}
	}
	return sErr
}

func antigravityRequestError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func antigravityResponseReadError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	return markResponseStarted(err)
}

func sendAntigravityStreamReadError(
	ctx context.Context,
	out chan cliproxyexecutor.StreamChunk,
	err error,
) {
	chunk := cliproxyexecutor.StreamChunk{Err: err}
	if ctx == nil || ctx.Err() == nil {
		_ = sendStreamChunk(ctx, out, chunk)
		return
	}

	select {
	case out <- chunk:
		return
	default:
	}
	select {
	case <-out:
	default:
	}
	out <- chunk
}

func antigravityStreamPayloadTerminal(payload []byte) bool {
	for _, path := range []string{
		"response.candidates.0.finishReason",
		"candidates.0.finishReason",
	} {
		finishReason := gjson.GetBytes(payload, path)
		if finishReason.Exists() && strings.TrimSpace(finishReason.String()) != "" {
			return true
		}
	}
	return false
}

func antigravityIncompleteStreamError() error {
	return statusErr{code: http.StatusBadGateway, msg: "antigravity stream ended before terminal event"}
}

// Execute performs a non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	needsCompactionScope := opts.Alt == "responses/compact" ||
		helps.HasResponsesCompactionTrigger(req.Payload) ||
		helps.HasResponsesCompactionTrigger(opts.OriginalRequest) ||
		helps.HasResponsesCompactionItem(req.Payload) ||
		helps.HasResponsesCompactionItem(opts.OriginalRequest)
	var compactionScope helps.AntigravityCompactionScope
	if needsCompactionScope {
		var errScope error
		compactionScope, errScope = e.compactionScope(auth, req, opts, baseModel)
		if errScope != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errScope.Error()}
		}
	}

	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload, compactionScope)
		if errExpand != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
	}
	if helps.HasResponsesCompactionItem(opts.OriginalRequest) {
		expandedOriginal, errOriginal := helps.ExpandAntigravityCompactionCapsules(
			opts.OriginalRequest,
			compactionScope,
		)
		if errOriginal != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errOriginal.Error()}
		}
		opts.OriginalRequest = expandedOriginal
	}
	if opts.Alt == "responses/compact" || helps.HasResponsesCompactionTrigger(req.Payload) ||
		helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompaction(ctx, auth, req, opts, compactionScope)
	}
	isClaude := strings.Contains(strings.ToLower(baseModel), "claude")

	if isClaude || strings.Contains(baseModel, "gemini-3-pro") || strings.Contains(
		baseModel, "gemini-3.1-flash-image",
	) {
		return e.executeClaudeNonStream(ctx, auth, req, opts)
	}

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(baseModel, from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	originalTranslated, translated := e.translateAntigravityRequestPair(
		ctx, req, opts.Headers, from, to, baseModel, originalPayload, req.Payload, false,
	)

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(
		e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel,
	)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	reporter.setTranslatedReasoningEffort(translated, to.String())

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	attempts := antigravityRetryAttempts(auth, e.cfg)

	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)
	requestPayload := translated
	if useCredits {
		if creditPayload := injectEnabledCreditTypes(translated); len(creditPayload) > 0 {
			requestPayload = creditPayload
		}
	}
	requestPayload, replayScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(
		ctx, baseModel, req, opts, requestPayload,
	)
	if errReplay != nil {
		return resp, errReplay
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
	httpResp, err := e.sendWithRetry(
		ctx, httpClient, baseURLs, attempts, baseModel, auth, func(baseURL string) (*http.Request, error) {
			return e.buildRequest(
				ctx, auth, token, baseModel, requestPayload, false, opts.Alt, baseURL,
				helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata),
			)
		},
	)
	if err != nil {
		if errClear := clearAntigravityReasoningReplayOnError(ctx, replayScope, err); errClear != nil {
			return resp, errClear
		}
		return resp, err
	}

	bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		errRead = antigravityResponseReadError(ctx, errRead)
		recordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	appendAPIResponseChunk(ctx, e.cfg, bodyBytes)
	cacheAntigravityReasoningReplayFromResponse(ctx, replayScope, requestPayload, bodyBytes)
	bodyBytes = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, requestPayload, bodyBytes)

	reporter.publish(ctx, parseAntigravityUsage(bodyBytes))
	var param any
	converted := sdktranslator.TranslateNonStream(
		ctx, to, from, req.Model, opts.OriginalRequest, requestPayload, bodyBytes, &param,
	)
	resp = cliproxyexecutor.Response{Payload: converted, Headers: httpResp.Header.Clone()}
	reporter.ensurePublished(ctx)
	return resp, nil
}

func (e *AntigravityExecutor) executeCompaction(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	compactionScope helps.AntigravityCompactionScope,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)
	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return cliproxyexecutor.Response{}, errSummary
	}
	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, compactionScope)
	if errSeal != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usageDetail := parseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usageDetail.InputTokens)
		outputTokens = int(usageDetail.OutputTokens)
		totalTokens = int(usageDetail.TotalTokens)
	}

	return cliproxyexecutor.Response{
		Payload: helps.BuildAntigravityCompactionResponse(
			baseModel, capsule, inputTokens, outputTokens, totalTokens,
		),
		Headers: summaryResp.Headers,
	}, nil
}

// executeClaudeNonStream performs a claude non-streaming request to the Antigravity API.
func (e *AntigravityExecutor) executeClaudeNonStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(baseModel, from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	originalTranslated, translated := e.translateAntigravityRequestPair(
		ctx, req, opts.Headers, from, to, baseModel, originalPayload, req.Payload, true,
	)

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(
		e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel,
	)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	reporter.setTranslatedReasoningEffort(translated, to.String())

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	attempts := antigravityRetryAttempts(auth, e.cfg)
	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)
	requestPayload := translated
	if useCredits {
		if creditPayload := injectEnabledCreditTypes(translated); len(creditPayload) > 0 {
			requestPayload = creditPayload
		}
	}
	requestPayload, replayScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(
		ctx, baseModel, req, opts, requestPayload,
	)
	if errReplay != nil {
		return resp, errReplay
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)

	httpResp, err := e.sendWithRetry(
		ctx, httpClient, baseURLs, attempts, baseModel, auth, func(baseURL string) (*http.Request, error) {
			return e.buildRequest(
				ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL,
				helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata),
			)
		},
	)
	if err != nil {
		if errClear := clearAntigravityReasoningReplayOnError(ctx, replayScope, err); errClear != nil {
			return resp, errClear
		}
		return resp, err
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func(resp *http.Response) {
		defer close(out)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
		}()
		replayAccumulator := newAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		sawTerminalEvent := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			replayAccumulator.observeSSELine(line)

			// Filter usage metadata for all models
			// Only retain usage statistics in the terminal chunk
			line = filterSSEUsageMetadata(line)

			payload := jsonPayload(line)
			if payload == nil {
				continue
			}
			if antigravityStreamPayloadTerminal(payload) {
				sawTerminalEvent = true
			}

			if detail, ok := parseAntigravityStreamUsage(payload); ok {
				reporter.publish(ctx, detail)
			}

			if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: payload}) {
				return
			}
		}
		if errScan := antigravityResponseReadError(ctx, scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			sendAntigravityStreamReadError(ctx, out, errScan)
			return
		}
		if !sawTerminalEvent {
			errIncomplete := antigravityIncompleteStreamError()
			recordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.publishFailure(ctx)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errIncomplete})
			return
		}
		replayAccumulator.commit(ctx)
		reporter.ensurePublished(ctx)
	}(httpResp)

	var buffer bytes.Buffer
	for chunk := range out {
		if chunk.Err != nil {
			return resp, chunk.Err
		}
		if len(chunk.Payload) > 0 {
			_, _ = buffer.Write(chunk.Payload)
			_, _ = buffer.Write([]byte("\n"))
		}
	}
	resp = cliproxyexecutor.Response{Payload: e.convertStreamToNonStream(buffer.Bytes())}
	resp.Payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, requestPayload, resp.Payload)

	reporter.publish(ctx, parseAntigravityUsage(resp.Payload))
	var param any
	converted := sdktranslator.TranslateNonStream(
		ctx, to, from, req.Model, opts.OriginalRequest, requestPayload, resp.Payload, &param,
	)
	resp = cliproxyexecutor.Response{Payload: converted, Headers: httpResp.Header.Clone()}
	reporter.ensurePublished(ctx)

	return resp, nil
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	parts := make([]map[string]any, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingThoughtSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			parts = append(parts, map[string]any{"text": text})
		case "thought":
			if strings.TrimSpace(text) == "" && pendingThoughtSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingThoughtSig = ""
				return
			}
			part := map[string]any{"thought": true}
			part["text"] = text
			if pendingThoughtSig != "" {
				part["thoughtSignature"] = pendingThoughtSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingThoughtSig = ""
	}

	normalizePart := func(partResult gjson.Result) map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]any{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for line := range bytes.SplitSeq(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
			continue
		}

		root := gjson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() &&
			finishResult.String() != "" {
			finishReason = finishResult.String()
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() &&
			responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && pendingKind != kind {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if kind == "thought" && sig != "" {
						pendingThoughtSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	responseTemplate, _ = sjson.SetRaw(responseTemplate, "candidates.0.content.parts", string(partsJSON))
	if role != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.content.role", role)
	}
	if finishReason != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "candidates.0.finishReason", finishReason)
	}
	if modelVersion != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "modelVersion", modelVersion)
	}
	if responseID != "" {
		responseTemplate, _ = sjson.Set(responseTemplate, "responseId", responseID)
	}
	if usageRaw != "" {
		responseTemplate, _ = sjson.SetRaw(responseTemplate, "usageMetadata", usageRaw)
	} else if !gjson.Get(responseTemplate, "usageMetadata").Exists() {
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.promptTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.candidatesTokenCount", 0)
		responseTemplate, _ = sjson.Set(responseTemplate, "usageMetadata.totalTokenCount", 0)
	}

	output := `{"response":{},"traceId":""}`
	output, _ = sjson.SetRaw(output, "response", responseTemplate)
	if traceID != "" {
		output, _ = sjson.Set(output, "traceId", traceID)
	}
	return []byte(output)
}

// ExecuteStream performs a streaming request to the Antigravity API.
func (e *AntigravityExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	needsCompactionScope := helps.HasResponsesCompactionTrigger(req.Payload) ||
		helps.HasResponsesCompactionTrigger(opts.OriginalRequest) ||
		helps.HasResponsesCompactionItem(req.Payload) ||
		helps.HasResponsesCompactionItem(opts.OriginalRequest)
	var compactionScope helps.AntigravityCompactionScope
	if needsCompactionScope {
		var errScope error
		compactionScope, errScope = e.compactionScope(auth, req, opts, baseModel)
		if errScope != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errScope.Error()}
		}
	}
	if helps.HasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.ExpandAntigravityCompactionCapsules(req.Payload, compactionScope)
		if errExpand != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
	}
	if helps.HasResponsesCompactionItem(opts.OriginalRequest) {
		expandedOriginal, errOriginal := helps.ExpandAntigravityCompactionCapsules(
			opts.OriginalRequest,
			compactionScope,
		)
		if errOriginal != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errOriginal.Error()}
		}
		opts.OriginalRequest = expandedOriginal
	}
	if helps.HasResponsesCompactionTrigger(req.Payload) ||
		helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompactionStream(ctx, auth, req, opts, compactionScope)
	}

	ctx = util.WithAlt(ctx, "")

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(baseModel, from, originalPayload)
	if errValidate != nil {
		return nil, errValidate
	}
	req.Payload = originalPayload
	originalTranslated, translated := e.translateAntigravityRequestPair(
		ctx, req, opts.Headers, from, to, baseModel, originalPayload, req.Payload, true,
	)

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(
		e.cfg, baseModel, "antigravity", "request", translated, originalTranslated, requestedModel,
	)
	translated = e.obfuscateSensitiveWords(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	reporter.setTranslatedReasoningEffort(translated, to.String())

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	attempts := antigravityRetryAttempts(auth, e.cfg)
	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)
	requestPayload := translated
	if useCredits {
		if creditPayload := injectEnabledCreditTypes(translated); len(creditPayload) > 0 {
			requestPayload = creditPayload
		}
	}
	requestPayload, replayScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(
		ctx, baseModel, req, opts, requestPayload,
	)
	if errReplay != nil {
		return resp, errReplay
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)

	httpResp, err := e.sendWithRetry(
		ctx, httpClient, baseURLs, attempts, baseModel, auth, func(baseURL string) (*http.Request, error) {
			return e.buildRequest(
				ctx, auth, token, baseModel, requestPayload, true, opts.Alt, baseURL,
				helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata),
			)
		},
	)
	if err != nil {
		if errClear := clearAntigravityReasoningReplayOnError(ctx, replayScope, err); errClear != nil {
			return nil, errClear
		}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func(resp *http.Response) {
		defer close(out)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
		}()
		replayAccumulator := newAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		sawTerminalEvent := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)

			// Filter usage metadata for all models
			// Only retain usage statistics in the terminal chunk
			line = filterSSEUsageMetadata(line)
			replayAccumulator.observeSSELine(line)

			payload := jsonPayload(line)
			if payload == nil {
				continue
			}
			if antigravityStreamPayloadTerminal(payload) {
				sawTerminalEvent = true
			}

			if detail, ok := parseAntigravityStreamUsage(payload); ok {
				reporter.publish(ctx, detail)
			}
			payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, requestPayload, payload)

			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, opts.OriginalRequest, requestPayload, bytes.Clone(payload), &param,
			)
			for i := range chunks {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					return
				}
			}
		}
		if errScan := antigravityResponseReadError(ctx, scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			sendAntigravityStreamReadError(ctx, out, errScan)
			return
		}
		if !sawTerminalEvent {
			errIncomplete := antigravityIncompleteStreamError()
			recordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.publishFailure(ctx)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errIncomplete})
			return
		}
		tail := sdktranslator.TranslateStream(
			ctx, to, from, req.Model, opts.OriginalRequest, requestPayload, []byte("[DONE]"), &param,
		)
		for i := range tail {
			if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: tail[i]}) {
				return
			}
		}
		replayAccumulator.commit(ctx)
		reporter.ensurePublished(ctx)
	}(httpResp)
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *AntigravityExecutor) executeCompactionStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	compactionScope helps.AntigravityCompactionScope,
) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, baseModel)
	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return nil, errSummary
	}
	summaryText, errExtract := helps.ExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return nil, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.SealAntigravityCompaction(summaryText, compactionScope)
	if errSeal != nil {
		return nil, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(gjson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usageDetail := parseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usageDetail.InputTokens)
		outputTokens = int(usageDetail.OutputTokens)
		totalTokens = int(usageDetail.TotalTokens)
	}

	chunks := helps.BuildAntigravityCompactionStreamChunks(
		baseModel, capsule, inputTokens, outputTokens, totalTokens,
	)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

// Refresh refreshes the authentication credentials using the refresh token.
func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}

func (e *AntigravityExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	return antigravityProjectIDFromAuth(auth) == ""
}

func (e *AntigravityExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (
	*cliproxyauth.Auth,
	error,
) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return nil, nil
	}

	updated := auth.Clone()
	token, refreshedAuth, errToken := e.ensureAccessToken(ctx, updated)
	if errToken != nil {
		return nil, errToken
	}
	if refreshedAuth != nil {
		updated = refreshedAuth
	}
	if antigravityProjectIDFromAuth(updated) != "" {
		return updated, nil
	}

	projectID, errProject := e.fetchAntigravityProjectID(ctx, updated, token)
	if errProject != nil {
		return nil, missingAntigravityProjectIDError(errProject)
	}
	if projectID == "" {
		return nil, missingAntigravityProjectIDError(nil)
	}
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = projectID
	return updated, nil
}

// CountTokens counts tokens for the given request using the Antigravity API.
func (e *AntigravityExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return cliproxyexecutor.Response{}, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	cliproxyauth.NotifyAccessTokenFingerprint(ctx, auth)
	if strings.TrimSpace(token) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")
	respCtx := util.WithAlt(ctx, opts.Alt)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload, errValidate := validateAntigravityRequestSignatures(baseModel, from, originalPayloadSource)
	if errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	req.Payload = originalPayload
	_, payload := e.translateAntigravityRequestPair(
		ctx, req, opts.Headers, from, to, baseModel, originalPayload, req.Payload, false,
	)

	payload, err := helps.ApplyThinkingWithSourcePayload(
		payload, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	payload = e.obfuscateSensitiveWords(payload)
	payload = sanitizeAntigravityGeminiRequestSignatures(baseModel, payload)
	payload, _, errReplay := prepareAntigravityGeminiReasoningReplayPayload(
		ctx,
		baseModel,
		req,
		opts,
		payload,
	)
	if errReplay != nil {
		return cliproxyexecutor.Response{}, errReplay
	}
	payload = ensureAntigravityGeminiLeadingUserContent(baseModel, payload)

	for _, field := range []string{
		"project",
		"model",
		"request.safetySettings",
		"request.toolConfig",
		"request.labels",
		"request.sessionId",
	} {
		payload = deleteJSONField(payload, field)
	}

	base := resolveAntigravityRequestBaseURL(auth)
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(antigravityCountTokensPath)
	if opts.Alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(opts.Alt))
	}

	httpReq, errReq := http.NewRequestWithContext(
		ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload),
	)
	if errReq != nil {
		return cliproxyexecutor.Response{}, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	util.ApplyCredentialHeader(httpReq, "Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}

	recordUpstreamRequest(ctx, e.cfg, requestURL.String(), httpReq.Header.Clone(), payload, e.Identifier(), auth)

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		errDo = antigravityRequestError(ctx, errDo)
		recordAPIResponseError(ctx, e.cfg, errDo)
		return cliproxyexecutor.Response{}, errDo
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if errClose := httpResp.Body.Close(); errClose != nil {
		log.Errorf("antigravity executor: close response body error: %v", errClose)
	}
	if errRead != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			errRead = errCtx
		} else {
			errRead = markResponseStarted(errRead)
		}
		recordAPIResponseError(ctx, e.cfg, errRead)
		return cliproxyexecutor.Response{}, errRead
	}
	appendAPIResponseChunk(ctx, e.cfg, bodyBytes)

	if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
		count := gjson.GetBytes(bodyBytes, "totalTokens").Int()
		translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, bodyBytes)
		return cliproxyexecutor.Response{Payload: translated, Headers: httpResp.Header.Clone()}, nil
	}

	if httpResp.StatusCode == http.StatusTooManyRequests {
		CloseAntigravityAuthIdleTransports(auth)
	}
	return cliproxyexecutor.Response{}, buildStatusError(httpResp.StatusCode, bodyBytes, httpResp.Header)
}

func antigravityWebSearchModelIDs(payload []byte) map[string]struct{} {
	models := gjson.GetBytes(payload, "webSearchModelIds")
	if !models.IsArray() {
		return nil
	}
	out := make(map[string]struct{}, len(models.Array()))
	for _, model := range models.Array() {
		modelID := normalizeAntigravityCapabilityModelID(model.String())
		if modelID != "" {
			out[modelID] = struct{}{}
		}
	}
	return out
}

func normalizeAntigravityCapabilityModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// FetchAntigravityModels retrieves the complete model set using the supplied auth.
func FetchAntigravityModels(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	cfg *config.Config,
) ModelDiscoveryResult {
	if ctx == nil {
		ctx = context.Background()
	}
	exec := &AntigravityExecutor{cfg: cfg}
	token, updatedAuth, errToken := exec.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return failedAntigravityModelDiscovery(ctx, fmt.Errorf("resolve access token: %w", errToken))
	}
	if token == "" {
		return ModelDiscoveryResult{
			Outcome: ModelDiscoveryFailed,
			Err:     fmt.Errorf("resolve access token: empty access token"),
		}
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	withAuthUpdate := func(result ModelDiscoveryResult) ModelDiscoveryResult {
		result.Auth = updatedAuth
		return result
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, cfg, auth, 0)
	var lastErr error
	var incomplete bool

	for idx, baseURL := range baseURLs {
		modelsURL := baseURL + antigravityModelsPath

		var payload []byte
		if auth != nil && auth.Metadata != nil {
			if pid, ok := auth.Metadata["project_id"].(string); ok && strings.TrimSpace(pid) != "" {
				payload = fmt.Appendf(nil, `{"project": "%s"}`, strings.TrimSpace(pid))
			}
		}
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, bytes.NewReader(payload))
		if errReq != nil {
			lastErr = fmt.Errorf("create models request for %s: %w", baseURL, errReq)
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		util.ApplyCredentialHeader(httpReq, "Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			lastErr = fmt.Errorf("request models from %s: %w", baseURL, errDo)
			if ctx.Err() != nil {
				return withAuthUpdate(failedAntigravityModelDiscovery(ctx, lastErr))
			}
			if idx+1 < len(baseURLs) {
				log.Debugf(
					"antigravity executor: models request error on base url %s, retrying with fallback base url: %s",
					baseURL, baseURLs[idx+1],
				)
			}
			continue
		}

		bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			lastErr = fmt.Errorf("read models response from %s: %w", baseURL, errRead)
			if ctx.Err() != nil {
				return withAuthUpdate(failedAntigravityModelDiscovery(ctx, lastErr))
			}
			if idx+1 < len(baseURLs) {
				log.Debugf(
					"antigravity executor: models read error on base url %s, retrying with fallback base url: %s",
					baseURL, baseURLs[idx+1],
				)
			}
			continue
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			if httpResp.StatusCode == http.StatusTooManyRequests {
				CloseAntigravityAuthIdleTransports(auth)
			}
			lastErr = buildStatusError(httpResp.StatusCode, bodyBytes, httpResp.Header)
			if idx+1 < len(baseURLs) {
				log.Debugf(
					"antigravity executor: models request failed with status %d on base url %s, retrying with fallback base url: %s",
					httpResp.StatusCode,
					baseURL,
					baseURLs[idx+1],
				)
			}
			continue
		}

		result := gjson.GetBytes(bodyBytes, "models")
		if !result.Exists() || !result.IsObject() {
			lastErr = fmt.Errorf("models response from %s does not contain an object models field", baseURL)
			incomplete = true
			if idx+1 < len(baseURLs) {
				log.Debugf(
					"antigravity executor: models field missing or invalid on base url %s, retrying with fallback base url: %s",
					baseURL, baseURLs[idx+1],
				)
			}
			continue
		}
		webSearchModelIDs := antigravityWebSearchModelIDs(bodyBytes)

		now := time.Now().Unix()
		modelConfig := registry.GetAntigravityModelConfig()
		models := make([]*registry.ModelInfo, 0, len(result.Map()))
		for originalName, modelData := range result.Map() {
			modelID := strings.TrimSpace(originalName)
			if modelID == "" {
				continue
			}
			switch modelID {
			case "chat_20706",
				"chat_23310",
				"tab_flash_lite_preview",
				"tab_jump_flash_lite_preview",
				"gemini-2.5-flash-thinking",
				"gemini-2.5-pro":
				continue
			}
			modelCfg := modelConfig[modelID]

			displayName := modelData.Get("displayName").String()
			if displayName == "" {
				displayName = modelID
			}

			modelInfo := &registry.ModelInfo{
				ID:          modelID,
				Name:        modelID,
				Description: displayName,
				DisplayName: displayName,
				Version:     modelID,
				Object:      "model",
				Created:     now,
				OwnedBy:     antigravityAuthType,
				Type:        antigravityAuthType,
			}

			inputModalities := []string{"TEXT"}
			if modelData.Get("supportsImages").Bool() {
				inputModalities = append(inputModalities, "IMAGE")
			}
			if modelData.Get("supportsVideo").Bool() {
				inputModalities = append(inputModalities, "VIDEO")
			}
			modelInfo.SupportedInputModalities = inputModalities
			modelInfo.SupportedOutputModalities = []string{"TEXT"}
			_, modelInfo.SupportsWebSearch = webSearchModelIDs[normalizeAntigravityCapabilityModelID(modelID)]

			if maxTok := modelData.Get("maxTokens").Int(); maxTok > 0 {
				modelInfo.InputTokenLimit = int(maxTok)
			}
			if maxOut := modelData.Get("maxOutputTokens").Int(); maxOut > 0 {
				modelInfo.OutputTokenLimit = int(maxOut)
			}
			modelInfo.SupportedGenerationMethods = []string{"generateContent", "countTokens"}

			if modelCfg != nil {
				if modelCfg.Thinking != nil {
					modelInfo.Thinking = modelCfg.Thinking
				}
				if modelCfg.MaxCompletionTokens > 0 {
					modelInfo.MaxCompletionTokens = modelCfg.MaxCompletionTokens
				}
			}
			models = append(models, modelInfo)
		}
		return withAuthUpdate(ModelDiscoveryResult{Outcome: ModelDiscoveryComplete, Models: models})
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Antigravity model discovery endpoint is available")
	}
	if incomplete {
		return withAuthUpdate(ModelDiscoveryResult{Outcome: ModelDiscoveryInconclusive, Err: lastErr})
	}
	return withAuthUpdate(failedAntigravityModelDiscovery(ctx, lastErr))
}

func failedAntigravityModelDiscovery(ctx context.Context, err error) ModelDiscoveryResult {
	if ctx != nil && ctx.Err() != nil {
		return ModelDiscoveryResult{Outcome: ModelDiscoveryInconclusive, Err: ctx.Err()}
	}
	return ModelDiscoveryResult{Outcome: ModelDiscoveryFailed, Err: err}
}

func (e *AntigravityExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (
	string,
	*cliproxyauth.Auth,
	error,
) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry := tokenExpiry(auth.Metadata)
	if accessToken != "" && expiry.After(time.Now().Add(refreshSkew)) {
		e.maybeRefreshAntigravityCreditsHint(ctx, auth, accessToken)
		return accessToken, nil, nil
	}
	refreshCtx := context.Background()
	if ctx != nil {
		if rt := util.RoundTripperValue(ctx); rt != nil {
			refreshCtx = util.WithRoundTripper(refreshCtx, rt)
		}
	}
	if refreshed, handled, err := refreshAuthViaHome(refreshCtx, e.cfg, auth); handled {
		if err != nil {
			return "", nil, err
		}
		token := metaStringValue(refreshed.Metadata, "access_token")
		if strings.TrimSpace(token) == "" {
			return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
		}
		e.maybeRefreshAntigravityCreditsHint(ctx, refreshed, token)
		return token, refreshed, nil
	}

	updated, errRefresh := e.refreshToken(refreshCtx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

type antigravityTokenRefreshData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (e *AntigravityExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token"}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	result, errRefresh, _ := antigravityRefreshGroup.Do(
		refreshToken, func() (any, error) {
			return e.refreshTokenSingleFlight(context.WithoutCancel(ctx), auth, refreshToken)
		},
	)
	if errRefresh != nil {
		return auth, errRefresh
	}
	tokenResp, ok := result.(*antigravityTokenRefreshData)
	if !ok || tokenResp == nil {
		return auth, fmt.Errorf("antigravity token refresh failed: invalid single-flight result")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	now := time.Now()
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = antigravityAuthType
	if errProject := e.ensureAntigravityProjectID(ctx, auth, tokenResp.AccessToken); errProject != nil {
		log.Warnf("antigravity executor: ensure project id failed: %v", errProject)
	}
	e.updateAntigravityCreditsBalance(ctx, auth, tokenResp.AccessToken)
	return auth, nil
}

func (e *AntigravityExecutor) refreshTokenSingleFlight(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	refreshToken string,
) (*antigravityTokenRefreshData, error) {
	form := url.Values{}
	form.Set("client_id", antigravity.ClientID)
	form.Set("client_secret", antigravity.ClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, errReq := http.NewRequestWithContext(
		ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()),
	)
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Host", "oauth2.googleapis.com")
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Real Antigravity uses Go's default User-Agent for OAuth token refresh
	httpReq.Header.Set("User-Agent", "Go-http-client/2.0")

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, antigravityRequestError(ctx, errDo)
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if errRead != nil {
		return nil, antigravityResponseReadError(ctx, errRead)
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		if httpResp.StatusCode == http.StatusTooManyRequests {
			CloseAntigravityAuthIdleTransports(auth)
		}
		return nil, buildStatusError(httpResp.StatusCode, bodyBytes, httpResp.Header)
	}

	var tokenResp antigravityTokenRefreshData
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return nil, markResponseStarted(errUnmarshal)
	}
	return &tokenResp, nil
}

func (e *AntigravityExecutor) maybeRefreshAntigravityCreditsHint(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	accessToken string,
) {
	if e == nil || auth == nil || !antigravityCreditsRetryEnabled(e.cfg) {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if hint, ok := cliproxyauth.GetAntigravityCreditsHint(authID); ok && hint.Known {
		return
	}
	if strings.TrimSpace(accessToken) == "" {
		accessToken = metaStringValue(auth.Metadata, "access_token")
	}
	if strings.TrimSpace(accessToken) == "" {
		return
	}

	state := &antigravityCreditsHintRefreshState{}
	if existing, loaded := antigravityCreditsHintRefreshByID.LoadOrStore(authID, state); loaded {
		if cast, ok := existing.(*antigravityCreditsHintRefreshState); ok && cast != nil {
			state = cast
		} else {
			antigravityCreditsHintRefreshByID.Delete(authID)
			antigravityCreditsHintRefreshByID.Store(authID, state)
		}
	}

	now := time.Now()
	if !state.mu.TryLock() {
		return
	}
	if !state.lastAttempt.IsZero() && now.Sub(state.lastAttempt) < antigravityCreditsHintRefreshInterval {
		state.mu.Unlock()
		return
	}
	state.lastAttempt = now

	refreshCtx := context.Background()
	if rt := util.RoundTripperValue(ctx); rt != nil {
		refreshCtx = util.WithRoundTripper(refreshCtx, rt)
	}
	refreshCtx, cancel := context.WithTimeout(refreshCtx, antigravityCreditsHintRefreshTimeout)
	authCopy := auth.Clone()
	go func(state *antigravityCreditsHintRefreshState, auth *cliproxyauth.Auth, token string) {
		defer cancel()
		defer state.mu.Unlock()
		e.updateAntigravityCreditsBalance(refreshCtx, auth, token)
	}(state, authCopy, accessToken)
}

func (e *AntigravityExecutor) updateAntigravityCreditsBalance(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	accessToken string,
) {
	if e == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || !antigravityCreditsRetryEnabled(e.cfg) {
		return
	}
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return
	}

	userAgent := resolveLoadCodeAssistUserAgent(auth)
	loadReqBody, errMarshal := json.Marshal(
		map[string]any{
			"metadata": map[string]string{
				"ide_type":    "ANTIGRAVITY",
				"ide_version": misc.AntigravityVersionFromUserAgent(userAgent),
				"ide_name":    "antigravity",
			},
		},
	)
	if errMarshal != nil {
		log.Debugf("antigravity executor: marshal loadCodeAssist request error: %v", errMarshal)
		return
	}

	base := strings.TrimSuffix(buildBaseURL(auth), "/")
	endpointURL := base + antigravityLoadCodeAssistPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(loadReqBody))
	if errReq != nil {
		log.Debugf("antigravity executor: create loadCodeAssist request error: %v", errReq)
		return
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("X-Goog-Api-Client", misc.AntigravityGoogAPIClientUA)
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		log.Debugf("antigravity executor: loadCodeAssist request error: %v", errDo)
		return
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close loadCodeAssist response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if httpResp.StatusCode == http.StatusTooManyRequests {
		CloseAntigravityAuthIdleTransports(auth)
	}
	if errRead != nil || httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		log.Debugf("antigravity executor: loadCodeAssist returned status %d, err=%v", httpResp.StatusCode, errRead)
		return
	}

	authID := strings.TrimSpace(auth.ID)
	paidTierID := strings.TrimSpace(gjson.GetBytes(bodyBytes, "paidTier.id").String())
	credits := gjson.GetBytes(bodyBytes, "paidTier.availableCredits")
	if !credits.IsArray() {
		cliproxyauth.SetAntigravityCreditsHint(
			authID, cliproxyauth.AntigravityCreditsHint{
				Known:      true,
				Available:  false,
				PaidTierID: paidTierID,
				UpdatedAt:  time.Now(),
			},
		)
		return
	}
	for _, credit := range credits.Array() {
		if !strings.EqualFold(credit.Get("creditType").String(), "GOOGLE_ONE_AI") {
			continue
		}
		creditAmount, errCreditAmount := strconv.ParseFloat(strings.TrimSpace(credit.Get("creditAmount").String()), 64)
		if errCreditAmount != nil {
			continue
		}
		minAmount, errMinAmount := strconv.ParseFloat(
			strings.TrimSpace(credit.Get("minimumCreditAmountForUsage").String()), 64,
		)
		if errMinAmount != nil {
			continue
		}
		cliproxyauth.SetAntigravityCreditsHint(
			authID, cliproxyauth.AntigravityCreditsHint{
				Known:           true,
				Available:       creditAmount >= minAmount,
				CreditAmount:    creditAmount,
				MinCreditAmount: minAmount,
				PaidTierID:      paidTierID,
				UpdatedAt:       time.Now(),
			},
		)
		return
	}
	cliproxyauth.SetAntigravityCreditsHint(
		authID, cliproxyauth.AntigravityCreditsHint{
			Known:      true,
			Available:  false,
			PaidTierID: paidTierID,
			UpdatedAt:  time.Now(),
		},
	)
}

func (e *AntigravityExecutor) ensureAntigravityProjectID(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	accessToken string,
) error {
	if auth == nil {
		return nil
	}
	if antigravityProjectIDFromAuth(auth) != "" {
		return nil
	}

	projectID, errFetch := e.fetchAntigravityProjectID(ctx, auth, accessToken)
	if errFetch != nil {
		return errFetch
	}
	if projectID == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = projectID

	return nil
}

func (e *AntigravityExecutor) fetchAntigravityProjectID(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	accessToken string,
) (string, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return "", nil
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(ctx, token, httpClient)
	if errFetch != nil {
		return "", antigravityRequestError(ctx, errFetch)
	}
	return strings.TrimSpace(projectID), nil
}

func (e *AntigravityExecutor) projectIDForRequest(_ context.Context, auth *cliproxyauth.Auth, _ string) (
	string,
	error,
) {
	if projectID := antigravityProjectIDFromAuth(auth); projectID != "" {
		return projectID, nil
	}
	return "", missingAntigravityProjectIDError(nil)
}

func antigravityProjectIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if projectID, ok := auth.Metadata["project_id"].(string); ok {
		return strings.TrimSpace(projectID)
	}
	return ""
}

func missingAntigravityProjectIDError(cause error) statusErr {
	msg := "antigravity auth missing project_id"
	if cause != nil {
		msg = fmt.Sprintf("%s: %v", msg, cause)
	}
	return statusErr{code: http.StatusBadRequest, msg: msg}
}

func (e *AntigravityExecutor) buildRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	token, modelName string,
	payload []byte,
	stream bool,
	alt, baseURL string,
	derivedSessionIDs ...string,
) (*http.Request, error) {
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = buildBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID, errProject := e.projectIDForRequest(ctx, auth, token)
	if errProject != nil {
		return nil, errProject
	}
	payload = geminiToAntigravity(modelName, payload, projectID, derivedSessionIDs...)

	useAntigravitySchema := strings.Contains(modelName, "claude") || strings.Contains(
		modelName, "gemini-3-pro",
	) || strings.Contains(modelName, "gemini-3.1-pro")
	payloadStr := string(payload)
	if antigravityRequestNeedsSchemaSanitization(payload) {
		payloadStr = sanitizeAntigravityRequestSchemas(payloadStr, useAntigravitySchema)
	}

	// if useAntigravitySchema {
	// 	systemInstructionPartsResult := gjson.Get(payloadStr, "request.systemInstruction.parts")
	// 	payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.role", "user")
	// 	payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.0.text", systemInstruction)
	// 	payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.1.text", fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", systemInstruction))

	// 	if systemInstructionPartsResult.Exists() && systemInstructionPartsResult.IsArray() {
	// 		for _, partResult := range systemInstructionPartsResult.Array() {
	// 			payloadStr, _ = sjson.SetRaw(payloadStr, "request.systemInstruction.parts.-1", partResult.Raw)
	// 		}
	// 	}
	// }

	if strings.Contains(modelName, "claude") {
		payloadStr, _ = sjson.Set(payloadStr, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	} else {
		payloadStr, _ = sjson.Delete(payloadStr, "request.generationConfig.maxOutputTokens")
	}
	payload = applyAntigravityNativeSignatureReplayIfNeeded(modelName, []byte(payloadStr))

	httpReq, errReq := http.NewRequestWithContext(
		ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload),
	)
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	util.ApplyCredentialHeader(httpReq, "Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}

	var payloadLog []byte
	if requestLogCaptureEnabled(e.cfg) {
		payloadLog = bytes.Clone(payload)
	}
	recordUpstreamRequest(ctx, e.cfg, requestURL.String(), httpReq.Header.Clone(), payloadLog, e.Identifier(), auth)

	return httpReq, nil
}

// sanitizeAntigravityRequestSchemas cleans only payload locations that carry JSON schemas.
// Whole-document cleaning would rewrite schema-like keys in replayed function arguments.
func sanitizeAntigravityRequestSchemas(payloadStr string, useAntigravitySchema bool) string {
	payloadStr = sanitizeAntigravityToolSchemas(payloadStr, useAntigravitySchema)
	return sanitizeAntigravityGenerationSchemas(payloadStr)
}

// sanitizeAntigravityToolSchemas applies declaration rewrites to request.tools
// and writes the complete subtree back once.
func sanitizeAntigravityToolSchemas(payloadStr string, useAntigravitySchema bool) string {
	tools := gjson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return payloadStr
	}

	toolDocument := `{"request":{"tools":` + tools.Raw + `}}`
	toolDocument = sanitizeAntigravityToolSchemaDocument(toolDocument, useAntigravitySchema)
	cleanedTools := gjson.Get(toolDocument, "request.tools")
	if !cleanedTools.IsArray() || cleanedTools.Raw == tools.Raw {
		return payloadStr
	}
	updated, errSet := sjson.SetRawBytes([]byte(payloadStr), "request.tools", []byte(cleanedTools.Raw))
	if errSet != nil {
		log.Debugf("antigravity: failed to write cleaned request.tools: %v", errSet)
		return payloadStr
	}
	return string(updated)
}

func sanitizeAntigravityToolSchemaDocument(payloadStr string, useAntigravitySchema bool) string {
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		oldPath := base + ".parametersJsonSchema"
		if !gjson.Get(payloadStr, oldPath).Exists() {
			continue
		}
		renamed, errRename := util.RenameKey(payloadStr, oldPath, base+".parameters")
		if errRename != nil {
			log.Debugf("antigravity: failed to rename %s: %v", oldPath, errRename)
			continue
		}
		payloadStr = renamed
	}

	toolSchemaCleaner := util.CleanJSONSchemaForGemini
	if useAntigravitySchema {
		toolSchemaCleaner = util.CleanJSONSchemaForAntigravity
	}
	cleanNestedToolSchema := func(schemaRaw string) string {
		return cleanNestedSchema(toolSchemaCleaner, schemaRaw)
	}
	return cleanAntigravitySchemasAtPaths(
		payloadStr,
		antigravityDeclarationSchemaPaths(payloadStr),
		cleanNestedToolSchema,
	)
}

// sanitizeAntigravityGenerationSchemas batches every schema edit within one
// generation config before replacing that config in the full request.
func sanitizeAntigravityGenerationSchemas(payloadStr string) string {
	for _, container := range antigravityGenerationConfigContainers {
		generationConfig := gjson.Get(payloadStr, container)
		if !generationConfig.IsObject() {
			continue
		}
		cleanedConfig := generationConfig.Raw
		for _, key := range antigravityGenerationSchemaKeys {
			schema := gjson.Get(cleanedConfig, key)
			if !schema.IsObject() {
				continue
			}
			cleanedSchema := util.CleanJSONSchemaForAntigravityResponse(schema.Raw)
			if cleanedSchema == schema.Raw {
				continue
			}
			updated, errSet := sjson.SetRawBytes([]byte(cleanedConfig), key, []byte(cleanedSchema))
			if errSet != nil {
				log.Debugf("antigravity: failed to write cleaned schema at %s.%s: %v", container, key, errSet)
				continue
			}
			cleanedConfig = string(updated)
		}
		if cleanedConfig == generationConfig.Raw {
			continue
		}
		updated, errSet := sjson.SetRawBytes([]byte(payloadStr), container, []byte(cleanedConfig))
		if errSet != nil {
			log.Debugf("antigravity: failed to write cleaned %s: %v", container, errSet)
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

func cleanAntigravitySchemasAtPaths(
	payloadStr string,
	schemaPaths []string,
	clean func(string) string,
) string {
	for _, schemaPath := range schemaPaths {
		schema := gjson.Get(payloadStr, schemaPath)
		if !schema.Exists() {
			continue
		}
		cleanedSchema := clean(schema.Raw)
		if cleanedSchema == schema.Raw {
			continue
		}
		updated, errSet := sjson.SetRawBytes(
			[]byte(payloadStr),
			schemaPath,
			[]byte(cleanedSchema),
		)
		if errSet != nil {
			log.Debugf("antigravity: failed to write cleaned schema at %s: %v", schemaPath, errSet)
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

const antigravitySchemaWrapperKey = "schema"

// cleanNestedSchema preserves the cleaner's nested-schema placeholder behavior.
func cleanNestedSchema(clean func(string) string, schemaRaw string) string {
	wrapped, errWrap := sjson.SetRaw("{}", antigravitySchemaWrapperKey, schemaRaw)
	if errWrap != nil {
		return clean(schemaRaw)
	}
	if unwrapped := gjson.Get(clean(wrapped), antigravitySchemaWrapperKey); unwrapped.Exists() {
		return unwrapped.Raw
	}
	return clean(schemaRaw)
}

// antigravityFunctionDeclarationPaths returns all function declaration paths in the request.
func antigravityFunctionDeclarationPaths(payloadStr string) []string {
	tools := gjson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return nil
	}
	paths := make([]string, 0, len(tools.Array()))
	for i, tool := range tools.Array() {
		for _, declarationKey := range []string{"functionDeclarations", "function_declarations"} {
			declarations := tool.Get(declarationKey)
			if !declarations.IsArray() {
				continue
			}
			for j := range declarations.Array() {
				paths = append(
					paths,
					fmt.Sprintf("request.tools.%d.%s.%d", i, declarationKey, j),
				)
			}
		}
	}
	return paths
}

func antigravityDeclarationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, 8)
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		for _, key := range antigravityDeclarationSchemaKeys {
			path := base + "." + key
			if gjson.Get(payloadStr, path).IsObject() {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

var (
	antigravityDeclarationSchemaKeys = []string{
		"parameters", "parametersJsonSchema", "parameters_json_schema",
		"response", "responseJsonSchema", "response_json_schema",
	}
	antigravityGenerationConfigContainers = []string{
		"request.generationConfig", "request.generation_config",
	}
	antigravityGenerationSchemaKeys = []string{
		"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
	}
)

func antigravityRequestNeedsSchemaSanitization(payload []byte) bool {
	if gjson.GetBytes(payload, "request.tools.0").Exists() {
		return true
	}
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			if gjson.GetBytes(payload, container+"."+key).Exists() {
				return true
			}
		}
	}
	return false
}

func tokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	if expStr, ok := metadata["expired"].(string); ok {
		expStr = strings.TrimSpace(expStr)
		if expStr != "" {
			if parsed, errParse := time.Parse(time.RFC3339, expStr); errParse == nil {
				return parsed
			}
		}
	}
	expiresIn, hasExpires := int64Value(metadata["expires_in"])
	tsMs, hasTimestamp := int64Value(metadata["timestamp"])
	if hasExpires && hasTimestamp {
		return time.Unix(0, tsMs*int64(time.Millisecond)).Add(time.Duration(expiresIn) * time.Second)
	}
	return time.Time{}
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case []byte:
			return strings.TrimSpace(string(typed))
		}
	}
	return ""
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i, true
		}
	case string:
		if strings.TrimSpace(typed) == "" {
			return 0, false
		}
		if i, errParse := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); errParse == nil {
			return i, true
		}
	}
	return 0, false
}

func buildBaseURL(auth *cliproxyauth.Auth) string {
	if baseURLs := antigravityBaseURLFallbackOrder(auth); len(baseURLs) > 0 {
		return baseURLs[0]
	}
	return antigravityBaseURLDaily
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	// noinspection HttpUrlsUsage
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityRequestUserAgent(antigravityConfiguredUserAgent(auth))
}

func resolveLoadCodeAssistUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityLoadCodeAssistUserAgent(antigravityConfiguredUserAgent(auth))
}

func antigravityConfiguredUserAgent(auth *cliproxyauth.Auth) string {
	raw := ""
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				raw = ua
			}
		}
		if raw == "" && auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				raw = strings.TrimSpace(ua)
			}
		}
	}
	return raw
}

func antigravityRetryAttempts(auth *cliproxyauth.Auth, cfg *config.Config) int {
	retry := 0
	if cfg != nil {
		retry = cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		retry = 0
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "no capacity available")
}

func antigravityNoCapacityRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := min(time.Duration(attempt+1)*250*time.Millisecond, 2*time.Second)
	return delay
}

func antigravityWait(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func antigravityBaseURLFallbackOrder(auth *cliproxyauth.Auth) []string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return []string{base}
	}
	return []string{
		antigravityBaseURLDaily,
		antigravitySandboxBaseURLDaily,
	}
}

func resolveAntigravityRequestBaseURL(auth *cliproxyauth.Auth) string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return base
	}
	return antigravityBaseURLDaily
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func geminiToAntigravity(modelName string, payload []byte, projectID string, derivedSessionIDs ...string) []byte {
	template := payload
	template = helps.SetStringIfDifferent(template, "model", modelName)
	template = helps.SetStringIfDifferent(template, "userAgent", "antigravity")

	isImageModel := strings.Contains(modelName, "image")
	reqType := strings.TrimSpace(gjson.GetBytes(template, "requestType").String())
	if reqType == "" {
		if isImageModel {
			reqType = "image_gen"
		} else {
			reqType = "agent"
		}
		template = helps.SetStringIfDifferent(template, "requestType", reqType)
	}

	template = helps.SetStringIfDifferent(template, "project", projectID)

	if isImageModel {
		template, _ = sjson.SetBytes(template, "requestId", generateImageGenRequestID())
	} else if reqType != "web_search" {
		template, _ = sjson.SetBytes(template, "requestId", generateRequestID())
		sessionID := cliproxysession.NormalizeExplicitID(gjson.GetBytes(template, "request.sessionId").String())
		if sessionID == "" && len(derivedSessionIDs) > 0 {
			sessionID = cliproxysession.NormalizeExplicitID(derivedSessionIDs[0])
		}
		if sessionID == "" {
			sessionID = generateStableSessionID(payload)
		}
		template = helps.SetStringIfDifferent(template, "request.sessionId", sessionID)
	}

	if gjson.GetBytes(template, "request.safetySettings").Exists() {
		template, _ = sjson.DeleteBytes(template, "request.safetySettings")
	}
	if toolConfig := gjson.GetBytes(template, "toolConfig"); toolConfig.Exists() && !gjson.GetBytes(
		template, "request.toolConfig",
	).Exists() {
		template, _ = sjson.SetRawBytes(template, "request.toolConfig", []byte(toolConfig.Raw))
		template, _ = sjson.DeleteBytes(template, "toolConfig")
	}
	return template
}

func generateRequestID() string {
	return "agent-" + uuid.NewString()
}

func generateImageGenRequestID() string {
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func generateSessionID() string {
	randSourceMutex.Lock()
	n := randSource.Int63n(9_000_000_000_000_000_000)
	randSourceMutex.Unlock()
	return "-" + strconv.FormatInt(n, 10)
}

func generateStableSessionID(payload []byte) string {
	contents := gjson.GetBytes(payload, "request.contents")
	if contents.IsArray() {
		for _, content := range contents.Array() {
			if content.Get("role").String() == "user" {
				text := content.Get("parts.0.text").String()
				if text != "" {
					h := sha256.Sum256([]byte(text))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return "-" + strconv.FormatInt(n, 10)
				}
			}
		}
	}
	return generateSessionID()
}
