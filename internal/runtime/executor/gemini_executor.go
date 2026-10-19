// Package executor provides runtime execution capabilities for various AI service providers.
// It includes stateless executors that handle API requests, streaming responses,
// token counting, and authentication refresh for different AI service providers.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
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
	// glEndpoint is the base URL for the Google Generative Language API.
	glEndpoint = "https://generativelanguage.googleapis.com"

	// glAPIVersion is the API version used for Gemini requests.
	glAPIVersion = "v1beta"

	// streamScannerBuffer is the buffer size for SSE stream scanning.
	streamScannerBuffer = 52_428_800

	// geminiInteractionsAPIRevision is the default API revision for native Interactions requests.
	geminiInteractionsAPIRevision = "2026-05-20"
)

// GeminiExecutor is a stateless executor for the official Gemini API using API keys.
// It handles both API key and OAuth bearer token authentication, supporting both
// regular and streaming requests to the Google Generative Language API.
type GeminiExecutor struct {
	// cfg holds the application configuration.
	cfg        *config.Config
	identifier string
}

// geminiRequest holds the prepared artifacts shared by Execute and ExecuteStream.
type geminiRequest struct {
	responseFormat, to sdktranslator.Format
	body               []byte
	httpReq            *http.Request
	reporter           *usageReporter
}

// NewGeminiExecutor creates a new Gemini executor instance.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *GeminiExecutor: A new Gemini executor instance
func NewGeminiExecutor(cfg *config.Config) *GeminiExecutor {
	return &GeminiExecutor{cfg: cfg, identifier: "gemini"}
}

// NewGeminiInteractionsExecutor creates a Gemini executor bound to the native Interactions provider.
func NewGeminiInteractionsExecutor(cfg *config.Config) *GeminiExecutor {
	return &GeminiExecutor{cfg: cfg, identifier: "gemini-interactions"}
}

// Identifier returns the executor identifier.
func (e *GeminiExecutor) Identifier() string {
	if e == nil || strings.TrimSpace(e.identifier) == "" {
		return "gemini"
	}
	return e.identifier
}

// RequestToFormat reports the upstream request format used after auth selection.
func (e *GeminiExecutor) RequestToFormat(
	_ cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) sdktranslator.Format {
	if strings.EqualFold(
		strings.TrimSpace(e.Identifier()), "gemini-interactions",
	) && nativeInteractionsSourceFormat(opts.SourceFormat) {
		return sdktranslator.FormatInteractions
	}
	return sdktranslator.FormatGemini
}

func capGeminiMaxOutputTokens(body []byte, modelName string) []byte {
	maxOut := gjson.GetBytes(body, "generationConfig.maxOutputTokens")
	if !maxOut.Exists() || maxOut.Type != gjson.Number {
		return body
	}
	modelInfo := registry.LookupModelInfo(modelName, "gemini")
	if modelInfo == nil {
		return body
	}
	limit := modelInfo.OutputTokenLimit
	if limit <= 0 {
		limit = modelInfo.MaxCompletionTokens
	}
	if limit <= 0 || maxOut.Int() <= int64(limit) {
		return body
	}
	body, _ = sjson.SetBytes(body, "generationConfig.maxOutputTokens", limit)
	return body
}

// PrepareRequest injects Gemini credentials into the outgoing HTTP request.
func (e *GeminiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, bearer := geminiCreds(auth)
	credentialName, credentialValue := "Authorization", ""
	if apiKey != "" {
		credentialName, credentialValue = "X-Goog-Api-Key", apiKey
	} else if bearer != "" {
		credentialValue = "Bearer " + bearer
	}
	util.ApplyCredentialHeader(req, credentialName, credentialValue)
	applyGeminiHeaders(req, auth)
	return nil
}

