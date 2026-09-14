package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg                 *config.Config
	requestLogProvider  string
	oauthProfileFetcher claudeOAuthProfileFetcher
}

const claudeFingerprintSalt = "59cf53e54c78"

const maxClaudeResponseBytes = 50 << 20

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return signature.ProviderFromModelName(baseModel) == signature.ProviderClaude
}

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := helps.IsAnthropicUpstreamURL(req.URL)
	if isAnthropicBase && useAPIKey {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	enforceDeepSeekAnthropicHeaders(req)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newClaudeUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return doClaudeUpstreamRequest(httpClient, httpReq)
}

// claudeRequest holds the prepared artifacts from prepareClaudeRequest.
type claudeRequest struct {
	apiKey                   string
	to, responseFormat       sdktranslator.Format
	translationStream        bool // whether the upstream response uses SSE
	bodyForTranslation       []byte
	oauthToolNamesReverseMap map[string]string
	diagnosticsState         claudeDiagnosticsRequestState
	fastRequest              bool
	httpReq                  *http.Request
	reporter                 *usageReporter
}

type claudeUpstreamHTTPError struct {
	cause error
}

func (e claudeUpstreamHTTPError) Error() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e claudeUpstreamHTTPError) StatusCode() int {
	type statusCoder interface {
		StatusCode() int
	}
	if cause, ok := e.cause.(statusCoder); ok {
		return cause.StatusCode()
	}
	return 0
}

func (e claudeUpstreamHTTPError) Unwrap() error {
	return e.cause
}

type claudeUpstreamSSEError struct {
	statusErr
	errorType string
}

func (e claudeUpstreamSSEError) Unwrap() error {
	return e.statusErr
}

func (e claudeUpstreamSSEError) rejectsReplayState() bool {
	switch strings.ToLower(strings.TrimSpace(e.errorType)) {
	case "invalid_request_error", "bad_request_error":
		return true
	default:
		return false
	}
}

type claudeStreamOutcome uint8

const (
	claudeStreamOutcomeIncomplete claudeStreamOutcome = iota
	claudeStreamOutcomeComplete
	claudeStreamOutcomeUpstreamError
	claudeStreamOutcomeValidationFailure
	claudeStreamOutcomeReadFailure
	claudeStreamOutcomeCanceled
)

type claudeStreamObserver interface {
	ObserveClaudeSSELine([]byte)
	FinishClaudeStream(claudeStreamOutcome)
}

type claudeStreamingValidator struct {
	hasData         bool
	hasMessageStart bool
	hasMessageDelta bool
	hasMessageStop  bool
}

func (v *claudeStreamingValidator) observe(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	v.hasData = true
	if !gjson.ValidBytes(payload) {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "error":
		errorType := strings.TrimSpace(root.Get("error.type").String())
		message := strings.TrimSpace(root.Get("error.message").String())
		if message == "" {
			message = errorType
		}
		if message == "" {
			message = "unknown upstream error"
		}
		return claudeUpstreamSSEError{
			code:      http.StatusBadGateway,
			msg:       "claude executor: upstream returned error event: " + message,
			errorType: errorType,
		}
	case "message_start":
		message := root.Get("message")
		if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
			return statusErr{
				code: http.StatusBadGateway,
				msg:  "claude executor: upstream stream message_start is missing id or model",
			}
		}
		v.hasMessageStart = true
	case "message_delta":
		v.hasMessageDelta = true
	case "message_stop":
		v.hasMessageStop = true
	}
	return nil
}

func (v *claudeStreamingValidator) finish() error {
	if !v.hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !v.hasMessageStart {
		return statusErr{
			code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start",
		}
	}
	if !v.hasMessageDelta {
		return statusErr{
			code: http.StatusBadGateway,
			msg:  "claude executor: upstream stream response ended before message completion",
		}
	}
	if !v.hasMessageStop {
		return statusErr{
			code: http.StatusBadGateway,
			msg:  "claude executor: upstream stream response is missing message_stop",
		}
	}
	return nil
}

func consumeClaudeStream(
	ctx context.Context,
	cfg *config.Config,
	body io.Reader,
	out chan cliproxyexecutor.StreamChunk,
	reporter *usageReporter,
	restoreOAuthToolNames bool,
	reverseMap map[string]string,
	fastRequest bool,
	statusCode int,
	transform func([]byte) [][]byte,
	observer claudeStreamObserver,
) {
	var streamUsage claudeStreamUsageBuffer
	outcome := claudeStreamOutcomeIncomplete
	defer func() {
		if observer != nil {
			observer.FinishClaudeStream(outcome)
		}
		if !streamUsage.publish(ctx, reporter) {
			reporter.ensurePublished(ctx)
		}
	}()

	validator := &claudeStreamingValidator{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, maxClaudeResponseBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		appendAPIResponseChunk(ctx, cfg, line)
		if observer != nil {
			observer.ObserveClaudeSSELine(line)
		}
		if errValidate := validator.observe(line); errValidate != nil {
			if _, ok := errors.AsType[claudeUpstreamSSEError](errValidate); ok {
				outcome = claudeStreamOutcomeUpstreamError
			} else {
				outcome = claudeStreamOutcomeValidationFailure
			}
			errValidate = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errValidate))
			sendClaudeStreamError(ctx, cfg, out, reporter, &streamUsage, errValidate)
			return
		}
		streamUsage.observe(line)
		if restoreOAuthToolNames {
			restoredLine, errRestore := restoreClaudeOAuthToolNamesFromStreamLine(line, reverseMap)
			if errRestore != nil {
				outcome = claudeStreamOutcomeValidationFailure
				errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
				errRestore = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errRestore))
				sendClaudeStreamError(ctx, cfg, out, reporter, &streamUsage, errRestore)
				return
			}
			line = restoredLine
		}
		if !sendStreamPayloads(ctx, out, transform(bytes.Clone(line))) {
			if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
				canceled := false
				if cancelErr := newClaudeOAuthCancellationError(
					ctx, restoreOAuthToolNames, errStream,
				); cancelErr != nil {
					errStream = cancelErr
					canceled = true
				} else {
					errStream = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errStream))
				}
				recordAPIResponseError(ctx, cfg, errStream)
				streamUsage.publishFailure(ctx, reporter, errStream)
				if canceled {
					sendClaudeCancellationResult(out, errStream)
				}
			}
			outcome = claudeStreamOutcomeCanceled
			return
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		if ctx != nil && ctx.Err() != nil {
			outcome = claudeStreamOutcomeCanceled
		} else {
			outcome = claudeStreamOutcomeReadFailure
		}
		if cancelErr := newClaudeOAuthCancellationError(ctx, restoreOAuthToolNames, errScan); cancelErr != nil {
			errScan = cancelErr
			recordAPIResponseError(ctx, cfg, errScan)
			streamUsage.publishFailure(ctx, reporter, errScan)
			sendClaudeCancellationResult(out, errScan)
			return
		}
		errScan = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errScan))
		sendClaudeStreamError(ctx, cfg, out, reporter, &streamUsage, errScan)
		return
	}
	if errValidate := validator.finish(); errValidate != nil {
		outcome = claudeStreamOutcomeIncomplete
		errValidate = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errValidate))
		sendClaudeStreamError(ctx, cfg, out, reporter, &streamUsage, errValidate)
		return
	}
	outcome = claudeStreamOutcomeComplete
}

