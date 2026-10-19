package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
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

const kimiReasoningUnavailable = "[reasoning unavailable]"

// KimiExecutor is a stateless executor for Kimi API using OpenAI-compatible chat completions.
type KimiExecutor struct {
	ClaudeExecutor
	cfg      *config.Config
	provider string
}

// kimiRequest holds the prepared artifacts shared by Execute and ExecuteStream.
type kimiRequest struct {
	responseFormat, to sdktranslator.Format
	body               []byte
	httpReq            *http.Request
	reporter           *usageReporter
}

// NewKimiExecutor creates a new Kimi executor.
func NewKimiExecutor(cfg *config.Config) *KimiExecutor {
	return &KimiExecutor{
		ClaudeExecutor: ClaudeExecutor{cfg: cfg, requestLogProvider: "kimi"},
		cfg:            cfg,
		provider:       "kimi",
	}
}

// NewKimiAIExecutor creates an executor for the Kimi.ai service.
func NewKimiAIExecutor(cfg *config.Config) *KimiExecutor {
	e := NewKimiExecutor(cfg)
	e.provider = "kimi-ai"
	e.ClaudeExecutor.requestLogProvider = "kimi-ai"
	return e
}

// Identifier returns the executor identifier.
func (e *KimiExecutor) Identifier() string {
	if e.provider == "" {
		return "kimi"
	}
	return e.provider
}

func applyKimiCustomHeaders(req *http.Request, attrs map[string]string, clientHeaders ...http.Header) {
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders[0])
	} else {
		util.ApplyCustomHeadersFromAttrs(req, attrs)
	}
	if req != nil {
		req.Header.Del("Content-Length")
	}
}

// PrepareRequest injects Kimi credentials into the outgoing HTTP request.
func (e *KimiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := kimiCreds(auth)
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(req, "Authorization", bearer)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	applyKimiCustomHeaders(req, attrs)
	return nil
}

