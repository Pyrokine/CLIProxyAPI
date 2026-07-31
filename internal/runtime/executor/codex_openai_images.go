package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexOpenAIImageSourceFormat     = "openai-image"
	codexImagesGenerationsPath       = "/v1/images/generations"
	codexImagesEditsPath             = "/v1/images/edits"
	codexDirectImagesGenerations     = "/images/generations"
	codexDirectImagesEdits           = "/images/edits"
	codexGPTImage15Model             = "gpt-image-1.5"
	codexOpenAIImagesMainModel       = "gpt-5.4-mini"
	codexDefaultImageToolModel       = "gpt-image-2"
	maxCodexOpenAIImageResponseBytes = 50 << 20
	maxCodexOpenAIImageOutputItems   = 1024
)

type codexOpenAIImagePreparedRequest struct {
	Body           []byte
	ResponseFormat string
	StreamPrefix   string
}

type codexImageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

type codexImageOutputCollector struct {
	byIndex    map[int64][]byte
	fallback   [][]byte
	itemCount  int
	totalBytes int
}

func newCodexImageOutputCollector() *codexImageOutputCollector {
	return &codexImageOutputCollector{byIndex: make(map[int64][]byte)}
}

func (c *codexImageOutputCollector) add(eventData []byte) error {
	if c == nil {
		return fmt.Errorf("codex image output collector is nil")
	}
	item := gjson.GetBytes(eventData, "item")
	if !item.Exists() || item.Type != gjson.JSON {
		return nil
	}
	raw := []byte(item.Raw)
	if len(raw) > maxCodexOpenAIImageResponseBytes {
		return fmt.Errorf("codex image output item exceeds maximum allowed size of %d bytes", maxCodexOpenAIImageResponseBytes)
	}
	index := gjson.GetBytes(eventData, "output_index")
	if index.Exists() {
		key := index.Int()
		previous, exists := c.byIndex[key]
		itemCount := c.itemCount
		if !exists {
			itemCount++
		}
		totalBytes := c.totalBytes - len(previous) + len(raw)
		if itemCount > maxCodexOpenAIImageOutputItems || totalBytes > maxCodexOpenAIImageResponseBytes {
			return fmt.Errorf("codex image output items exceed configured memory limits")
		}
		c.byIndex[key] = bytes.Clone(raw)
		c.itemCount = itemCount
		c.totalBytes = totalBytes
		return nil
	}
	if c.itemCount+1 > maxCodexOpenAIImageOutputItems || c.totalBytes+len(raw) > maxCodexOpenAIImageResponseBytes {
		return fmt.Errorf("codex image output items exceed configured memory limits")
	}
	c.fallback = append(c.fallback, bytes.Clone(raw))
	c.itemCount++
	c.totalBytes += len(raw)
	return nil
}

func readCodexOpenAIImageResponseBody(body io.Reader) ([]byte, error) {
	data, errRead := io.ReadAll(io.LimitReader(body, maxCodexOpenAIImageResponseBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(data) > maxCodexOpenAIImageResponseBytes {
		return nil, fmt.Errorf(
			"codex image response exceeds maximum allowed size of %d bytes", maxCodexOpenAIImageResponseBytes,
		)
	}
	return data, nil
}

func parseCodexOpenAIImageEvent(frame []byte) ([]byte, bool, error) {
	eventData, hasData, valid := sseDataPayload(frame)
	if !hasData || len(eventData) == 0 || bytes.Equal(eventData, []byte("[DONE]")) {
		return nil, false, nil
	}
	if !valid {
		return nil, false, statusErr{
			code: http.StatusBadGateway,
			msg:  "upstream returned invalid image SSE JSON",
		}
	}
	return normalizeCodexCompletionEvent(eventData), true, nil
}

func isCodexOpenAIImageRequest(opts cliproxyexecutor.Options) bool {
	if !strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), codexOpenAIImageSourceFormat) {
		return false
	}
	return codexIsImagesEndpointPath(payloadRequestPath(opts))
}

func codexIsImagesEndpointPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == codexImagesGenerationsPath || path == codexImagesEditsPath {
		return true
	}
	return strings.HasSuffix(path, codexImagesGenerationsPath) || strings.HasSuffix(path, codexImagesEditsPath)
}

func (e *codexExecutor) resolveGPTImage2BaseModel() string {
	if e == nil || e.cfg == nil {
		return codexOpenAIImagesMainModel
	}
	model := strings.TrimSpace(e.cfg.GPTImage2BaseModel)
	if model == "" {
		return codexOpenAIImagesMainModel
	}
	if strings.HasPrefix(strings.ToLower(model), "gpt-") {
		return model
	}
	return codexOpenAIImagesMainModel
}

