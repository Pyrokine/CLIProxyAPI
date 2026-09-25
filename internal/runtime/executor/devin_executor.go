package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxDevinErrorBodySize = 1 << 20

// DevinExecutor executes requests against Devin's Connect-RPC chat backend.
type DevinExecutor struct {
	cfg *config.Config

	matcherMu     sync.RWMutex
	lastWordsKey  string
	cachedMatcher *helps.SensitiveWordMatcher
}

type devinPreparedRequest struct {
	body     []byte
	httpReq  *http.Request
	reporter *usageReporter
}

// NewDevinExecutor creates a Devin executor.
func NewDevinExecutor(cfg *config.Config) *DevinExecutor {
	return &DevinExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *DevinExecutor) Identifier() string { return "devin" }

// RequestToFormat reports the intermediate request format consumed by Devin.
func (e *DevinExecutor) RequestToFormat(
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) sdktranslator.Format {
	return sdktranslator.FormatInteractions
}

// PrepareRequest applies Devin authentication and Connect-RPC headers.
func (e *DevinExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, _ := devinAuthCredentials(auth)
	basic := ""
	if token != "" {
		basic = "Basic " + token + "-" + token
	}
	util.ApplyCredentialHeader(req, "Authorization", basic)
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")
	if !isDevinUnaryRequest(req) && req.Header.Get("Sentry-Trace") == "" {
		req.Header.Set("Sentry-Trace", helps.GenerateDevinSentryTrace())
	}
	req.Header["User-Agent"] = []string{""}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

func isDevinUnaryRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	path := req.URL.Path
	return strings.Contains(path, "GetUserStatus") ||
		strings.Contains(path, "GetCliModelConfigs") ||
		strings.Contains(path, "SeatManagementService")
}