// HttpRequest injects Kimi credentials into the request and executes it.
func (e *KimiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("kimi executor: request is nil")
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

// prepareKimiRequest builds the translated, config-applied HTTP request shared by Execute and ExecuteStream.
func (e *KimiExecutor) prepareKimiRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	isStream bool,
) (*kimiRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := kimiCreds(auth)

	reporter := newUsageReporter(ctx, e, baseModel, auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	originalTranslated, _ := sdktranslator.TranslateRequestChecked(ctx, from, to, baseModel, originalPayload, isStream)
	body, err := sdktranslator.TranslateRequestChecked(ctx, from, to, baseModel, bytes.Clone(req.Payload), isStream)
	if err != nil {
		return nil, err
	}

	upstreamModel := normalizeKimiUpstreamModel(baseModel)
	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), "kimi", e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	if isStream {
		body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
		if err != nil {
			return nil, fmt.Errorf("kimi executor: failed to set stream_options in payload: %w", err)
		}
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, payloadRequestPath(opts), opts.Headers, e.Identifier())
	body, err = normalizeKimiMessages(body)
	if err != nil {
		return nil, err
	}
	body = normalizeKimiTools(body)
	body = normalizeKimiTemperature(body)
	reporter.setTranslatedReasoningEffort(body, e.Identifier())

	url := helps.ResolveKimiChatURL(auth)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyKimiHeadersWithAuth(httpReq, token, isStream, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	applyKimiCustomHeaders(httpReq, attrs, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	return &kimiRequest{
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
		to:             to,
		body:           body,
		httpReq:        httpReq,
		reporter:       reporter,
	}, nil
}

// Execute performs a non-streaming chat completion request to Kimi.
func (e *KimiExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		if auth == nil {
			return resp, fmt.Errorf("kimi executor: auth is nil")
		}
		auth = auth.Clone()
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes["base_url"] = helps.ResolveKimiClaudeBaseURL(auth)
		req.Model = normalizeKimiUpstreamModel(req.Model)
		preparedReq, replayScope, errPrepare := prepareKimiThinkingReplayRequest(ctx, req, opts)
		if errPrepare != nil {
			return resp, errPrepare
		}
		claudeResp, errExecute := e.ClaudeExecutor.Execute(ctx, auth, preparedReq, opts)
		if errExecute != nil {
			if replayScope.replayApplied && shouldClearKimiThinkingReplayAfterError(errExecute) {
				clearKimiThinkingReplayContent(ctx, replayScope)
			}
			return claudeResp, errExecute
		}
		cacheKimiThinkingReplayResponse(ctx, replayScope, claudeResp.Payload)
		return claudeResp, nil
	}
	if from == sdktranslator.FormatOpenAIResponse {
		return e.executeResponses(ctx, auth, req, opts)
	}

	prepared, err := e.prepareKimiRequest(ctx, auth, req, opts, false)
	if err != nil {
		return resp, err
	}
	defer prepared.reporter.ensurePublished(ctx)
	defer prepared.reporter.trackFailure(ctx, &err)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(prepared.httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
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
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	observeResponseTokenEvent(ctx, data)
	prepared.reporter.observeResponseModel(data)
	detail := parseOpenAIUsage(data)
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(
		ctx, prepared.to, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, data, &param,
	)
	translationErr := sdktranslator.ResponseTranslationError(&param)
	if translationErr == nil && len(out) == 0 {
		translationErr = fmt.Errorf("kimi executor: response translation returned an empty payload")
	}
	if translationErr != nil {
		translationErr = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
		recordAPIResponseError(ctx, e.cfg, translationErr)
		prepared.reporter.publishFailureWithDetail(ctx, detail, translationErr)
		return resp, translationErr
	}
	prepared.reporter.publish(ctx, detail)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Kimi.
func (e *KimiExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		if auth == nil {
			return nil, fmt.Errorf("kimi executor: auth is nil")
		}
		auth = auth.Clone()
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string)
		}
		auth.Attributes["base_url"] = helps.ResolveKimiClaudeBaseURL(auth)
		req.Model = normalizeKimiUpstreamModel(req.Model)
		preparedReq, replayScope, errPrepare := prepareKimiThinkingReplayRequest(ctx, req, opts)
		if errPrepare != nil {
			return nil, errPrepare
		}
		var observer claudeStreamObserver
		if replayScope.valid() && replayScope.cacheReady {
			observer = newKimiThinkingReplayStreamObserver(ctx, replayScope)
		}
		claudeResult, errExecute := e.ClaudeExecutor.executeStreamWithObserver(
			ctx,
			auth,
			preparedReq,
			opts,
			observer,
		)
		if errExecute != nil {
			if replayScope.replayApplied && shouldClearKimiThinkingReplayAfterError(errExecute) {
				clearKimiThinkingReplayContent(ctx, replayScope)
			}
			return nil, errExecute
		}
		return claudeResult, nil
	}
	if from == sdktranslator.FormatOpenAIResponse {
		return e.executeResponsesStream(ctx, auth, req, opts)
	}

	prepared, err := e.prepareKimiRequest(ctx, auth, req, opts, true)
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
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), b),
		)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
		err = newUpstreamStatusErr(httpResp.StatusCode, b, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter := prepared.reporter
	body := prepared.body
	from = prepared.responseFormat
	to := prepared.to
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		defer func() {
			if errStream := scannerOrContextError(ctx, nil); errStream != nil {
				recordAPIResponseError(ctx, e.cfg, errStream)
				reporter.publishFailure(ctx, errStream)
			} else {
				streamUsage.publish(ctx, reporter)
				reporter.ensurePublished(ctx)
			}
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kimi executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		var param any
		translationFailed := func() bool {
			translationErr := sdktranslator.ResponseTranslationError(&param)
			if translationErr == nil {
				return false
			}
			translationErr = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
			recordAPIResponseError(ctx, e.cfg, translationErr)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, translationErr)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: translationErr})
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			observeResponseTokenEvent(ctx, line)
			reporter.observeResponseModel(line)
			streamUsage.observeOpenAIStream(line)
			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param,
			)
			if translationFailed() {
				return
			}
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
			return
		}
		doneChunks := sdktranslator.TranslateStream(
			ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param,
		)
		if translationFailed() {
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

func (e *KimiExecutor) executeResponses(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.ensurePublished(ctx)
	defer reporter.trackFailure(ctx, &err)

	body := bytes.Clone(req.Payload)
	body, err = sjson.SetBytes(body, "model", normalizeKimiUpstreamModel(baseModel))
	if err != nil {
		return resp, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}
	body = helps.SetBoolIfDifferent(body, "stream", false)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body, err = helps.ApplyThinkingWithSourcePayload(
		body,
		req.Payload,
		originalPayloadSource,
		req.Model,
		opts.SourceFormat.String(),
		sdktranslator.FormatCodex.String(),
		e.Identifier(),
		req,
	)
	if err != nil {
		return resp, err
	}
	body = applyPayloadConfigWithRequest(
		e.cfg,
		baseModel,
		sdktranslator.FormatOpenAIResponse.String(),
		opts.SourceFormat.String(),
		"",
		body,
		req.Payload,
		payloadRequestedModel(opts, req.Model),
		payloadRequestPath(opts),
		opts.Headers,
		e.Identifier(),
	)
	body = normalizeKimiTools(body)
	body = normalizeKimiTemperature(body)
	body, err = helps.NormalizeKimiResponsesInput(body)
	if err != nil {
		return resp, err
	}
	reporter.setTranslatedReasoningEffort(body, e.Identifier())

	url := helps.ResolveKimiResponsesURL(auth)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyKimiHeadersWithAuth(httpReq, kimiCreds(auth), false, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	applyKimiCustomHeaders(httpReq, attrs, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s",
			httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newUpstreamStatusErr(httpResp.StatusCode, data, httpResp.Header)
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
	reporter.observeResponseModel(data)
	detail := parseOpenAIUsage(data)
	if nativeDetail, ok := parseCodexUsage(data); ok {
		detail = nativeDetail
	}

	out := data
	if responseFormat != sdktranslator.FormatOpenAIResponse {
		var param any
		out = sdktranslator.TranslateNonStream(
			ctx,
			sdktranslator.FormatOpenAIResponse,
			responseFormat,
			req.Model,
			opts.OriginalRequest,
			body,
			data,
			&param,
		)
		translationErr := sdktranslator.ResponseTranslationError(&param)
		if translationErr == nil && len(out) == 0 {
			translationErr = fmt.Errorf("kimi executor: response translation returned an empty payload")
		}
		if translationErr != nil {
			translationErr = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
			recordAPIResponseError(ctx, e.cfg, translationErr)
			reporter.publishFailureWithDetail(ctx, detail, translationErr)
			return resp, translationErr
		}
	}
	reporter.publish(ctx, detail)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *KimiExecutor) executeResponsesStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	body := bytes.Clone(req.Payload)
	body, err = sjson.SetBytes(body, "model", normalizeKimiUpstreamModel(baseModel))
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}
	body = helps.SetBoolIfDifferent(body, "stream", true)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body, err = helps.ApplyThinkingWithSourcePayload(
		body,
		req.Payload,
		originalPayloadSource,
		req.Model,
		opts.SourceFormat.String(),
		sdktranslator.FormatCodex.String(),
		e.Identifier(),
		req,
	)
	if err != nil {
		return nil, err
	}
	body = applyPayloadConfigWithRequest(
		e.cfg,
		baseModel,
		sdktranslator.FormatOpenAIResponse.String(),
		opts.SourceFormat.String(),
		"",
		body,
		req.Payload,
		payloadRequestedModel(opts, req.Model),
		payloadRequestPath(opts),
		opts.Headers,
		e.Identifier(),
	)
	body = normalizeKimiTools(body)
	body = normalizeKimiTemperature(body)
	body, err = helps.NormalizeKimiResponsesInput(body)
	if err != nil {
		return nil, err
	}
	reporter.setTranslatedReasoningEffort(body, e.Identifier())

	url := helps.ResolveKimiResponsesURL(auth)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyKimiHeadersWithAuth(httpReq, kimiCreds(auth), true, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	applyKimiCustomHeaders(httpReq, attrs, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		appendAPIResponseChunk(ctx, e.cfg, data)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s",
			httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newUpstreamStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		defer func() {
			if errStream := scannerOrContextError(ctx, nil); errStream != nil {
				recordAPIResponseError(ctx, e.cfg, errStream)
				reporter.publishFailure(ctx, errStream)
			} else {
				streamUsage.publish(ctx, reporter)
				reporter.ensurePublished(ctx)
			}
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kimi executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 50<<20)
		var param any
		emitLine := func(line []byte) bool {
			if responseFormat == sdktranslator.FormatOpenAIResponse {
				payload := append(bytes.Clone(line), '\n')
				return sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: payload})
			}
			chunks := sdktranslator.TranslateStream(
				ctx,
				sdktranslator.FormatOpenAIResponse,
				responseFormat,
				req.Model,
				opts.OriginalRequest,
				body,
				bytes.Clone(line),
				&param,
			)
			if translationErr := sdktranslator.ResponseTranslationError(&param); translationErr != nil {
				translationErr = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
				recordAPIResponseError(ctx, e.cfg, translationErr)
				reporter.publishFailureWithDetail(ctx, streamUsage.detail, translationErr)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: translationErr})
				return false
			}
			for i := range chunks {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					return false
				}
			}
			return true
		}

		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			observeResponseTokenEvent(ctx, line)
			reporter.observeResponseModel(line)
			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[len(dataTag):])
				switch gjson.GetBytes(data, "type").String() {
				case "response.completed", "response.incomplete", "response.done":
					if detail, ok := parseCodexUsage(data); ok {
						hasUsage := openAIStyleUsageTokenFieldsExist(gjson.GetBytes(data, "response.usage"))
						streamUsage.observe(detail, hasUsage, detail.ResponseServiceTier != "")
					} else if detail, ok := parseOpenAIStreamUsage(data); ok {
						hasUsage := openAIStyleUsageTokenFieldsExist(gjson.GetBytes(data, "usage"))
						streamUsage.observe(detail, hasUsage, detail.ResponseServiceTier != "")
					}
				}
			}
			if !emitLine(line) {
				if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
					recordAPIResponseError(ctx, e.cfg, errStream)
					reporter.publishFailure(ctx, errStream)
				}
				return
			}
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			errChunk := cliproxyexecutor.StreamChunk{Err: errScan}
			if !sendStreamChunk(ctx, out, errChunk) && ctx.Err() != nil {
				select {
				case out <- errChunk:
				default:
				}
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Kimi requests.
func (e *KimiExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	if auth == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("kimi executor: auth is nil")
	}
	auth = auth.Clone()
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["base_url"] = helps.ResolveKimiClaudeBaseURL(auth)
	req.Model = normalizeKimiUpstreamModel(req.Model)
	return e.ClaudeExecutor.countTokensUpstream(ctx, auth, req, opts)
}