func codexOpenAIImageServiceTier(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if value := gjson.GetBytes(req.Payload, "service_tier"); value.Type == gjson.String {
		if tier := strings.TrimSpace(value.String()); tier != "" {
			return tier
		}
	}
	if len(opts.Metadata) == 0 {
		return ""
	}
	var tier string
	switch value := opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey].(type) {
	case string:
		tier = strings.TrimSpace(value)
	case []byte:
		tier = strings.TrimSpace(string(value))
	}
	if tier == usage.DefaultServiceTier {
		return ""
	}
	return tier
}

func (e *codexExecutor) executeOpenAIImage(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImage(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return resp, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := newUsageReporter(ctx, e, mainModel, auth)
	defer reporter.trackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return resp, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, upstreamBody, identityState, errCache := e.cacheHelper(
		ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body,
	)
	if errCache != nil {
		return resp, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, mainModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)

	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := readCodexOpenAIImageResponseBody(httpResp.Body)
	if errRead != nil {
		recordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	appendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	outputItems := newCodexImageOutputCollector()
	var completedData []byte
	remaining := data
	for len(remaining) > 0 {
		frameLen := sseFrameLen(remaining)
		if frameLen == 0 {
			if len(bytes.TrimSpace(remaining)) > 0 {
				errIncomplete := statusErr{
					code: http.StatusBadGateway,
					msg:  "upstream image stream ended with incomplete SSE frame",
				}
				recordAPIResponseError(ctx, e.cfg, errIncomplete)
				return resp, errIncomplete
			}
			break
		}
		frame := remaining[:frameLen]
		remaining = remaining[frameLen:]
		eventData, ok, errEventData := parseCodexOpenAIImageEvent(frame)
		if errEventData != nil {
			recordAPIResponseError(ctx, e.cfg, errEventData)
			return resp, errEventData
		}
		if !ok {
			continue
		}
		if errEvent, isError := parseCodexEventError(eventData); isError {
			recordAPIResponseError(ctx, e.cfg, errEvent)
			return resp, errEvent
		}
		switch gjson.GetBytes(eventData, "type").String() {
		case "response.output_item.done":
			if errCollect := outputItems.add(eventData); errCollect != nil {
				return resp, errCollect
			}
		case "response.completed":
			if len(completedData) == 0 {
				completedData = bytes.Clone(eventData)
			}
		}
	}
	if len(completedData) == 0 {
		err = statusErr{code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion"}
		return resp, err
	}

	results, createdAt, usageRaw, firstMeta, errExtract := codexExtractImageResults(
		completedData, outputItems.byIndex, outputItems.fallback,
	)
	if errExtract != nil {
		return resp, errExtract
	}
	if len(results) == 0 {
		return resp, statusErr{code: http.StatusBadGateway, msg: "upstream did not return image output"}
	}
	out, errOutput := codexBuildImagesAPIResponse(
		results, createdAt, usageRaw, firstMeta, prepared.ResponseFormat,
	)
	if errOutput != nil {
		return resp, errOutput
	}
	if detail, ok := parseCodexUsage(completedData); ok {
		reporter.publish(ctx, detail)
	}
	publishCodexImageToolUsage(ctx, reporter, body, completedData)
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *codexExecutor) executeOpenAIImageStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if directEndpoint := codexDirectOpenAIImageEndpoint(req, opts); directEndpoint != "" {
		return e.executeDirectOpenAIImageStream(ctx, auth, req, opts, directEndpoint)
	}

	prepared, errPrepare := codexPrepareOpenAIImageRequest(req, opts)
	if errPrepare != nil {
		return nil, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	mainModel := e.resolveGPTImage2BaseModel()
	reporter := newUsageReporter(ctx, e, mainModel, auth)
	defer reporter.trackFailure(ctx, &err)

	body, errBuild := e.prepareCodexOpenAIImageBody(prepared.Body, req, opts, mainModel)
	if errBuild != nil {
		return nil, errBuild
	}
	reporter.SetTranslatedReasoningEffort(body, "codex")

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, upstreamBody, identityState, errCache := e.cacheHelper(
		ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body,
	)
	if errCache != nil {
		return nil, errCache
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, mainModel)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)

	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := readCodexOpenAIImageResponseBody(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, maxCodexOpenAIImageResponseBytes)
		scanner.Split(scanSSEFrame)
		outputItems := newCodexImageOutputCollector()
		var completedData []byte
		for scanner.Scan() {
			frame := applyCodexIdentityConfuseResponsePayload(bytes.Clone(scanner.Bytes()), identityState)
			if len(bytes.TrimSpace(frame)) == 0 {
				continue
			}
			appendAPIResponseChunk(ctx, e.cfg, frame)
			if !isCompleteSSEFrame(frame) {
				errIncomplete := statusErr{
					code: http.StatusBadGateway,
					msg:  "upstream image stream ended with incomplete SSE frame",
				}
				recordAPIResponseError(ctx, e.cfg, errIncomplete)
				reporter.publishFailure(ctx, errIncomplete)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errIncomplete})
				return
			}
			eventData, ok, errEventData := parseCodexOpenAIImageEvent(frame)
			if errEventData != nil {
				recordAPIResponseError(ctx, e.cfg, errEventData)
				reporter.publishFailure(ctx, errEventData)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errEventData})
				return
			}
			if !ok {
				continue
			}
			if errEvent, isError := parseCodexEventError(eventData); isError {
				recordAPIResponseError(ctx, e.cfg, errEvent)
				reporter.publishFailure(ctx, errEvent)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errEvent})
				return
			}
			switch gjson.GetBytes(eventData, "type").String() {
			case "response.output_item.done":
				if errCollect := outputItems.add(eventData); errCollect != nil {
					recordAPIResponseError(ctx, e.cfg, errCollect)
					reporter.publishFailure(ctx, errCollect)
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errCollect})
					return
				}
			case "response.image_generation_call.partial_image":
				frame := codexBuildImagePartialFrame(eventData, prepared.ResponseFormat, prepared.StreamPrefix)
				if len(frame) > 0 && !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: frame}) {
					if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
						recordAPIResponseError(ctx, e.cfg, errStream)
						reporter.publishFailure(ctx, errStream)
					}
					return
				}
			case "response.completed":
				if len(completedData) == 0 {
					completedData = bytes.Clone(eventData)
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx, errScan)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		if len(completedData) == 0 {
			errIncomplete := statusErr{
				code: http.StatusGatewayTimeout, msg: "stream error: stream disconnected before completion",
			}
			recordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.publishFailure(ctx, errIncomplete)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errIncomplete})
			return
		}
		results, _, usageRaw, _, errExtract := codexExtractImageResults(
			completedData, outputItems.byIndex, outputItems.fallback,
		)
		if errExtract != nil {
			recordAPIResponseError(ctx, e.cfg, errExtract)
			reporter.publishFailure(ctx, errExtract)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errExtract})
			return
		}
		if len(results) == 0 {
			errOutput := statusErr{code: http.StatusBadGateway, msg: "upstream did not return image output"}
			recordAPIResponseError(ctx, e.cfg, errOutput)
			reporter.publishFailure(ctx, errOutput)
			_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errOutput})
			return
		}
		for _, img := range results {
			frame := codexBuildImageCompletedFrame(
				img, usageRaw, prepared.ResponseFormat, prepared.StreamPrefix,
			)
			if len(frame) > 0 && !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: frame}) {
				if errStream := scannerOrContextError(ctx, scanner); errStream != nil {
					recordAPIResponseError(ctx, e.cfg, errStream)
					reporter.publishFailure(ctx, errStream)
				}
				return
			}
		}
		if detail, ok := parseCodexUsage(completedData); ok {
			reporter.publish(ctx, detail)
		}
		publishCodexImageToolUsage(ctx, reporter, body, completedData)
		reporter.ensurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *codexExecutor) executeDirectOpenAIImage(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	endpointPath string,
) (resp cliproxyexecutor.Response, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, false)
	if errPrepare != nil {
		return resp, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	reporter := newUsageReporter(ctx, e, model, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, upstreamBody, identityState, errCache := e.cacheHelper(
		ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body,
	)
	if errCache != nil {
		return resp, errCache
	}
	applyCodexDirectImageHeaders(httpReq, auth, apiKey, false, e.cfg)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyModelHeaderOverrides(httpReq.Header, model)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)

	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, errRead := readCodexOpenAIImageResponseBody(httpResp.Body)
	if errRead != nil {
		recordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	data = applyCodexIdentityConfuseResponsePayload(data, identityState)
	appendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return resp, err
	}

	reporter.publish(ctx, parseOpenAIUsage(data))
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *codexExecutor) executeDirectOpenAIImageStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	endpointPath string,
) (_ *cliproxyexecutor.StreamResult, err error) {
	body, contentType, model, errPrepare := codexPrepareDirectOpenAIImageBody(req, opts, true)
	if errPrepare != nil {
		return nil, errPrepare
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	reporter := newUsageReporter(ctx, e, model, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, "openai")

	url := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, upstreamBody, identityState, errCache := e.cacheHelper(
		ctx, sdktranslator.FromString(codexOpenAIImageSourceFormat), url, auth, req, req.Payload, body,
	)
	if errCache != nil {
		return nil, errCache
	}
	applyCodexDirectImageHeaders(httpReq, auth, apiKey, true, e.cfg)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	applyModelHeaderOverrides(httpReq.Header, model)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), upstreamBody, e.Identifier(), auth)

	httpClient := newProtectedUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		recordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := readCodexOpenAIImageResponseBody(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = newCodexStatusErr(httpResp.StatusCode, data)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamUsage streamUsageBuffer
		var streamErr error
		sawPayload := false
		defer func() {
			if streamErr != nil {
				reporter.publishFailure(ctx, streamErr)
			} else {
				streamUsage.flushOpenAIStream()
				streamUsage.publish(ctx, reporter)
				reporter.ensurePublished(ctx)
			}
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()

		buffer := make([]byte, 32*1024)
		for {
			n, errRead := httpResp.Body.Read(buffer)
			if errRead != nil && errRead != io.EOF {
				streamErr = errRead
				recordAPIResponseError(ctx, e.cfg, errRead)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if n > 0 {
				chunk := bytes.Clone(buffer[:n])
				chunk = applyCodexIdentityConfuseResponsePayload(chunk, identityState)
				appendAPIResponseChunk(ctx, e.cfg, chunk)
				streamUsage.observeOpenAIStreamChunk(chunk)
				if !sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Payload: chunk}) {
					streamErr = ctx.Err()
					return
				}
				sawPayload = true
			}
			if errRead == io.EOF {
				if !sawPayload {
					streamErr = statusErr{
						code: http.StatusBadGateway, msg: "upstream returned an empty image stream",
					}
					recordAPIResponseError(ctx, e.cfg, streamErr)
					_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: streamErr})
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func codexDirectOpenAIImageEndpoint(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if codexDirectOpenAIImageModel(req) == "" {
		return ""
	}
	path := payloadRequestPath(opts)
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesGenerationsPath) {
		return codexDirectImagesGenerations
	}
	if strings.HasSuffix(strings.TrimSpace(path), codexImagesEditsPath) {
		return codexDirectImagesEdits
	}
	return ""
}

func codexPrepareDirectOpenAIImageBody(
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
) ([]byte, string, string, error) {
	model := codexDirectOpenAIImageModel(req)
	if model == "" {
		return nil, "", "", fmt.Errorf("unsupported direct OpenAI image model %q", req.Model)
	}
	body, contentType, errPrepare := codexPrepareDirectOpenAIImagePayload(req, opts, model, stream)
	if errPrepare != nil {
		return nil, "", "", errPrepare
	}
	return body, contentType, model, nil
}

func codexPrepareDirectOpenAIImagePayload(
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	model string,
	stream bool,
) ([]byte, string, error) {
	contentType := opts.Headers.Get("Content-Type")
	path := strings.TrimSpace(payloadRequestPath(opts))
	if strings.HasSuffix(path, codexImagesEditsPath) {
		return codexPrepareDirectOpenAIImageEditPayload(req.Payload, model, contentType, stream)
	}
	return prepareOpenAICompatImagesPayload(req.Payload, model, contentType, stream)
}

func codexPrepareDirectOpenAIImageEditPayload(
	payload []byte,
	model string,
	contentType string,
	stream bool,
) ([]byte, string, error) {
	if json.Valid(payload) {
		return prepareOpenAICompatImagesPayload(payload, model, contentType, stream)
	}

	mediaType, params, errParse := mime.ParseMediaType(strings.TrimSpace(contentType))
	if errParse != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "multipart/") {
		return nil, "", fmt.Errorf("unsupported OpenAI image edit Content-Type %q", contentType)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is missing")
	}
	return codexRewriteOpenAIImageEditMultipartToJSON(payload, model, boundary, stream)
}