// HttpRequest applies Devin credentials to an arbitrary request and executes it.
func (e *DevinExecutor) HttpRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("devin executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return newDevinHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

// Execute performs a non-streaming Devin request.
func (e *DevinExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	prepared, err := e.prepareDevinRequest(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	defer prepared.reporter.trackFailure(ctx, &err)

	httpClient := prepared.reporter.trackHTTPClient(newDevinHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(prepared.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if closeErr := httpResp.Body.Close(); closeErr != nil {
			log.Errorf("devin executor: close response body error: %v", closeErr)
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxDevinErrorBodySize))
		appendAPIResponseChunk(ctx, e.cfg, body)
		err = newUpstreamStatusErr(httpResp.StatusCode, body, httpResp.Header)
		return resp, err
	}

	interaction, summary, consumeErr := consumeDevinFrames(httpResp.Body, req.Model)
	if len(summary) > 0 {
		appendAPIResponseChunk(ctx, e.cfg, summary)
	}
	if consumeErr != nil {
		recordAPIResponseError(ctx, e.cfg, consumeErr)
		return resp, consumeErr
	}
	prepared.reporter.publish(ctx, parseInteractionsUsage(interaction))

	responseFormat := devinResponseFormat(opts)
	var parameter any
	translated := sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FormatInteractions,
		responseFormat,
		req.Model,
		opts.OriginalRequest,
		prepared.body,
		interaction,
		&parameter,
	)
	return cliproxyexecutor.Response{
		Payload: translated,
		Headers: httpResp.Header.Clone(),
	}, nil
}

// ExecuteStream performs a streaming Devin request.
func (e *DevinExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	prepared, err := e.prepareDevinRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	defer prepared.reporter.trackFailure(ctx, &err)

	httpClient := prepared.reporter.trackHTTPClient(newDevinHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(prepared.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxDevinErrorBodySize))
		if closeErr := httpResp.Body.Close(); closeErr != nil {
			log.Errorf("devin executor: close error response body error: %v", closeErr)
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
		return nil, newUpstreamStatusErr(httpResp.StatusCode, body, httpResp.Header)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	streamCtx, cancelStream := context.WithCancel(ctx)
	go func() {
		select {
		case <-streamCtx.Done():
			_ = httpResp.Body.Close()
		case <-ctx.Done():
			_ = httpResp.Body.Close()
		}
	}()
	go func() {
		defer close(out)
		defer cancelStream()
		defer func() {
			if closeErr := httpResp.Body.Close(); closeErr != nil {
				log.Errorf("devin executor: close stream body error: %v", closeErr)
			}
		}()
		e.streamDevinFrames(
			streamCtx,
			httpResp.Body,
			req,
			opts,
			prepared.body,
			prepared.reporter,
			out,
		)
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: httpResp.Header.Clone(),
		Chunks:  out,
	}, nil
}

func (e *DevinExecutor) prepareDevinRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
) (*devinPreparedRequest, error) {
	token, baseURL, deviceSeed := devinAuthCredentials(auth)
	if token == "" {
		return nil, statusErr{
			code:          http.StatusUnauthorized,
			msg:           "devin credentials missing: api_key or session_token required",
			dispatchPhase: cliproxyexecutor.DispatchPhaseNotSent,
		}
	}

	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatInteractions
	}
	originalSource := opts.OriginalRequest
	if len(originalSource) == 0 {
		originalSource = req.Payload
	}
	body := translateDevinRequest(ctx, from, req.Model, req.Payload, stream)
	originalTranslated := translateDevinRequest(ctx, from, req.Model, originalSource, stream)
	var err error
	body, err = helps.ApplyThinkingWithSourcePayload(
		body,
		req.Payload,
		originalSource,
		req.Model,
		from.String(),
		sdktranslator.FormatInteractions.String(),
		e.Identifier(),
		req,
	)
	if err != nil {
		return nil, devinRequestError(err)
	}
	body = applyPayloadConfigWithRequest(
		e.cfg,
		thinking.ParseSuffix(req.Model).ModelName,
		sdktranslator.FormatInteractions.String(),
		from.String(),
		"",
		body,
		originalTranslated,
		payloadRequestedModel(opts, req.Model),
		payloadRequestPath(opts),
		opts.Headers,
	)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, devinRequestError(errors.New("translated Devin request is not valid JSON"))
	}

	parsed := parseDevinInteractionsPayload(body, opts.OriginalRequest)
	parsed.sessionID, parsed.cascadeID = resolveDevinSessionAndCascadeIDs(
		ctx,
		auth,
		parsed.sessionID,
		parsed.cascadeID,
		req.Metadata,
		opts.Metadata,
	)
	chatModelUID, modelInfo, err := helps.ResolveDevinChatModel(
		req.Model,
		parsed.thinkingLevel,
		parsed.budgetTokens,
	)
	if err != nil {
		return nil, devinRequestError(err)
	}
	if modelInfo != nil {
		limit := modelInfo.MaxCompletionTokens
		if limit <= 0 {
			limit = modelInfo.OutputTokenLimit
		}
		if limit > 0 && (parsed.maxTokens <= 0 || parsed.maxTokens > limit) {
			parsed.maxTokens = limit
		}
	}

	matcher := e.sensitiveWordMatcher()
	protoBody := helps.BuildDevinGetChatMessageRequest(
		token,
		deviceSeed,
		chatModelUID,
		parsed.systemPrompt,
		parsed.prompts,
		parsed.tools,
		parsed.temperature,
		parsed.maxTokens,
		parsed.sessionID,
		parsed.cascadeID,
		matcher,
	)
	framedBody := helps.WrapConnectEnvelope(protoBody)
	endpoint := strings.TrimRight(baseURL, "/") + helps.DevinChatPath
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(framedBody),
	)
	if err != nil {
		return nil, err
	}
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	logBody := devinRequestLogBody(
		body,
		helps.SanitizeDevinSystemPrompt(parsed.systemPrompt, matcher),
	)
	recordUpstreamRequest(
		ctx,
		e.cfg,
		endpoint,
		httpReq.Header.Clone(),
		logBody,
		e.Identifier(),
		auth,
	)

	reporter := newUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	reporter.setTranslatedReasoningEffort(body, sdktranslator.FormatInteractions.String())
	return &devinPreparedRequest{
		body:     body,
		httpReq:  httpReq,
		reporter: reporter,
	}, nil
}