func sendClaudeCancellationResult(out chan cliproxyexecutor.StreamChunk, err error) {
	chunk := cliproxyexecutor.StreamChunk{Err: err}
	select {
	case out <- chunk:
		return
	default:
	}
	select {
	case <-out:
	default:
	}
	select {
	case out <- chunk:
	default:
	}
}

func sendClaudeStreamError(
	ctx context.Context,
	cfg *config.Config,
	out chan<- cliproxyexecutor.StreamChunk,
	reporter *usageReporter,
	streamUsage *claudeStreamUsageBuffer,
	err error,
) {
	recordAPIResponseError(ctx, cfg, err)
	streamUsage.publishFailure(ctx, reporter, err)
	_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: err})
}

// prepareClaudeRequest builds the translated, cloaked, and header-enriched HTTP request
// shared by Execute and ExecuteStream. The downstream response format determines
// whether Execute requests Claude SSE, while ExecuteStream always forces SSE.
func (e *ClaudeExecutor) prepareClaudeRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	forceStream bool,
) (*claudeRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	oauthToken := isClaudeOAuthToken(apiKey)

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	upstreamStream := forceStream || responseFormat != to
	isCompat := helps.APIKeyModelIsCompat(req)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	incomingHeaders, claudeCodeDetection := detectIncomingClaudeCodeRequest(
		ctx,
		opts.Headers,
		originalPayload,
		false,
		e.cfg,
	)
	confirmedClaudeCode := claudeCodeDetection.Confirmed
	claudeSessionID := ""
	if oauthToken {
		claudeSessionID = helps.ClaudeAgentSessionUUIDForRequest(
			incomingHeaders,
			originalPayload,
			req.Payload,
			confirmedClaudeCode,
			opts.Metadata,
			req.Metadata,
		)
	}
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		originalPayload,
		upstreamStream,
		isCompat,
	)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		upstreamStream,
		isCompat,
	)
	body = helps.SetStringIfDifferent(body, "model", baseModel)

	body, err := helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	var cloaked bool
	body, cloaked, err = applyCloaking(
		ctx,
		e.cfg,
		auth,
		body,
		baseModel,
		apiKey,
		confirmedClaudeCode,
	)
	if err != nil {
		return nil, err
	}

	diagnosticsState := claudeDiagnosticsRequestState{}
	contextManagementState := claudeCodeContextManagementState{
		eligible:    cloaked && isAnthropicUpstreamBase(baseURL),
		callerOwned: gjson.GetBytes(body, "context_management").Exists(),
	}
	if contextManagementState.eligible {
		body, contextManagementState.automaticallyInjected = injectClaudeCodeContextManagement(body)
		if oauthToken {
			body, diagnosticsState = injectClaudeDiagnostics(body, auth, claudeSessionID)
		}
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body, contextManagementState.payloadRuleTouched = applyPayloadConfigWithRootTracked(
		e.cfg,
		baseModel,
		to.String(),
		"",
		body,
		originalTranslated,
		requestedModel,
		"context_management",
	)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = reconcileClaudeCodeContextManagement(body, contextManagementState)
	body = normalizeClaudeSamplingForUpstream(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	if countCacheControls(body) == 0 {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// (e.g. Amp CLI) already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	body = normalizeCacheControlTTL(body)
	// Payload rules may rewrite stream. Keep the request body, transport headers,
	// response parser, and downstream response format on one authority.
	body = helps.SetBoolIfDifferent(body, "stream", upstreamStream)

	// Clamp max_tokens to the model's documented limit (works around Claude Code SDK off-by-one).
	body = capMaxTokens(body, baseModel)
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || isCompat {
		body, _ = signature.SanitizeClaudeMessagesForClaudeUpstream(
			body,
			baseModel,
			isCompat,
		)
	}

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	extraBetas = appendClaudeFastModeBeta(body, extraBetas)
	if thinking.HasContext1MTag(req.Model) && !containsTrimmedString(extraBetas, "context-1m-2025-08-07") {
		extraBetas = append(extraBetas, "context-1m-2025-08-07")
	}
	bodyForTranslation := body
	bodyForUpstream := body
	var oauthToolNamesReverseMap map[string]string
	if oauthToken && cloaked {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(
			bodyForUpstream,
			resolveClaudeMCPAliasOptions(ctx),
		)
	}
	if oauthToken {
		bodyForUpstream, _, err = helps.ApplyClaudeCredentialMetadata(bodyForUpstream, auth, claudeSessionID)
		if err != nil {
			return nil, fmt.Errorf("apply Claude credential metadata: %w", err)
		}
	}
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) {
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}
	reporter.setTranslatedReasoningEffort(bodyForUpstream, to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	if errHeaders := applyClaudeHeaders(
		httpReq,
		auth,
		apiKey,
		upstreamStream,
		extraBetas,
		bodyForUpstream,
		e.cfg,
		incomingHeaders,
		confirmedClaudeCode && !cloaked,
		claudeSessionID,
	); errHeaders != nil {
		return nil, errHeaders
	}
	fastRequest := isAnthropicUpstreamBase(baseURL) && claudeRequestIsFast(httpReq, bodyForUpstream)
	recordUpstreamRequest(
		ctx,
		e.cfg,
		url,
		httpReq.Header.Clone(),
		bodyForUpstream,
		e.upstreamRequestLogProvider(),
		auth,
	)

	return &claudeRequest{
		apiKey:                   apiKey,
		to:                       to,
		responseFormat:           responseFormat,
		translationStream:        upstreamStream,
		bodyForTranslation:       bodyForTranslation,
		oauthToolNamesReverseMap: oauthToolNamesReverseMap,
		diagnosticsState:         diagnosticsState,
		fastRequest:              fastRequest,
		httpReq:                  httpReq,
		reporter:                 reporter,
	}, nil
}

func (e *ClaudeExecutor) openClaudeResponse(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	request *claudeRequest,
) (*http.Response, io.ReadCloser, error) {
	httpClient := newClaudeUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := doClaudeUpstreamRequest(httpClient, request.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, wrapClaudeFastRequestError(request.fastRequest, 0, err)
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses and support both declared and magic-byte-detected compression.
		errBody, errDecode := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
		if errDecode != nil {
			recordAPIResponseError(ctx, e.cfg, errDecode)
			message := fmt.Sprintf("failed to decode error response body: %v", errDecode)
			logWithRequestID(ctx).Warn(message)
			return nil, nil, wrapClaudeFastRequestError(
				request.fastRequest,
				httpResp.StatusCode,
				claudeUpstreamHTTPError{newUpstreamStatusErrMessage(httpResp.StatusCode, message, httpResp.Header)},
			)
		}
		body, errRead := io.ReadAll(io.LimitReader(errBody, 50<<20))
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			message := fmt.Sprintf("failed to read error response body: %v", errRead)
			logWithRequestID(ctx).Warn(message)
			body = []byte(message)
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
		)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		if request.fastRequest {
			return nil, nil, newClaudeFastDirectResponseError(httpResp, body)
		}
		return nil, nil, claudeUpstreamHTTPError{
			classifyClaudeUpstreamError(httpResp.StatusCode, body, httpResp.Header),
		}
	}
	decodedBody, errDecode := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
	if errDecode != nil {
		recordAPIResponseError(ctx, e.cfg, errDecode)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, nil, wrapClaudeFastRequestError(
			request.fastRequest, httpResp.StatusCode, markResponseStarted(errDecode),
		)
	}
	return httpResp, decodedBody, nil
}

func (e *ClaudeExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	req, thinkingReplayScope := prepareClaudeThinkingReplayRequest(ctx, auth, req, opts)
	defer func() {
		clearClaudeThinkingReplayAfterError(ctx, thinkingReplayScope, err)
	}()

	cr, err := e.prepareClaudeRequest(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	defer cr.reporter.trackFailure(ctx, &err)

	httpResp, decodedBody, err := e.openClaudeResponse(ctx, auth, cr)
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(decodedBody, maxClaudeResponseBytes+1))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(err))
	}
	if len(data) > maxClaudeResponseBytes {
		err = newUpstreamStatusErrMessage(
			http.StatusBadGateway,
			"claude executor: upstream response exceeds 50 MiB limit",
			httpResp.Header,
		)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(err))
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	oauthToken := isClaudeOAuthToken(cr.apiKey)
	if cr.translationStream {
		var streamUsage claudeStreamUsageBuffer
		validator := &claudeStreamingValidator{}
		lines := bytes.Split(data, []byte("\n"))
		for index, line := range lines {
			if errValidate := validator.observe(line); errValidate != nil {
				recordAPIResponseError(ctx, e.cfg, errValidate)
				streamUsage.publishFailure(ctx, cr.reporter, errValidate)
				err = wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(errValidate))
				return resp, err
			}
			streamUsage.observe(line)
			if oauthToken {
				restoredLine, errRestore := restoreClaudeOAuthToolNamesFromStreamLine(
					line,
					cr.oauthToolNamesReverseMap,
				)
				if errRestore != nil {
					errRestore = fmt.Errorf("restore Claude OAuth tool name from streaming response: %w", errRestore)
					recordAPIResponseError(ctx, e.cfg, errRestore)
					streamUsage.publishFailure(ctx, cr.reporter, errRestore)
					err = wrapClaudeFastRequestError(
						cr.fastRequest, httpResp.StatusCode, markResponseStarted(errRestore),
					)
					return resp, err
				}
				lines[index] = restoredLine
			}
		}
		if errValidate := validator.finish(); errValidate != nil {
			recordAPIResponseError(ctx, e.cfg, errValidate)
			streamUsage.publishFailure(ctx, cr.reporter, errValidate)
			err = wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(errValidate))
			return resp, err
		}
		if !streamUsage.publish(ctx, cr.reporter) {
			cr.reporter.ensurePublished(ctx)
		}
		commitClaudeDiagnostics(cr.diagnosticsState, claudeMessageIDFromSSE(data))
		data = bytes.Join(lines, []byte("\n"))
		cacheClaudeThinkingReplayResponse(ctx, thinkingReplayScope, data)
	} else {
		commitClaudeDiagnostics(cr.diagnosticsState, claudeMessageIDFromResponse(data))
		cr.reporter.publish(ctx, parseClaudeUsage(data))
		if oauthToken {
			var errRestore error
			data, errRestore = restoreClaudeOAuthToolNamesFromResponse(
				data,
				cr.oauthToolNamesReverseMap,
			)
			if errRestore != nil {
				errRestore = fmt.Errorf("restore Claude OAuth tool name from response: %w", errRestore)
				recordAPIResponseError(ctx, e.cfg, errRestore)
				err = wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(errRestore))
				return resp, err
			}
		}
		cacheClaudeThinkingReplayResponse(ctx, thinkingReplayScope, data)
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		cr.to,
		cr.responseFormat,
		req.Model,
		opts.OriginalRequest,
		cr.bodyForTranslation,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	req, thinkingReplayScope := prepareClaudeThinkingReplayRequest(ctx, auth, req, opts)
	result, err := e.executeStreamWithObserver(
		ctx,
		auth,
		req,
		opts,
		newClaudeThinkingReplayStreamObserver(ctx, thinkingReplayScope),
	)
	clearClaudeThinkingReplayAfterError(ctx, thinkingReplayScope, err)
	return result, err
}

