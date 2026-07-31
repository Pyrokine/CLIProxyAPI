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
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
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
	from, to sdktranslator.Format
	body     []byte
	httpReq  *http.Request
	reporter *usageReporter
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
	if strings.EqualFold(strings.TrimSpace(e.Identifier()), "gemini-interactions") && nativeInteractionsSourceFormat(opts.SourceFormat) {
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
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
		req.Header.Del("Authorization")
	} else if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Del("x-goog-api-key")
	}
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
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequestWithContext(ctx, from, to, baseModel, originalPayload, isStream)
	body := sdktranslator.TranslateRequestWithContext(ctx, from, to, baseModel, req.Payload, isStream)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = fixGeminiImageAspectRatio(baseModel, body)
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = capGeminiMaxOutputTokens(body, baseModel)
	body = signature.SanitizeGeminiRequestThoughtSignatures(body, "contents")

	action := "generateContent"
	if isStream {
		action = "streamGenerateContent"
	}
	if req.Metadata != nil {
		if a, _ := req.Metadata["action"].(string); a == "countTokens" {
			action = "countTokens"
		}
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
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	return &geminiRequest{
		from:     from,
		to:       to,
		body:     body,
		httpReq:  httpReq,
		reporter: reporter,
	}, nil
}

// Execute performs a non-streaming request to the Gemini API.
func (e *GeminiExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
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
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	prepared.reporter.publish(ctx, parseGeminiUsage(data))
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx, prepared.to, prepared.from, req.Model, opts.OriginalRequest, prepared.body, data, &param,
	)
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
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter := prepared.reporter
	body := prepared.body
	from := prepared.from
	to := prepared.to
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		defer func() {
			streamUsage.publish(ctx, reporter)
			reporter.ensurePublished(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("gemini executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseGeminiStreamUsage(line); ok {
				streamUsage.observe(detail, true, false)
			}
			filtered := filterSSEUsageMetadata(line)
			payload := jsonPayload(filtered)
			if len(payload) == 0 {
				continue
			}
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
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
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
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *GeminiExecutor) executeInteractions(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	apiKey, bearer := geminiCreds(auth)
	reporter := newUsageReporter(ctx, e, targetName, auth)
	defer reporter.trackFailure(ctx, &err)

	body := translateGeminiInteractionsRequestBody(ctx, targetName, req.Payload, opts, false)
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body, _ = sjson.SetBytes(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req.Model)
	if err != nil {
		return resp, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	originalTranslated := geminiInteractionsPayloadConfigSource(ctx, targetName, req.Payload, opts, false)
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
	)
	reporter.SetTranslatedReasoningEffort(body, sdktranslator.FormatInteractions.String())

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/interactions", baseURL, glAPIVersion)
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errRequest != nil {
		return resp, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth)
	applyGeminiInteractionsRequestHeaders(httpReq, opts.Headers)
	applyGeminiInteractionsRevisionHeader(httpReq)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

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
		recordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return resp, err
	}
	reporter.publish(ctx, parseInteractionsUsage(data))
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
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *GeminiExecutor) executeInteractionsStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	targetName := thinking.ParseSuffix(req.Model).ModelName
	apiKey, bearer := geminiCreds(auth)
	reporter := newUsageReporter(ctx, e, targetName, auth)
	defer reporter.trackFailure(ctx, &err)

	body := translateGeminiInteractionsRequestBody(ctx, targetName, req.Payload, opts, true)
	if gjson.GetBytes(body, "model").Exists() && targetName != "" {
		body, _ = sjson.SetBytes(body, "model", targetName)
	}
	body, err = applyGeminiInteractionsThinking(body, req.Model)
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	fromProtocol := opts.SourceFormat.String()
	originalTranslated := geminiInteractionsPayloadConfigSource(ctx, targetName, req.Payload, opts, true)
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
	)
	body, _ = sjson.SetBytes(body, "stream", true)
	reporter.SetTranslatedReasoningEffort(body, sdktranslator.FormatInteractions.String())

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/interactions", baseURL, glAPIVersion)
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth)
	applyGeminiInteractionsRequestHeaders(httpReq, opts.Headers)
	applyGeminiInteractionsRevisionHeader(httpReq)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

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
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		defer func() {
			streamUsage.publish(ctx, reporter)
			reporter.ensurePublished(ctx)
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
			if geminiInteractionsSSEDone(rawFrame) {
				sawTerminalEvent = true
				if len(payload) == 0 {
					payload = []byte("[DONE]")
				}
			}
			if len(payload) == 0 && len(trimmed) > 0 && trimmed[0] == '{' {
				payload = trimmed
			}
			if len(payload) > 0 {
				detail, hasUsage, hasTier := parseInteractionsStreamUsageState(payload)
				streamUsage.observe(detail, hasUsage, hasTier)
			}
			if responseFormat == sdktranslator.FormatInteractions {
				visibleFrame := append(bytes.TrimRight(rawFrame, "\r\n"), '\n', '\n')
				return sendChunk(cliproxyexecutor.StreamChunk{Payload: visibleFrame})
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
			return true
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
					msg:  fmt.Sprintf("gemini interactions SSE frame exceeds maximum allowed size of %d bytes", streamScannerBuffer),
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
		if errScan := scanner.Err(); errScan != nil {
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
	translatedReq := sdktranslator.TranslateRequestWithContext(ctx, from, to, baseModel, req.Payload, false)

	translatedReq, err := thinking.ApplyThinking(translatedReq, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	translatedReq = fixGeminiImageAspectRatio(baseModel, translatedReq)
	translatedReq = signature.SanitizeGeminiRequestThoughtSignatures(translatedReq, "contents")
	respCtx := util.WithAlt(ctx, opts.Alt)
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "tools")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "generationConfig")
	translatedReq, _ = sjson.DeleteBytes(translatedReq, "safetySettings")
	translatedReq, _ = sjson.SetBytes(translatedReq, "model", baseModel)

	baseURL := resolveGeminiBaseURL(auth)
	url := fmt.Sprintf("%s/%s/models/%s:%s", baseURL, glAPIVersion, baseModel, "countTokens")

	requestBody := bytes.NewReader(translatedReq)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, requestBody)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", apiKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	applyGeminiHeaders(httpReq, auth)
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
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", resp.StatusCode,
			summarizeErrorBody(resp.Header.Get("Content-Type"), data),
		)
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(data)}
	}

	count := gjson.GetBytes(data, "totalTokens").Int()
	translated := sdktranslator.TranslateTokenCount(respCtx, to, from, count, data)
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

