package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
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

const (
	openAICompatImageHandlerType            = "openai-image"
	openAICompatImagesGenerationsPath       = "/images/generations"
	openAICompatImagesEditsPath             = "/images/edits"
	openAICompatDefaultImageEndpoint        = openAICompatImagesGenerationsPath
	openAICompatMultipartMemory       int64 = 32 << 20
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	clientHeaders := req.Header.Clone()
	_, apiKey := e.resolveCredentials(auth)
	bearer := ""
	if strings.TrimSpace(apiKey) != "" {
		bearer = "Bearer " + apiKey
	}
	util.ApplyCredentialHeader(req, "Authorization", bearer)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req *http.Request,
) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
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

func (e *OpenAICompatExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}
	clineNonstream := endpoint == "/chat/completions" && isClineCompatEndpoint(baseURL)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	normalizeToolResults := helps.ShouldNormalizeOpenAIToolResultsForModel(
		e.resolveCompatConfig(auth, req), baseModel, requestedModel,
	)
	originalPayload := originalPayloadSource
	translatedPayload := req.Payload
	if normalizeToolResults && from == sdktranslator.FormatClaude {
		originalPayload = helps.NormalizeClaudeToolResultsTextOnly(originalPayload)
		translatedPayload = helps.NormalizeClaudeToolResultsTextOnly(translatedPayload)
	}
	originalTranslated, translated, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, translatedPayload, opts.Stream, isCompat, &req,
	)
	if err != nil {
		return resp, err
	}
	if normalizeToolResults {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	if opts.Alt != "responses/compact" {
		translated, err = helps.NormalizeOpenAIMaxTokens(
			translated, helps.ShouldUseMaxCompletionTokensForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel),
		)
		if err != nil {
			return resp, err
		}
		translated = e.applyPromptCacheKey(auth, from, baseModel, req, opts, translated, ctx)
	}

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return resp, err
	}
	if clineNonstream {
		translated = helps.SetBoolIfDifferent(translated, "stream", true)
		translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
	}
	translated = applyPayloadConfigWithRequest(
		e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated,
		requestedModel, payloadRequestPath(opts), opts.Headers, e.Identifier(),
	)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
		translated = sanitizeOpenAIResponsesReasoningEncryptedContent(
			ctx,
			"openai-compatible compact",
			translated,
			isCompat,
		)
	}
	reporter.setTranslatedReasoningEffort(translated, to.String())

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	credential := ""
	if apiKey != "" {
		credential = "Bearer " + apiKey
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", credential)
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	if clineNonstream && gjson.GetBytes(translated, "stream").Bool() {
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Cache-Control", "no-cache")
	}
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), translated, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	if clineNonstream {
		httpClient = reporter.trackHTTPClient(httpClient)
	}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		err = newUpstreamStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return resp, err
	}
	var body []byte
	if clineNonstream {
		var observed streamUsageBuffer
		body, err = e.readClineChatCompletion(ctx, httpResp, reporter, &observed)
		if err != nil {
			recordAPIResponseError(ctx, e.cfg, err)
			reporter.publishFailureWithDetail(ctx, observed.detail, err)
			return resp, err
		}
	} else {
		body, err = io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if err != nil {
			err = markResponseStarted(err)
			recordAPIResponseError(ctx, e.cfg, err)
			return resp, err
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
	}
	observeResponseTokenEvent(ctx, body)
	reporter.observeResponseModel(body)
	// 将响应转换为调用方指定的格式
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, opts.OriginalRequest, translated, body, &param)
	if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil || len(out) == 0 {
		if errTranslate == nil {
			errTranslate = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "upstream response translation returned an empty payload"})
		}
		recordAPIResponseError(ctx, e.cfg, errTranslate)
		reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(body), errTranslate)
		return resp, errTranslate
	}
	reporter.publish(ctx, parseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(ctx)
	responseHeaders := httpResp.Header.Clone()
	if clineNonstream {
		if responseHeaders == nil {
			responseHeaders = make(http.Header)
		}
		responseHeaders.Set("Content-Type", "application/json")
		responseHeaders.Del("Content-Length")
		responseHeaders.Del("Content-Encoding")
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: responseHeaders}
	return resp, nil
}

