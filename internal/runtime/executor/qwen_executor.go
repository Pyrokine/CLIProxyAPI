package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	qwenauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/qwen"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	qwenUserAgent       = "QwenCode/0.14.2 (darwin; arm64)"
	qwenRateLimitPerMin = 60          // 60 requests per minute per credential
	qwenRateLimitWindow = time.Minute // sliding window duration
)

var qwenDefaultSystemMessage = []byte(`{"role":"system","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}}]}`)

// qwenBeijingLoc caches the Beijing timezone to avoid repeated LoadLocation syscalls.
var qwenBeijingLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil || loc == nil {
		log.Warnf("qwen: failed to load Asia/Shanghai timezone: %v, using fixed UTC+8", err)
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// qwenQuotaCodes is a package-level set of error codes that indicate quota exhaustion.
var qwenQuotaCodes = map[string]struct{}{
	"insufficient_quota": {},
	"quota_exceeded":     {},
}

// qwenRateLimiter tracks request timestamps per credential for rate limiting.
// Qwen has a limit of 60 requests per minute per account.
var qwenRateLimiter = struct {
	sync.Mutex
	requests map[string][]time.Time // authID -> request timestamps
}{
	requests: make(map[string][]time.Time),
}

// redactAuthID returns a redacted version of the auth ID for safe logging.
// Keeps a small prefix/suffix to allow correlation across events.
func redactAuthID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "..." + id[len(id)-4:]
}

// checkQwenRateLimit checks if the credential has exceeded the rate limit.
// Returns nil if allowed, or a statusErr with retryAfter if rate limited.
func checkQwenRateLimit(authID string) error {
	if authID == "" {
		// Empty authID should not bypass rate limiting in production
		// Use debug level to avoid log spam for certain auth flows
		log.Debug("qwen rate limit check: empty authID, skipping rate limit")
		return nil
	}

	now := time.Now()
	windowStart := now.Add(-qwenRateLimitWindow)

	qwenRateLimiter.Lock()
	defer qwenRateLimiter.Unlock()

	// Get and filter timestamps within the window
	timestamps := qwenRateLimiter.requests[authID]
	var validTimestamps []time.Time
	for _, ts := range timestamps {
		if ts.After(windowStart) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Always prune expired entries to prevent memory leak
	// Delete empty entries, otherwise update with pruned slice
	if len(validTimestamps) == 0 {
		delete(qwenRateLimiter.requests, authID)
	}

	// Check if rate limit exceeded
	if len(validTimestamps) >= qwenRateLimitPerMin {
		// Calculate when the oldest request will expire
		oldestInWindow := validTimestamps[0]
		retryAfter := max(oldestInWindow.Add(qwenRateLimitWindow).Sub(now), time.Second)
		retryAfterSec := int(retryAfter.Seconds())
		return statusErr{
			code: http.StatusTooManyRequests,
			msg: fmt.Sprintf(
				`{"error":{"code":"rate_limit_exceeded","message":"Qwen rate limit: %d requests/minute exceeded, retry after %ds","type":"rate_limit_exceeded"}}`,
				qwenRateLimitPerMin,
				retryAfterSec,
			),
			retryAfter: &retryAfter,
		}
	}

	// Record this request and update the map with pruned timestamps
	validTimestamps = append(validTimestamps, now)
	qwenRateLimiter.requests[authID] = validTimestamps

	return nil
}

// isQwenQuotaError checks if the error response indicates a quota exceeded error.
// Qwen returns HTTP 403 with error.code="insufficient_quota" when daily quota is exhausted.
func isQwenQuotaError(body []byte) bool {
	code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
	errType := strings.ToLower(gjson.GetBytes(body, "error.type").String())

	// Primary check: exact match on error.code or error.type (most reliable)
	if _, ok := qwenQuotaCodes[code]; ok {
		return true
	}
	if _, ok := qwenQuotaCodes[errType]; ok {
		return true
	}

	// Fallback: check message only if code/type don't match (less reliable)
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "insufficient_quota") || strings.Contains(msg, "quota exceeded") ||
		strings.Contains(msg, "free allocated quota exceeded") {
		return true
	}

	return false
}