func translateGeminiInteractionsRequestBody(
	ctx context.Context,
	model string,
	payload []byte,
	opts cliproxyexecutor.Options,
	stream bool,
) []byte {
	if opts.SourceFormat == "" || opts.SourceFormat == sdktranslator.FormatInteractions {
		return bytes.Clone(payload)
	}
	return sdktranslator.TranslateRequestWithContext(ctx, opts.SourceFormat, sdktranslator.FormatInteractions, model, payload, stream)
}

func geminiInteractionsPayloadConfigSource(
	ctx context.Context,
	model string,
	payload []byte,
	opts cliproxyexecutor.Options,
	stream bool,
) []byte {
	source := opts.OriginalRequest
	if len(source) == 0 {
		source = payload
	}
	return translateGeminiInteractionsRequestBody(ctx, model, source, opts, stream)
}

func isNativeInteractionsAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "gemini-interactions")
}

func applyGeminiInteractionsThinking(body []byte, model string) ([]byte, error) {
	return thinking.ApplyThinking(body, model, sdktranslator.FormatInteractions.String(), sdktranslator.FormatInteractions.String(), "gemini")
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

func geminiInteractionsSSEDone(frame []byte) bool {
	trimmed := bytes.TrimSpace(frame)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return true
	}
	lines := bytes.Split(frame, []byte{'\n'})
	sawDoneEvent := false
	for _, line := range lines {
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.EqualFold(line, []byte("event: done")) {
			sawDoneEvent = true
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[len("data:"):])
			if bytes.Equal(data, []byte("[DONE]")) {
				return true
			}
		}
	}
	return sawDoneEvent
}

func applyGeminiHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
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
