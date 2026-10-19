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
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
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

// ClaudeExecutor routes Anthropic Claude requests over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg                 *config.Config
	requestLogProvider  string
	oauthProfileFetcher claudeOAuthProfileFetcher
	oauthToolAliases    claudeOAuthToolAliasStore
}

const claudeFingerprintSalt = "59cf53e54c78"

const maxClaudeResponseBytes = 50 << 20

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return signature.ProviderFromModelName(baseModel) == signature.ProviderClaude
}

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

func (e *ClaudeExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Claude.ModelLevelCooling
}

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
	useAPIKey := !claudeCredentialUsesOAuth(auth, apiKey)
	isAnthropicBase := helps.IsAnthropicUpstreamURL(req.URL)
	if isAnthropicBase && useAPIKey {
		util.ApplyCredentialHeader(req, "X-Api-Key", apiKey)
	} else {
		bearer := ""
		if strings.TrimSpace(apiKey) != "" {
			bearer = "Bearer " + apiKey
		}
		util.ApplyCredentialHeader(req, "Authorization", bearer)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	enforceDeepSeekAnthropicHeaders(req, apiKey)
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
	oauthToolAliasScope      string
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
	if !v.hasMessageDelta && !v.hasMessageStop {
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
	oauthCancellation bool,
	reverseMap map[string]string,
	fastRequest bool,
	statusCode int,
	transform func([]byte) ([][]byte, error),
	observer claudeStreamObserver,
) {
	var streamUsage claudeStreamUsageBuffer
	outcome := claudeStreamOutcomeIncomplete
	defer func() {
		if observer != nil {
			observer.FinishClaudeStream(outcome)
		}
		publishCtx := ctx
		if outcome == claudeStreamOutcomeComplete {
			publishCtx = context.WithoutCancel(ctx)
		}
		if !streamUsage.publish(publishCtx, reporter) {
			reporter.ensurePublished(publishCtx)
		}
	}()

	validator := &claudeStreamingValidator{}
	terminalDelivered := false
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, maxClaudeResponseBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		appendAPIResponseChunk(ctx, cfg, line)
		observeResponseTokenEvent(ctx, line)
		reporter.observeResponseModel(line)
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
		payloads, errTranslate := transform(bytes.Clone(line))
		if errTranslate != nil {
			outcome = claudeStreamOutcomeValidationFailure
			errTranslate = wrapClaudeFastRequestError(fastRequest, statusCode, markResponseStarted(errTranslate))
			sendClaudeStreamError(ctx, cfg, out, reporter, &streamUsage, errTranslate)
			return
		}
		if ctx.Err() != nil || !sendStreamPayloads(ctx, out, payloads) {
			if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
				canceled := false
				if cancelErr := newClaudeOAuthCancellationError(
					ctx, oauthCancellation, errStream,
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
		if validator.hasMessageStop && ((len(payloads) > 0 && bytes.HasSuffix(payloads[len(payloads)-1], []byte("\n\n"))) || len(bytes.TrimSpace(line)) == 0) {
			terminalDelivered = true
			break
		}
	}
	if errScan := scanner.Err(); errScan != nil && !terminalDelivered {
		if ctx != nil && ctx.Err() != nil {
			outcome = claudeStreamOutcomeCanceled
		} else {
			outcome = claudeStreamOutcomeReadFailure
		}
		if cancelErr := newClaudeOAuthCancellationError(ctx, oauthCancellation, errScan); cancelErr != nil {
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
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case out <- cliproxyexecutor.StreamChunk{Err: err}:
	case <-timer.C:
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
	fingerprint := resolveClaudeFingerprintPolicy(e.cfg, auth, apiKey)

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
	if fingerprint.ProfileClaudeCodeCLI {
		claudeSessionID = helps.ClaudeAgentSessionUUIDForRequest(
			incomingHeaders,
			originalPayload,
			req.Payload,
			confirmedClaudeCode,
			opts.Metadata,
			req.Metadata,
		)
	}
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, upstreamStream, isCompat, &req,
	)
	if err != nil {
		return nil, err
	}
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.NormalizeClaudeToolCallIDs(body)
	preThinkingErr := claudeSonnet55ThinkingError(body)

	body, err = helps.ApplyThinkingWithSourcePayload(
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
	explicitCacheMode := isExplicitPromptCacheMode(originalPayload, req.Payload, body)
	bodyBeforeCloaking := body
	initialProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(bodyBeforeCloaking)
	var cloaked bool
	body, cloaked, err = applyCloaking(
		ctx,
		e.cfg,
		auth,
		body,
		baseModel,
		apiKey,
		confirmedClaudeCode,
		explicitCacheMode,
	)
	if err != nil {
		return nil, err
	}
	systemPlacementState := captureClaudeCodeSystemPlacement(
		bodyBeforeCloaking,
		body,
		cloaked,
		explicitCacheMode,
	)
	fableState := captureClaudeCodeFableState(
		bodyBeforeCloaking,
		body,
		cloaked,
	)

	diagnosticsState := claudeDiagnosticsRequestState{}
	contextManagementState := claudeCodeContextManagementState{
		eligible:    cloaked && isAnthropicUpstreamBase(baseURL),
		callerOwned: gjson.GetBytes(body, "context_management").Exists(),
	}
	if contextManagementState.eligible {
		body, contextManagementState.automaticallyInjected = injectClaudeCodeContextManagement(body)
	}

	body = disableThinkingIfToolChoiceForced(body)
	body = reconcileClaudeCodeContextManagement(body, contextManagementState)

	requestedModel := payloadRequestedModel(opts, req.Model)
	var touchedPayloadPaths map[string]bool
	body, touchedPayloadPaths = applyPayloadConfigWithRequestTracked(
		e.cfg,
		baseModel,
		to.String(),
		from.String(),
		"",
		body,
		originalTranslated,
		requestedModel,
		payloadRequestPath(opts), opts.Headers, e.Identifier(),
		"context_management", "diagnostics", "system", "messages", "max_tokens", "output_config", "tools",
		"fallbacks", "thinking.type", "thinking.display",
	)
	contextManagementState.payloadRuleTouched = touchedPayloadPaths["context_management"]
	body = reconcileClaudeCodeSystemPlacementAfterPayload(body, systemPlacementState)
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
	if initialProbeOrHelper &&
		!touchedPayloadPaths["system"] &&
		!touchedPayloadPaths["messages"] &&
		!touchedPayloadPaths["max_tokens"] &&
		!touchedPayloadPaths["output_config"] &&
		!touchedPayloadPaths["tools"] {
		isProbeOrHelper = true
	}
	body = reconcileClaudeCodeFableModelAfterPayload(
		body,
		fableState,
		touchedPayloadPaths,
		cloaked,
		isProbeOrHelper,
	)

	if isClaudeSonnet55Model(gjson.GetBytes(body, "model").String()) &&
		!touchedPayloadPaths["thinking.type"] && preThinkingErr != nil {
		return nil, preThinkingErr
	}
	body, err = normalizeClaudeSonnet55Request(body)
	if err != nil {
		return nil, err
	}
	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = reconcileClaudeCodeContextManagement(body, contextManagementState)
	body = normalizeClaudeSamplingForUpstream(body, confirmedClaudeCode)

	cpaOwnsCacheControl := !explicitCacheMode && shouldEnsureCacheControl(
		body,
		cloaked,
		confirmedClaudeCode,
	)
	if cpaOwnsCacheControl {
		body = ensureCacheControl(body, touchedPayloadPaths["system"])
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// (e.g. Amp CLI) already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, body)
	if cloaked && fingerprint.ProfileClaudeCodeCLI && isSubagent && !isProbeOrHelper {
		body = injectClaudeSubagentBillingTag(body)
	}
	subagent1h := isSubagent && helps.ClaudeSubagentRequests1h(incomingHeaders, body)
	if cpaOwnsCacheControl &&
		fingerprint.ProfileClaudeCodeCLI &&
		(!isSubagent || subagent1h) &&
		!isProbeOrHelper {
		body = upgradeClaudeCacheControlTTL(body, claudeCacheControlTTL1h)
	} else if !explicitCacheMode && (isProbeOrHelper || (isSubagent && !subagent1h)) {
		body = stripClaudeCacheControlTTL(body)
	}

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	if !explicitCacheMode {
		body = normalizeCacheControlTTL(body)
	}
	// Payload rules may rewrite stream. Keep the request body, transport headers,
	// response parser, and downstream response format on one authority.
	streamField := gjson.GetBytes(body, "stream")
	if !claudeCodeDetection.HelperProfile || streamField.Exists() || upstreamStream {
		body = helps.SetBoolIfDifferent(body, "stream", upstreamStream)
	}

	// Clamp max_tokens to the model's documented limit (works around Claude Code SDK off-by-one).
	body = capMaxTokens(body, baseModel)
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || isCompat {
		body, _ = signature.SanitizeClaudeMessagesForClaudeUpstream(
			body,
			baseModel,
			isCompat,
		)
	}

	if cloaked {
		hasExecutionMetadata := false
		for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
			if value, ok := metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string); ok {
				hasExecutionMetadata = strings.TrimSpace(value) != ""
				break
			}
		}
		if isProbeOrHelper {
			body = helps.StripClaudeBillingTags(body)
		} else if fingerprint.ProfileClaudeCodeCLI {
			continuityBody := bodyBeforeCloaking
			if initialProbeOrHelper {
				continuityBody = body
			}
			body, diagnosticsState = prepareClaudeContinuity(body, continuityBody, auth, claudeSessionID, hasExecutionMetadata,
				claudeCCHSigningEnabled(apiKey, fingerprint.ProfileClaudeCodeCLI, baseURL), isAnthropicUpstreamBase(baseURL), touchedPayloadPaths["diagnostics"])
			if diagnosticsState.key != "" {
				current := claudeCodeCurrentTime(e.cfg, auth)
				currentDate := current.Format("2006-01-02")
				pinnedDate := helps.PinClaudeSessionDate(diagnosticsState.key, currentDate)
				if pinnedDate != currentDate && !touchedPayloadPaths["messages"] {
					pinnedTime, errDate := time.ParseInLocation("2006-01-02", pinnedDate, current.Location())
					if errDate != nil {
						return nil, fmt.Errorf("claude executor: invalid pinned session date: %w", errDate)
					}
					body = injectClaudeCodeCurrentDate(body, pinnedTime, explicitCacheMode)
				}
			}
		}
		_, settings := resolveClaudeWirePolicy(e.cfg, auth, apiKey, confirmedClaudeCode)
		if len(settings.sensitiveWords) > 0 {
			body = obfuscateSensitiveWords(body, buildSensitiveWordMatcher(settings.sensitiveWords))
		}
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
	var oauthToolAliasScope string
	if fingerprint.ProfileClaudeCodeCLI && cloaked {
		oauthToolAliasScope = claudeOAuthToolAliasScope(ctx, auth)
		bodyForUpstream, oauthToolNamesReverseMap, err = e.prepareClaudeOAuthToolNamesForRequest(
			bodyForUpstream, resolveClaudeMCPAliasOptions(ctx), oauthToolAliasScope,
		)
		if err != nil {
			return nil, err
		}
	}
	if fingerprint.ProfileClaudeCodeCLI {
		bodyForUpstream, err = applyClaudeCLIIdentity(bodyForUpstream, auth, apiKey, claudeSessionID, fingerprint)
		if err != nil {
			return nil, fmt.Errorf("apply Claude credential metadata: %w", err)
		}
	}
	if !fingerprint.ProfileClaudeCodeCLI && isKimiMessagesUpstream(auth, baseURL) {
		bodyForUpstream = stripKimiClaudeAttribution(bodyForUpstream)
	}
	bodyForUpstream = finalizeClaudeCompactionSummaryBody(ctx, bodyForUpstream)
	bodyForUpstream, err = sanitizeClaudeWebSearchDomains(bodyForUpstream)
	if err != nil {
		return nil, err
	}
	bodyForUpstream, err = stripPromptCacheOptions(bodyForUpstream)
	if err != nil {
		return nil, fmt.Errorf("remove Claude prompt cache options: %w", err)
	}
	if claudeCCHSigningEnabled(apiKey, fingerprint.ProfileClaudeCodeCLI, baseURL) {
		fallback := ""
		if claudeBodyNeedsBillingFallback(bodyForUpstream) {
			fallback = generateClaudeBillingHeader(true, helps.DefaultClaudeVersion(e.cfg), claudeBillingFingerprintMessageText(bodyForUpstream), "cli", getClaudeWorkloadFromContext(ctx))
		}
		bodyForUpstream, err = finalizeAnthropicMessagesBodyCCH(bodyForUpstream, fallback)
		if err != nil {
			return nil, err
		}
	} else if fingerprint.ProfileClaudeCodeCLI {
		bodyForUpstream = stripClaudeBillingCCH(bodyForUpstream)
	}
	if errMidSystem := validateClaudeMidSystemMessageModel(
		bodyForUpstream,
		confirmedClaudeCode,
		isAnthropicUpstreamBase(baseURL),
	); errMidSystem != nil {
		return nil, errMidSystem
	}
	reporter.setTranslatedReasoningEffort(bodyForUpstream, to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	if errHeaders := applyClaudeHeadersWithNativeProfile(
		httpReq,
		auth,
		apiKey,
		upstreamStream,
		extraBetas,
		bodyForUpstream,
		e.cfg,
		incomingHeaders,
		confirmedClaudeCode && !cloaked,
		claudeCodeDetection.HelperProfile,
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
		oauthToolAliasScope:      oauthToolAliasScope,
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
	request.diagnosticsState.requestID = helps.HeaderValueCaseInsensitive(httpResp.Header, "Request-Id")
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses and support both declared and magic-byte-detected compression.
		errBody, errDecode := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
		if errDecode != nil {
			recordAPIResponseError(ctx, e.cfg, errDecode)
			message := fmt.Sprintf("failed to decode error response body: %v", errDecode)
			logWithRequestID(ctx).Warn(message)
			localErr := newUpstreamStatusErrMessage(httpResp.StatusCode, message, httpResp.Header)
			localErr.requestScoped = true
			return nil, nil, wrapClaudeFastRequestError(
				request.fastRequest,
				httpResp.StatusCode,
				claudeUpstreamHTTPError{localErr},
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
			return nil, nil, newClaudeFastDirectResponseError(httpResp, body, e.modelLevelCooling())
		}
		return nil, nil, claudeUpstreamHTTPError{
			classifyClaudeUpstreamError(httpResp.StatusCode, body, e.modelLevelCooling(), httpResp.Header),
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
	opts.OriginalRequest = originalRequestPayload(req, opts)
	compactionScope, compactionRequested, errCompaction := e.prepareClaudeCompaction(auth, &req, &opts)
	if errCompaction != nil {
		return resp, errCompaction
	}
	if compactionRequested {
		return e.executeClaudeCompaction(ctx, auth, req, opts, compactionScope)
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
	var streamUsage claudeStreamUsageBuffer
	if cr.translationStream {
		validator := &claudeStreamingValidator{}
		lines := bytes.Split(data, []byte("\n"))
		for index, line := range lines {
			if errValidate := validator.observe(line); errValidate != nil {
				recordAPIResponseError(ctx, e.cfg, errValidate)
				streamUsage.publishFailure(ctx, cr.reporter, errValidate)
				err = wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(errValidate))
				return resp, err
			}
			observeResponseTokenEvent(ctx, line)
			cr.reporter.observeResponseModel(line)
			streamUsage.observe(line)
			if len(cr.oauthToolNamesReverseMap) > 0 {
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
		data = bytes.Join(lines, []byte("\n"))
	} else {
		observeResponseTokenEvent(ctx, data)
		cr.reporter.observeResponseModel(data)
		if len(cr.oauthToolNamesReverseMap) > 0 {
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
	errTranslate := sdktranslator.ResponseTranslationError(&param)
	if errTranslate == nil && len(out) == 0 {
		errTranslate = wrapClaudeFastRequestError(cr.fastRequest, httpResp.StatusCode, markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty response"}))
	}
	if errTranslate != nil {
		recordAPIResponseError(ctx, e.cfg, errTranslate)
		if cr.translationStream {
			streamUsage.publishFailure(ctx, cr.reporter, errTranslate)
		} else {
			cr.reporter.publishFailureWithDetail(ctx, parseClaudeUsage(data), errTranslate)
		}
		return resp, errTranslate
	}
	messageID := claudeMessageIDFromResponse(data)
	if cr.translationStream {
		messageID = claudeMessageIDFromSSE(data)
		if !streamUsage.publish(ctx, cr.reporter) {
			cr.reporter.ensurePublished(ctx)
		}
	} else {
		cr.reporter.publish(ctx, parseClaudeUsage(data))
	}
	commitClaudeDiagnostics(cr.diagnosticsState, messageID)
	e.rememberClaudeOAuthToolAliases(cr.bodyForTranslation, cr.oauthToolNamesReverseMap, messageID, cr.oauthToolAliasScope)
	cacheClaudeThinkingReplayResponse(ctx, thinkingReplayScope, data)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	compactionScope, compactionRequested, errCompaction := e.prepareClaudeCompaction(auth, &req, &opts)
	if errCompaction != nil {
		return nil, errCompaction
	}
	if compactionRequested {
		return e.executeClaudeCompactionStream(ctx, auth, req, opts, compactionScope)
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
	defer cr.reporter.trackFailure(ctx, &err)

	httpResp, decodedBody, err := e.openClaudeResponse(ctx, auth, cr)
	if err != nil {
		return nil, err
	}
	observer = combineClaudeStreamObservers(
		observer,
		newClaudeDiagnosticsStreamObserver(cr.diagnosticsState),
	)
	if cr.oauthToolAliasScope != "" {
		observer = combineClaudeStreamObservers(observer, &claudeOAuthToolAliasStreamObserver{
			executor: e, request: cr.bodyForTranslation,
			aliases: cr.oauthToolNamesReverseMap, scope: cr.oauthToolAliasScope,
		})
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	transform := func(line []byte) ([][]byte, error) {
		if gjson.GetBytes(jsonPayload(line), "type").String() == "message_stop" {
			return [][]byte{append(line, '\n', '\n')}, nil
		}
		return [][]byte{append(line, '\n')}, nil
	}
	if cr.responseFormat != cr.to {
		var param any
		transform = func(line []byte) ([][]byte, error) {
			chunks := sdktranslator.TranslateStream(
				ctx,
				cr.to,
				cr.responseFormat,
				req.Model,
				opts.OriginalRequest,
				cr.bodyForTranslation,
				line,
				&param,
			)
			return chunks, sdktranslator.ResponseTranslationError(&param)
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
			len(cr.oauthToolNamesReverseMap) > 0,
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
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if strings.TrimSpace(apiKey) != "" && isAnthropicUpstreamBase(baseURL) {
		return e.countTokensUpstream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	stream := from != to
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, stream, isCompat, &req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.NormalizeClaudeToolCallIDs(body)
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated,
		payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier())
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
	body, err = normalizeClaudeSonnet55Request(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body, err = stripPromptCacheOptions(body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("remove Claude prompt cache options: %w", err)
	}
	body, err = sanitizeClaudeWebSearchDomains(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errValidate := validateClaudeTokenCountRequest(body, false); errValidate != nil {
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

func validateClaudeTokenCountRequest(body []byte, allowMidSystem bool) error {
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
		if role != "user" && role != "assistant" && !(allowMidSystem && role == "system") {
			allowed := "Claude token count request message role must be user or assistant"
			if allowMidSystem {
				allowed = "Claude token count request message role must be user, assistant, or system"
			}
			return newClaudeTokenCountValidationError(allowed)
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
	fingerprint := resolveClaudeFingerprintPolicy(e.cfg, auth, apiKey)
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
	if fingerprint.ProfileClaudeCodeCLI {
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
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, stream, isCompat, &req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.NormalizeClaudeToolCallIDs(body)
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated,
		payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier())
	if isClaudeSonnet55Model(baseModel) {
		if errThinking := claudeSonnet55ThinkingError(body); errThinking != nil {
			return cliproxyexecutor.Response{}, errThinking
		}
		body, err = normalizeClaudeSonnet55Request(body)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		body, err = helps.ApplyThinkingWithSourcePayload(
			body, req.Payload, originalPayload, req.Model, from.String(), to.String(), e.Identifier(), req,
		)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
	}
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	directAnthropic := isAnthropicUpstreamBase(baseURL)
	explicitCacheMode := isExplicitPromptCacheMode(originalPayload, req.Payload, body)
	var cloaked bool
	if directAnthropic || fingerprint.ProfileClaudeCodeCLI {
		policy, settings := resolveClaudeWirePolicy(e.cfg, auth, apiKey, confirmedClaudeCode)
		cloaked = policy.Cloak
		if cloaked {
			if !settings.strictMode {
				if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(body, "system")); errSystem != nil {
					return cliproxyexecutor.Response{}, errSystem
				}
			}
			body = relocateClaudeSystemPromptForCountTokens(body, settings.strictMode, explicitCacheMode, !policy.OAuth)
			if len(settings.sensitiveWords) > 0 {
				matcher := buildSensitiveWordMatcher(settings.sensitiveWords)
				body = obfuscateSensitiveWords(body, matcher)
			}
		}
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	if !explicitCacheMode {
		body = normalizeCacheControlTTL(body)
	}

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	extraBetas = append(extraBetas, claudeTokenCountingBeta)
	if fingerprint.ProfileClaudeCodeCLI && cloaked {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, resolveClaudeMCPAliasOptions(ctx))
	}
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || isCompat {
		body, _ = signature.SanitizeClaudeMessagesForClaudeUpstream(
			body,
			baseModel,
			isCompat,
		)
	}
	if directAnthropic || fingerprint.ProfileClaudeCodeCLI {
		body, _ = sjson.DeleteBytes(body, "metadata")
		body, _ = sjson.DeleteBytes(body, "context_management")
		body, _ = sjson.DeleteBytes(body, "diagnostics")
	}

	if !fingerprint.ProfileClaudeCodeCLI && isKimiMessagesUpstream(auth, baseURL) {
		body = stripKimiClaudeAttribution(body)
	}
	body, err = normalizeClaudeSonnet55Request(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	body, err = stripPromptCacheOptions(body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("remove Claude prompt cache options: %w", err)
	}
	if errMidSystem := validateClaudeMidSystemMessageModel(
		body,
		confirmedClaudeCode,
		directAnthropic,
	); errMidSystem != nil {
		return cliproxyexecutor.Response{}, errMidSystem
	}
	body, err = sanitizeClaudeWebSearchDomains(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errValidate := validateClaudeTokenCountRequest(body, true); errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}
	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errHeaders := applyClaudeHeadersWithNativeProfile(
		httpReq,
		auth,
		apiKey,
		false,
		extraBetas,
		body,
		e.cfg,
		incomingHeaders,
		confirmedClaudeCode && !cloaked,
		claudeCodeDetection.HelperProfile,
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
			localErr := newUpstreamStatusErrMessage(resp.StatusCode, msg, resp.Header)
			localErr.requestScoped = true
			return cliproxyexecutor.Response{}, localErr
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
		return cliproxyexecutor.Response{}, classifyClaudeUpstreamError(resp.StatusCode, b, e.modelLevelCooling(), resp.Header)
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
	out := sdktranslator.TranslateTokenCount(ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func claudeRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if refreshToken := claudeauth.ReadMetadataString(&auth.Metadata, "refresh_token"); refreshToken != "" {
		return refreshToken
	}
	return claudeauth.ReadMetadataString(&auth.Metadata, "refreshToken")
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	refreshToken := claudeRefreshToken(auth)
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

type claudeSonnet55RequestError struct {
	statusErr
}

func (claudeSonnet55RequestError) IsRequestScoped() bool { return true }

func newClaudeSonnet55RequestError(message string) error {
	return claudeSonnet55RequestError{statusErr{code: http.StatusBadRequest, msg: message}}
}

func normalizeClaudeSonnet55Request(body []byte) ([]byte, error) {
	if !isClaudeSonnet55Model(gjson.GetBytes(body, "model").String()) {
		return body, nil
	}
	if choice := gjson.GetBytes(body, "tool_choice.type").String(); choice == "any" || choice == "tool" {
		return body, newClaudeSonnet55RequestError(`claude-sonnet-5-5 does not support tool_choice type "any" or "tool"`)
	}

	if gjson.GetBytes(body, "thinking.type").String() == "disabled" {
		body, _ = sjson.SetBytes(body, "thinking.type", "between_tools")
		for _, field := range []string{"thinking.display", "thinking.budget_tokens", "thinking.block_binding"} {
			body, _ = sjson.DeleteBytes(body, field)
		}
	}
	return body, claudeSonnet55ThinkingError(body)
}

func claudeSonnet55ThinkingError(body []byte) error {
	switch gjson.GetBytes(body, "thinking.type").String() {
	case "enabled":
		return newClaudeSonnet55RequestError("claude-sonnet-5-5 does not support manual thinking budgets")
	case "between_tools":
		for _, field := range []string{"thinking.display", "thinking.budget_tokens", "thinking.block_binding"} {
			if gjson.GetBytes(body, field).Exists() {
				return newClaudeSonnet55RequestError("claude-sonnet-5-5 between_tools does not accept " + field)
			}
		}
		if effort := gjson.GetBytes(body, "output_config.effort"); effort.Exists() &&
			effort.String() != "low" && effort.String() != "medium" && effort.String() != "high" {
			return newClaudeSonnet55RequestError("claude-sonnet-5-5 between_tools supports only low, medium, or high effort")
		}
	}
	return nil
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error.
		body, _ = sjson.DeleteBytes(body, "thinking")
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if outputConfig := gjson.GetBytes(body, "output_config"); outputConfig.Exists() && outputConfig.IsObject() && len(outputConfig.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

func normalizeClaudeSamplingForUpstream(body []byte, nativeOwned bool) []byte {
	thinkingActive := false
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String())) {
	case "enabled", "adaptive", "auto":
		thinkingActive = true
	}

	if !nativeOwned {
		body, _ = sjson.DeleteBytes(body, "temperature")
		body, _ = sjson.DeleteBytes(body, "top_p")
		if thinkingActive {
			body, _ = sjson.DeleteBytes(body, "top_k")
		}
		return body
	}

	if thinkingActive {
		if temperature := gjson.GetBytes(body, "temperature"); temperature.Exists() && temperature.Num != 1 {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
		if topP := gjson.GetBytes(body, "top_p"); topP.Exists() && topP.Num < 0.95 {
			body, _ = sjson.DeleteBytes(body, "top_p")
		}
		body, _ = sjson.DeleteBytes(body, "top_k")
		return body
	}
	if gjson.GetBytes(body, "temperature").Exists() &&
		gjson.GetBytes(body, "top_p").Exists() {
		body, _ = sjson.DeleteBytes(body, "top_p")
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

func enforceDeepSeekAnthropicHeaders(r *http.Request, apiKey string) {
	if !isDeepSeekAnthropicRequest(r) {
		return
	}
	r.Header.Del("Anthropic-Version")
	util.ApplyCredentialHeader(r, "X-Api-Key", apiKey)
}

func claudeCredentialUsesOAuth(auth *cliproxyauth.Auth, apiKey string) bool {
	if isClaudeOAuthToken(apiKey) {
		return true
	}
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		return false
	}
	hasAPIKeyAttr := auth != nil &&
		auth.Attributes != nil &&
		strings.TrimSpace(auth.Attributes["api_key"]) != ""
	return !hasAPIKeyAttr
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
	return applyClaudeHeadersWithNativeProfile(
		r,
		auth,
		apiKey,
		stream,
		extraBetas,
		body,
		cfg,
		incomingHeaders,
		confirmedClaudeCode,
		false,
		sessionIDs...,
	)
}

func applyClaudeHeadersWithNativeProfile(
	r *http.Request,
	auth *cliproxyauth.Auth,
	apiKey string,
	stream bool,
	extraBetas []string,
	body []byte,
	cfg *config.Config,
	incomingHeaders http.Header,
	confirmedClaudeCode bool,
	helperProfile bool,
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

	credentialUsesBearer := claudeCredentialUsesOAuth(auth, apiKey)
	useAPIKey := !credentialUsesBearer
	fingerprint := resolveClaudeFingerprintPolicy(cfg, auth, apiKey)
	wirePolicy, _ := resolveClaudeWirePolicy(
		cfg,
		auth,
		apiKey,
		confirmedClaudeCode,
	)
	applyCLIFingerprint := fingerprint.ProfileClaudeCodeCLI || wirePolicy.Cloak
	preserveCallerFingerprint := !applyCLIFingerprint && !confirmedClaudeCode
	useOAuthBetas := fingerprint.UseOAuthBetas
	isAnthropicBase := helps.IsAnthropicUpstreamURL(r.URL)
	if isAnthropicBase && useAPIKey {
		util.ApplyCredentialHeader(r, "X-Api-Key", apiKey)
	} else {
		bearer := ""
		if strings.TrimSpace(apiKey) != "" {
			bearer = "Bearer " + apiKey
		}
		util.ApplyCredentialHeader(r, "Authorization", bearer)
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

	incomingBetas := strings.TrimSpace(strings.Join(
		helps.HeaderValuesCaseInsensitive(incomingHeaders, "Anthropic-Beta"),
		",",
	))
	countTokens := r.URL != nil && strings.HasSuffix(r.URL.Path, "/count_tokens")
	requestedBetas := claudeRequestedBetas(incomingBetas, extraBetas)
	advisorNeeded := requestedBetas[claudeAdvisorToolBeta] || claudeBodyHasAdvisorTool(body)

	baseBetas := incomingBetas
	if !preserveCallerFingerprint {
		baseBetas = claudeCodeCLIBetas(body, requestedBetas, useOAuthBetas)
		if countTokens {
			baseBetas = claudeCountTokensBetasForCredential(useOAuthBetas)
			if advisorNeeded {
				baseBetas = withClaudeAdvisorToolBeta(baseBetas)
			}
		}
	}
	if confirmedClaudeCode && incomingBetas != "" {
		baseBetas = incomingBetas
		if advisorNeeded {
			baseBetas = withClaudeAdvisorToolBeta(baseBetas)
		}
		if useOAuthBetas && !helperProfile {
			if countTokens {
				baseBetas = withClaudeCountTokensOAuthBeta(baseBetas)
			} else {
				isSubagent := helps.IsClaudeSubagentRequest(incomingHeaders, body)
				isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
				subagent1h := isSubagent &&
					helps.ClaudeSubagentRequests1h(incomingHeaders, body)
				includeExtendedCacheTTL := (!isSubagent || subagent1h) &&
					!isProbeOrHelper
				baseBetas = withClaudeOAuthCredentialBetas(
					baseBetas,
					includeExtendedCacheTTL,
				)
			}
		}
	}
	if preserveCallerFingerprint && advisorNeeded {
		baseBetas = withClaudeAdvisorToolBeta(baseBetas)
	}
	if !claudeRequestSupportsEffort(body, nil) {
		baseBetas = withoutClaudeBeta(baseBetas, claudeEffortBeta)
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
		if strings.TrimSpace(baseBetas) == "" {
			baseBetas = beta
		} else {
			baseBetas += "," + beta
		}
		existingSet[beta] = true
	}
	if preserveCallerFingerprint {
		if !countTokens && strings.EqualFold(
			strings.TrimSpace(gjson.GetBytes(body, "speed").String()),
			"fast",
		) {
			appendBeta(claudeFastModeBeta)
		}
		for _, beta := range extraBetas {
			appendBeta(beta)
		}
	} else {
		if !confirmedClaudeCode && incomingBetas != "" {
			for beta := range strings.SplitSeq(incomingBetas, ",") {
				beta = strings.TrimSpace(beta)
				if beta == "" || (isAnthropicBase && isManagedClaudeBeta(beta)) {
					continue
				}
				appendBeta(beta)
			}
		}
		if !isAnthropicBase {
			for _, beta := range extraBetas {
				appendBeta(beta)
			}
		}
	}
	applyBetaHeader := func() {
		if !claudeRequestSupportsEffort(body, nil) {
			baseBetas = withoutClaudeBeta(baseBetas, claudeEffortBeta)
		}
		requestProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(body)
		if requestProbeOrHelper && !helperProfile {
			baseBetas = withoutClaudeBeta(baseBetas, claudeServerSideFallbackBeta)
			baseBetas = withoutClaudeBeta(baseBetas, claudeThinkingDisplayUpdatesBeta)
			baseBetas = withoutClaudeBeta(baseBetas, claudeExtendedCacheTTLBeta)
		}
		if strings.EqualFold(
			strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()),
			"disabled",
		) {
			baseBetas = withoutClaudeBeta(baseBetas, claudeThinkingDisplayUpdatesBeta)
		}
		if helps.IsClaudeSubagentRequest(incomingHeaders, body) &&
			!helps.ClaudeSubagentRequests1h(incomingHeaders, body) {
			baseBetas = withoutClaudeBeta(baseBetas, claudeExtendedCacheTTLBeta)
		}
		if !requestProbeOrHelper && !countTokens && helps.ClaudePayloadHas1hTTL(body) {
			baseBetas = withClaudeExtendedCacheTTLBeta(baseBetas)
		}
		if isClaudeHaikuModel(gjson.GetBytes(body, "model").String()) &&
			!gjson.GetBytes(body, "fallbacks").Exists() && !helperProfile {
			baseBetas = withoutClaudeBeta(baseBetas, claudeServerSideFallbackBeta)
		}
		if strings.TrimSpace(baseBetas) == "" {
			r.Header.Del("Anthropic-Beta")
			return
		}
		r.Header.Set("Anthropic-Beta", baseBetas)
	}
	applyBetaHeader()

	if preserveCallerFingerprint {
		defaultAccept := "application/json"
		defaultAcceptEncoding := "gzip, deflate, br, zstd"
		if stream && !isAnthropicBase {
			defaultAccept = "text/event-stream"
			defaultAcceptEncoding = "identity"
		}
		copyClaudeCallerFingerprintHeaders(r.Header, incomingHeaders)
		misc.EnsureHeader(
			r.Header,
			incomingHeaders,
			"Anthropic-Version",
			"2023-06-01",
		)
		misc.EnsureHeader(r.Header, incomingHeaders, "Accept", defaultAccept)
		misc.EnsureHeader(
			r.Header,
			incomingHeaders,
			"Accept-Encoding",
			defaultAcceptEncoding,
		)
		misc.EnsureHeader(
			r.Header,
			incomingHeaders,
			"User-Agent",
			"CLIProxyAPI/"+buildinfo.Version,
		)
		applyBetaHeader()
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(r, attrs, incomingHeaders)
		restoreCallerTransport := func() {
			resetHeader := func(name, fallback string) {
				if value := strings.TrimSpace(
					helps.HeaderValueCaseInsensitive(incomingHeaders, name),
				); value != "" {
					r.Header.Set(name, value)
					return
				}
				r.Header.Set(name, fallback)
			}
			resetHeader("Accept", defaultAccept)
			resetHeader("Accept-Encoding", defaultAcceptEncoding)
		}
		if isAnthropicBase {
			applyBetaHeader()
			restoreCallerTransport()
		} else if stream {
			restoreCallerTransport()
		}
		enforceDeepSeekAnthropicHeaders(r, apiKey)
		return nil
	}

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
	if confirmedClaudeCode &&
		helps.HeaderValueCaseInsensitive(incomingHeaders, "X-Stainless-Async") == "async" {
		r.Header.Set("X-Stainless-Async", "async")
	}
	if !countTokens {
		identityHeader("X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	} else if confirmedClaudeCode {
		if incomingTimeout := helps.HeaderValueCaseInsensitive(
			incomingHeaders,
			"X-Stainless-Timeout",
		); incomingTimeout != "" {
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
	for _, header := range []string{
		"X-Claude-Code-Agent-Id",
		"X-Claude-Code-Parent-Agent-Id",
		"X-Claude-Remote-Container-Id",
		"X-Claude-Remote-Session-Id",
		"X-Client-App",
		"X-Anthropic-Additional-Protection",
	} {
		if value := helps.HeaderValueCaseInsensitive(incomingHeaders, header); value != "" {
			r.Header.Set(header, value)
		}
	}
	if confirmedClaudeCode {
		for _, header := range []string{
			"X-Claude-Code-Request-Class",
			"X-Claude-Code-Agent-Type",
			"X-Claude-Code-Prev-Tool-Durations",
			"X-Claude-Code-Compaction",
			"X-Claude-Code-Context-Compacted",
		} {
			if value := helps.HeaderValueCaseInsensitive(incomingHeaders, header); value != "" {
				r.Header.Set(header, value)
			}
		}
	}
	if isAnthropicBase ||
		(helperProfile &&
			helps.HeaderValueCaseInsensitive(
				incomingHeaders,
				"x-client-request-id",
			) != "") {
		identityHeader("x-client-request-id", uuid.NewString())
	}
	r.Header.Set("Connection", "keep-alive")

	applyTransportNegotiation := func() {
		if helperProfile {
			identityHeader("Accept", "application/json")
			identityHeader("Accept-Encoding", "gzip, deflate, br, zstd")
			return
		}
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
	util.ApplyCustomHeadersFromAttrs(r, attrs, incomingHeaders)
	enforceDeepSeekAnthropicHeaders(r, apiKey)
	if isAnthropicBase {
		applyBetaHeader()
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
	entry := resolveClaudeKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return entry.Cloak
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
	explicitCacheMode ...bool,
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

	fingerprint := resolveClaudeFingerprintPolicy(cfg, auth, apiKey)
	_, baseURL := claudeCreds(auth)
	cchSigning := claudeCCHSigningEnabled(apiKey, fingerprint.ProfileClaudeCodeCLI, baseURL)
	payload = checkClaudeSystemInstructionsWithSigningModeAt(
		payload,
		settings.strictMode,
		cchSigning,
		helps.DefaultClaudeVersion(cfg),
		"cli",
		getClaudeWorkloadFromContext(ctx),
		claudeCodeCurrentTime(cfg, auth),
		isExplicitPromptCacheMode(payload) || (len(explicitCacheMode) > 0 && explicitCacheMode[0]),
		!policy.OAuth,
	)
	isProbeOrHelper := helps.IsClaudeProbeOrHelperRequest(payload)
	if isClaudeOpus55Model(gjson.GetBytes(payload, "model").String()) &&
		!isProbeOrHelper && !gjson.GetBytes(payload, "fallbacks").Exists() {
		payload, _ = sjson.SetRawBytes(payload, "fallbacks", []byte(`[{"model":"claude-opus-4-8"}]`))
	}
	if isClaudeFable51Model(gjson.GetBytes(payload, "model").String()) &&
		!isProbeOrHelper {
		if !gjson.GetBytes(payload, "fallbacks").Exists() {
			payload, _ = sjson.SetRawBytes(
				payload,
				"fallbacks",
				[]byte(`[{"model":"claude-opus-5"}]`),
			)
		}
	}
	if !isProbeOrHelper {
		payload = applyClaudeCloakThinkingDisplay(payload, false)
	}
	if !fingerprint.ProfileClaudeCodeCLI {
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

// ensureCacheControl injects the default breakpoints used by the native request builder:
// the last system block and the last cacheable message. Tools are only marked when
// no cacheable system prompt exists, so translated requests still cover a large tools
// prefix without adding the redundant tools marker that native Claude Code omits.
// Each section injects independently and enforceCacheControlLimit applies the API cap.
func ensureCacheControl(payload []byte, systemOwned bool) []byte {
	if !claudePayloadHasCacheableSystem(payload) {
		payload = injectToolsCacheControl(payload)
	}
	if !systemOwned {
		payload = injectSystemCacheControl(payload)
	}
	payload = injectMessagesCacheControl(payload)
	return payload
}

func claudePayloadHasCacheableSystem(payload []byte) bool {
	system := gjson.GetBytes(payload, "system")
	switch {
	case !system.Exists():
		return false
	case system.IsArray():
		return system.Get("#").Int() > 0
	case system.Type == gjson.String:
		return strings.TrimSpace(system.String()) != ""
	default:
		return false
	}
}

func isExplicitPromptCacheMode(payloads ...[]byte) bool {
	for _, payload := range payloads {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_options.mode").String()), "explicit") {
			return true
		}
	}
	return false
}

func stripPromptCacheOptions(payload []byte) ([]byte, error) {
	if !gjson.GetBytes(payload, "prompt_cache_options").Exists() {
		return payload, nil
	}
	return sjson.DeleteBytes(payload, "prompt_cache_options")
}

func shouldEnsureCacheControl(payload []byte, cloaked, confirmedClaudeCode bool) bool {
	return !confirmedClaudeCode && (cloaked || countCacheControls(payload) == 0)
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

const claudeCacheControlTTL1h = "1h"

func upgradeClaudeCacheControlTTL(payload []byte, ttl string) []byte {
	if ttl == "" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	forEachClaudeCacheControlBlock(payload, func(path string, block gjson.Result) {
		cacheControl := block.Get("cache_control")
		if !cacheControl.IsObject() || cacheControl.Get("ttl").Exists() {
			return
		}
		blockType := cacheControl.Get("type")
		if blockType.Type != gjson.String {
			return
		}
		upgraded := `{"type":` + marshalClaudeJSONString(blockType.String()) + `,"ttl":` + marshalClaudeJSONString(ttl)
		if scope := cacheControl.Get("scope"); scope.Exists() {
			upgraded += `,"scope":` + scope.Raw
		}
		upgraded += "}"
		if updated, err := sjson.SetRawBytes(payload, path+".cache_control", []byte(upgraded)); err == nil {
			payload = updated
		}
	})
	return payload
}

func stripClaudeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	forEachClaudeCacheControlBlock(payload, func(path string, block gjson.Result) {
		cacheControl := block.Get("cache_control")
		if !cacheControl.IsObject() || !cacheControl.Get("ttl").Exists() {
			return
		}
		if updated, err := sjson.DeleteBytes(payload, path+".cache_control.ttl"); err == nil {
			payload = updated
		}
	})
	return payload
}

func forEachClaudeCacheControlBlock(payload []byte, visit func(path string, block gjson.Result)) {
	if tools := gjson.GetBytes(payload, "tools"); tools.IsArray() {
		tools.ForEach(func(index, item gjson.Result) bool {
			visit(fmt.Sprintf("tools.%d", int(index.Int())), item)
			return true
		})
	}
	if system := gjson.GetBytes(payload, "system"); system.IsArray() {
		system.ForEach(func(index, item gjson.Result) bool {
			visit(fmt.Sprintf("system.%d", int(index.Int())), item)
			return true
		})
	}
	if messages := gjson.GetBytes(payload, "messages"); messages.IsArray() {
		messages.ForEach(func(messageIndex, message gjson.Result) bool {
			content := message.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIndex, item gjson.Result) bool {
				visit(
					fmt.Sprintf("messages.%d.content.%d", int(messageIndex.Int()), int(itemIndex.Int())),
					item,
				)
				return true
			})
			return true
		})
	}
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

	thread := gjson.GetBytes(payload, "thread")
	if thread.Exists() && thread.Type != gjson.Null && maxBlocks > 0 {
		maxBlocks--
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

// injectMessagesCacheControl adds cache_control to the message selected by the
// native rolling-breakpoint rules. User turns are eligible. Assistant turns are
// eligible unless their final array block is thinking-like. Internal system turns
// are skipped, except for the native final-system string special case.
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	lastMessageIndex := int(messages.Get("#").Int()) - 1
	lastEligibleIndex := -1
	messages.ForEach(func(index, message gjson.Result) bool {
		if role := message.Get("role").String(); role != "user" && role != "assistant" {
			return true
		}
		if claudeMessageEligibleForRollingCache(message) {
			lastEligibleIndex = int(index.Int())
		}
		return true
	})

	if lastEligibleIndex >= 0 {
		finalMessage := messages.Get(fmt.Sprintf("%d", lastMessageIndex))
		finalContent := finalMessage.Get("content")
		if finalMessage.Get("role").String() == "system" &&
			finalContent.Type == gjson.String &&
			strings.TrimSpace(finalContent.String()) != "" {
			return injectClaudeFinalSystemCacheControl(
				payload,
				lastMessageIndex,
				finalContent.String(),
			)
		}
	}
	if lastEligibleIndex < 0 {
		return payload
	}

	contentPath := fmt.Sprintf("messages.%d.content", lastEligibleIndex)
	content := gjson.GetBytes(payload, contentPath)
	if messageContentHasCacheControl(content) {
		return payload
	}

	cacheControl := map[string]string{"type": "ephemeral"}
	if content.IsArray() {
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf(
				"messages.%d.content.%d.cache_control",
				lastEligibleIndex,
				contentCount-1,
			)
			result, err := sjson.SetBytes(payload, cacheControlPath, cacheControl)
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		newContent := "[" + buildClaudeTextBlock(content.String(), cacheControl) + "]"
		result, err := sjson.SetRawBytes(payload, contentPath, []byte(newContent))
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

func claudeMessageEligibleForRollingCache(message gjson.Result) bool {
	content := message.Get("content")
	if content.Type == gjson.String {
		return true
	}
	if !content.IsArray() || content.Get("#").Int() == 0 {
		return false
	}
	if message.Get("role").String() != "assistant" {
		return true
	}
	lastBlock := content.Get(fmt.Sprintf("%d", content.Get("#").Int()-1))
	switch lastBlock.Get("type").String() {
	case "thinking", "redacted_thinking":
		return false
	default:
		return true
	}
}

func injectClaudeFinalSystemCacheControl(payload []byte, messageIndex int, text string) []byte {
	contentPath := fmt.Sprintf("messages.%d.content", messageIndex)
	newContent := "[" + buildClaudeTextBlock(
		text,
		map[string]string{"type": "ephemeral"},
	) + "]"
	result, err := sjson.SetRawBytes(payload, contentPath, []byte(newContent))
	if err != nil {
		log.Warnf("failed to inject cache_control into trailing system message: %v", err)
		return payload
	}
	return result
}

func messageContentHasCacheControl(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	found := false
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Get("cache_control").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
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
		if strings.TrimSpace(system.String()) == "" {
			return payload
		}
		newSystem := "[" + buildClaudeTextBlock(
			system.String(),
			map[string]string{"type": "ephemeral"},
		) + "]"
		result, err := sjson.SetRawBytes(payload, "system", []byte(newSystem))
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

func sanitizeClaudeWebSearchDomains(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}
	for index, tool := range tools.Array() {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			continue
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if !value.IsArray() || len(value.Array()) != 0 {
				continue
			}
			var err error
			body, err = sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.%s", index, field))
			if err != nil {
				return nil, fmt.Errorf("normalize Claude web search domains: %w", err)
			}
		}
	}
	return body, nil
}