func normalizeKimiToolMessageLinks(body []byte) ([]byte, error) {
	return normalizeKimiMessages(body)
}

func normalizeKimiMessages(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}

	messages := util.GetGJSONBytesNoCopy(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	type messagePatch struct {
		index        int
		path         string
		value        string
		errorContext string
	}

	msgs := messages.Array()
	droppedMessages := make([]bool, len(msgs))
	patches := make([]messagePatch, 0)
	pending := make([]string, 0)
	dropped := 0
	patched := 0
	patchedReasoning := 0
	ambiguous := 0
	latestReasoning := ""
	hasLatestReasoning := false

	removePending := func(id string) {
		for idx := range pending {
			if pending[idx] != id {
				continue
			}
			pending = append(pending[:idx], pending[idx+1:]...)
			return
		}
	}

	for msgIndex, msg := range msgs {
		if shouldDropKimiAssistantMessage(msg) {
			droppedMessages[msgIndex] = true
			dropped++
			continue
		}

		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			reasoning := msg.Get("reasoning_content")
			if reasoning.Exists() {
				reasoningText := reasoning.String()
				if isUsableKimiReasoning(reasoningText) {
					latestReasoning = reasoningText
					hasLatestReasoning = true
				}
			}

			toolCalls := msg.Get("tool_calls")
			if toolCalls.Exists() && toolCalls.IsArray() {
				toolCallItems := toolCalls.Array()
				if len(toolCallItems) > 0 {
					if !reasoning.Exists() || !isUsableKimiReasoning(reasoning.String()) {
						patches = append(
							patches, messagePatch{
								index:        msgIndex,
								path:         "reasoning_content",
								value:        fallbackAssistantReasoning(msg, hasLatestReasoning, latestReasoning),
								errorContext: "failed to set assistant reasoning_content",
							},
						)
						patchedReasoning++
					}
					for _, toolCall := range toolCallItems {
						id := strings.TrimSpace(toolCall.Get("id").String())
						if id != "" {
							pending = append(pending, id)
						}
					}
				}
			}
		case "tool":
			toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())
			if toolCallID == "" {
				toolCallID = strings.TrimSpace(msg.Get("call_id").String())
				if toolCallID != "" {
					patches = append(
						patches, messagePatch{
							index:        msgIndex,
							path:         "tool_call_id",
							value:        toolCallID,
							errorContext: "failed to set tool_call_id from call_id",
						},
					)
					patched++
				}
			}
			if toolCallID == "" {
				if len(pending) == 1 {
					toolCallID = pending[0]
					patches = append(
						patches, messagePatch{
							index:        msgIndex,
							path:         "tool_call_id",
							value:        toolCallID,
							errorContext: "failed to infer tool_call_id",
						},
					)
					patched++
				} else if len(pending) > 1 {
					ambiguous++
				}
			}
			if toolCallID != "" {
				removePending(toolCallID)
			}
		}
	}

	if dropped > 0 {
		log.WithField("dropped_assistant_messages", dropped).Debug("kimi executor: dropped empty assistant messages")
	}
	if dropped == 0 && len(patches) == 0 {
		if ambiguous > 0 {
			log.WithFields(
				log.Fields{
					"ambiguous_tool_messages": ambiguous,
					"pending_tool_calls":      len(pending),
				},
			).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
		}
		return body, nil
	}

	var out []byte
	if dropped == 0 && len(patches) == 1 {
		patch := patches[0]
		path := fmt.Sprintf("messages.%d.%s", patch.index, patch.path)
		updated, errSet := sjson.SetBytes(body, path, patch.value)
		if errSet != nil {
			return body, fmt.Errorf("kimi executor: %s: %w", patch.errorContext, errSet)
		}
		out = updated
	} else {
		messageItems := make([]string, 0, len(msgs)-dropped)
		patchIndex := 0
		for msgIndex, msg := range msgs {
			if droppedMessages[msgIndex] {
				continue
			}
			messageJSON := msg.Raw
			for patchIndex < len(patches) && patches[patchIndex].index == msgIndex {
				patch := patches[patchIndex]
				next, errSet := sjson.SetBytes([]byte(messageJSON), patch.path, patch.value)
				if errSet != nil {
					return body, fmt.Errorf("kimi executor: %s: %w", patch.errorContext, errSet)
				}
				messageJSON = string(next)
				patchIndex++
			}
			messageItems = append(messageItems, messageJSON)
		}
		updated, errSet := sjson.SetRawBytes(body, "messages", helps.JoinRawJSONStrings(messageItems))
		if errSet != nil {
			if dropped > 0 {
				return body, fmt.Errorf("kimi executor: failed to drop empty assistant messages: %w", errSet)
			}
			return body, fmt.Errorf("kimi executor: %s: %w", patches[0].errorContext, errSet)
		}
		out = updated
	}

	if patched > 0 || patchedReasoning > 0 {
		log.WithFields(
			log.Fields{
				"patched_tool_messages":      patched,
				"patched_reasoning_messages": patchedReasoning,
			},
		).Debug("kimi executor: normalized tool message fields")
	}
	if ambiguous > 0 {
		log.WithFields(
			log.Fields{
				"ambiguous_tool_messages": ambiguous,
				"pending_tool_calls":      len(pending),
			},
		).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
	}
	return out, nil
}