func (e *OpenAICompatExecutor) executeImages(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	endpointPath string,
) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(
		req.Payload, baseModel, opts.Headers.Get("Content-Type"), false,
		func(payload []byte) []byte {
			original := opts.OriginalRequest
			if !json.Valid(original) {
				original = payload
			}
			return applyPayloadConfigWithRequest(
				e.cfg, baseModel, "openai", opts.SourceFormat.String(), "", payload, original,
				payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier(),
			)
		},
	)
	if errPrepare != nil {
		err = errPrepare
		return resp, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	reporter.setTranslatedReasoningEffort(payload, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	credential := ""
	if apiKey != "" {
		credential = "Bearer " + apiKey
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", credential)
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), payload, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	body, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if errRead != nil {
		err = markResponseStarted(errRead)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
		)
		err = newUpstreamStatusErr(httpResp.StatusCode, body, httpResp.Header)
		return resp, err
	}

	observeResponseTokenEvent(ctx, body)
	reporter.observeResponseModel(body)
	reporter.publish(ctx, parseOpenAIUsage(body))
	reporter.ensurePublished(ctx)
	resp = cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if endpointPath := openAICompatImageEndpointPath(opts); endpointPath != "" {
		return e.executeImagesStream(ctx, auth, req, opts, endpointPath)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	normalizeToolResults := helps.ShouldNormalizeOpenAIToolResultsForModel(
		e.resolveCompatConfig(auth, req), baseModel, requestedModel,
	)
	originalPayload := originalPayloadSource
	translatedPayload := req.Payload
	if normalizeToolResults && from == sdktranslator.FormatClaude {
		originalPayload = helps.NormalizeClaudeToolResultsTextOnly(originalPayload)
		translatedPayload = helps.NormalizeClaudeToolResultsTextOnly(translatedPayload)
	}
	originalTranslated, translated, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, translatedPayload, true, isCompat, &req,
	)
	if err != nil {
		return nil, err
	}
	// Request usage by default; explicit payload rules retain final control.
	translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
	if normalizeToolResults {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	translated, err = helps.NormalizeOpenAIMaxTokens(
		translated, helps.ShouldUseMaxCompletionTokensForModel(e.resolveCompatConfig(auth, req), baseModel, requestedModel),
	)
	if err != nil {
		return nil, err
	}
	translated = e.applyPromptCacheKey(auth, from, baseModel, req, opts, translated, ctx)

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}
	translated = applyPayloadConfigWithRequest(
		e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated,
		requestedModel, payloadRequestPath(opts), opts.Headers, e.Identifier(),
	)

	reporter.setTranslatedReasoningEffort(translated, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	credential := ""
	if apiKey != "" {
		credential = "Bearer " + apiKey
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", credential)
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), translated, e.Identifier(), auth)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = newUpstreamStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		terminalDelivered := false
		defer func() {
			publishCtx := ctx
			if terminalDelivered && errors.Is(ctx.Err(), context.Canceled) {
				publishCtx = context.WithoutCancel(ctx)
			}
			streamUsage.publish(publishCtx, reporter)
			reporter.ensurePublished(publishCtx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Split(scanSSEFrame)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		sourceDone := false
		publishStreamError := func(streamErr error) {
			recordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, streamErr)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: streamErr})
		}
		sendTranslated := func(data []byte) bool {
			if isOpenAICompatTokenEvent(data) {
				logging.MarkFirstResponseToken(ctx)
			}
			chunks := sdktranslator.TranslateStream(
				ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, data, &param,
			)
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
				publishStreamError(errTranslate)
				return false
			}
			for _, chunk := range chunks {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunk}) {
					if errStream := scannerOrContextError(ctx, scanner); errStream != nil &&
						!(terminalDelivered && errors.Is(errStream, context.Canceled)) {
						recordAPIResponseError(ctx, e.cfg, errStream)
						reporter.publishFailure(ctx, errStream)
					}
					return false
				}
				if responseFormat == sdktranslator.FormatOpenAIResponse {
					for _, line := range bytes.Split(chunk, []byte("\n")) {
						switch gjson.GetBytes(jsonPayload(line), "type").String() {
						case "response.completed", "response.incomplete":
							terminalDelivered = true
						}
					}
				}
			}
			return true
		}
		for scanner.Scan() {
			frame := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, frame)
			data, hasData, _ := sseDataPayload(frame)
			if !hasData {
				for _, line := range bytes.Split(frame, []byte("\n")) {
					trimmed := bytes.TrimSpace(line)
					if bytes.HasPrefix(trimmed, []byte("{")) || bytes.HasPrefix(trimmed, []byte("[")) {
						publishStreamError(statusErr{code: http.StatusBadGateway, msg: string(trimmed)})
						return
					}
				}
				continue
			}
			if len(data) == 0 {
				continue
			}
			observeResponseTokenEvent(ctx, data)
			reporter.observeResponseModel(data)
			streamUsage.observeOpenAIStream(data)
			if bytes.Equal(data, []byte("[DONE]")) {
				sourceDone = true
			}
			if !sendTranslated(encodeSSEDataFrame(data)) {
				return
			}
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil && !terminalDelivered {
			publishStreamError(errScan)
			return
		}
		if errContext := ctx.Err(); errContext != nil && !terminalDelivered {
			publishStreamError(markResponseStarted(errContext))
			return
		}
		if responseFormat != sdktranslator.FormatOpenAIResponse || terminalDelivered {
			return
		}
		if !sourceDone && ctx.Err() == nil {
			if state, ok := param.(interface{ CanFinalizeResponseStream() bool }); ok && state.CanFinalizeResponseStream() {
				if !sendTranslated([]byte("data: [DONE]")) {
					return
				}
			}
		}
		if !terminalDelivered {
			publishStreamError(statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before a completed response"})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func isOpenAICompatTokenEvent(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true
	}
	if !gjson.ValidBytes(payload) {
		return false
	}
	if gjson.GetBytes(payload, "error").Exists() {
		return true
	}
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		for _, body := range []gjson.Result{choice.Get("delta"), choice.Get("message")} {
			if body.Get("content").String() != "" || body.Get("reasoning_content").String() != "" ||
				body.Get("reasoning").String() != "" || body.Get("refusal").String() != "" {
				return true
			}
			for _, tool := range body.Get("tool_calls").Array() {
				if tool.Get("function.arguments").String() != "" || tool.Get("function.name").String() != "" ||
					tool.Get("custom.input").String() != "" {
					return true
				}
			}
		}
		if choice.Get("finish_reason").String() != "" {
			return true
		}
	}
	return false
}