// HttpRequest injects Gemini credentials into the request and executes it.
func (e *GeminiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("gemini executor: request is nil")
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

// prepareGeminiRequest builds the translated, config-applied HTTP request shared by Execute and ExecuteStream.
func (e *GeminiExecutor) prepareGeminiRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	isStream bool,
) (*geminiRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, bearer := geminiCreds(auth)

	reporter := newUsageReporter(ctx, e, baseModel, auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("gemini")
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, isStream, isCompat, &req,
	)
	if err != nil {
		return nil, err
	}

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = signature.SanitizeGeminiRequestThoughtSignatures(
		body,
		"contents",
		isCompat,
	)

	action := "generateContent"
	if isStream {
		action = "streamGenerateContent"
	}
	if req.Metadata != nil {
		if a, _ := req.Metadata["action"].(string); a == "countTokens" {
			action = "countTokens"
		}
	}
	body = helps.EnsureGeminiLeadingUserContent(body, "contents")
	if action != "countTokens" {
		body = helps.EnsureGeminiTrailingUserContent(body, "contents")
	}
	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, action)
	if isStream {
		if opts.Alt == "" {
			url = url + "?alt=sse"
		} else {
			url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
		}
	} else if opts.Alt != "" && action != "countTokens" {
		url = url + fmt.Sprintf("?$alt=%s", opts.Alt)
	}

	body, _ = sjson.DeleteBytes(body, "session_id")
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body,
		originalTranslated, requestedModel, payloadRequestPath(opts), opts.Headers, e.Identifier())
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = signature.SanitizeGeminiRequestThoughtSignatures(body, "contents", isCompat)
	reporter.setTranslatedReasoningEffort(body, to.String())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	credentialName, credentialValue := "Authorization", ""
	if apiKey != "" {
		credentialName, credentialValue = "X-Goog-Api-Key", apiKey
	} else if bearer != "" {
		credentialValue = "Bearer " + bearer
	}
	util.ApplyCredentialHeader(httpReq, credentialName, credentialValue)
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	return &geminiRequest{
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
		to:             to,
		body:           body,
		httpReq:        httpReq,
		reporter:       reporter,
	}, nil
}