func codexRewriteOpenAIImageEditMultipartToJSON(
	payload []byte,
	model string,
	boundary string,
	stream bool,
) ([]byte, string, error) {
	reader := multipart.NewReader(bytes.NewReader(payload), boundary)
	form, errRead := reader.ReadForm(openAICompatMultipartMemory)
	if errRead != nil {
		return nil, "", fmt.Errorf("read multipart form failed: %w", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart form files error: %v", errRemove)
		}
	}()

	out := []byte(`{"model":""}`)
	out, _ = sjson.SetBytes(out, "model", model)
	if stream {
		out, _ = sjson.SetBytes(out, "stream", true)
	}

	for key, values := range form.Value {
		key = strings.TrimSpace(key)
		if key == "" || key == "model" || key == "stream" {
			continue
		}
		out = codexSetOpenAIImageEditFormValues(out, key, values)
	}

	for _, fileHeader := range codexMultipartImageFiles(form) {
		dataURL, errData := codexMultipartFileToDataURL(fileHeader)
		if errData != nil {
			return nil, "", errData
		}
		out, _ = sjson.SetBytes(out, "images.-1.image_url", dataURL)
	}
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return nil, "", errData
		}
		out, _ = sjson.SetBytes(out, "mask.image_url", dataURL)
	}

	return out, "application/json", nil
}