func (e *ClaudeExecutor) executeStreamWithObserver(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	observer claudeStreamObserver,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	cr, err := e.prepareClaudeRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	oauthToken := isClaudeOAuthToken(cr.apiKey)
	defer func() {
		if cancelErr := newClaudeOAuthCancellationError(ctx, oauthToken, err); cancelErr != nil {
			err = cancelErr
		}
	}()
	observer = combineClaudeStreamObservers(
		observer,
		newClaudeDiagnosticsStreamObserver(cr.diagnosticsState),
	)
	defer cr.reporter.trackFailure(ctx, &err)

	httpResp, decodedBody, err := e.openClaudeResponse(ctx, auth, cr)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	transform := func(line []byte) [][]byte {
		return [][]byte{append(line, '\n')}
	}
	if cr.responseFormat != cr.to {
		var param any
		transform = func(line []byte) [][]byte {
			return sdktranslator.TranslateStream(
				ctx,
				cr.to,
				cr.responseFormat,
				req.Model,
				opts.OriginalRequest,
				cr.bodyForTranslation,
				line,
				&param,
			)
		}
	}
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()
		consumeClaudeStream(
			ctx,
			e.cfg,
			decodedBody,
			out,
			cr.reporter,
			oauthToken,
			cr.oauthToolNamesReverseMap,
			cr.fastRequest,
			httpResp.StatusCode,
			transform,
			observer,
		)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *ClaudeExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	stream := from != to
	isCompat := helps.APIKeyModelIsCompat(req)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		stream,
		isCompat,
	)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || isCompat {
		body, _ = signature.SanitizeClaudeMessagesForClaudeUpstream(
			body,
			baseModel,
			isCompat,
		)
	}
	if errValidate := validateClaudeTokenCountRequest(body); errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	count, err := countClaudeInputTokens(body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("claude executor: local token counting failed: %w", err)
	}
	usageJSON := []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
	out := sdktranslator.TranslateTokenCount(ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), count, usageJSON)
	return cliproxyexecutor.Response{Payload: out}, nil
}