// Execute performs a non-streaming request to the Gemini API.
func (e *GeminiExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if shouldExecuteNativeInteractions(auth, opts) {
		return e.executeInteractions(ctx, auth, req, opts)
	}

	prepared, err := e.prepareGeminiRequest(ctx, auth, req, opts, false)
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
			log.Errorf("gemini executor: close response body error: %v", errClose)
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
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx, prepared.to, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, data, &param,
	)
	if errTranslate := geminiResponseError(&param, out); errTranslate != nil {
		recordAPIResponseError(ctx, e.cfg, errTranslate)
		prepared.reporter.publishFailureWithDetail(ctx, parseGeminiUsage(data), errTranslate)
		return resp, errTranslate
	}
	prepared.reporter.publish(ctx, parseGeminiUsage(data))
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming request to the Gemini API.
func (e *GeminiExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	opts.OriginalRequest = originalRequestPayload(req, opts)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	if shouldExecuteNativeInteractions(auth, opts) {
		return e.executeInteractionsStream(ctx, auth, req, opts)
	}

	prepared, err := e.prepareGeminiRequest(ctx, auth, req, opts, true)
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
			log.Errorf("gemini executor: close response body error: %v", errClose)
		}
		err = newUpstreamStatusErr(httpResp.StatusCode, b, httpResp.Header)
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
			if errStream := scannerOrContextError(ctx, nil); errStream != nil {
				recordAPIResponseError(ctx, e.cfg, errStream)
				reporter.publishFailure(ctx, errStream)
			} else {
				streamUsage.publish(ctx, reporter)
				reporter.ensurePublished(ctx)
			}
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		receivedResponse := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			observeResponseTokenEvent(ctx, line)
			reporter.observeResponseModel(line)
			streamUsage.observeGeminiStream(line)
			filtered := filterSSEUsageMetadata(line)
			payload := jsonPayload(filtered)
			if len(payload) == 0 {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				break
			}
			receivedResponse = true
			lines := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(payload), &param,
			)
			for i := range lines {
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: lines[i]}) {
					if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
						recordAPIResponseError(ctx, e.cfg, errStream)
						reporter.publishFailure(ctx, errStream)
					}
					return
				}
			}
			if errTranslate := geminiResponseError(&param, lines...); errTranslate != nil {
				recordAPIResponseError(ctx, e.cfg, errTranslate)
				reporter.publishFailureWithDetail(ctx, streamUsage.detail, errTranslate)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTranslate})
				return
			}
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		if !receivedResponse {
			errEmpty := markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "gemini executor: empty upstream stream"})
			recordAPIResponseError(ctx, e.cfg, errEmpty)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, errEmpty)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errEmpty})
			return
		}
		lines := sdktranslator.TranslateStream(
			ctx, to, from, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param,
		)
		for i := range lines {
			if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: lines[i]}) {
				if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
					recordAPIResponseError(ctx, e.cfg, errStream)
					reporter.publishFailure(ctx, errStream)
				}
				return
			}
		}
		if errTranslate := geminiResponseError(&param, lines...); errTranslate != nil {
			recordAPIResponseError(ctx, e.cfg, errTranslate)
			reporter.publishFailureWithDetail(ctx, streamUsage.detail, errTranslate)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTranslate})
			return
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *GeminiExecutor) prepareInteractionsRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	body []byte,
	headers http.Header,
	stream bool,
) (*http.Request, error) {
	apiKey, bearer := geminiCreds(auth)
	baseURL := resolveGeminiBaseURL(auth)
	requestURL := fmt.Sprintf("%s/%s/interactions", baseURL, glAPIVersion)
	if e.Identifier() == "vertex" {
		apiKey, baseURL = vertexAPICreds(auth)
		var projectID string
		if apiKey == "" {
			var saJSON []byte
			var err error
			projectID, _, saJSON, err = vertexCreds(auth)
			if err != nil {
				return nil, err
			}
			bearer, err = vertexAccessToken(ctx, e.cfg, auth, saJSON)
			if err != nil {
				log.Errorf("vertex executor: interactions access token error: %v", err)
				return nil, statusErr{code: http.StatusInternalServerError, msg: "internal server error"}
			}
			if strings.TrimSpace(bearer) == "" {
				return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
			}
		} else if auth != nil {
			projectID, _ = auth.Metadata["project_id"].(string)
		}
		baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		if baseURL == "" {
			baseURL = "https://aiplatform.googleapis.com"
		}
		requestURL = baseURL + "/v1beta1/interactions"
		if projectID = strings.TrimSpace(projectID); projectID != "" {
			requestURL = fmt.Sprintf("%s/v1beta1/projects/%s/locations/global/interactions", baseURL, url.PathEscape(projectID))
		}
		if stream {
			requestURL += "?alt=sse"
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	credentialName, credentialValue := "Authorization", ""
	if apiKey != "" {
		credentialName, credentialValue = "X-Goog-Api-Key", apiKey
	} else if bearer != "" {
		credentialValue = "Bearer " + bearer
	}
	util.ApplyCredentialHeader(httpReq, credentialName, credentialValue)
	applyGeminiHeaders(httpReq, auth, headers)
	applyGeminiInteractionsRequestHeaders(httpReq, headers)
	applyGeminiInteractionsRevisionHeader(httpReq)
	return httpReq, nil
}

func (e *GeminiExecutor) executeInteractions(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, targetName, auth)
	defer reporter.trackFailure(ctx, &err)
	isCompat := helps.APIKeyModelIsCompat(req)

	originalTranslated, body, err := translateGeminiInteractionsRequestPair(
		ctx, e.cfg, targetName, req.Payload, opts, false, isCompat,
	)
	if err != nil {
		return resp, err
	}
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body = helps.SetStringIfDifferent(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req)
	if err != nil {
		return resp, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	body = applyPayloadConfigWithRequest(
		e.cfg,
		targetName,
		sdktranslator.FormatInteractions.String(),
		fromProtocol,
		"",
		body,
		originalTranslated,
		requestedModel,
		requestPath,
		opts.Headers,
		e.Identifier(),
	)
	body = sanitizeGeminiInteractionsUnsupportedInputIDs(body)
	reporter.setTranslatedReasoningEffort(body, sdktranslator.FormatInteractions.String())

	httpReq, errRequest := e.prepareInteractionsRequest(ctx, auth, body, opts.Headers, false)
	if errRequest != nil {
		return resp, errRequest
	}
	recordUpstreamRequest(ctx, e.cfg, httpReq.URL.String(), httpReq.Header.Clone(), body, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close interactions response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if errRead != nil {
		errRead = markResponseStarted(errRead)
		recordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newUpstreamStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return resp, err
	}
	observeResponseTokenEvent(ctx, data)
	reporter.observeResponseModel(data)
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FormatInteractions,
		cliproxyexecutor.ResponseFormatOrSource(opts),
		req.Model,
		opts.OriginalRequest,
		body,
		data,
		&param,
	)
	if errTranslate := geminiResponseError(&param, data, out); errTranslate != nil {
		recordAPIResponseError(ctx, e.cfg, errTranslate)
		reporter.publishFailureWithDetail(ctx, parseInteractionsUsage(data), errTranslate)
		return resp, errTranslate
	}
	reporter.publish(ctx, parseInteractionsUsage(data))
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *GeminiExecutor) executeInteractionsStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e, targetName, auth)
	defer reporter.trackFailure(ctx, &err)
	isCompat := helps.APIKeyModelIsCompat(req)

	originalTranslated, body, err := translateGeminiInteractionsRequestPair(
		ctx, e.cfg, targetName, req.Payload, opts, true, isCompat,
	)
	if err != nil {
		return nil, err
	}
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body = helps.SetStringIfDifferent(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req)
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	body = applyPayloadConfigWithRequest(
		e.cfg,
		targetName,
		sdktranslator.FormatInteractions.String(),
		fromProtocol,
		"",
		body,
		originalTranslated,
		requestedModel,
		requestPath,
		opts.Headers,
		e.Identifier(),
	)
	body = sanitizeGeminiInteractionsUnsupportedInputIDs(body)
	body = helps.SetBoolIfDifferent(body, "stream", true)
	reporter.setTranslatedReasoningEffort(body, sdktranslator.FormatInteractions.String())

	httpReq, errRequest := e.prepareInteractionsRequest(ctx, auth, body, opts.Headers, true)
	if errRequest != nil {
		return nil, errRequest
	}
	recordUpstreamRequest(ctx, e.cfg, httpReq.URL.String(), httpReq.Header.Clone(), body, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("gemini executor: close interactions error response body error: %v", errClose)
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		return nil, newUpstreamStatusErr(httpResp.StatusCode, data, httpResp.Header)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
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
				log.Errorf("gemini executor: close interactions stream body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		var frame []byte
		sawTerminalEvent := false
		sendChunk := func(chunk cliproxyexecutor.StreamChunk) bool {
			if sendStreamChunk(ctx, out, chunk) {
				return true
			}
			if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
				recordAPIResponseError(ctx, e.cfg, errStream)
				reporter.publishFailure(ctx, errStream)
			}
			return false
		}
		emitFrame := func() bool {
			rawFrame := bytes.Clone(frame)
			trimmed := bytes.TrimSpace(rawFrame)
			frame = frame[:0]
			if len(trimmed) == 0 {
				return true
			}
			payload := geminiInteractionsSSEPayload(rawFrame)
			terminal, done := geminiInteractionsSSEDone(rawFrame, responseFormat == sdktranslator.FormatInteractions)
			if terminal {
				sawTerminalEvent = true
				if len(payload) == 0 {
					payload = []byte("[DONE]")
				}
			}
			if len(payload) == 0 && len(trimmed) > 0 && trimmed[0] == '{' {
				payload = trimmed
			}
			if len(payload) > 0 {
				observeResponseTokenEvent(ctx, payload)
				reporter.observeResponseModel(payload)
				detail, hasUsage, hasTier := parseInteractionsStreamUsageState(payload)
				streamUsage.observe(detail, hasUsage, hasTier)
			}
			if responseFormat == sdktranslator.FormatInteractions {
				visibleFrame := append(bytes.TrimRight(rawFrame, "\r\n"), '\n', '\n')
				return sendChunk(cliproxyexecutor.StreamChunk{Payload: visibleFrame}) && !done
			}
			if len(payload) == 0 {
				return true
			}
			lines := sdktranslator.TranslateStream(
				ctx,
				sdktranslator.FormatInteractions,
				responseFormat,
				req.Model,
				opts.OriginalRequest,
				body,
				payload,
				&param,
			)
			for i := range lines {
				if !sendChunk(cliproxyexecutor.StreamChunk{Payload: lines[i]}) {
					return false
				}
			}
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
				recordAPIResponseError(ctx, e.cfg, errTranslate)
				reporter.publishFailureWithDetail(ctx, streamUsage.detail, errTranslate)
				_ = sendChunk(cliproxyexecutor.StreamChunk{Err: errTranslate})
				return false
			}
			return !done
		}
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				if !emitFrame() {
					return
				}
				continue
			}
			separatorBytes := 0
			if len(frame) > 0 {
				separatorBytes = 1
			}
			if len(frame)+separatorBytes+len(line) > streamScannerBuffer {
				errFrame := statusErr{
					code: http.StatusBadGateway,
					msg: fmt.Sprintf(
						"gemini interactions SSE frame exceeds maximum allowed size of %d bytes", streamScannerBuffer,
					),
				}
				recordAPIResponseError(ctx, e.cfg, errFrame)
				reporter.publishFailure(ctx, errFrame)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errFrame})
				return
			}
			if separatorBytes != 0 {
				frame = append(frame, '\n')
			}
			frame = append(frame, line...)
		}
		if errScan := markResponseStarted(scanner.Err()); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		if !emitFrame() {
			return
		}
		if !sawTerminalEvent {
			errTerminal := statusErr{
				code: http.StatusBadGateway,
				msg:  "gemini interactions stream ended before terminal event",
			}
			recordAPIResponseError(ctx, e.cfg, errTerminal)
			reporter.publishFailure(ctx, errTerminal)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errTerminal})
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens counts tokens for the given request using the Gemini API.
func (e *GeminiExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, bearer := geminiCreds(auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("gemini")
	isCompat := helps.APIKeyModelIsCompat(req)
	if shouldExecuteNativeInteractions(auth, opts) && from == sdktranslator.FormatGemini {
		if _, err := translateGeminiInteractionsRequestBody(ctx, e.cfg, baseModel, req.Payload, opts, false, isCompat); err != nil {
			return cliproxyexecutor.Response{}, err
		}
	}
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated, translatedReq, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayloadSource, req.Payload, false, isCompat, &req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translatedReq, err = helps.ApplyThinkingWithSourcePayload(
		translatedReq, req.Payload, originalPayloadSource, req.Model, from.String(), to.String(), e.Identifier(), req,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translatedReq = fixGeminiImageAspectRatio(baseModel, translatedReq)
	translatedReq = signature.SanitizeGeminiRequestThoughtSignatures(
		translatedReq,
		"contents",
		isCompat,
	)
	respCtx := util.WithAlt(ctx, opts.Alt)
	translatedReq = helps.EnsureGeminiLeadingUserContent(translatedReq, "contents")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "tools")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "generationConfig")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "safetySettings")
	translatedReq = applyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translatedReq,
		originalTranslated, payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier())
	translatedReq = signature.SanitizeGeminiRequestThoughtSignatures(translatedReq, "contents", isCompat)
	translatedReq = helps.SetStringIfDifferent(translatedReq, "model", baseModel)

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "countTokens")

	requestBody := bytes.NewReader(translatedReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	credentialName, credentialValue := "Authorization", ""
	if apiKey != "" {
		credentialName, credentialValue = "X-Goog-Api-Key", apiKey
	} else if bearer != "" {
		credentialValue = "Bearer " + bearer
	}
	util.ApplyCredentialHeader(httpReq, credentialName, credentialValue)
	applyGeminiHeaders(httpReq, auth, opts.Headers)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), translatedReq, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	recordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())

	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		err = markResponseStarted(err)
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", resp.StatusCode,
			summarizeErrorBody(resp.Header.Get("Content-Type"), data),
		)
		return cliproxyexecutor.Response{}, newUpstreamStatusErr(resp.StatusCode, data, resp.Header)
	}

	count := gjson.GetBytes(data, "totalTokens").Int()
	translated := sdktranslator.TranslateTokenCount(respCtx, to, cliproxyexecutor.ResponseFormatOrSource(opts), count, data)
	return cliproxyexecutor.Response{Payload: translated, Headers: resp.Header.Clone()}, nil
}