func shouldDropKimiAssistantMessage(msg gjson.Result) bool {
	if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
		return false
	}
	if hasKimiToolCalls(msg) || hasKimiLegacyFunctionCall(msg) || hasKimiAssistantReasoning(msg) {
		return false
	}
	return isKimiAssistantContentEmpty(msg.Get("content"))
}

func hasKimiToolCalls(msg gjson.Result) bool {
	toolCalls := msg.Get("tool_calls")
	return toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0
}

func hasKimiLegacyFunctionCall(msg gjson.Result) bool {
	functionCall := msg.Get("function_call")
	if !functionCall.Exists() || functionCall.Type == gjson.Null {
		return false
	}
	if functionCall.IsObject() && strings.TrimSpace(functionCall.Raw) == "{}" {
		return false
	}
	return strings.TrimSpace(functionCall.Raw) != ""
}

func hasKimiAssistantReasoning(msg gjson.Result) bool {
	reasoning := msg.Get("reasoning_content")
	return reasoning.Exists() && strings.TrimSpace(reasoning.String()) != ""
}

func isKimiAssistantContentEmpty(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return true
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if !isKimiAssistantContentPartEmpty(part) {
			return false
		}
	}
	return true
}

func isKimiAssistantContentPartEmpty(part gjson.Result) bool {
	if !part.Exists() || part.Type == gjson.Null {
		return true
	}
	if part.Type == gjson.String {
		return strings.TrimSpace(part.String()) == ""
	}
	if !part.IsObject() {
		return false
	}
	if text := part.Get("text"); text.Exists() {
		return strings.TrimSpace(text.String()) == ""
	}
	if strings.TrimSpace(part.Get("type").String()) == "text" {
		return true
	}
	return strings.TrimSpace(part.Raw) == "{}"
}