func codexSetOpenAIImageEditFormValues(out []byte, key string, values []string) []byte {
	if len(values) == 0 {
		return out
	}
	path := codexOpenAIImageEditFormJSONPath(key)
	if path == "" {
		return out
	}
	if len(values) == 1 {
		return codexSetOpenAIImageEditFormValue(out, path, values[0])
	}
	out, _ = sjson.SetRawBytes(out, path, []byte(`[]`))
	for _, value := range values {
		item := codexOpenAIImageEditFormJSONValue(key, value)
		out, _ = sjson.SetRawBytes(out, path+".-1", item)
	}
	return out
}

func codexSetOpenAIImageEditFormValue(out []byte, path string, value string) []byte {
	item := codexOpenAIImageEditFormJSONValue(path, value)
	out, _ = sjson.SetRawBytes(out, path, item)
	return out
}

func codexOpenAIImageEditFormJSONValue(key string, value string) []byte {
	value = strings.TrimSpace(value)
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "n", "output_compression", "partial_images":
		if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
			raw, _ := json.Marshal(parsed)
			return raw
		}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func codexOpenAIImageEditFormJSONPath(key string) string {
	key = strings.TrimSpace(key)
	switch key {
	case "mask[file_id]":
		return "mask.file_id"
	case "mask[image_url]":
		return "mask.image_url"
	default:
		return key
	}
}