// Refresh refreshes the authentication credentials (no-op for Gemini API key).
func (e *GeminiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func geminiCreds(a *cliproxyauth.Auth) (apiKey, bearer string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		if v := a.Attributes["api_key"]; v != "" {
			apiKey = v
		}
	}
	if a.Metadata != nil {
		// TokenStorage.Token is a map that may contain access_token
		if v, ok := a.Metadata["access_token"].(string); ok && v != "" {
			bearer = v
		}
		if token, ok := a.Metadata["token"].(map[string]any); ok && token != nil {
			if v, ok2 := token["access_token"].(string); ok2 && v != "" {
				bearer = v
			}
		}
	}
	return
}

func resolveGeminiBaseURL(auth *cliproxyauth.Auth) string {
	base := glEndpoint
	if auth != nil && auth.Attributes != nil {
		if custom := strings.TrimSpace(auth.Attributes["base_url"]); custom != "" {
			base = strings.TrimRight(custom, "/")
		}
	}
	if base == "" {
		return glEndpoint
	}
	return base
}

func shouldExecuteNativeInteractions(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) bool {
	return nativeInteractionsSourceFormat(opts.SourceFormat) && isNativeInteractionsAuth(auth)
}

func nativeInteractionsSourceFormat(format sdktranslator.Format) bool {
	switch format {
	case sdktranslator.FormatInteractions, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, sdktranslator.FormatGemini:
		return true
	default:
		return false
	}
}