func translateDevinRequest(
	ctx context.Context,
	from sdktranslator.Format,
	model string,
	payload []byte,
	stream bool,
) []byte {
	if from == sdktranslator.FormatInteractions {
		return bytes.Clone(payload)
	}
	return sdktranslator.TranslateRequestWithContext(
		ctx,
		from,
		sdktranslator.FormatInteractions,
		model,
		bytes.Clone(payload),
		stream,
	)
}

func devinRequestLogBody(payload []byte, sanitizedSystemPrompt string) []byte {
	logged := bytes.Clone(payload)
	for _, path := range []string{"system_instruction", "systemInstruction"} {
		if !gjson.GetBytes(logged, path).Exists() {
			continue
		}
		if sanitizedSystemPrompt == "" {
			logged, _ = sjson.DeleteBytes(logged, path)
		} else {
			logged, _ = sjson.SetBytes(logged, path, sanitizedSystemPrompt)
		}
	}
	return logged
}

func devinRequestError(err error) error {
	if err == nil {
		return nil
	}
	return statusErr{
		code:          http.StatusBadRequest,
		msg:           err.Error(),
		requestScoped: true,
		dispatchPhase: cliproxyexecutor.DispatchPhaseNotSent,
	}
}

func devinResponseFormat(opts cliproxyexecutor.Options) sdktranslator.Format {
	format := cliproxyexecutor.ResponseFormatOrSource(opts)
	if format == "" {
		return sdktranslator.FormatInteractions
	}
	return format
}

// CountTokens returns a local estimate because Devin has no token-count endpoint.
func (e *DevinExecutor) CountTokens(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	count := int64((len(req.Payload) + 3) / 4)
	metadata := map[string]any{
		"estimated": true,
		"method":    "utf8_bytes_div_4",
	}
	targetFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var payload []byte
	if targetFormat == "" || targetFormat == sdktranslator.FormatInteractions {
		payload = []byte(fmt.Sprintf(`{"total_tokens":%d,"input_tokens":%d}`, count, count))
	} else {
		payload = sdktranslator.TranslateTokenCount(
			ctx,
			sdktranslator.FormatOpenAI,
			targetFormat,
			count,
			buildOpenAIUsageJSON(count),
		)
	}
	payload, err := sjson.SetBytes(payload, "estimated", true)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("mark Devin token estimate: %w", err)
	}
	return cliproxyexecutor.Response{Payload: payload, Metadata: metadata}, nil
}

// Refresh updates Devin account and quota observations without invoking reset APIs.
func (e *DevinExecutor) Refresh(
	ctx context.Context,
	auth *cliproxyauth.Auth,
) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, errors.New("devin executor: auth is nil")
	}
	token, baseURL, deviceSeed := devinAuthCredentials(auth)
	if token == "" {
		return auth, nil
	}

	client := newDevinHTTPClient(ctx, e.cfg, auth, 30*time.Second)
	service := devinauth.NewDevinAuthService(client)
	service.SetServerBaseURL(baseURL)
	status, err := service.FetchUserStatus(ctx, token, deviceSeed)
	if err != nil {
		return auth, err
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	now := time.Now().UTC()
	applyDevinIdentityStatus(updated, status)
	applyDevinQuotaStatus(updated, status, now)
	updated.LastRefreshedAt = now
	return updated, nil
}