type claudeTokenCountValidationError struct {
	statusErr
}

func (claudeTokenCountValidationError) IsRequestScoped() bool {
	return true
}

func newClaudeTokenCountValidationError(message string) error {
	return claudeTokenCountValidationError{statusErr{code: http.StatusBadRequest, msg: message}}
}

func validateClaudeTokenCountRequest(body []byte) error {
	if !gjson.ValidBytes(body) {
		return newClaudeTokenCountValidationError("invalid Claude token count request JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return newClaudeTokenCountValidationError("Claude token count request must be a JSON object")
	}
	messages := root.Get("messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return newClaudeTokenCountValidationError("Claude token count request messages must be a non-empty array")
	}
	for _, message := range messages.Array() {
		if !message.IsObject() {
			return newClaudeTokenCountValidationError("Claude token count request messages must contain objects")
		}
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			return newClaudeTokenCountValidationError("Claude token count request message role must be user or assistant")
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			continue
		}
		if !content.IsArray() {
			return newClaudeTokenCountValidationError("Claude token count request message content must be a string or array")
		}
		for _, block := range content.Array() {
			if !block.IsObject() || block.Get("type").Type != gjson.String || block.Get("type").String() == "" {
				return newClaudeTokenCountValidationError("Claude token count request content blocks must be typed objects")
			}
		}
	}
	return nil
}

func (e *ClaudeExecutor) countTokensUpstream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	oauthToken := isClaudeOAuthToken(apiKey)
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	incomingHeaders, claudeCodeDetection := detectIncomingClaudeCodeRequest(
		ctx,
		opts.Headers,
		originalPayload,
		true,
		e.cfg,
	)
	confirmedClaudeCode := claudeCodeDetection.Confirmed
	claudeSessionID := ""
	if oauthToken {
		claudeSessionID = helps.ClaudeAgentSessionUUIDForRequest(
			incomingHeaders,
			originalPayload,
			req.Payload,
			confirmedClaudeCode,
			opts.Metadata,
			req.Metadata,
		)
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	isCompat := helps.APIKeyModelIsCompat(req)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		stream,
		isCompat,
	)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	directAnthropic := isAnthropicUpstreamBase(baseURL)
	var cloaked bool
	if directAnthropic {
		policy, settings := resolveClaudeWirePolicy(e.cfg, auth, apiKey, confirmedClaudeCode)
		cloaked = policy.Cloak
		if cloaked {
			if !settings.strictMode {
				if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(body, "system")); errSystem != nil {
					return cliproxyexecutor.Response{}, errSystem
				}
			}
			body = relocateClaudeSystemPromptForCountTokens(body, settings.strictMode)
			if len(settings.sensitiveWords) > 0 {
				matcher := buildSensitiveWordMatcher(settings.sensitiveWords)
				body = obfuscateSensitiveWords(body, matcher)
			}
		}
	} else {
		var errCloaking error
		body, cloaked, errCloaking = applyCloaking(
			ctx,
			e.cfg,
			auth,
			body,
			baseModel,
			apiKey,
			confirmedClaudeCode,
		)
		if errCloaking != nil {
			return cliproxyexecutor.Response{}, errCloaking
		}
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	extraBetas = append(extraBetas, claudeTokenCountingBeta)
	if oauthToken && cloaked {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, resolveClaudeMCPAliasOptions(ctx))
	}
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || isCompat {
		body, _ = signature.SanitizeClaudeMessagesForClaudeUpstream(
			body,
			baseModel,
			isCompat,
		)
	}
	if directAnthropic {
		body, _ = sjson.DeleteBytes(body, "metadata")
		body, _ = sjson.DeleteBytes(body, "context_management")
		body, _ = sjson.DeleteBytes(body, "diagnostics")
	}

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errHeaders := applyClaudeHeaders(
		httpReq,
		auth,
		apiKey,
		false,
		extraBetas,
		body,
		e.cfg,
		incomingHeaders,
		confirmedClaudeCode && !cloaked,
		claudeSessionID,
	); errHeaders != nil {
		return cliproxyexecutor.Response{}, errHeaders
	}
	recordUpstreamRequest(
		ctx,
		e.cfg,
		url,
		httpReq.Header.Clone(),
		body,
		e.upstreamRequestLogProvider(),
		auth,
	)

	httpClient := newClaudeUtlsHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := doClaudeUpstreamRequest(httpClient, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (maybe empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.
		errBody, decErr := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
		if decErr != nil {
			recordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			logWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, newUpstreamStatusErrMessage(resp.StatusCode, msg, resp.Header)
		}
		b, readErr := io.ReadAll(io.LimitReader(errBody, 50<<20))
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			logWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		appendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, newUpstreamStatusErr(resp.StatusCode, b, resp.Header)
	}
	decodedBody, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(decodedBody, 50<<20))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, from, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	refreshToken := claudeauth.ReadMetadataString(&auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	claudeauth.EnsureMetadataMap(&auth.Metadata)
	claudeauth.StoreMetadataValue(&auth.Metadata, "access_token", td.AccessToken)
	claudeauth.StoreMetadataString(&auth.Metadata, "refresh_token", td.RefreshToken)
	claudeauth.StoreMetadataString(&auth.Metadata, "email", td.Email)
	claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", td.AccountUUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_uuid", td.OrganizationUUID)
	claudeauth.StoreMetadataString(&auth.Metadata, "organization_name", td.OrganizationName)
	claudeauth.StoreMetadataValue(&auth.Metadata, "expired", td.Expire)
	claudeauth.StoreMetadataValue(&auth.Metadata, "type", "claude")
	claudeauth.StoreMetadataValue(&auth.Metadata, "last_refresh", time.Now().Format(time.RFC3339))
	return auth, nil
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func containsTrimmedString(values []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

func appendClaudeFastModeBeta(body []byte, betas []string) []string {
	speed := gjson.GetBytes(body, "speed")
	if speed.Type != gjson.String || !strings.EqualFold(strings.TrimSpace(speed.String()), "fast") {
		return betas
	}
	for _, beta := range betas {
		if strings.TrimSpace(beta) == claudeFastModeBeta {
			return betas
		}
	}
	return append(betas, claudeFastModeBeta)
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
	}
	return body
}