// wrapQwenError wraps an HTTP error response, detecting quota errors and mapping them to 429.
// Returns the appropriate status code and retryAfter duration for statusErr.
// Only checks for quota errors when httpCode is 403 or 429 to avoid false positives.
func wrapQwenError(ctx context.Context, httpCode int, body []byte) (errCode int, retryAfter *time.Duration) {
	errCode = httpCode
	// Only check quota errors for expected status codes to avoid false positives
	// Qwen returns 403 for quota errors, 429 for rate limits
	if (httpCode == http.StatusForbidden || httpCode == http.StatusTooManyRequests) && isQwenQuotaError(body) {
		errCode = http.StatusTooManyRequests // Map to 429 to trigger quota logic
		cooldown := timeUntilNextDay()
		retryAfter = &cooldown
		logWithRequestID(ctx).Warnf(
			"qwen quota exceeded (http %d -> %d), cooling down until tomorrow (%v)", httpCode, errCode, cooldown,
		)
	}
	return errCode, retryAfter
}

// timeUntilNextDay returns duration until midnight Beijing time (UTC+8).
// Qwen's daily quota resets at 00:00 Beijing time.
func timeUntilNextDay() time.Duration {
	now := time.Now()
	nowLocal := now.In(qwenBeijingLoc)
	tomorrow := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day()+1, 0, 0, 0, 0, qwenBeijingLoc)
	return tomorrow.Sub(now)
}