// Gemini Interactions requires id on function_call, call_id on function_result,
// and rejects id on other steps and content parts.
func sanitizeGeminiInteractionsUnsupportedInputIDs(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for i, item := range input.Array() {
		if item.Get("type").String() == "function_call" {
			if callID := item.Get("call_id"); callID.Type == gjson.String && callID.String() != "" && item.Get("id").String() != callID.String() {
				body, _ = sjson.SetBytes(body, fmt.Sprintf("input.%d.id", i), callID.String())
			}
			if item.Get("call_id").Exists() {
				body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.call_id", i))
			}
		} else if item.Get("id").Exists() {
			body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.id", i))
		}
		if content := item.Get("content"); content.IsArray() {
			for j, part := range content.Array() {
				if part.Get("id").Exists() {
					body, _ = sjson.DeleteBytes(body, fmt.Sprintf("input.%d.content.%d.id", i, j))
				}
			}
		}
	}
	return body
}

func translateGeminiInteractionsRequestBody(
	ctx context.Context,
	cfg *config.Config,
	model string,
	payload []byte,
	opts cliproxyexecutor.Options,
	stream,
	isCompat bool,
) ([]byte, error) {
	if opts.SourceFormat == "" || opts.SourceFormat == sdktranslator.FormatInteractions {
		return bytes.Clone(payload), nil
	}
	return helps.TranslateRequestWithAPIKeyModelCompatibilityChecked(
		ctx,
		opts.Headers,
		cfg,
		opts.SourceFormat,
		sdktranslator.FormatInteractions,
		model,
		payload,
		stream,
		isCompat,
	)
}