func normalizeClaudeSamplingForUpstream(body []byte) []byte {
	body, _ = sjson.DeleteBytes(body, "temperature")
	body, _ = sjson.DeleteBytes(body, "top_p")

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		body, _ = sjson.DeleteBytes(body, "top_p")
		body, _ = sjson.DeleteBytes(body, "top_k")
	}
	return body
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func newClaudeZstdReader(reader io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(
		reader,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(maxClaudeResponseBytes),
		zstd.WithDecoderMaxMemory(maxClaudeResponseBytes+(8<<20)),
	)
}

func claudeResponseContentEncoding(header http.Header) string {
	return strings.Join(header.Values("Content-Encoding"), ",")
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header. Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := newClaudeZstdReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}

	encodings := strings.Split(contentEncoding, ",")
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	cleanup := func() {
		for _, decoderCloser := range slices.Backward(decoderClosers) {
			_ = decoderCloser()
		}
		_ = body.Close()
	}
	for _, encoding := range slices.Backward(encodings) {
		encoding := strings.TrimSpace(strings.ToLower(encoding))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			// Ownership transfers to decoderClosers and then to the returned compositeReadCloser.
			// noinspection GoResourceLeak
			gzipReader, errGzip := gzip.NewReader(reader)
			if errGzip != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create gzip reader: %w", errGzip)
			}
			reader = gzipReader
			decoderClosers = append(decoderClosers, gzipReader.Close)
		case "deflate":
			// Ownership transfers to decoderClosers and then to the returned compositeReadCloser.
			// noinspection GoResourceLeak
			deflateReader, errDeflate := newClaudeDeflateReader(reader)
			if errDeflate != nil {
				cleanup()
				return nil, errDeflate
			}
			reader = deflateReader
			decoderClosers = append(decoderClosers, deflateReader.Close)
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			decoder, errZstd := newClaudeZstdReader(reader)
			if errZstd != nil {
				cleanup()
				return nil, fmt.Errorf("failed to create zstd reader: %w", errZstd)
			}
			reader = decoder
			decoderClosers = append(
				decoderClosers, func() error {
					decoder.Close()
					return nil
				},
			)
		default:
			cleanup()
			return nil, fmt.Errorf("unsupported content encoding %q", encoding)
		}
	}
	if len(decoderClosers) == 0 && reader == body {
		return body, nil
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for _, decoderCloser := range slices.Backward(decoderClosers) {
		closers = append(closers, decoderCloser)
	}
	closers = append(closers, body.Close)
	return &compositeReadCloser{Reader: reader, closers: closers}, nil
}

func newClaudeDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, errPeek := buffered.Peek(2)
	if errPeek == nil && isZlibHeader(header) {
		zlibReader, errZlib := zlib.NewReader(buffered)
		if errZlib != nil {
			return nil, fmt.Errorf("failed to create zlib deflate reader: %w", errZlib)
		}
		return zlibReader, nil
	}
	return flate.NewReader(buffered), nil
}

func isZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}

func isDeepSeekAnthropicRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || !strings.EqualFold(r.URL.Hostname(), "api.deepseek.com") {
		return false
	}
	path := strings.ToLower(strings.TrimSpace(r.URL.Path))
	return path == "/anthropic" || strings.HasPrefix(path, "/anthropic/")
}

func enforceDeepSeekAnthropicHeaders(r *http.Request) {
	if !isDeepSeekAnthropicRequest(r) {
		return
	}
	r.Header.Del("Anthropic-Version")
	r.Header.Del("x-api-key")
}