func (e *OpenAICompatExecutor) executeImagesStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	endpointPath string,
) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	payload, contentType, errPrepare := prepareOpenAICompatImagesPayload(
		req.Payload, baseModel, opts.Headers.Get("Content-Type"), true,
		func(payload []byte) []byte {
			original := opts.OriginalRequest
			if !json.Valid(original) {
				original = payload
			}
			return applyPayloadConfigWithRequest(
				e.cfg, baseModel, "openai", opts.SourceFormat.String(), "", payload, original,
				payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier(),
			)
		},
	)
	if errPrepare != nil {
		err = errPrepare
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	reporter.setTranslatedReasoningEffort(payload, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	credential := ""
	if apiKey != "" {
		credential = "Bearer " + apiKey
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", credential)
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), payload, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			errRead = markResponseStarted(errRead)
			recordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, body)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
		)
		return nil, newUpstreamStatusErr(httpResp.StatusCode, body, httpResp.Header)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		streamUsage := streamUsageBuffer{modelReporter: reporter}
		defer func() {
			streamUsage.flushOpenAIStream()
			streamUsage.publish(ctx, reporter)
			reporter.ensurePublished(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if errRead != nil && errRead != io.EOF {
				errRead = markResponseStarted(errRead)
				recordAPIResponseError(ctx, e.cfg, errRead)
				reporter.publishFailure(ctx, errRead)
			}
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				appendAPIResponseChunk(ctx, e.cfg, chunk)
				streamUsage.observeOpenAIStreamChunk(chunk)
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunk}) {
					return
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errRead})
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenAICompatExecutor) CountTokens(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := originalRequestPayload(req, opts)
	originalTranslated, translated, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayloadSource, req.Payload, false, helps.APIKeyModelIsCompat(req), &req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	modelForCounting := baseModel

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translated = applyPayloadConfigWithRequest(
		e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated,
		payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier(),
	)

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, cliproxyexecutor.ResponseFormatOrSource(opts), count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
// OAuth credentials must be bound to a plugin or Home refresh owner.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if openAICompatAuthHasRefreshToken(auth) {
		provider := e.Identifier()
		if provider == "" && auth != nil {
			provider = strings.TrimSpace(auth.Provider)
		}
		return nil, fmt.Errorf("openai compat executor cannot refresh oauth credentials for provider %s", provider)
	}
	return auth, nil
}