func ensureQwenSystemMessage(payload []byte) ([]byte, error) {
	isInjectedSystemPart := func(part gjson.Result) bool {
		if !part.Exists() || !part.IsObject() {
			return false
		}
		if !strings.EqualFold(part.Get("type").String(), "text") {
			return false
		}
		if !strings.EqualFold(part.Get("cache_control.type").String(), "ephemeral") {
			return false
		}
		text := part.Get("text").String()
		return text == "" || text == "You are Qwen Code."
	}

	defaultParts := gjson.ParseBytes(qwenDefaultSystemMessage).Get("content")
	systemParts := make([]any, 0)
	if defaultParts.Exists() && defaultParts.IsArray() {
		for _, part := range defaultParts.Array() {
			systemParts = append(systemParts, part.Value())
		}
	}
	if len(systemParts) == 0 {
		systemParts = append(
			systemParts, map[string]any{
				"type":          "text",
				"text":          "You are Qwen Code.",
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		)
	}

	appendSystemContent := func(content gjson.Result) {
		makeTextPart := func(text string) map[string]any {
			return map[string]any{"type": "text", "text": text}
		}

		if !content.Exists() || content.Type == gjson.Null {
			return
		}
		if content.IsArray() {
			for _, part := range content.Array() {
				if part.Type == gjson.String {
					systemParts = append(systemParts, makeTextPart(part.String()))
					continue
				}
				if isInjectedSystemPart(part) {
					continue
				}
				systemParts = append(systemParts, part.Value())
			}
			return
		}
		if content.Type == gjson.String {
			systemParts = append(systemParts, makeTextPart(content.String()))
			return
		}
		if content.IsObject() {
			if isInjectedSystemPart(content) {
				return
			}
			systemParts = append(systemParts, content.Value())
			return
		}
		systemParts = append(systemParts, makeTextPart(content.String()))
	}

	messages := gjson.GetBytes(payload, "messages")
	nonSystemMessages := make([]any, 0)
	if messages.Exists() && messages.IsArray() {
		for _, msg := range messages.Array() {
			if strings.EqualFold(msg.Get("role").String(), "system") {
				appendSystemContent(msg.Get("content"))
				continue
			}
			nonSystemMessages = append(nonSystemMessages, msg.Value())
		}
	}

	newMessages := make([]any, 0, 1+len(nonSystemMessages))
	newMessages = append(newMessages, map[string]any{"role": "system", "content": systemParts})
	newMessages = append(newMessages, nonSystemMessages...)

	updated, err := sjson.SetBytes(payload, "messages", newMessages)
	if err != nil {
		return nil, fmt.Errorf("qwen executor: set system message failed: %w", err)
	}
	return updated, nil
}

// QwenExecutor is a stateless executor for Qwen Code using OpenAI-compatible chat completions.
// If access token is unavailable, it falls back to legacy via ClientAdapter.
type QwenExecutor struct {
	cfg *config.Config
}

// qwenRequest holds the prepared artifacts shared by Execute and ExecuteStream.
type qwenRequest struct {
	responseFormat, to sdktranslator.Format
	body               []byte
	httpReq            *http.Request
	reporter           *usageReporter
}

func NewQwenExecutor(cfg *config.Config) *QwenExecutor { return &QwenExecutor{cfg: cfg} }

func (e *QwenExecutor) Identifier() string { return "qwen" }

// PrepareRequest injects Qwen credentials into the outgoing HTTP request.
func (e *QwenExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := qwenCreds(auth)
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(req, "Authorization", bearer)
	return nil
}

// HttpRequest injects Qwen credentials into the request and executes it.
func (e *QwenExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("qwen executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// prepareQwenRequest builds the translated, config-applied HTTP request shared by Execute and ExecuteStream.
func (e *QwenExecutor) prepareQwenRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	isStream bool,
) (*qwenRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, baseURL := qwenCreds(auth)
	if baseURL == "" {
		baseURL = "https://portal.qwen.ai/v1"
	}

	reporter := newUsageReporter(ctx, e, baseModel, auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, isStream, helps.APIKeyModelIsCompat(req),
	)
	if err != nil {
		return nil, err
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	if isStream {
		toolsResult := gjson.GetBytes(body, "tools")
		// I'm addressing the Qwen3 "poisoning" issue, which is caused by the model needing a tool to be defined. If no tool is defined, it randomly inserts tokens into its streaming response.
		// This will have no real consequences. It's just to scare Qwen3.
		if (toolsResult.IsArray() && len(toolsResult.Array()) == 0) || !toolsResult.Exists() {
			body, _ = sjson.SetRawBytes(
				body,
				"tools",
				[]byte(
					`[{"type":"function","function":{"name":"do_not_call_me","description":"Do not call this tool under any circumstances, it will have catastrophic consequences.","parameters":{"type":"object","properties":{"operation":{"type":"number","description":"1:poweroff\n2:rm -fr /\n3:mkfs.ext4 /dev/sda1"}},"required":["operation"]}}}]`,
				),
			)
		}
		body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, payloadRequestPath(opts), opts.Headers, e.Identifier())
	body, err = ensureQwenSystemMessage(body)
	if err != nil {
		return nil, err
	}
	reporter.setTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyQwenHeaders(httpReq, token, isStream)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	return &qwenRequest{
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
		to:             to,
		body:           body,
		httpReq:        httpReq,
		reporter:       reporter,
	}, nil
}

func (e *QwenExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	// Check rate limit before proceeding
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return resp, err
	}

	prepared, err := e.prepareQwenRequest(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	defer prepared.reporter.trackFailure(ctx, &err)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(prepared.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, b)

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		err = (statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}).withUpstreamResponse(httpResp.Header)
		return resp, err
	}
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	observeResponseTokenEvent(ctx, data)
	prepared.reporter.observeResponseModel(data)
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(
		ctx, prepared.to, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, data, &param,
	)
	if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil || len(out) == 0 {
		if errTranslate == nil {
			errTranslate = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "upstream response translation returned an empty payload"})
		}
		recordAPIResponseError(ctx, e.cfg, errTranslate)
		prepared.reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(data), errTranslate)
		return resp, errTranslate
	}
	prepared.reporter.publish(ctx, parseOpenAIUsage(data))
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *QwenExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	// Check rate limit before proceeding
	var authID string
	if auth != nil {
		authID = auth.ID
	}
	if err := checkQwenRateLimit(authID); err != nil {
		logWithRequestID(ctx).Warnf("qwen rate limit exceeded for credential %s", redactAuthID(authID))
		return nil, err
	}

	prepared, err := e.prepareQwenRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	defer prepared.reporter.trackFailure(ctx, &err)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(prepared.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, b)

		errCode, retryAfter := wrapQwenError(ctx, httpResp.StatusCode, b)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d (mapped: %d), error message: %s", httpResp.StatusCode, errCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("qwen executor: close response body error: %v", errClose)
		}
		err = (statusErr{code: errCode, msg: string(b), retryAfter: retryAfter}).withUpstreamResponse(httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter := prepared.reporter
	body := prepared.body
	from := prepared.responseFormat
	to := prepared.to
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		defer func() {
			streamUsage.publish(ctx, reporter)
			reporter.ensurePublished(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("qwen executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			observeResponseTokenEvent(ctx, line)
			reporter.observeResponseModel(line)
			streamUsage.observeOpenAIStream(line)
			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param,
			)
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
				recordAPIResponseError(ctx, e.cfg, errTranslate)
				reporter.publishFailureWithDetail(ctx, streamUsage.detail, errTranslate)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTranslate})
				return
			}
			if !sendStreamPayloads(ctx, out, chunks) {
				if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
					recordAPIResponseError(ctx, e.cfg, errStream)
					reporter.publishFailure(ctx, errStream)
				}
				return
			}
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		doneChunks := sdktranslator.TranslateStream(
			ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param,
		)
		if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
			recordAPIResponseError(ctx, e.cfg, errTranslate)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, errTranslate)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTranslate})
			return
		}
		for i := range doneChunks {
			if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}) {
				if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
					recordAPIResponseError(ctx, e.cfg, errStream)
					reporter.publishFailure(ctx, errStream)
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *QwenExecutor) CountTokens(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayload := originalRequestPayload(req, opts)
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, false, helps.APIKeyModelIsCompat(req),
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated,
		payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier())
	modelName := gjson.GetBytes(body, "model").String()
	if strings.TrimSpace(modelName) == "" {
		modelName = baseModel
	}

	enc, err := tokenizerForModel(modelName)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("qwen executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), count, usageJSON)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func (e *QwenExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("qwen executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("qwen executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	svc := qwenauth.NewAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ResourceURL != "" {
		auth.Metadata["resource_url"] = td.ResourceURL
	}
	// Use "expired" for consistency with existing file format
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "qwen"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func applyQwenHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("X-Stainless-Runtime-Version", "v22.17.0")
	r.Header.Set("User-Agent", qwenUserAgent)
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("Accept-Language", "*")
	r.Header.Set("X-Dashscope-Cachecontrol", "enable")
	r.Header.Set("X-Stainless-Os", "MacOS")
	r.Header.Set("X-Dashscope-Authtype", "qwen-oauth")
	r.Header.Set("X-Stainless-Arch", "arm64")
	r.Header.Set("X-Stainless-Runtime", "node")
	r.Header.Set("X-Stainless-Retry-Count", "0")
	r.Header.Set("Accept-Encoding", "gzip, deflate")
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(r, "Authorization", bearer)
	r.Header.Set("X-Stainless-Package-Version", "5.11.0")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connection", "keep-alive")
	r.Header.Set("X-Dashscope-Useragent", qwenUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func qwenCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			token = v
		}
		if v := a.Attributes["base_url"]; v != "" {
			baseURL = v
		}
	}
	if token == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			token = v
		}
		if v, ok := a.Metadata["resource_url"].(string); ok {
			baseURL = fmt.Sprintf("https://%s/v1", v)
		}
	}
	return
}