func applyDevinIdentityStatus(auth *cliproxyauth.Auth, status *devinauth.DevinUserStatus) {
	if auth == nil || status == nil {
		return
	}
	fields := []struct {
		key   string
		value string
	}{
		{key: "email", value: status.Email},
		{key: "user_name", value: status.UserName},
		{key: "user_id", value: status.UserID},
		{key: "team_id", value: status.TeamID},
		{key: "org_id", value: status.OrgID},
		{key: "org_name", value: status.OrgName},
		{key: "plan", value: status.Plan},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			continue
		}
		auth.Metadata[field.key] = field.value
		auth.Attributes[field.key] = field.value
	}
}

func applyDevinQuotaStatus(
	auth *cliproxyauth.Auth,
	status *devinauth.DevinUserStatus,
	observedAt time.Time,
) {
	if auth == nil || status == nil {
		return
	}
	for _, key := range []string{
		"daily_quota_present",
		"daily_quota_remaining_percent",
		"daily_quota_reset_at",
		"weekly_quota_present",
		"weekly_quota_remaining_percent",
		"weekly_quota_reset_at",
		"plan_start",
		"plan_end",
		"quota_observed_at",
	} {
		delete(auth.Metadata, key)
	}
	devinauth.ApplyQuotaStatus(&auth.Quota, status, observedAt)
}

func (e *DevinExecutor) sensitiveWordMatcher() *helps.SensitiveWordMatcher {
	if e == nil || e.cfg == nil || len(e.cfg.Devin.SensitiveWords) == 0 {
		return nil
	}
	words := e.cfg.Devin.SensitiveWords
	key := strings.Join(words, "\x00")
	e.matcherMu.RLock()
	if e.lastWordsKey == key {
		matcher := e.cachedMatcher
		e.matcherMu.RUnlock()
		return matcher
	}
	e.matcherMu.RUnlock()

	e.matcherMu.Lock()
	defer e.matcherMu.Unlock()
	if e.lastWordsKey != key {
		e.lastWordsKey = key
		e.cachedMatcher = helps.BuildSensitiveWordMatcher(words)
	}
	return e.cachedMatcher
}

func devinAuthCredentials(auth *cliproxyauth.Auth) (token, baseURL, deviceSeed string) {
	return devinauth.CredentialsFromAuth(auth)
}

func resolveDevinSessionAndCascadeIDs(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	requestSession string,
	requestCascade string,
	metadataSets ...map[string]any,
) (string, string) {
	canonicalSession := helps.ProviderSessionUUID("devin", metadataSets...)
	rawSession := canonicalSession
	if rawSession == "" {
		rawSession = strings.TrimSpace(requestSession)
	}
	if rawSession == "" {
		rawSession = util.SessionIDFromContext(ctx)
	}
	if rawSession == "" {
		rawSession = uuid.NewString()
	}

	authScope := devinAuthScope(auth)
	sessionID := normalizeDevinUUID(rawSession)
	if authScope != "" {
		sessionID = scopedDevinUUID("session", authScope, sessionID)
	}
	if canonicalSession != "" || strings.TrimSpace(requestCascade) == "" || requestCascade == requestSession {
		return sessionID, sessionID
	}
	cascadeID := normalizeDevinUUID(requestCascade)
	if authScope != "" {
		cascadeID = scopedDevinUUID("cascade", authScope, cascadeID)
	}
	return sessionID, cascadeID
}

func normalizeDevinUUID(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := uuid.Parse(value); err == nil {
		return parsed.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(value)).String()
}

func scopedDevinUUID(kind, authScope, sessionID string) string {
	seed := strings.Join([]string{"cli-proxy-api", "devin", kind, authScope, sessionID}, "\x00")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func devinAuthScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := firstNonEmptyDevinValue(auth.Index, auth.ID, auth.FileName); value != "" {
		return value
	}
	token, _, _ := devinAuthCredentials(auth)
	if token == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(token))
	return fmt.Sprintf("token:%x", digest[:16])
}
