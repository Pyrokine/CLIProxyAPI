package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	normalizeToolResults := helps.ShouldNormalizeOpenAIToolResultsForModel(
		e.resolveCompatConfig(auth), baseModel, requestedModel,
	)
	originalPayload := originalPayloadSource
	translatedPayload := req.Payload
	if normalizeToolResults && from == sdktranslator.FormatClaude {
		originalPayload = helps.NormalizeClaudeToolResultsTextOnly(originalPayload)
		translatedPayload = helps.NormalizeClaudeToolResultsTextOnly(translatedPayload)
	}
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		originalPayload,
		opts.Stream,
		isCompat,
	)
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		translatedPayload,
		opts.Stream,
		isCompat,
	)
	translated = applyPayloadConfigWithRoot(
		e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel,
	)
	if normalizeToolResults {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	if opts.Alt != "responses/compact" {
		translated = e.applyPromptCacheKey(auth, from, baseModel, req, opts, translated, ctx)
	}
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

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return resp, err
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
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), translated, e.Identifier(), auth)

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
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	reporter.publish(ctx, parseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(ctx)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
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
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	normalizeToolResults := helps.ShouldNormalizeOpenAIToolResultsForModel(
		e.resolveCompatConfig(auth), baseModel, requestedModel,
	)
	originalPayload := originalPayloadSource
	translatedPayload := req.Payload
	if normalizeToolResults && from == sdktranslator.FormatClaude {
		originalPayload = helps.NormalizeClaudeToolResultsTextOnly(originalPayload)
		translatedPayload = helps.NormalizeClaudeToolResultsTextOnly(translatedPayload)
	}
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		originalPayload,
		true,
		isCompat,
	)
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		translatedPayload,
		true,
		isCompat,
	)
	translated = applyPayloadConfigWithRoot(
		e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel,
	)
	if normalizeToolResults {
		translated = helps.NormalizeOpenAIToolResultsTextOnly(translated)
	}
	translated = e.applyPromptCacheKey(auth, from, baseModel, req, opts, translated, ctx)

	translated, err = helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	// Request usage data in the final streaming chunk so that token statistics
	// are captured even when the upstream is an OpenAI-compatible provider.
	translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
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

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
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
		defer func() {
			streamUsage.publish(ctx, reporter)
			reporter.ensurePublished(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			streamUsage.observeOpenAIStream(line)
			trimmedLine := bytes.TrimSpace(line)
			if len(trimmedLine) == 0 {
				continue
			}

			if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
				if bytes.HasPrefix(trimmedLine, []byte(":")) || bytes.HasPrefix(trimmedLine, []byte("event:")) ||
					bytes.HasPrefix(trimmedLine, []byte("id:")) || bytes.HasPrefix(trimmedLine, []byte("retry:")) {
					continue
				}
				if bytes.HasPrefix(trimmedLine, []byte("{")) || bytes.HasPrefix(trimmedLine, []byte("[")) {
					streamErr := statusErr{code: http.StatusBadGateway, msg: string(trimmedLine)}
					recordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.publishFailure(ctx, streamErr)
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: streamErr})
					return
				}
				continue
			}

			// OpenAI-compatible streams must use SSE data lines.
			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(trimmedLine), &param,
			)
			for i := range chunks {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
						recordAPIResponseError(ctx, e.cfg, errStream)
						reporter.publishFailure(ctx, errStream)
					}
					return
				}
			}
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
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
		var streamUsage streamUsageBuffer
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
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		e.cfg,
		from,
		to,
		baseModel,
		req.Payload,
		false,
		helps.APIKeyModelIsCompat(req),
	)

	modelForCounting := baseModel

	translated, err := helps.ApplyThinkingWithSourcePayload(
		translated, req.Payload, req.Payload, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
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

func prepareOpenAICompatImagesPayload(payload []byte, model string, contentType string, stream bool) (
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
	return rewriteOpenAICompatImagesMultipartPayload(payload, model, boundary, stream)
}

func cloneOpenAICompatMIMEHeader(src textproto.MIMEHeader) textproto.MIMEHeader {
	dst := make(textproto.MIMEHeader, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func rewriteOpenAICompatImagesMultipartPayload(payload []byte, model string, boundary string, stream bool) (
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
	if model != "" {
		if errWrite := writer.WriteField("model", model); errWrite != nil {
			return nil, "", fmt.Errorf("write model field failed: %w", errWrite)
		}
	}
	if stream {
		if errWrite := writer.WriteField("stream", "true"); errWrite != nil {
			return nil, "", fmt.Errorf("write stream field failed: %w", errWrite)
		}
	}
	for key, values := range form.Value {
		if key == "model" || key == "stream" {
			continue
		}
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
	compat := e.resolveCompatConfig(auth)
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

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
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