func openAICompatAuthHasRefreshToken(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if token, _ := auth.Metadata["refresh_token"].(string); strings.TrimSpace(token) != "" {
		return true
	}
	if token, _ := auth.Metadata["refreshToken"].(string); strings.TrimSpace(token) != "" {
		return true
	}
	return false
}

func openAICompatImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != openAICompatImageHandlerType {
		return ""
	}
	path := payloadRequestPath(opts)
	if strings.HasSuffix(path, "/images/edits") {
		return openAICompatImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return openAICompatImagesGenerationsPath
	}
	return openAICompatDefaultImageEndpoint
}

func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool, configure ...func([]byte) []byte) (
	[]byte,
	string,
	error,
) {
	model = strings.TrimSpace(model)
	contentType = strings.TrimSpace(contentType)
	if json.Valid(payload) {
		if model != "" {
			payload = helps.SetStringIfDifferent(payload, "model", model)
		}
		if stream {
			payload = helps.SetBoolIfDifferent(payload, "stream", true)
		} else {
			payload, _ = sjson.DeleteBytes(payload, "stream")
		}
		if len(configure) > 0 {
			payload = configure[0](payload)
		}
		return payload, "application/json", nil
	}

	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return payload, contentType, nil
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream, configure...)
}

func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool, configure ...func([]byte) []byte) (
	out []byte,
	contentType string,
	err error,
) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("openai compat executor: remove multipart form files error: %v", errRemove)
		}
	}()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	defer func() {
		if err != nil {
			_ = writer.Close()
		}
	}()
	delete(form.Value, "model")
	delete(form.Value, "stream")
	if model != "" {
		form.Value["model"] = []string{model}
	}
	if stream {
		form.Value["stream"] = []string{"true"}
	}
	if len(configure) > 0 {
		fields := make(map[string]any, len(form.Value))
		for key, values := range form.Value {
			if len(values) == 1 {
				fields[key] = values[0]
			} else {
				fields[key] = values
			}
		}
		encoded, errEncode := json.Marshal(fields)
		if errEncode != nil {
			return nil, "", fmt.Errorf("encode multipart fields failed: %w", errEncode)
		}
		configured := configure[0](encoded)
		if !gjson.ValidBytes(configured) || !gjson.ParseBytes(configured).IsObject() {
			return nil, "", fmt.Errorf("configured multipart fields must be a JSON object")
		}
		form.Value = make(map[string][]string)
		gjson.ParseBytes(configured).ForEach(func(key, value gjson.Result) bool {
			if value.IsArray() {
				for _, item := range value.Array() {
					form.Value[key.String()] = append(form.Value[key.String()], item.String())
				}
			} else {
				form.Value[key.String()] = []string{value.String()}
			}
			return true
		})
	}
	for key, values := range form.Value {
		for _, value := range values {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				return nil, "", fmt.Errorf("write form field %s failed: %w", key, errWrite)
			}
		}
	}
	for key, files := range form.File {
		for _, fileHeader := range files {
			if fileHeader == nil {
				continue
			}
			header := cloneOpenAICompatMIMEHeader(fileHeader.Header)
			header.Set("Content-Disposition", multipart.FileContentDisposition(key, fileHeader.Filename))
			if header.Get("Content-Type") == "" {
				header.Set("Content-Type", "application/octet-stream")
			}
			part, errCreate := writer.CreatePart(header)
			if errCreate != nil {
				return nil, "", fmt.Errorf("create file field %s failed: %w", key, errCreate)
			}
			src, errOpen := fileHeader.Open()
			if errOpen != nil {
				return nil, "", fmt.Errorf("open upload file failed: %w", errOpen)
			}
			_, errCopy := io.Copy(part, src)
			if errClose := src.Close(); errClose != nil {
				log.Errorf("openai compat executor: close upload file error: %v", errClose)
				if errCopy == nil {
					errCopy = errClose
				}
			}
			if errCopy != nil {
				return nil, "", fmt.Errorf("copy upload file failed: %w", errCopy)
			}
		}
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, "", fmt.Errorf("close multipart writer failed: %w", errClose)
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func (e *OpenAICompatExecutor) applyPromptCacheKey(
	auth *cliproxyauth.Auth,
	from sdktranslator.Format,
	baseModel string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	translated []byte,
	ctx context.Context,
) []byte {
	if ctx == nil {
		ctx = context.Background()
	}
	compat := e.resolveCompatConfig(auth, req)
	if compat == nil || !compat.SupportPromptCacheKey {
		return translated
	}

	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest, translated} {
		if promptCacheKey := strings.TrimSpace(
			gjson.GetBytes(
				payload, "prompt_cache_key",
			).String(),
		); promptCacheKey != "" {
			updated, errSet := sjson.SetBytes(translated, "prompt_cache_key", promptCacheKey)
			if errSet == nil {
				return updated
			}
			return translated
		}
	}

	modelName := strings.TrimSpace(gjson.GetBytes(translated, "model").String())
	if modelName == "" {
		modelName = baseModel
	}
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		if cached, ok := codexPromptCacheFromClaudeRequest(
			ctx,
			modelName,
			req.Payload,
			opts.Headers,
		); ok {
			updated, errSet := sjson.SetBytes(translated, "prompt_cache_key", cached.ID)
			if errSet == nil {
				return updated
			}
			return translated
		}
	}

	sessionID := xaiExecutionSessionID(req, opts)
	if sessionID == "" {
		return translated
	}
	provider := strings.TrimSpace(e.provider)
	if provider == "" {
		provider = strings.TrimSpace(compat.Name)
	}
	identity := strings.Join(
		[]string{
			"cli-proxy-api:openai-compat:prompt-cache",
			strings.ToLower(provider),
			strings.ToLower(modelName),
			strings.ToLower(strings.TrimSpace(from.String())),
			sessionID,
		}, "\x00",
	)
	promptCacheKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
	updated, errSet := sjson.SetBytes(translated, "prompt_cache_key", promptCacheKey)
	if errSet != nil {
		return translated
	}
	return updated
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(
	auth *cliproxyauth.Auth, req cliproxyexecutor.Request,
) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	if e.cfg.Home.Enabled {
		var options struct {
			SupportPromptCacheKey bool                              `json:"support-prompt-cache-key"`
			Models                []config.OpenAICompatibilityModel `json:"models"`
		}
		if raw, exists := auth.Metadata["credential_options"]; exists {
			if data, errMarshal := json.Marshal(raw); errMarshal == nil {
				if errDecode := json.Unmarshal(data, &options); errDecode != nil {
					log.Warn("home credential options cannot be decoded")
				}
			} else {
				log.Warn("home credential options cannot be decoded")
			}
		}
		if raw, exists := auth.Attributes["support_prompt_cache_key"]; exists {
			if value, errParse := strconv.ParseBool(raw); errParse == nil {
				options.SupportPromptCacheKey = value
			} else {
				log.Warn("home credential prompt cache option is invalid")
			}
		}
		if model, configured := cliproxyauth.ResolvedHomeModelOptions(req); configured {
			options.Models = []config.OpenAICompatibilityModel{model}
		}
		return &config.OpenAICompatibility{
			Name:                  auth.Attributes["compat_name"],
			SupportPromptCacheKey: options.SupportPromptCacheKey,
			Models:                options.Models,
		}
	}
	if auth.Attributes != nil {
		if rawIndex := strings.TrimSpace(auth.Attributes["config_index"]); rawIndex != "" {
			configIndex, errIndex := strconv.Atoi(rawIndex)
			if errIndex == nil && configIndex >= 0 && configIndex < len(e.cfg.OpenAICompatibility) {
				compat := &e.cfg.OpenAICompatibility[configIndex]
				if !compat.Disabled {
					return compat
				}
			}
		}
	}

	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["compat_name"]); value != "" {
			candidates = append(candidates, value)
		}
		if value := strings.TrimSpace(auth.Attributes["provider_key"]); value != "" {
			candidates = append(candidates, value)
		}
	}
	if value := strings.TrimSpace(auth.Provider); value != "" {
		candidates = append(candidates, value)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if strings.EqualFold(strings.TrimSpace(compat.Name), candidate) {
				return compat
			}
		}
	}
	return nil
}

type statusErr struct {
	code           int
	msg            string
	logDiagnostic  string
	retryAfter     *time.Duration
	requestScoped  bool
	directResponse bool
	dispatchPhase  cliproxyexecutor.DispatchPhase
}

func (e statusErr) DirectResponse() bool { return e.directResponse }
func (e statusErr) ResponseBody() []byte {
	if !e.directResponse {
		return nil
	}
	return []byte(e.msg)
}

func (e statusErr) Error() string {
	if e.msg != "" || e.directResponse {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
func (e statusErr) IsRequestScoped() bool      { return e.requestScoped }