func translateGeminiInteractionsRequestPair(
	ctx context.Context,
	cfg *config.Config,
	model string,
	payload []byte,
	opts cliproxyexecutor.Options,
	stream,
	isCompat bool,
) (original, working []byte, err error) {
	source := opts.OriginalRequest
	if len(source) == 0 {
		source = payload
	}
	if opts.SourceFormat == "" || opts.SourceFormat == sdktranslator.FormatInteractions {
		return bytes.Clone(source), bytes.Clone(payload), nil
	}
	return helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, cfg, opts.SourceFormat, sdktranslator.FormatInteractions,
		model, source, payload, stream, isCompat,
	)
}

func isNativeInteractionsAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "gemini-interactions")
}

func applyGeminiInteractionsThinking(body []byte, req cliproxyexecutor.Request) ([]byte, error) {
	return helps.ApplyThinkingWithSourcePayload(
		body,
		body,
		body,
		req.Model,
		sdktranslator.FormatInteractions.String(),
		sdktranslator.FormatInteractions.String(),
		"gemini",
		req,
	)
}

func applyGeminiInteractionsRevisionHeader(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("Api-Revision") == "" {
		req.Header.Set("Api-Revision", geminiInteractionsAPIRevision)
	}
}

func applyGeminiInteractionsRequestHeaders(req *http.Request, headers http.Header) {
	if req == nil || headers == nil || req.Header.Get("Api-Revision") != "" {
		return
	}
	if revision := headers.Get("Api-Revision"); revision != "" {
		req.Header.Set("Api-Revision", revision)
	}
}

func geminiInteractionsSSEPayload(frame []byte) []byte {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return trimmed
	}
	lines := bytes.Split(frame, []byte{'\n'})
	var payload []byte
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[bytes.Index(line, []byte("data:"))+len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if len(payload) > 0 {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
	}
	if len(payload) == 0 {
		return nil
	}
	return payload
}

func geminiInteractionsSSEDone(frame []byte, nativeResponse bool) (bool, bool) {
	trimmed := bytes.TrimSpace(frame)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return true, true
	}
	lines := bytes.Split(frame, []byte{'\n'})
	sawDoneEvent := false
	eventName := ""
	for _, line := range lines {
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		}
		if bytes.EqualFold(line, []byte("event: done")) {
			sawDoneEvent = true
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(data, []byte("[DONE]")) {
				return true, true
			}
		}
	}
	if sawDoneEvent {
		return true, true
	}
	payload := geminiInteractionsSSEPayload(frame)
	if !gjson.ValidBytes(payload) {
		return false, false
	}
	root := gjson.ParseBytes(payload)
	interaction := root.Get("interaction")
	if nativeResponse && eventName == "interaction.completed" && !interaction.Exists() {
		status := root.Get("status").String()
		return root.IsObject() && (status == "completed" || status == "requires_action") &&
			strings.TrimSpace(root.Get("id").String()) != "" && root.Get("steps").IsArray(), false
	}
	status := interaction.Get("status").String()
	return root.Get("event_type").String() == "interaction.completed" &&
		interaction.IsObject() && (status == "completed" || status == "requires_action") &&
		strings.TrimSpace(interaction.Get("id").String()) != "" && interaction.Get("steps").IsArray(), false
}