func applyClaudeHeaders(
	r *http.Request,
	auth *cliproxyauth.Auth,
	apiKey string,
	stream bool,
	extraBetas []string,
	body []byte,
	cfg *config.Config,
	incomingHeaders http.Header,
	confirmedClaudeCode bool,
	sessionIDs ...string,
) error {
	if r == nil {
		return nil
	}
	hdrDefault := func(cfgVal, fallback string) string {
		if cfgVal != "" {
			return cfgVal
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	hasAPIKeyAttr := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	oauthToken := isClaudeOAuthToken(apiKey) || !hasAPIKeyAttr
	useAPIKey := !oauthToken
	isAnthropicBase := helps.IsAnthropicUpstreamURL(r.URL)
	if isAnthropicBase && useAPIKey {
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", apiKey)
	} else {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")

	if incomingHeaders == nil {
		if ginCtx, ok := util.GinContextValue(r.Context()).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			incomingHeaders = ginCtx.Request.Header
		}
	}
	stabilizeDeviceProfile := helps.ClaudeDeviceProfileStabilizationEnabled(cfg)
	var deviceProfile helps.ClaudeDeviceProfile
	if stabilizeDeviceProfile && confirmedClaudeCode {
		var errDeviceProfile error
		deviceProfile, errDeviceProfile = helps.ResolveClaudeDeviceProfileRequired(
			r.Context(), auth, apiKey, incomingHeaders, cfg,
		)
		if errDeviceProfile != nil {
			return errDeviceProfile
		}
	}

	incomingBetas := strings.TrimSpace(strings.Join(incomingHeaders.Values("Anthropic-Beta"), ","))
	countTokens := r.URL != nil && strings.HasSuffix(r.URL.Path, "/count_tokens")
	baseBetas := claudeCodeCLIBetas(body, claudeRequestedBetas(incomingBetas, extraBetas), oauthToken)
	if countTokens {
		baseBetas = claudeCountTokensBetasForCredential(oauthToken)
	}
	if confirmedClaudeCode && incomingBetas != "" {
		baseBetas = incomingBetas
		if oauthToken {
			if countTokens {
				baseBetas = withClaudeCountTokensOAuthBeta(baseBetas)
			} else {
				baseBetas = withClaudeOAuthCredentialBetas(baseBetas)
			}
		}
	}
	existingSet := make(map[string]bool)
	for beta := range strings.SplitSeq(baseBetas, ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			existingSet[beta] = true
		}
	}
	appendBeta := func(beta string) {
		beta = strings.TrimSpace(beta)
		if beta == "" || existingSet[beta] {
			return
		}
		baseBetas += "," + beta
		existingSet[beta] = true
	}
	if !confirmedClaudeCode && incomingBetas != "" && !isAnthropicBase {
		for beta := range strings.SplitSeq(incomingBetas, ",") {
			appendBeta(beta)
		}
	}
	if !isAnthropicBase {
		for _, beta := range extraBetas {
			appendBeta(beta)
		}
	}
	r.Header.Set("Anthropic-Beta", baseBetas)

	identityHeader := func(name, fallback string) {
		if confirmedClaudeCode {
			misc.EnsureHeader(r.Header, incomingHeaders, name, fallback)
			return
		}
		r.Header.Set(name, fallback)
	}
	if !isDeepSeekAnthropicRequest(r) {
		identityHeader("Anthropic-Version", "2023-06-01")
	}
	identityHeader("Anthropic-Dangerous-Direct-Browser-Access", "true")
	identityHeader("X-App", "cli")
	identityHeader("X-Stainless-Retry-Count", "0")
	identityHeader("X-Stainless-Runtime", "node")
	identityHeader("X-Stainless-Lang", "js")
	if !countTokens {
		identityHeader("X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	} else if confirmedClaudeCode {
		if incomingTimeout := incomingHeaders.Get("X-Stainless-Timeout"); incomingTimeout != "" {
			r.Header.Set("X-Stainless-Timeout", incomingTimeout)
		}
	}

	sessionID := ""
	for _, candidate := range sessionIDs {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			sessionID = candidate
			break
		}
	}
	if sessionID != "" {
		r.Header.Set("X-Claude-Code-Session-Id", sessionID)
	} else {
		var errSessionID error
		sessionID, errSessionID = helps.CachedSessionIDRequired(r.Context(), apiKey)
		if errSessionID != nil {
			return errSessionID
		}
		identityHeader("X-Claude-Code-Session-Id", sessionID)
	}
	if isAnthropicBase {
		identityHeader("x-client-request-id", uuid.NewString())
	}
	r.Header.Set("Connection", "keep-alive")

	applyTransportNegotiation := func() {
		if stream && !isAnthropicBase {
			r.Header.Set("Accept", "text/event-stream")
			r.Header.Set("Accept-Encoding", "identity")
			return
		}
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	applyTransportNegotiation()
	if stabilizeDeviceProfile {
		if confirmedClaudeCode {
			helps.ApplyClaudeDeviceProfileHeaders(r, deviceProfile)
		} else {
			helps.ApplyClaudeDefaultDeviceProfileHeaders(r, cfg)
		}
	} else {
		helps.ApplyClaudeLegacyDeviceHeaders(r, incomingHeaders, cfg, confirmedClaudeCode)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	enforceDeepSeekAnthropicHeaders(r)
	if isAnthropicBase {
		r.Header.Set("Anthropic-Beta", baseBetas)
		applyTransportNegotiation()
	} else if stream {
		applyTransportNegotiation()
	}
	return nil
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(
		func(_, message gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
				movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
				return true
			}
			keptMessages = append(keptMessages, message.Raw)
			return true
		},
	)
	if len(movedSystemParts) == 0 {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, errSetSystem := sjson.SetRawBytes(
			payload, "system", rawJSONArray(systemParts),
		); errSetSystem == nil {
			payload = updated
		}
	}
	updated, errSetMessages := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages))
	if errSetMessages == nil {
		payload = updated
	}
	return payload
}

func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	var parts []string
	content.ForEach(
		func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				text := item.String()
				if strings.TrimSpace(text) != "" {
					block := []byte(`{"type":"text","text":""}`)
					block, _ = sjson.SetBytes(block, "text", text)
					parts = append(parts, string(block))
				}
				return true
			}
			if item.IsObject() && item.Get("type").String() == "text" && strings.TrimSpace(item.Get("text").String()) != "" {
				parts = append(parts, item.Raw)
			}
			return true
		},
	)
	return parts
}

func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names (those with a non-empty "type" field) so we can
	// skip them consistently in both tools and message history.
	builtinTools := map[string]bool{}
	for _, name := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		builtinTools[name] = true
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(
			func(index, tool gjson.Result) bool {
				// Skip built-in tools (web_search, code_execution, etc.) which have
				// a "type" field and require their name to remain unchanged.
				if tool.Get("type").Exists() && tool.Get("type").String() != "" {
					if n := tool.Get("name").String(); n != "" {
						builtinTools[n] = true
					}
					return true
				}
				name := tool.Get("name").String()
				if name == "" || strings.HasPrefix(name, prefix) {
					return true
				}
				path := fmt.Sprintf("tools.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, prefix+name)
				return true
			},
		)
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(
			func(msgIndex, msg gjson.Result) bool {
				content := msg.Get("content")
				if !content.Exists() || !content.IsArray() {
					return true
				}
				content.ForEach(
					func(contentIndex, part gjson.Result) bool {
						partType := part.Get("type").String()
						switch partType {
						case "tool_use":
							name := part.Get("name").String()
							if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
								return true
							}
							path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
							body, _ = sjson.SetBytes(body, path, prefix+name)
						case "tool_reference":
							toolName := part.Get("tool_name").String()
							if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
								return true
							}
							path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
							body, _ = sjson.SetBytes(body, path, prefix+toolName)
						case "tool_result":
							// Handle nested tool_reference blocks inside tool_result.content[]
							nestedContent := part.Get("content")
							if nestedContent.Exists() && nestedContent.IsArray() {
								nestedContent.ForEach(
									func(nestedIndex, nestedPart gjson.Result) bool {
										if nestedPart.Get("type").String() == "tool_reference" {
											nestedToolName := nestedPart.Get("tool_name").String()
											if nestedToolName != "" && !strings.HasPrefix(
												nestedToolName, prefix,
											) && !builtinTools[nestedToolName] {
												nestedPath := fmt.Sprintf(
													"messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(),
													contentIndex.Int(), nestedIndex.Int(),
												)
												body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
											}
										}
										return true
									},
								)
							}
						}
						return true
					},
				)
				return true
			},
		)
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(
		func(index, part gjson.Result) bool {
			partType := part.Get("type").String()
			switch partType {
			case "tool_use":
				name := part.Get("name").String()
				if !strings.HasPrefix(name, prefix) {
					return true
				}
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
			case "tool_reference":
				toolName := part.Get("tool_name").String()
				if !strings.HasPrefix(toolName, prefix) {
					return true
				}
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
			case "tool_result":
				// Handle nested tool_reference blocks inside tool_result.content[]
				nestedContent := part.Get("content")
				if nestedContent.Exists() && nestedContent.IsArray() {
					nestedContent.ForEach(
						func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if strings.HasPrefix(nestedToolName, prefix) {
									nestedPath := fmt.Sprintf(
										"content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int(),
									)
									body, _ = sjson.SetBytes(
										body, nestedPath, strings.TrimPrefix(nestedToolName, prefix),
									)
								}
							}
							return true
						},
					)
				}
			}
			return true
		},
	)
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