func codexDirectOpenAIImageModel(req cliproxyexecutor.Request) string {
	for _, model := range []string{gjson.GetBytes(req.Payload, "model").String(), req.Model} {
		baseModel := codexOpenAIImageBaseModel(model)
		if codexIsDirectOpenAIImageModel(baseModel) {
			return baseModel
		}
	}
	return ""
}

func codexOpenAIImageBaseModel(model string) string {
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = strings.TrimSpace(model[idx+1:])
	}
	return strings.ToLower(strings.TrimSpace(model))
}

func codexIsDirectOpenAIImageModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case codexGPTImage15Model, codexDefaultImageToolModel:
		return true
	default:
		return false
	}
}

func (e *codexExecutor) prepareCodexOpenAIImageBody(
	body []byte,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	mainModel string,
) ([]byte, error) {
	out := body
	mainModel = strings.TrimSpace(mainModel)
	if mainModel == "" {
		mainModel = codexOpenAIImagesMainModel
	}
	var errThinking error
	out, errThinking = thinking.ApplyThinking(out, mainModel, codexOpenAIImageSourceFormat, "codex", e.Identifier())
	if errThinking != nil {
		return nil, errThinking
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	out = applyPayloadConfigWithRequest(
		e.cfg, mainModel, "codex", codexOpenAIImageSourceFormat, "", out, body, requestedModel, requestPath,
		opts.Headers,
	)
	out, _ = sjson.SetBytes(out, "model", mainModel)
	out, _ = sjson.SetBytes(out, "stream", true)
	if serviceTier := codexOpenAIImageServiceTier(req, opts); serviceTier != "" {
		out, _ = sjson.SetBytes(out, "service_tier", serviceTier)
	}
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "prompt_cache_retention")
	out, _ = sjson.DeleteBytes(out, "safety_identifier")
	out, _ = sjson.DeleteBytes(out, "stream_options")
	return normalizeCodexInstructions(out), nil
}

func codexPrepareOpenAIImageRequest(
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (codexOpenAIImagePreparedRequest, error) {
	path := payloadRequestPath(opts)
	if strings.HasSuffix(path, codexImagesGenerationsPath) {
		return codexPrepareOpenAIImageGenerationJSON(req.Payload, req.Model)
	}
	if !strings.HasSuffix(path, codexImagesEditsPath) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("unsupported OpenAI image endpoint path %q", path)
	}

	contentType := codexImageContentType(opts.Headers)
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return codexPrepareOpenAIImageEditMultipart(req.Payload, req.Model, contentType)
	}
	return codexPrepareOpenAIImageEditJSON(req.Payload, req.Model)
}

func codexPrepareOpenAIImageGenerationJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image generation request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	tool := codexBuildOpenAIImageTool(
		rawJSON, routeModel, "generate", []string{"size", "quality", "background", "output_format", "moderation"},
		[]string{"output_compression", "partial_images"},
	)
	body := codexBuildImagesResponsesRequest(prompt, nil, tool)
	return codexOpenAIImagePreparedRequest{
		Body: body, ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON), StreamPrefix: "image_generation",
	}, nil
}

func codexPrepareOpenAIImageEditJSON(rawJSON []byte, routeModel string) (codexOpenAIImagePreparedRequest, error) {
	if !json.Valid(rawJSON) {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("invalid OpenAI image edit request JSON")
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	images := make([]string, 0)
	if imagesResult := gjson.GetBytes(rawJSON, "images"); imagesResult.IsArray() {
		for _, img := range imagesResult.Array() {
			url := strings.TrimSpace(img.Get("image_url").String())
			if url != "" {
				images = append(images, url)
			}
		}
	}
	tool := codexBuildOpenAIImageTool(
		rawJSON, routeModel, "edit",
		[]string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"},
		[]string{"output_compression", "partial_images"},
	)
	if mask := strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.image_url").String()); mask != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", mask)
	}
	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{
		Body: body, ResponseFormat: codexOpenAIImageResponseFormatFromJSON(rawJSON), StreamPrefix: "image_edit",
	}, nil
}

func codexPrepareOpenAIImageEditMultipart(
	rawBody []byte,
	routeModel string,
	contentType string,
) (codexOpenAIImagePreparedRequest, error) {
	_, params, errMedia := mime.ParseMediaType(contentType)
	if errMedia != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart content type failed: %w", errMedia)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("multipart boundary is required")
	}
	reader := multipart.NewReader(bytes.NewReader(rawBody), boundary)
	form, errForm := reader.ReadForm(32 << 20)
	if errForm != nil {
		return codexOpenAIImagePreparedRequest{}, fmt.Errorf("parse multipart form failed: %w", errForm)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			log.Errorf("codex openai images: remove multipart temp files error: %v", errRemove)
		}
	}()

	prompt := strings.TrimSpace(codexFormValue(form, "prompt"))
	responseFormat := codexNormalizeImageResponseFormat(codexFormValue(form, "response_format"))
	tool := []byte(`{"type":"image_generation","action":"edit"}`)
	tool, _ = sjson.SetBytes(tool, "model", codexOpenAIImageToolModel(codexFormValue(form, "model"), routeModel))
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"} {
		if value := strings.TrimSpace(codexFormValue(form, field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := strings.TrimSpace(codexFormValue(form, field)); value != "" {
			if parsed, errParse := strconv.ParseInt(value, 10, 64); errParse == nil {
				tool, _ = sjson.SetBytes(tool, field, parsed)
			}
		}
	}

	images := make([]string, 0)
	for _, fh := range codexMultipartImageFiles(form) {
		dataURL, errData := codexMultipartFileToDataURL(fh)
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		images = append(images, dataURL)
	}
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, errData := codexMultipartFileToDataURL(maskFiles[0])
		if errData != nil {
			return codexOpenAIImagePreparedRequest{}, errData
		}
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", dataURL)
	}

	body := codexBuildImagesResponsesRequest(prompt, images, tool)
	return codexOpenAIImagePreparedRequest{Body: body, ResponseFormat: responseFormat, StreamPrefix: "image_edit"}, nil
}

func codexImageContentType(headers http.Header) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get("Content-Type"))
}

func codexOpenAIImageResponseFormatFromJSON(rawJSON []byte) string {
	return codexNormalizeImageResponseFormat(gjson.GetBytes(rawJSON, "response_format").String())
}

func codexNormalizeImageResponseFormat(responseFormat string) string {
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		return "url"
	}
	return "b64_json"
}

func codexOpenAIImageToolModel(requestModel string, routeModel string) string {
	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = strings.TrimSpace(routeModel)
	}
	if model == "" {
		model = codexDefaultImageToolModel
	}
	return model
}