func isUsableKimiReasoning(reasoning string) bool {
	trimmed := strings.TrimSpace(reasoning)
	return trimmed != "" && trimmed != kimiReasoningUnavailable
}

func fallbackAssistantReasoning(msg gjson.Result, hasLatest bool, latest string) string {
	if hasLatest && isUsableKimiReasoning(latest) {
		return latest
	}

	content := msg.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			return text
		}
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return kimiReasoningUnavailable
}

// Refresh refreshes the Kimi token using the refresh token.
func (e *KimiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("kimi executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("kimi executor: auth is nil")
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

	domain := kimiauth.ResolveKimiDomainFromAuth(auth)
	client := kimiauth.NewDeviceFlowClientWithDomainAndDeviceID(
		e.cfg, domain, resolveKimiDeviceID(auth), newProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second),
	)
	td, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		updated.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ExpiresAt > 0 {
		updated.Metadata["expired"] = time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	provider := "kimi"
	if kimiauth.IsKimiAIDomain(domain) {
		provider = "kimi-ai"
	}
	baseURL := kimiauth.ResolveKimiAPIBaseURL(domain)
	updated.Metadata["type"] = provider
	updated.Metadata["domain"] = domain
	updated.Metadata["base_url"] = baseURL
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	updated.Attributes["access_token"] = td.AccessToken
	updated.Attributes["domain"] = domain
	updated.Attributes["base_url"] = baseURL
	now := time.Now()
	updated.Metadata["last_refresh"] = now.UTC().Format(time.RFC3339)
	if storage, ok := auth.Storage.(*kimiauth.TokenStorage); ok && storage != nil {
		copyStorage := *storage
		copyStorage.Type = provider
		copyStorage.Domain = domain
		copyStorage.BaseURL = baseURL
		copyStorage.AccessToken = td.AccessToken
		if td.RefreshToken != "" {
			copyStorage.RefreshToken = td.RefreshToken
		}
		if td.ExpiresAt > 0 {
			copyStorage.Expired = time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		copyStorage.Metadata = updated.Metadata
		updated.Storage = &copyStorage
	}
	updated.LastRefreshedAt = now
	return updated, nil
}

// applyKimiHeaders sets required headers for Kimi API requests.
func applyKimiHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(r, "Authorization", bearer)
	r.Header.Set("User-Agent", "CLIProxyAPI/"+buildinfo.Version)
	r.Header.Set("X-Msh-Platform", "CLIProxyAPI")
	r.Header.Set("X-Msh-Version", buildinfo.Version)
	r.Header.Set("X-Msh-Device-Name", getKimiHostname())
	r.Header.Set("X-Msh-Device-Model", getKimiDeviceModel())
	r.Header.Set("X-Msh-Device-Id", getKimiDeviceID())
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func resolveKimiDeviceIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	deviceIDRaw, ok := auth.Metadata["device_id"]
	if !ok {
		return ""
	}

	deviceID, ok := deviceIDRaw.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(deviceID)
}

func resolveKimiDeviceIDFromStorage(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}

	storage, ok := auth.Storage.(*kimiauth.TokenStorage)
	if !ok || storage == nil {
		return ""
	}

	return strings.TrimSpace(storage.DeviceID)
}