// getCloakConfigFromAuth extracts cloak configuration from auth attributes and metadata.
func getCloakConfigFromAuth(auth *cliproxyauth.Auth) (string, bool, []string, bool) {
	if auth == nil {
		return "", false, nil, false
	}
	lookup := func(key string) string {
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
		return strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, key))
	}
	cloakMode := lookup("cloak_mode")
	strictMode := strings.EqualFold(lookup("cloak_strict_mode"), "true")
	var sensitiveWords []string
	if words := lookup("cloak_sensitive_words"); words != "" {
		sensitiveWords = strings.Split(words, ",")
		for index := range sensitiveWords {
			sensitiveWords[index] = strings.TrimSpace(sensitiveWords[index])
		}
	}
	cacheUserID := strings.EqualFold(lookup("cloak_cache_user_id"), "true")
	return cloakMode, strictMode, sensitiveWords, cacheUserID
}

// resolveClaudeKeyCloakConfig finds the matching ClaudeKey config and returns its CloakConfig.
func resolveClaudeKeyCloakConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CloakConfig {
	if cfg == nil || auth == nil {
		return nil
	}

	apiKey, baseURL := claudeCreds(auth)
	if apiKey == "" {
		return nil
	}

	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)

		// Match by API key
		if strings.EqualFold(cfgKey, apiKey) {
			// If baseURL is specified, also check it
			if baseURL != "" && cfgBase != "" && !strings.EqualFold(cfgBase, baseURL) {
				continue
			}
			return entry.Cloak
		}
	}

	return nil
}

// applyCloaking applies the shared Claude wire policy and reports whether cloaking ran.
func applyCloaking(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	payload []byte,
	model string,
	apiKey string,
	confirmedClaudeCode bool,
) ([]byte, bool, error) {
	policy, settings := resolveClaudeWirePolicy(cfg, auth, apiKey, confirmedClaudeCode)
	if !policy.Cloak {
		return payload, false, nil
	}
	if !settings.strictMode {
		if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(payload, "system")); errSystem != nil {
			return nil, false, errSystem
		}
	}

	cchSigning := policy.OAuth || experimentalCCHSigningEnabled(cfg, auth)
	payload = checkClaudeSystemInstructionsWithSigningModeAt(
		payload,
		settings.strictMode,
		cchSigning,
		helps.DefaultClaudeVersion(cfg),
		"cli",
		getClaudeWorkloadFromContext(ctx),
		claudeCodeCurrentTime(cfg, auth),
	)
	if !policy.OAuth {
		if strings.HasPrefix(model, "gpt-") {
			settings.cacheUserID = false
		}
		var errFakeUserID error
		payload, errFakeUserID = injectClaudeFakeUserID(ctx, payload, apiKey, settings.cacheUserID)
		if errFakeUserID != nil {
			return nil, false, errFakeUserID
		}
	}
	if len(settings.sensitiveWords) > 0 {
		matcher := buildSensitiveWordMatcher(settings.sensitiveWords)
		payload = obfuscateSensitiveWords(payload, matcher)
	}
	return payload, true, nil
}

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST non-deferred tool in the tools array (caches all preceding tool definitions)
// 2. The LAST element in the system array (caches system prompt)
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
//
// Up to 4 cache breakpoints are allowed per request. Tools, System, and Messages are INDEPENDENT breakpoints.
// This enables up to 90% cost reduction on cached tokens (cache read = 0.1x base price).
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
func ensureCacheControl(payload []byte) []byte {
	// 1. Inject cache_control into the LAST non-deferred tool
	// Tools are cached first in the hierarchy, so this is the most important breakpoint.
	payload = injectToolsCacheControl(payload)

	// 2. Inject cache_control into the LAST system prompt element
	// System is the second level in the cache hierarchy.
	payload = injectSystemCacheControl(payload)

	// 3. Inject cache_control into messages for multi-turn conversation caching
	// This caches the conversation history up to the second-to-last user turn.
	payload = injectMessagesCacheControl(payload)

	return payload
}

func countCacheControls(payload []byte) int {
	count := 0

	// Check system
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(
			func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					count++
				}
				return true
			},
		)
	}

	// Check tools
	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(
			func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					count++
				}
				return true
			},
		)
	}

	// Check messages
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(
			func(_, msg gjson.Result) bool {
				content := msg.Get("content")
				if content.IsArray() {
					content.ForEach(
						func(_, item gjson.Result) bool {
							if item.Get("cache_control").Exists() {
								count++
							}
							return true
						},
					)
				}
				return true
			},
		)
	}

	return count
}

// normalizeCacheControlTTL ensures cache_control TTL values don't violate the
// prompt-caching-scope-2026-01-05 ordering constraint: a 1h-TTL block must not
// appear after a 5m-TTL block anywhere in the evaluation order.
//
// Anthropic evaluates blocks in order: tools → system (indexes 0 through N) → messages.
// Within each section, blocks are evaluated in array order. A 5m (default) block
// followed by a 1h block at ANY later position is an error — including within
// the same section (e.g. system[1]=5m then system[3]=1h).
//
// Strategy: walk all cache_control blocks in evaluation order. Once a 5m block
// is seen, strip ttl from ALL subsequent 1h blocks (downgrading them to 5m).
func normalizeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	original := payload
	seen5m := false
	modified := false

	processBlock := func(path string, obj gjson.Result) {
		cc := obj.Get("cache_control")
		if !cc.Exists() {
			return
		}
		if !cc.IsObject() {
			seen5m = true
			return
		}
		ttl := cc.Get("ttl")
		if ttl.Type != gjson.String || ttl.String() != "1h" {
			seen5m = true
			return
		}
		if !seen5m {
			return
		}
		updated, errDel := sjson.DeleteBytes(payload, path+".cache_control.ttl")
		if errDel != nil {
			return
		}
		payload = updated
		modified = true
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(
			func(idx, item gjson.Result) bool {
				processBlock(fmt.Sprintf("tools.%d", int(idx.Int())), item)
				return true
			},
		)
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(
			func(idx, item gjson.Result) bool {
				processBlock(fmt.Sprintf("system.%d", int(idx.Int())), item)
				return true
			},
		)
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(
			func(msgIdx, msg gjson.Result) bool {
				content := msg.Get("content")
				if !content.IsArray() {
					return true
				}
				content.ForEach(
					func(itemIdx, item gjson.Result) bool {
						processBlock(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
						return true
					},
				)
				return true
			},
		)
	}

	if !modified {
		return original
	}
	return payload
}