func codexBuildOpenAIImageTool(
	rawJSON []byte,
	routeModel string,
	action string,
	stringFields []string,
	numberFields []string,
) []byte {
	tool := []byte(`{"type":"image_generation","action":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(
		tool, "model", codexOpenAIImageToolModel(gjson.GetBytes(rawJSON, "model").String(), routeModel),
	)
	for _, field := range stringFields {
		if value := strings.TrimSpace(gjson.GetBytes(rawJSON, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range numberFields {
		if value := gjson.GetBytes(rawJSON, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	return tool
}

func codexBuildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	req, _ = sjson.SetBytes(req, "model", codexOpenAIImagesMainModel)
	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	contentIndex := 1
	for _, img := range images {
		if strings.TrimSpace(img) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", img)
		input, _ = sjson.SetRawBytes(input, fmt.Sprintf("0.content.%d", contentIndex), part)
		contentIndex++
	}
	req, _ = sjson.SetRawBytes(req, "input", input)
	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools.-1", toolJSON)
	}
	return req
}

func codexFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) == 0 {
		return ""
	}
	return strings.TrimSpace(form.Value[key][0])
}

func codexMultipartImageFiles(form *multipart.Form) []*multipart.FileHeader {
	if form == nil {
		return nil
	}
	if files := form.File["image[]"]; len(files) > 0 {
		return files
	}
	return form.File["image"]
}

func codexMultipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	f, errOpen := fileHeader.Open()
	if errOpen != nil {
		return "", fmt.Errorf("open upload file failed: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.Errorf("codex openai images: close upload file error: %v", errClose)
		}
	}()
	data, errRead := io.ReadAll(f)
	if errRead != nil {
		return "", fmt.Errorf("read upload file failed: %w", errRead)
	}
	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func codexExtractImageResults(
	completed []byte,
	itemsByIndex map[int64][]byte,
	fallback [][]byte,
) (results []codexImageCallResult, createdAt int64, usageRaw []byte, firstMeta codexImageCallResult, err error) {
	if gjson.GetBytes(completed, "type").String() != "response.completed" {
		return nil, 0, nil, codexImageCallResult{}, fmt.Errorf("unexpected event type")
	}
	createdAt = gjson.GetBytes(completed, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	appendItem := func(item gjson.Result) {
		if item.Get("type").String() != "image_generation_call" {
			return
		}
		res := strings.TrimSpace(item.Get("result").String())
		if res == "" {
			return
		}
		entry := codexImageCallResult{
			Result:        res,
			RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
			OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
			Size:          strings.TrimSpace(item.Get("size").String()),
			Background:    strings.TrimSpace(item.Get("background").String()),
			Quality:       strings.TrimSpace(item.Get("quality").String()),
		}
		if len(results) == 0 {
			firstMeta = entry
		}
		results = append(results, entry)
	}

	var outputItems []gjson.Result
	if output := gjson.GetBytes(completed, "response.output"); output.Exists() && output.IsArray() {
		outputItems = output.Array()
	}
	if len(outputItems) > 0 {
		results = make([]codexImageCallResult, 0, len(outputItems))
		for _, item := range outputItems {
			appendItem(item)
		}
	} else if len(itemsByIndex) > 0 || len(fallback) > 0 {
		results = make([]codexImageCallResult, 0, len(itemsByIndex)+len(fallback))
		if len(itemsByIndex) > 0 {
			indexes := make([]int64, 0, len(itemsByIndex))
			for idx := range itemsByIndex {
				indexes = append(indexes, idx)
			}
			sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
			for _, idx := range indexes {
				appendItem(gjson.ParseBytes(itemsByIndex[idx]))
			}
		}
		for _, raw := range fallback {
			appendItem(gjson.ParseBytes(raw))
		}
	}

	if usageValue := gjson.GetBytes(
		completed, "response.tool_usage.image_gen",
	); usageValue.Exists() && usageValue.IsObject() {
		usageRaw = []byte(usageValue.Raw)
	}
	return results, createdAt, usageRaw, firstMeta, nil
}

func codexBuildImagesAPIResponse(
	results []codexImageCallResult,
	createdAt int64,
	usageRaw []byte,
	firstMeta codexImageCallResult,
	responseFormat string,
) ([]byte, error) {
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)
	responseFormat = codexNormalizeImageResponseFormat(responseFormat)
	for _, img := range results {
		item := []byte(`{}`)
		if responseFormat == "url" {
			item, _ = sjson.SetBytes(
				item, "url", "data:"+codexMimeTypeFromOutputFormat(img.OutputFormat)+";base64,"+img.Result,
			)
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", img.Result)
		}
		if img.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", img.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}
	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}
	return out, nil
}

func codexBuildImagePartialFrame(payload []byte, responseFormat string, streamPrefix string) []byte {
	b64 := strings.TrimSpace(gjson.GetBytes(payload, "partial_image_b64").String())
	if b64 == "" {
		return nil
	}
	outputFormat := strings.TrimSpace(gjson.GetBytes(payload, "output_format").String())
	eventName := strings.TrimSpace(streamPrefix) + ".partial_image"
	data := []byte(`{"type":"","partial_image_index":0}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	data, _ = sjson.SetBytes(data, "partial_image_index", gjson.GetBytes(payload, "partial_image_index").Int())
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(data, "url", "data:"+codexMimeTypeFromOutputFormat(outputFormat)+";base64,"+b64)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", b64)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildImageCompletedFrame(
	img codexImageCallResult,
	usageRaw []byte,
	responseFormat string,
	streamPrefix string,
) []byte {
	eventName := strings.TrimSpace(streamPrefix) + ".completed"
	data := []byte(`{"type":""}`)
	data, _ = sjson.SetBytes(data, "type", eventName)
	if codexNormalizeImageResponseFormat(responseFormat) == "url" {
		data, _ = sjson.SetBytes(
			data, "url", "data:"+codexMimeTypeFromOutputFormat(img.OutputFormat)+";base64,"+img.Result,
		)
	} else {
		data, _ = sjson.SetBytes(data, "b64_json", img.Result)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		data, _ = sjson.SetRawBytes(data, "usage", usageRaw)
	}
	return codexBuildSSEFrame(eventName, data)
}

func codexBuildSSEFrame(eventName string, data []byte) []byte {
	var buf bytes.Buffer
	if strings.TrimSpace(eventName) != "" {
		buf.WriteString("event: ")
		buf.WriteString(eventName)
		buf.WriteString("\n")
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

func codexMimeTypeFromOutputFormat(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func publishCodexImageToolUsage(ctx context.Context, reporter *usageReporter, body []byte, completedData []byte) {
	detail, ok := parseCodexImageToolUsage(completedData)
	if !ok {
		return
	}
	reporter.ensurePublished(ctx)
	reporter.publishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}

func parseCodexImageToolUsage(data []byte) (usage.Detail, bool) {
	usageNode := gjson.ParseBytes(data).Get("response.tool_usage.image_gen")
	if !openAIStyleUsageTokenFieldsExist(usageNode) {
		return usage.Detail{}, false
	}
	detail := parseOpenAIStyleUsageNode(usageNode)
	detail.ResponseServiceTier = extractResponseServiceTier(data)
	return detail, true
}

func openAIStyleUsageTokenFieldsExist(usageNode gjson.Result) bool {
	if !usageNode.Exists() || !usageNode.IsObject() {
		return false
	}
	for _, path := range []string{
		"prompt_tokens",
		"input_tokens",
		"completion_tokens",
		"output_tokens",
		"total_tokens",
		"prompt_tokens_details.cached_tokens",
		"input_tokens_details.cached_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"input_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"completion_tokens_details.reasoning_tokens",
		"output_tokens_details.reasoning_tokens",
	} {
		if validUsageTokenNode(usageNode.Get(path)) {
			return true
		}
	}
	return false
}

func parseOpenAIStyleUsageNode(usageNode gjson.Result) usage.Detail {
	inputNode := firstExistingUsageNode(usageNode, "prompt_tokens", "input_tokens")
	outputNode := firstExistingUsageNode(usageNode, "completion_tokens", "output_tokens")
	totalNode := firstExistingUsageNode(usageNode, "total_tokens")
	detail := usage.Detail{
		InputTokens: inputNode.Int(), OutputTokens: outputNode.Int(), TotalTokens: totalNode.Int(),
	}
	cached := firstExistingUsageNode(
		usageNode,
		"prompt_tokens_details.cached_tokens",
		"input_tokens_details.cached_tokens",
	)
	if cached.Exists() {
		detail.CachedTokens = cached.Int()
		detail.CacheReadTokens = cached.Int()
	}
	cacheCreation := firstExistingUsageNode(
		usageNode,
		"input_tokens_details.cache_creation_tokens",
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_write_tokens",
	)
	if cacheCreation.Exists() {
		detail.CacheCreationTokens = cacheCreation.Int()
	}
	reasoning := firstExistingUsageNode(
		usageNode,
		"completion_tokens_details.reasoning_tokens",
		"output_tokens_details.reasoning_tokens",
	)
	if reasoning.Exists() {
		detail.ReasoningTokens = reasoning.Int()
	}
	return detail
}