func resolveKimiDeviceID(auth *cliproxyauth.Auth) string {
	deviceID := resolveKimiDeviceIDFromAuth(auth)
	if deviceID != "" {
		return deviceID
	}
	return resolveKimiDeviceIDFromStorage(auth)
}

func applyKimiHeadersWithAuth(r *http.Request, token string, stream bool, auth *cliproxyauth.Auth) {
	applyKimiHeaders(r, token, stream)

	if deviceID := resolveKimiDeviceID(auth); deviceID != "" {
		r.Header.Set("X-Msh-Device-Id", deviceID)
	}
}

// getKimiHostname returns the machine hostname.
func getKimiHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// getKimiDeviceModel returns a device model string matching kimi-cli format.
func getKimiDeviceModel() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

// getKimiDeviceID returns a stable device ID, matching kimi-cli storage location.
func getKimiDeviceID() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "cli-proxy-api-device"
	}
	// Check kimi-cli's device_id location first (platform-specific)
	var kimiShareDir string
	switch runtime.GOOS {
	case "darwin":
		kimiShareDir = filepath.Join(homeDir, "Library", "Application Support", "kimi")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(homeDir, "AppData", "Roaming")
		}
		kimiShareDir = filepath.Join(appData, "kimi")
	default: // linux and other unix-like
		kimiShareDir = filepath.Join(homeDir, ".local", "share", "kimi")
	}
	deviceIDPath := filepath.Join(kimiShareDir, "device_id")
	if data, err := os.ReadFile(deviceIDPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "cli-proxy-api-device"
}