func applyGeminiHeaders(req *http.Request, auth *cliproxyauth.Auth, clientHeaders ...http.Header) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders[0])
	} else {
		util.ApplyCustomHeadersFromAttrs(req, attrs)
	}
	if req != nil {
		req.Header.Del("Content-Length")
	}
}

func fixGeminiImageAspectRatio(modelName string, rawJSON []byte) []byte {
	if modelName == "gemini-2.5-flash-image-preview" {
		aspectRatioResult := gjson.GetBytes(rawJSON, "generationConfig.imageConfig.aspectRatio")
		if aspectRatioResult.Exists() {
			contents := gjson.GetBytes(rawJSON, "contents")
			contentArray := contents.Array()
			if len(contentArray) > 0 {
				hasInlineData := false
			loopContent:
				for i := range contentArray {
					parts := contentArray[i].Get("parts").Array()
					for j := range parts {
						if parts[j].Get("inlineData").Exists() {
							hasInlineData = true
							break loopContent
						}
					}
				}

				if !hasInlineData {
					emptyImageBase64ed, _ := util.CreateWhiteImageBase64(aspectRatioResult.String())
					emptyImagePart := `{"inlineData":{"mime_type":"image/png","data":""}}`
					emptyImagePart, _ = sjson.Set(emptyImagePart, "inlineData.data", emptyImageBase64ed)
					newPartsJson := `[]`
					newPartsJson, _ = sjson.SetRaw(
						newPartsJson,
						"-1",
						`{"text": "Based on the following requirements, create an image within the uploaded picture. The new content *MUST* completely cover the entire area of the original picture, maintaining its exact proportions, and *NO* blank areas should appear."}`,
					)
					newPartsJson, _ = sjson.SetRaw(newPartsJson, "-1", emptyImagePart)

					parts := contentArray[0].Get("parts").Array()
					for j := range parts {
						newPartsJson, _ = sjson.SetRaw(newPartsJson, "-1", parts[j].Raw)
					}

					rawJSON, _ = sjson.SetRawBytes(rawJSON, "contents.0.parts", []byte(newPartsJson))
					rawJSON, _ = sjson.SetRawBytes(
						rawJSON, "generationConfig.responseModalities", []byte(`["IMAGE", "TEXT"]`),
					)
				}
			}
			rawJSON, _ = sjson.DeleteBytes(rawJSON, "generationConfig.imageConfig")
		}
	}
	return rawJSON
}

func geminiResponseError(param *any, payloads ...[]byte) error {
	if err := sdktranslator.ResponseTranslationError(param); err != nil {
		return err
	}
	invalid := func() error {
		return markResponseStarted(statusErr{code: http.StatusBadGateway, msg: "gemini executor: invalid or failed upstream response"})
	}
	for _, payload := range payloads {
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			return invalid()
		}
		events := [][]byte{payload}
		if bytes.HasPrefix(payload, []byte("event:")) || bytes.HasPrefix(payload, []byte("data:")) {
			events = nil
			for len(payload) > 0 {
				size := sseFrameLen(payload)
				if size == 0 {
					size = len(payload)
				}
				data, present, valid := sseDataPayload(payload[:size])
				if present && len(data) > 0 {
					if !valid {
						return invalid()
					}
					events = append(events, data)
				}
				payload = payload[size:]
			}
		}
		for _, data := range events {
			data = jsonPayload(data)
			if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				continue
			}
			root := gjson.ParseBytes(data)
			upstreamError := root.Get("error")
			if !gjson.ValidBytes(data) || root.Get("type").String() == "response.failed" ||
				root.Get("status").String() == "failed" || root.Get("response.status").String() == "failed" ||
				(upstreamError.Exists() && upstreamError.Type != gjson.Null) {
				return invalid()
			}
		}
	}
	return nil
}