// enforceCacheControlLimit removes excess cache_control blocks from a payload
// so the total does not exceed the Anthropic API limit (currently 4).
//
// Anthropic evaluates cache breakpoints in order: tools → system → messages.
// The most valuable breakpoints are:
//  1. Last tool         — caches ALL tool definitions
//  2. Last system block — caches ALL system content
//  3. Recent messages   — cache conversation context
//
// Removal priority (strip lowest-value first):
//
//	Phase 1: system blocks earliest-first, preserving the last one.
//	Phase 2: tool blocks earliest-first, preserving the last one.
//	Phase 3: message content blocks earliest-first.
//	Phase 4: remaining system blocks (last system).
//	Phase 5: remaining tool blocks (last tool).
func enforceCacheControlLimit(payload []byte, maxBlocks int) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	total := countCacheControls(payload)
	if total <= maxBlocks {
		return payload
	}

	excess := total - maxBlocks

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		lastIdx := -1
		system.ForEach(
			func(idx, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					lastIdx = int(idx.Int())
				}
				return true
			},
		)
		if lastIdx >= 0 {
			system.ForEach(
				func(idx, item gjson.Result) bool {
					if excess <= 0 {
						return false
					}
					i := int(idx.Int())
					if i == lastIdx || !item.Get("cache_control").Exists() {
						return true
					}
					updated, errDel := sjson.DeleteBytes(payload, fmt.Sprintf("system.%d.cache_control", i))
					if errDel != nil {
						return true
					}
					payload = updated
					excess--
					return true
				},
			)
		}
	}
	if excess <= 0 {
		return payload
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		lastIdx := -1
		tools.ForEach(
			func(idx, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					lastIdx = int(idx.Int())
				}
				return true
			},
		)
		if lastIdx >= 0 {
			tools.ForEach(
				func(idx, item gjson.Result) bool {
					if excess <= 0 {
						return false
					}
					i := int(idx.Int())
					if i == lastIdx || !item.Get("cache_control").Exists() {
						return true
					}
					updated, errDel := sjson.DeleteBytes(payload, fmt.Sprintf("tools.%d.cache_control", i))
					if errDel != nil {
						return true
					}
					payload = updated
					excess--
					return true
				},
			)
		}
	}
	if excess <= 0 {
		return payload
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(
			func(msgIdx, msg gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				content := msg.Get("content")
				if !content.IsArray() {
					return true
				}
				content.ForEach(
					func(itemIdx, item gjson.Result) bool {
						if excess <= 0 {
							return false
						}
						if !item.Get("cache_control").Exists() {
							return true
						}
						path := fmt.Sprintf(
							"messages.%d.content.%d.cache_control", int(msgIdx.Int()), int(itemIdx.Int()),
						)
						updated, errDel := sjson.DeleteBytes(payload, path)
						if errDel != nil {
							return true
						}
						payload = updated
						excess--
						return true
					},
				)
				return true
			},
		)
	}
	if excess <= 0 {
		return payload
	}

	system = gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(
			func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				updated, errDel := sjson.DeleteBytes(payload, fmt.Sprintf("system.%d.cache_control", int(idx.Int())))
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			},
		)
	}
	if excess <= 0 {
		return payload
	}

	tools = gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(
			func(idx, item gjson.Result) bool {
				if excess <= 0 {
					return false
				}
				if !item.Get("cache_control").Exists() {
					return true
				}
				updated, errDel := sjson.DeleteBytes(payload, fmt.Sprintf("tools.%d.cache_control", int(idx.Int())))
				if errDel != nil {
					return true
				}
				payload = updated
				excess--
				return true
			},
		)
	}

	return payload
}

// injectMessagesCacheControl adds cache_control to the second-to-last user turn for multi-turn caching.
// Per Anthropic docs: "Place cache_control on the second-to-last User message to let the model reuse the earlier cache."
// This enables caching of conversation history, which is especially beneficial for long multi-turn conversations.
// Only adds cache_control if:
// - There are at least 2 user turns in the conversation
// - No message content already has cache_control
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Check if ANY message content already has cache_control
	hasCacheControlInMessages := false
	messages.ForEach(
		func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(
					func(_, item gjson.Result) bool {
						if item.Get("cache_control").Exists() {
							hasCacheControlInMessages = true
							return false
						}
						return true
					},
				)
			}
			return !hasCacheControlInMessages
		},
	)
	if hasCacheControlInMessages {
		return payload
	}

	// Find all user message indices
	var userMsgIndices []int
	messages.ForEach(
		func(index gjson.Result, msg gjson.Result) bool {
			if msg.Get("role").String() == "user" {
				userMsgIndices = append(userMsgIndices, int(index.Int()))
			}
			return true
		},
	)

	// Need at least 2 user turns to cache the second-to-last
	if len(userMsgIndices) < 2 {
		return payload
	}

	// Get the second-to-last user message index
	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]

	// Get the content of this message
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		// Add cache_control to the last content block of this message
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, contentCount-1)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		// Convert string content to array with cache_control
		text := content.String()
		newContent := []map[string]any{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

// injectToolsCacheControl adds cache_control to the last non-deferred tool in the tools array.
// Deferred tools cannot use prompt caching, so trailing deferred tools are skipped.
// This only adds cache_control if NO tool in the array already has it.
func injectToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	// Check if ANY tool already has cache_control and find the last eligible tool.
	hasCacheControlInTools := false
	lastEligibleToolIndex := -1
	tools.ForEach(
		func(index, tool gjson.Result) bool {
			if tool.Get("cache_control").Exists() {
				hasCacheControlInTools = true
				return false
			}
			if !tool.Get("defer_loading").Bool() {
				lastEligibleToolIndex = int(index.Int())
			}
			return true
		},
	)
	if hasCacheControlInTools || lastEligibleToolIndex < 0 {
		return payload
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", lastEligibleToolIndex)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

// injectSystemCacheControl adds cache_control to the last element in the system prompt.
// Converts string system prompts to array format if needed.
// This only adds cache_control if NO system element already has it.
func injectSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		// Check if ANY system element already has cache_control
		hasCacheControlInSystem := false
		system.ForEach(
			func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInSystem = true
					return false
				}
				return true
			},
		)
		if hasCacheControlInSystem {
			return payload
		}

		// Add cache_control to the last system element
		lastSystemPath := fmt.Sprintf("system.%d.cache_control", count-1)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String {
		// Convert string system prompt to array with cache_control
		// "system": "text" -> "system": [{"type": "text", "text": "text", "cache_control": {"type": "ephemeral"}}]
		text := system.String()
		newSystem := []map[string]any{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}