// kimiCreds extracts the access token from auth.
func kimiCreds(a *cliproxyauth.Auth) (token string) {
	if a == nil {
		return ""
	}
	// Check metadata first (OAuth flow stores tokens here)
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	// Fallback to attributes (API key style)
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			return v
		}
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	return ""
}

// stripKimiPrefix removes the "kimi-" prefix from model names for the upstream API.
func stripKimiPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "kimi-") {
		return model[5:]
	}
	return model
}

func normalizeKimiUpstreamModel(model string) string {
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	base := strings.ToLower(strings.TrimSpace(parsed.ModelName))
	base = strings.TrimSuffix(base, "[1m]")

	var normalized string
	switch base {
	case "kimi-k2.8", "k2.8", "kimi-k2.8-code", "k2.8-code", "kimi-k2.8-preview", "k2.8-preview",
		"kimi-k2.7-code", "k2.7-code", "kimi-for-coding", "for-coding":
		normalized = "kimi-for-coding"
	case "kimi-k2.7-code-highspeed", "k2.7-code-highspeed", "kimi-for-coding-highspeed", "for-coding-highspeed":
		normalized = "kimi-for-coding-highspeed"
	default:
		normalized = stripKimiPrefix(base)
	}
	if parsed.HasSuffix {
		return normalized + "(" + parsed.RawSuffix + ")"
	}
	return normalized
}

func normalizeKimiTools(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	body = normalizeKimiToolList(body, "tools", true)
	body = normalizeKimiToolList(body, "functions", false)
	return body
}

func normalizeKimiToolList(body []byte, arrayKey string, isTools bool) []byte {
	items := gjson.GetBytes(body, arrayKey)
	if !items.Exists() || !items.IsArray() {
		return body
	}
	arr := items.Array()
	if len(arr) == 0 {
		return body
	}

	changed := false
	updatedItems := make([]string, 0, len(arr))
	for _, item := range arr {
		itemRaw := item.Raw
		var paramPath string
		if isTools && item.Get("function.parameters").Exists() {
			paramPath = "function.parameters"
		} else if item.Get("parameters").Exists() {
			paramPath = "parameters"
		}

		if paramPath != "" {
			rawParams := item.Get(paramPath)
			if rawParams.IsObject() {
				normalizedParams := normalizeKimiParametersSchema(rawParams.Raw)
				if normalizedParams != rawParams.Raw {
					if updated, errSet := sjson.SetRawBytes([]byte(itemRaw), paramPath, []byte(normalizedParams)); errSet == nil {
						itemRaw = string(updated)
						changed = true
					}
				}
			}
		}
		updatedItems = append(updatedItems, itemRaw)
	}

	if !changed {
		return body
	}
	out, errSet := sjson.SetRawBytes(body, arrayKey, helps.JoinRawJSONStrings(updatedItems))
	if errSet != nil {
		return body
	}
	return out
}

func normalizeKimiParametersSchema(paramsRaw string) string {
	if strings.TrimSpace(paramsRaw) == "" {
		return paramsRaw
	}
	paramBytes := []byte(inlineKimiLocalRefs(paramsRaw))
	if gjson.GetBytes(paramBytes, "$defs").Exists() {
		paramBytes, _ = sjson.DeleteBytes(paramBytes, "$defs")
	}
	if gjson.GetBytes(paramBytes, "definitions").Exists() {
		paramBytes, _ = sjson.DeleteBytes(paramBytes, "definitions")
	}
	if !gjson.GetBytes(paramBytes, "type").Exists() {
		paramBytes, _ = sjson.SetBytes(paramBytes, "type", "object")
	}
	return string(paramBytes)
}

func inlineKimiLocalRefs(schema string) string {
	if !strings.Contains(schema, `"$ref"`) {
		return schema
	}
	decoder := json.NewDecoder(strings.NewReader(schema))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return schema
	}
	resolved := resolveKimiLocalRefs(root, root, make(map[string]bool))
	out, err := json.Marshal(resolved)
	if err != nil {
		return schema
	}
	return string(out)
}

func resolveKimiLocalRefs(root, value any, active map[string]bool) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = resolveKimiLocalRefs(root, item, active)
		}
		return out
	case map[string]any:
		ref, hasRef := node["$ref"].(string)
		if hasRef && strings.HasPrefix(ref, "#/") {
			if target, ok := resolveKimiJSONPointer(root, ref); ok {
				if active[ref] {
					return cyclicKimiRefFallback(node, target, ref)
				}
				active[ref] = true
				resolvedTarget := resolveKimiLocalRefs(root, target, active)
				delete(active, ref)
				if targetMap, okTarget := resolvedTarget.(map[string]any); okTarget {
					out := make(map[string]any, len(targetMap)+len(node))
					for key, item := range targetMap {
						out[key] = item
					}
					for key, item := range node {
						if key == "$ref" {
							continue
						}
						out[key] = resolveKimiLocalRefs(root, item, active)
					}
					return out
				}
			}
		}
		out := make(map[string]any, len(node))
		for key, item := range node {
			out[key] = resolveKimiLocalRefs(root, item, active)
		}
		return out
	default:
		return value
	}
}

func resolveKimiJSONPointer(root any, ref string) (any, bool) {
	current := root
	for _, rawPart := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(rawPart, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func cyclicKimiRefFallback(node map[string]any, target any, ref string) map[string]any {
	out := make(map[string]any, len(node)+2)
	if targetMap, ok := target.(map[string]any); ok {
		for _, key := range []string{"type", "nullable", "description"} {
			if value, exists := targetMap[key]; exists {
				out[key] = value
			}
		}
	}
	for key, value := range node {
		if key != "$ref" {
			out[key] = value
		}
	}
	name := ref
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		name = strings.ReplaceAll(strings.ReplaceAll(ref[index+1:], "~1", "/"), "~0", "~")
	}
	hint := "See: " + name
	if description, _ := out["description"].(string); description != "" {
		if !strings.Contains(description, hint) {
			out["description"] = description + " (" + hint + ")"
		}
	} else {
		out["description"] = hint
	}
	return out
}

func normalizeKimiTemperature(body []byte) []byte {
	temperature := gjson.GetBytes(body, "temperature")
	if !temperature.Exists() {
		return body
	}
	if strings.EqualFold(gjson.GetBytes(body, "thinking.type").String(), "disabled") {
		if temperature.Float() != 0.6 {
			body, _ = sjson.DeleteBytes(body, "temperature")
		}
		return body
	}
	if temperature.Float() != 1.0 {
		body, _ = sjson.DeleteBytes(body, "temperature")
	}
	return body
}

// RequestToFormat reports the wire selected for the after-auth interceptor.
func (e *KimiExecutor) RequestToFormat(_ cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	if opts.CountTokens || opts.SourceFormat == sdktranslator.FormatClaude {
		return sdktranslator.FormatClaude
	}
	if opts.SourceFormat == sdktranslator.FormatOpenAIResponse {
		return sdktranslator.FormatOpenAIResponse
	}
	return sdktranslator.FormatOpenAI
}
