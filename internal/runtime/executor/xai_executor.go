package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	multiagentv2 "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/optimize-multi-agent-v2"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

var (
	xaiDataTag  = []byte("data:")
	xaiEventTag = []byte("event:")
)

const (
	xaiImageHandlerType        = "openai-image"
	xaiVideoHandlerType        = "openai-video"
	xaiCustomToolType          = "custom"
	xaiFunctionToolType        = "function"
	xaiImageGenerationToolType = "image_generation"
	xaiNamespaceToolType       = "namespace"
	xaiToolSearchType          = "tool_search"
	xaiWebSearchToolType       = "web_search"
	xaiXSearchToolType         = "x_search"
	// Codex Desktop injects codex_app.automation_update with a large oneOf+$ref
	// schema. xAI's free/build Responses path accepts the request but never
	// emits SSE while that schema is present.
	xaiImagesGenerationsPath      = "/images/generations"
	xaiImagesEditsPath            = "/images/edits"
	xaiDefaultImageEndpointPath   = xaiImagesGenerationsPath
	xaiVideosGenerationsPath      = "/videos/generations"
	xaiVideosEditsPath            = "/videos/edits"
	xaiVideosExtensionsPath       = "/videos/extensions"
	xaiVideosPath                 = "/videos"
	xaiIdempotencyKeyMetaKey      = "idempotency_key"
	xaiComposerModelPrefix        = "grok-composer-"
	xaiTokenAuthHeader            = xaiauth.ShellTokenAuthHeader
	xaiTokenAuthValue             = xaiauth.ShellTokenAuthValue
	xaiClientVersionHeader        = xaiauth.ShellClientVersionHeader
	xaiClientVersionValue         = xaiauth.ShellClientVersionValue
	xaiClientIdentifierHeader     = xaiauth.ShellClientIdentifierHeader
	xaiClientIdentifierValue      = xaiauth.ShellClientIdentifierValue
	xaiAuthenticateResponseHeader = xaiauth.ShellAuthenticateResponseHeader
	xaiAuthenticateResponseValue  = xaiauth.ShellAuthenticateResponseValue
	xaiUsingAPIAttr               = "using_api"
	xaiMaxTools                   = 200
	maxXAIResponseBodyBytes       = 50 << 20
)

func readXAIResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	data, errRead := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if errRead != nil {
		return nil, markResponseStarted(errRead)
	}
	if int64(len(data)) > maxBytes {
		return nil, markResponseStarted(
			fmt.Errorf("xai response exceeds maximum allowed size of %d bytes", maxBytes),
		)
	}
	return data, nil
}

// XAIExecutor is a stateless executor for xAI Grok's Responses API.
type XAIExecutor struct {
	cfg *config.Config
}

// newXAIExecutor creates a new xAI executor.
func newXAIExecutor(cfg *config.Config) *XAIExecutor {
	return &XAIExecutor{cfg: cfg}
}

// Identifier returns the provider identifier.
func (e *XAIExecutor) Identifier() string {
	return "xai"
}

func (e *XAIExecutor) SupportsApplyPatch() bool {
	return e != nil
}

// PrepareRequest injects xAI credentials into the outgoing HTTP request.
func (e *XAIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	clientHeaders := req.Header.Clone()
	token, _ := xaiCreds(auth)
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(req, "Authorization", bearer)
	if xaiIsCLIChatProxyRequestURL(req.URL) {
		xaiauth.ApplyShellIdentityHeaders(req.Header)
	}
	applyXAICustomHeaders(req, auth, clientHeaders)
	return nil
}

// HttpRequest injects xAI credentials into the request and executes it.
func (e *XAIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("xai executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *XAIExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	if endpointPath := xaiImageEndpointPath(opts); endpointPath != "" {
		return e.executeImages(ctx, auth, req, endpointPath, opts)
	}
	if xaiIsVideoRequest(opts) {
		return e.executeVideos(ctx, auth, req, opts)
	}

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)

	prepared, err := e.prepareResponsesRequestWithContext(ctx, auth, req, opts, true, sdktranslator.FormatCodex)
	if err != nil {
		return resp, err
	}

	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.setTranslatedReasoningEffort(prepared.body, e.Identifier())

	upstreamURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(prepared.body))
	if err != nil {
		return resp, err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, upstreamURL, httpReq.Header.Clone(), prepared.body)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return resp, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		clearXAIReasoningReplayOnInvalidSignature(ctx, prepared.reasoningReplayScope, httpResp.StatusCode, data)
		return resp, xaiStatusErr(httpResp.StatusCode, data, httpResp.Header)
	}

	data, err := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	var observedUsage streamUsageBuffer
	defer func() {
		if err != nil {
			reporter.publishFailureWithDetail(ctx, observedUsage.detail, err)
		}
	}()
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	responseNormalizer := prepared.newResponseNormalizer()
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if !bytes.HasPrefix(line, xaiDataTag) {
			continue
		}
		for _, eventRaw := range xaiNormalizeReasoningSummaryDataEvents(bytes.TrimSpace(line[len(xaiDataTag):])) {
			if detail, ok := parseCodexUsage(eventRaw); ok {
				observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
			}
			for _, eventData := range responseNormalizer.normalize(eventRaw) {
				if len(eventData) == 0 {
					continue
				}
				observeResponseTokenEvent(ctx, eventData)
				reporter.observeResponseModel(eventData)
				switch gjson.GetBytes(eventData, "type").String() {
				case "response.output_item.done":
					xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
				case "error", "response.error", "response.failed":
					if eventErr, ok := parseCodexEventError(eventData); ok {
						recordAPIResponseError(ctx, e.cfg, eventErr)
						reporter.publishFailureWithDetail(ctx, observedUsage.detail, eventErr)
						return resp, eventErr
					}
					return resp, markResponseStarted(
						statusErr{code: http.StatusBadGateway, msg: "xai executor: invalid upstream tool call"},
					)
				case "response.completed", "response.incomplete":
					eventType := gjson.GetBytes(eventData, "type").String()
					terminalData := xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
					terminalData = xaiNormalizeReasoningSummaryData(terminalData)
					var param any
					out := sdktranslator.TranslateNonStream(
						ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body,
						terminalData, &param,
					)
					translationErr := sdktranslator.ResponseTranslationError(&param)
					if translationErr == nil && len(out) == 0 {
						translationErr = fmt.Errorf("xai executor: response translation returned an empty payload")
					}
					if translationErr != nil {
						err = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
						recordAPIResponseError(ctx, e.cfg, err)
						return resp, err
					}
					if err = ctx.Err(); err != nil {
						return resp, err
					}
					observedUsage.publish(ctx, reporter)
					if eventType == "response.completed" {
						cacheXAIReasoningReplayFromCompleted(ctx, prepared.reasoningReplayScope, terminalData)
					}
					return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
				}
			}
		}
	}

	return resp, markResponseStarted(
		statusErr{
			code: http.StatusRequestTimeout,
			msg:  "xai stream error: stream disconnected before response.completed or response.incomplete",
		},
	)
}

func (e *XAIExecutor) executeCompact(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	prepared, data, headers, reporter, errCompact := e.executeCompactRequest(ctx, auth, req, opts)
	if errCompact != nil {
		return resp, errCompact
	}
	defer reporter.trackFailure(ctx, &err)

	var param any
	out := sdktranslator.TranslateNonStream(
		ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, data, &param,
	)
	translationErr := sdktranslator.ResponseTranslationError(&param)
	if translationErr == nil && len(out) == 0 {
		translationErr = fmt.Errorf("xai executor: compact response translation returned an empty payload")
	}
	if translationErr != nil {
		err = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
		reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(data), err)
		return resp, err
	}
	if err = ctx.Err(); err != nil {
		reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(data), err)
		return resp, err
	}
	reporter.publish(ctx, parseOpenAIUsage(data))
	clearXAIReasoningReplayAfterCompaction(ctx, prepared.reasoningReplayScope)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *XAIExecutor) executeCompactRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*xaiPreparedRequest, []byte, http.Header, *usageReporter, error) {
	token, _ := xaiCreds(auth)
	baseURL := xaiCompactBaseURL(auth)

	opts.Alt = "responses/compact"
	prepared, err := e.prepareResponsesRequestWithContext(ctx, auth, req, opts, false, sdktranslator.FormatOpenAIResponse)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.setTranslatedReasoningEffort(prepared.body, e.Identifier())

	requestURL := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	applyXAIHeaders(httpReq, auth, token, false, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, requestURL, httpReq.Header.Clone(), prepared.body)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	data, err := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, nil, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		clearXAIReasoningReplayOnInvalidSignature(ctx, prepared.reasoningReplayScope, httpResp.StatusCode, data)
		err = xaiStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, nil, nil, nil, err
	}

	observeResponseTokenEvent(ctx, data)
	reporter.observeResponseModel(data)
	if _, _, err = validateXAIWebsocketCompactionResponse(data); err != nil {
		err = markResponseStarted(err)
		reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(data), err)
		return nil, nil, nil, nil, err
	}
	return prepared, data, httpResp.Header.Clone(), reporter, nil
}

func (e *XAIExecutor) executeCompactionTriggerStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	prepared, data, headers, reporter, err := e.executeCompactRequest(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	defer reporter.trackFailure(ctx, &err)

	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")

	chunks := xaiBuildCompactionTriggerStreamChunks(prepared, data)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)
	if err = ctx.Err(); err != nil {
		reporter.publishFailureWithDetail(ctx, parseOpenAIUsage(data), err)
		return nil, err
	}
	reporter.publish(ctx, parseOpenAIUsage(data))
	clearXAIReasoningReplayAfterCompaction(ctx, prepared.reasoningReplayScope)
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func xaiInputHasItemType(body []byte, itemType string) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if item.Get("type").String() == itemType {
			return true
		}
	}
	return false
}

func xaiRemoveInputItemsByType(body []byte, itemType string) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	kept := 0
	for _, item := range input.Array() {
		if item.Get("type").String() == itemType {
			continue
		}
		if kept > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(item.Raw)
		kept++
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(body, "input", buf.Bytes())
	if err != nil {
		return body
	}
	return updated
}

func xaiBuildCompactionTriggerStreamChunks(prepared *xaiPreparedRequest, compactData []byte) [][]byte {
	responseID := xaiCompactionResponseID(compactData)
	now := time.Now().Unix()
	createdAt := gjson.GetBytes(compactData, "created_at").Int()
	if createdAt == 0 {
		createdAt = now
	}
	completedAt := gjson.GetBytes(compactData, "completed_at").Int()
	if completedAt == 0 {
		completedAt = now
	}

	item := xaiCompactionOutputItem(compactData, responseID)
	output := make([]byte, 0, len(item)+2)
	output = append(output, '[')
	output = append(output, item...)
	output = append(output, ']')

	createdResponse := xaiBuildCompactionBaseResponse(prepared, compactData, responseID, createdAt, "in_progress")
	inProgressResponse := xaiBuildCompactionBaseResponse(prepared, compactData, responseID, createdAt, "in_progress")
	completedResponse := xaiBuildCompactionBaseResponse(prepared, compactData, responseID, createdAt, "completed")
	requestModelName := ""
	if prepared != nil {
		requestModelName = gjson.GetBytes(prepared.originalPayload, "model").String()
		if requestModelName == "" {
			requestModelName = prepared.baseModel
		}
	}
	if requestModelName == "" {
		requestModelName = gjson.GetBytes(compactData, "model").String()
	}
	if requestModelName != "" {
		createdResponse, _ = sjson.SetBytes(createdResponse, "model", requestModelName)
		inProgressResponse, _ = sjson.SetBytes(inProgressResponse, "model", requestModelName)
	}
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", completedAt)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", output)
	if usage := gjson.GetBytes(compactData, "usage"); usage.Exists() {
		completedResponse, _ = sjson.SetRawBytes(completedResponse, "usage", []byte(usage.Raw))
	}

	createdPayload := []byte(`{"type":"response.created","sequence_number":0}`)
	createdPayload, _ = sjson.SetRawBytes(createdPayload, "response", createdResponse)
	inProgressPayload := []byte(`{"type":"response.in_progress","sequence_number":1}`)
	inProgressPayload, _ = sjson.SetRawBytes(inProgressPayload, "response", inProgressResponse)
	addedPayload := []byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":0}`)
	addedPayload, _ = sjson.SetRawBytes(addedPayload, "item", item)
	keepalivePayload := []byte(`{"type":"keepalive","sequence_number":3}`)
	donePayload := []byte(`{"type":"response.output_item.done","sequence_number":4,"output_index":0}`)
	donePayload, _ = sjson.SetRawBytes(donePayload, "item", item)
	completedPayload := []byte(`{"type":"response.completed","sequence_number":5}`)
	completedPayload, _ = sjson.SetRawBytes(completedPayload, "response", completedResponse)

	return [][]byte{
		xaiBuildSSEFrame("response.created", createdPayload),
		xaiBuildSSEFrame("response.in_progress", inProgressPayload),
		xaiBuildSSEFrame("response.output_item.added", addedPayload),
		xaiBuildSSEFrame("keepalive", keepalivePayload),
		xaiBuildSSEFrame("response.output_item.done", donePayload),
		xaiBuildSSEFrame("response.completed", completedPayload),
	}
}

func xaiBuildCompactionBaseResponse(
	prepared *xaiPreparedRequest,
	compactData []byte,
	responseID string,
	createdAt int64,
	status string,
) []byte {
	response := []byte(`{"id":"","object":"response","created_at":0,"status":"","background":false,"error":null,"incomplete_details":null,"output":[]}`)
	response, _ = sjson.SetBytes(response, "id", responseID)
	response, _ = sjson.SetBytes(response, "created_at", createdAt)
	response, _ = sjson.SetBytes(response, "status", status)
	if model := gjson.GetBytes(compactData, "model").String(); model != "" {
		response, _ = sjson.SetBytes(response, "model", model)
	} else if prepared != nil && prepared.baseModel != "" {
		response, _ = sjson.SetBytes(response, "model", prepared.baseModel)
	}

	if prepared == nil {
		return response
	}
	for _, field := range []string{
		"instructions",
		"max_output_tokens",
		"max_tool_calls",
		"parallel_tool_calls",
		"previous_response_id",
		"prompt_cache_key",
		"reasoning",
		"text",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"truncation",
		"user",
		"metadata",
	} {
		if value := gjson.GetBytes(prepared.body, field); value.Exists() {
			response, _ = sjson.SetRawBytes(response, field, []byte(value.Raw))
		}
	}
	return response
}

func xaiCompactionOutputItem(compactData []byte, responseID string) []byte {
	itemResult := gjson.GetBytes(compactData, "output.0")
	item := []byte(`{"type":"compaction"}`)
	if itemResult.Exists() && itemResult.Type == gjson.JSON {
		item = []byte(itemResult.Raw)
	}
	if !gjson.GetBytes(item, "type").Exists() {
		item, _ = sjson.SetBytes(item, "type", "compaction")
	}
	if !gjson.GetBytes(item, "id").Exists() {
		item, _ = sjson.SetBytes(item, "id", xaiCompactionItemID(responseID))
	}
	return item
}

func xaiCompactionResponseID(compactData []byte) string {
	if responseID := strings.TrimSpace(gjson.GetBytes(compactData, "id").String()); responseID != "" {
		if strings.HasPrefix(responseID, "resp_") {
			return responseID
		}
		return "resp_" + strings.TrimPrefix(responseID, "cmp_")
	}
	return fmt.Sprintf("resp_xai_compaction_%d", time.Now().UnixNano())
}

func xaiCompactionItemID(responseID string) string {
	if suffix := strings.TrimPrefix(responseID, "resp_"); suffix != "" && suffix != responseID {
		return "cmp_" + suffix
	}
	return "cmp_" + responseID
}

func xaiBuildSSEFrame(eventName string, data []byte) []byte {
	out := make([]byte, 0, len(eventName)+len(data)+16)
	out = append(out, "event: "...)
	out = append(out, eventName...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, '\n', '\n')
	return out
}

func (e *XAIExecutor) executeImages(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	endpointPath string,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	model := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	reporter := newUsageReporter(ctx, e, model, auth)
	defer reporter.trackFailure(ctx, &err)

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	if endpointPath == "" {
		endpointPath = xaiDefaultImageEndpointPath
	}

	original := req.Payload
	if len(opts.OriginalRequest) > 0 {
		original = opts.OriginalRequest
	}
	payload := applyPayloadConfigWithRequest(
		e.cfg, model, "openai", opts.SourceFormat.String(), "", req.Payload, original,
		payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier(),
	)
	payload = normalizeXAIImageRefs(payload)
	upstreamURL := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, auth, token, false, "", opts.Headers)
	e.recordXAIRequest(ctx, auth, upstreamURL, httpReq.Header.Clone(), payload)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	data, err := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = xaiStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return resp, err
	}

	observeResponseTokenEvent(ctx, data)
	reporter.observeResponseModel(data)
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *XAIExecutor) executeVideos(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	model := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	reporter := newUsageReporter(ctx, e, model, auth)
	defer reporter.trackFailure(ctx, &err)

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)

	payload := req.Payload
	method := http.MethodPost
	endpointPath := xaiVideosGenerationsPath
	var body io.Reader

	switch path := xaiVideoEndpointPath(opts); path {
	case xaiVideosGenerationsPath, xaiVideosEditsPath, xaiVideosExtensionsPath:
		endpointPath = path
	default:
		if requestID := strings.TrimSpace(gjson.GetBytes(payload, "request_id").String()); requestID != "" {
			method = http.MethodGet
			endpointPath = xaiVideosPath + "/" + url.PathEscape(requestID)
		}
	}
	if method == http.MethodPost {
		original := req.Payload
		if len(opts.OriginalRequest) > 0 {
			original = opts.OriginalRequest
		}
		payload = applyPayloadConfigWithRequest(
			e.cfg, model, "openai", opts.SourceFormat.String(), "", payload, original,
			payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier(),
		)
		payload = normalizeXAIImageRefs(payload)
		body = bytes.NewReader(payload)
	}
	requestURL := strings.TrimSuffix(baseURL, "/") + endpointPath
	httpReq, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return resp, err
	}
	applyXAIHeaders(httpReq, auth, token, false, "", opts.Headers)
	if method == http.MethodPost {
		key := xaiMetadataString(opts.Metadata, xaiIdempotencyKeyMetaKey)
		if key == "" && opts.Headers != nil {
			key = strings.TrimSpace(opts.Headers.Get("x-idempotency-key"))
		}
		if key != "" {
			httpReq.Header.Set("x-idempotency-key", key)
		}
	}
	e.recordXAIRequest(ctx, auth, requestURL, httpReq.Header.Clone(), payload)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	data, err := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		err = xaiStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return resp, err
	}

	observeResponseTokenEvent(ctx, data)
	reporter.observeResponseModel(data)
	reporter.ensurePublished(ctx)
	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *XAIExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if xaiInputHasItemType(req.Payload, "compaction_trigger") {
		return e.executeCompactionTriggerStream(ctx, auth, req, opts)
	}

	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)

	prepared, err := e.prepareResponsesRequestWithContext(ctx, auth, req, opts, true, sdktranslator.FormatCodex)
	if err != nil {
		return nil, err
	}

	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	reporter.setTranslatedReasoningEffort(prepared.body, e.Identifier())

	upstreamURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, prepared.sessionID, opts.Headers)
	e.recordXAIRequest(ctx, auth, upstreamURL, httpReq.Header.Clone(), prepared.body)

	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf(
			"request error, error status: %d, error message: %s", httpResp.StatusCode,
			summarizeErrorBody(httpResp.Header.Get("Content-Type"), data),
		)
		clearXAIReasoningReplayOnInvalidSignature(ctx, prepared.reasoningReplayScope, httpResp.StatusCode, data)
		return nil, xaiStatusErr(httpResp.StatusCode, data, httpResp.Header)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		var observedUsage streamUsageBuffer
		defer func() {
			if errContext := ctx.Err(); errContext != nil {
				reporter.publishFailureWithDetail(ctx, observedUsage.detail, errContext)
			}
		}()
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		var pendingEventLine []byte
		responseNormalizer := prepared.newResponseNormalizer()
		sawTerminal := false
		var completedReplay []byte
		emitTranslatedLine := func(translatedLine []byte) bool {
			chunks := sdktranslator.TranslateStream(
				ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body,
				translatedLine, &param,
			)
			if translationErr := sdktranslator.ResponseTranslationError(&param); translationErr != nil {
				translationErr = markResponseStarted(statusErr{code: http.StatusBadGateway, msg: translationErr.Error()})
				recordAPIResponseError(ctx, e.cfg, translationErr)
				reporter.publishFailureWithDetail(ctx, observedUsage.detail, translationErr)
				_ = sendStreamChunk(ctx, out, cliproxyexecutor.StreamChunk{Err: translationErr})
				return false
			}
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if bytes.HasPrefix(line, xaiEventTag) {
				if pendingEventLine != nil && !emitTranslatedLine(
					xaiNormalizeReasoningSummaryEventLine(
						pendingEventLine, "",
					),
				) {
					return
				}
				pendingEventLine = bytes.Clone(line)
				continue
			}
			if bytes.HasPrefix(line, xaiDataTag) {
				eventDataList := xaiNormalizeReasoningSummaryDataEvents(bytes.TrimSpace(line[len(xaiDataTag):]))
				for _, eventRaw := range eventDataList {
					if detail, ok := parseCodexUsage(eventRaw); ok {
						observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
					}
					for _, eventData := range responseNormalizer.normalize(eventRaw) {
						if len(eventData) == 0 {
							pendingEventLine = nil
							continue
						}
						observeResponseTokenEvent(ctx, eventData)
						reporter.observeResponseModel(eventData)
						eventName := gjson.GetBytes(eventData, "type").String()
						switch eventName {
						case "response.output_item.done":
							xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
						case "error", "response.error", "response.failed":
							eventErr, ok := parseCodexEventError(eventData)
							if !ok {
								eventErr = markResponseStarted(
									statusErr{
										code: http.StatusBadGateway,
										msg:  "xai executor: upstream returned an unrecognized error event",
									},
								)
							}
							recordAPIResponseError(ctx, e.cfg, eventErr)
							reporter.publishFailureWithDetail(ctx, observedUsage.detail, eventErr)
							select {
							case out <- cliproxyexecutor.StreamChunk{Err: eventErr}:
							case <-ctx.Done():
							}
							return
						case "response.completed", "response.incomplete":
							sawTerminal = true
							eventData = xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
							eventData = xaiNormalizeReasoningSummaryData(eventData)
							eventName = gjson.GetBytes(eventData, "type").String()
							if eventName == "response.completed" {
								completedReplay = bytes.Clone(eventData)
							} else {
								completedReplay = nil
							}
						}
						if pendingEventLine != nil && !emitTranslatedLine(
							xaiNormalizeReasoningSummaryEventLine(
								pendingEventLine, eventName,
							),
						) {
							return
						}
						if !emitTranslatedLine(append([]byte("data: "), eventData...)) {
							return
						}
					}
				}
				pendingEventLine = nil
				continue
			}
			if pendingEventLine != nil {
				if !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
					return
				}
				pendingEventLine = nil
			}
			if !emitTranslatedLine(bytes.Clone(line)) {
				return
			}
		}
		if pendingEventLine != nil {
			if !emitTranslatedLine(xaiNormalizeReasoningSummaryEventLine(pendingEventLine, "")) {
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			errScan = markResponseStarted(errScan)
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailureWithDetail(ctx, observedUsage.detail, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if !sawTerminal && ctx.Err() == nil {
			errIncomplete := markResponseStarted(
				fmt.Errorf("xai stream ended before response.completed or response.incomplete"),
			)
			recordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.publishFailureWithDetail(ctx, observedUsage.detail, errIncomplete)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errIncomplete}:
			case <-ctx.Done():
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		observedUsage.publish(ctx, reporter)
		reporter.ensurePublished(ctx)
		if ctx.Err() == nil && len(completedReplay) > 0 {
			if cliproxyexecutor.StreamDeliveryAcknowledgement(ctx) {
				terminal := cliproxyexecutor.StreamChunk{
					Terminal: true,
					Acknowledge: func(errDelivery error) {
						if errDelivery == nil && ctx.Err() == nil {
							cacheXAIReasoningReplayFromCompleted(ctx, prepared.reasoningReplayScope, completedReplay)
						}
					},
				}
				_ = sendStreamChunk(ctx, out, terminal)
			} else {
				cacheXAIReasoningReplayFromCompleted(ctx, prepared.reasoningReplayScope, completedReplay)
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for xAI Responses requests.
func (e *XAIExecutor) CountTokens(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	prepared, err := e.prepareResponsesRequestWithContext(
		ctx, auth, req, opts, false, sdktranslator.FormatCodex,
	)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: tokenizer init failed: %w", err)
	}
	count, err := countXAIInputTokens(enc, prepared.body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: token counting failed: %w", err)
	}
	usageJSON := fmt.Sprintf(
		`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count,
	)
	translated := sdktranslator.TranslateTokenCount(
		ctx, prepared.to, prepared.responseFormat, count, []byte(usageJSON),
	)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func countXAIInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	segments := make([]string, 0, 32)
	xaiAppendTokenString(&segments, root.Get("instructions"))
	xaiCollectInputTokenSegments(root.Get("input"), &segments)
	xaiCollectToolTokenSegments(root.Get("tools"), &segments)

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		xaiAppendTokenString(&segments, textFormat.Get("name"))
		xaiAppendTokenJSON(&segments, textFormat.Get("schema"))
	}
	if len(segments) == 0 {
		return 0, nil
	}
	count, err := enc.Count(strings.Join(segments, "\n"))
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func xaiCollectInputTokenSegments(input gjson.Result, segments *[]string) {
	if input.Type == gjson.String {
		xaiAppendTokenString(segments, input)
		return
	}
	if !input.IsArray() {
		return
	}
	for _, item := range input.Array() {
		switch item.Get("type").String() {
		case "message":
			xaiCollectContentTokenSegments(item.Get("content"), segments)
		case "function_call":
			xaiAppendTokenString(segments, item.Get("name"))
			xaiAppendTokenJSON(segments, item.Get("arguments"))
		case "function_call_output":
			xaiAppendTokenJSON(segments, item.Get("output"))
		case "reasoning":
			for _, part := range item.Get("summary").Array() {
				xaiAppendTokenString(segments, part.Get("text"))
			}
		}
	}
}

func xaiCollectContentTokenSegments(content gjson.Result, segments *[]string) {
	if content.Type == gjson.String {
		xaiAppendTokenString(segments, content)
		return
	}
	if !content.IsArray() {
		return
	}
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			xaiAppendTokenString(segments, part.Get("text"))
		case "refusal":
			xaiAppendTokenString(segments, part.Get("refusal"))
		case "input_image":
			xaiAppendTokenString(segments, part.Get("image_url"))
			xaiAppendTokenString(segments, part.Get("file_id"))
		case "input_file":
			xaiAppendTokenString(segments, part.Get("file_data"))
			xaiAppendTokenString(segments, part.Get("file_url"))
			xaiAppendTokenString(segments, part.Get("file_id"))
			xaiAppendTokenString(segments, part.Get("filename"))
		case "input_audio":
			xaiAppendTokenString(segments, part.Get("data"))
			xaiAppendTokenString(segments, part.Get("input_audio.data"))
		}
	}
}

func xaiCollectToolTokenSegments(tools gjson.Result, segments *[]string) {
	if !tools.IsArray() {
		return
	}
	for _, tool := range tools.Array() {
		if tool.Get("type").String() != xaiFunctionToolType {
			continue
		}
		xaiAppendTokenString(segments, tool.Get("name"))
		xaiAppendTokenString(segments, tool.Get("description"))
		xaiAppendTokenJSON(segments, tool.Get("parameters"))
	}
}

func xaiAppendTokenString(segments *[]string, value gjson.Result) {
	if text := strings.TrimSpace(value.String()); text != "" {
		*segments = append(*segments, text)
	}
}

func xaiAppendTokenJSON(segments *[]string, value gjson.Result) {
	if !value.Exists() {
		return
	}
	if value.Type == gjson.String {
		xaiAppendTokenString(segments, value)
		return
	}
	if text := strings.TrimSpace(value.Raw); text != "" {
		*segments = append(*segments, text)
	}
}

// Refresh refreshes xAI OAuth credentials using the stored refresh token.
func (e *XAIExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("xai executor: refresh called")
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "xai executor: auth is nil"}
	}
	refreshToken := xaiMetadataString(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, nil
	}
	_, configuredBaseURL := xaiCreds(auth)
	if configuredBaseURL == "" {
		configuredBaseURL = xaiauth.DefaultAPIBaseURL
	}
	tokenEndpoint := xaiMetadataString(auth.Metadata, "token_endpoint")
	svc := xaiauth.NewXAIAuthWithProxyURL(e.cfg, auth.ProxyURL)
	td, err := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if err != nil {
		return nil, err
	}
	return applyXAIRefreshToken(auth, td, tokenEndpoint, configuredBaseURL), nil
}

func applyXAIRefreshToken(
	auth *cliproxyauth.Auth,
	tokenData *xaiauth.TokenData,
	tokenEndpoint string,
	configuredBaseURL string,
) *cliproxyauth.Auth {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["type"] = "xai"
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["access_token"] = tokenData.AccessToken
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.IDToken != "" {
		auth.Metadata["id_token"] = tokenData.IDToken
	}
	if tokenData.TokenType != "" {
		auth.Metadata["token_type"] = tokenData.TokenType
	}
	if tokenData.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = tokenData.ExpiresIn
	}
	if tokenData.Expire != "" {
		auth.Metadata["expired"] = tokenData.Expire
	}
	if tokenData.Email != "" {
		auth.Metadata["email"] = tokenData.Email
	}
	if tokenData.Subject != "" {
		auth.Metadata["sub"] = tokenData.Subject
	}
	if tokenEndpoint != "" {
		auth.Metadata["token_endpoint"] = tokenEndpoint
	}
	if xaiMetadataString(auth.Metadata, "base_url") == "" {
		auth.Metadata["base_url"] = configuredBaseURL
	}
	lastRefresh := time.Now().UTC().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = lastRefresh
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["auth_kind"] = "oauth"
	if strings.TrimSpace(auth.Attributes["base_url"]) == "" {
		auth.Attributes["base_url"] = configuredBaseURL
	}
	if storage, ok := auth.Storage.(*xaiauth.TokenStorage); ok && storage != nil {
		copyStorage := *storage
		storage = &copyStorage
		auth.Storage = storage
		storage.Type = "xai"
		storage.AuthKind = "oauth"
		storage.AccessToken = tokenData.AccessToken
		if tokenData.RefreshToken != "" {
			storage.RefreshToken = tokenData.RefreshToken
		}
		if tokenData.IDToken != "" {
			storage.IDToken = tokenData.IDToken
		}
		if tokenData.TokenType != "" {
			storage.TokenType = tokenData.TokenType
		}
		if tokenData.ExpiresIn > 0 {
			storage.ExpiresIn = tokenData.ExpiresIn
		}
		if tokenData.Expire != "" {
			storage.Expire = tokenData.Expire
		}
		if tokenData.Email != "" {
			storage.Email = tokenData.Email
		}
		if tokenData.Subject != "" {
			storage.Subject = tokenData.Subject
		}
		storage.BaseURL = xaiMetadataString(auth.Metadata, "base_url")
		storage.TokenEndpoint = xaiMetadataString(auth.Metadata, "token_endpoint")
		storage.LastRefresh = lastRefresh
		storage.SetMetadata(auth.Metadata)
	}
	return auth
}

type xaiPreparedRequest struct {
	baseModel             string
	from                  sdktranslator.Format
	responseFormat        sdktranslator.Format
	to                    sdktranslator.Format
	originalPayload       []byte
	body                  []byte
	patchDeclarations     []byte
	toolIdentity          *xaiToolIdentityNormalizer
	dispatcherState       *xaiNamespaceDispatcherState
	filterInternalXSearch bool
	sessionID             string
	reasoningReplayScope  xaiReasoningReplayScope
}

func (e *XAIExecutor) prepareResponsesRequest(
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
) (*xaiPreparedRequest, error) {
	return e.prepareResponsesRequestWithContext(context.Background(), nil, req, opts, stream, sdktranslator.FormatCodex)
}

func (e *XAIExecutor) prepareResponsesRequestWithContext(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	stream bool,
	to sdktranslator.Format,
	websocketBaseURL ...string,
) (*xaiPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalPayloadSource)
	translationOriginal := originalPayload
	translationPayload := bytes.Clone(req.Payload)
	if from == sdktranslator.FormatOpenAIResponse {
		translationOriginal = multiagentv2.RewriteCodexMultiAgentV2Input(ctx, opts.Headers, translationOriginal, e.cfg)
		translationPayload = multiagentv2.RewriteCodexMultiAgentV2Input(ctx, opts.Headers, translationPayload, e.cfg)
	}
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, translationOriginal, translationPayload, stream, isCompat, &req,
	)
	if err != nil {
		return nil, err
	}
	originalTranslated = preserveXAIResponsesOutputControls(originalTranslated, originalPayload, from)
	body = preserveXAIResponsesOutputControls(body, req.Payload, from)

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, req.Model, from.String(), e.Identifier(), e.Identifier(), req,
	)
	if err != nil {
		return nil, err
	}

	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.SetBoolIfDifferent(body, "stream", stream)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body = normalizeCodexInstructions(body)
	sessionID := xaiExecutionSessionID(req, opts)
	if sessionID == "" && xaiRequiresIsolatedConversation(baseModel) {
		sessionID = uuid.NewString()
	}
	if sessionID != "" {
		body = helps.SetStringIfDifferent(body, "prompt_cache_key", sessionID)
	}
	if len(websocketBaseURL) > 0 {
		if previousID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()); previousID != "" {
			body, _ = sjson.SetBytes(body, "previous_response_id", previousID)
		}
		body = buildXAIWebsocketRequestBody(body)
	}
	body = sanitizeXAIResponsesBody(body, baseModel, req)
	if opts.Alt == "responses/compact" {
		body, _ = sjson.DeleteBytes(body, "stream")
		body, _ = sjson.DeleteBytes(body, "tools")
		body = normalizeXAIToolChoiceForTools(body)
		for _, field := range []string{"max_output_tokens", "temperature", "top_p", "top_k", "stop"} {
			body, _ = sjson.DeleteBytes(body, field)
		}
		body = xaiRemoveInputItemsByType(body, "compaction_trigger")
		if previousResponseID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String()); previousResponseID != "" {
			body, _ = sjson.SetBytes(body, "previous_response_id", previousResponseID)
		}
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	requestPath := payloadRequestPath(opts)
	body = applyPayloadConfigWithRequest(
		e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath,
		opts.Headers, e.Identifier(),
	)
	patchDeclarations := bytes.Clone(body)
	toolIdentity := newXAIToolIdentityNormalizer(body)
	body, err = translatorcommon.NormalizeResponsesPatchTools(body)
	if err != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	body = promoteXAIAdditionalTools(body)
	willInjectXSearch := e.cfg != nil && e.cfg.XAI.InjectXSearch && opts.Alt != "responses/compact"
	var dispatcherState *xaiNamespaceDispatcherState
	if xaiShouldFoldNamespaceTools(body, willInjectXSearch) {
		body, dispatcherState, err = foldXAINamespaceTools(body)
		if err != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
		}
		dispatcherState.registerDeclarations(toolIdentity)
		body = dispatcherState.normalizeToolChoice(body)
	}
	body, err = toolIdentity.normalizeDeclarations(body)
	if err != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	body, err = normalizeXAIImageGeneration(body, baseModel)
	if err != nil {
		return nil, err
	}
	body = normalizeXAIToolChoiceForTools(body)
	if !xaiToolChoiceRequiresHostedToolOnly(body) {
		if xaiRequestHasNativeXSearch(body) || willInjectXSearch {
			body = ensureXAINativeXSearchTool(body)
		}
	}
	body = clampXAIToolsLimit(body, xaiMaxTools, dispatcherState)
	var replayScope xaiReasoningReplayScope
	body, replayScope = applyXAIReasoningReplayCache(ctx, auth, from, req, opts, body, websocketBaseURL...)
	body = dispatcherState.normalizeInputHistory(body)
	body = toolIdentity.normalizeInputHistory(body)
	body, err = toolIdentity.aliasClientWebSearchInput(body)
	if err != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	body = normalizeXAIInputReasoningItems(body)
	body = sanitizeXAIInputEncryptedContent(ctx, body, isCompat)
	body = normalizeXAIImageRefs(body)

	return &xaiPreparedRequest{
		baseModel:             baseModel,
		from:                  from,
		responseFormat:        responseFormat,
		to:                    to,
		originalPayload:       originalPayload,
		body:                  body,
		patchDeclarations:     patchDeclarations,
		toolIdentity:          toolIdentity,
		dispatcherState:       dispatcherState,
		filterInternalXSearch: xaiRequestHasNativeXSearch(body),
		sessionID:             sessionID,
		reasoningReplayScope:  replayScope,
	}, nil
}

func (e *XAIExecutor) recordXAIRequest(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	url string,
	headers http.Header,
	body []byte,
) {
	recordUpstreamRequest(ctx, e.cfg, url, headers, body, e.Identifier(), auth)
}

func xaiCreds(auth *cliproxyauth.Auth) (token, baseURL string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		token = strings.TrimSpace(auth.Attributes["api_key"])
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if auth.Metadata != nil {
		if token == "" {
			token = xaiMetadataString(auth.Metadata, "access_token")
		}
		if baseURL == "" {
			baseURL = xaiMetadataString(auth.Metadata, "base_url")
		}
	}
	return token, baseURL
}

func xaiUsingAPI(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return true
	}
	if raw := strings.TrimSpace(auth.Attributes[xaiUsingAPIAttr]); raw != "" {
		if parsed, errParse := strconv.ParseBool(raw); errParse == nil {
			return parsed
		}
	}
	if raw, ok := auth.Metadata[xaiUsingAPIAttr]; ok && raw != nil {
		switch value := raw.(type) {
		case bool:
			return value
		case string:
			if parsed, errParse := strconv.ParseBool(strings.TrimSpace(value)); errParse == nil {
				return parsed
			}
		}
	}
	if raw := strings.TrimSpace(auth.Attributes["auth_kind"]); raw != "" {
		return !strings.EqualFold(raw, "oauth")
	}
	return !strings.EqualFold(xaiMetadataString(auth.Metadata, "auth_kind"), "oauth")
}

// xaiChatBaseURL returns the target for non-media xAI HTTP chat requests.
// WebSocket and media paths intentionally continue to use xaiCreds directly.
func xaiChatBaseURL(auth *cliproxyauth.Auth) string {
	_, baseURL := xaiCreds(auth)
	if xaiUsingAPI(auth) {
		if baseURL == "" {
			return xaiauth.DefaultAPIBaseURL
		}
		return baseURL
	}
	if baseURL != "" && !xaiIsDefaultAPIBaseURL(baseURL) {
		return baseURL
	}
	return xaiauth.CLIChatProxyBaseURL
}

func xaiCompactBaseURL(auth *cliproxyauth.Auth) string {
	_, baseURL := xaiCreds(auth)
	if baseURL == "" || xaiIsCLIChatProxyBaseURL(baseURL) {
		return xaiauth.DefaultAPIBaseURL
	}
	return baseURL
}

func xaiNormalizeBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	parsed, errParse := url.Parse(baseURL)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(baseURL, "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	parsed.Host = host
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}

func xaiIsDefaultAPIBaseURL(baseURL string) bool {
	return xaiNormalizeBaseURL(baseURL) == xaiNormalizeBaseURL(xaiauth.DefaultAPIBaseURL)
}

func xaiIsCLIChatProxyBaseURL(baseURL string) bool {
	return xaiNormalizeBaseURL(baseURL) == xaiNormalizeBaseURL(xaiauth.CLIChatProxyBaseURL)
}

func xaiIsCLIChatProxyRequestURL(requestURL *url.URL) bool {
	if requestURL == nil {
		return false
	}
	baseURL, errParse := url.Parse(xaiauth.CLIChatProxyBaseURL)
	if errParse != nil || !xaiSameURLAuthority(requestURL, baseURL) {
		return false
	}
	requestPath := strings.TrimRight(requestURL.EscapedPath(), "/")
	basePath := strings.TrimRight(baseURL.EscapedPath(), "/")
	return requestPath == basePath || strings.HasPrefix(requestPath, basePath+"/")
}

func xaiSameURLAuthority(left *url.URL, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return xaiEffectiveURLPort(left) == xaiEffectiveURLPort(right)
}

func xaiEffectiveURLPort(value *url.URL) string {
	if value == nil {
		return ""
	}
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func applyXAIHeaders(
	r *http.Request,
	auth *cliproxyauth.Auth,
	token string,
	stream bool,
	sessionID string,
	clientHeaders ...http.Header,
) {
	applyXAIDefaultHeaders(r, token, stream, sessionID)
	applyXAICustomHeaders(r, auth, clientHeaders...)
}

func applyXAIDefaultHeaders(r *http.Request, token string, stream bool, sessionID string) {
	r.Header.Set("Content-Type", "application/json")
	bearer := ""
	if strings.TrimSpace(token) != "" {
		bearer = "Bearer " + token
	}
	util.ApplyCredentialHeader(r, "Authorization", bearer)
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")
	if sessionID != "" {
		r.Header.Set("x-grok-conv-id", sessionID)
	}
}

func applyXAICustomHeaders(r *http.Request, auth *cliproxyauth.Auth, clientHeaders ...http.Header) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, clientHeaders...)
}

func applyXAIChatHeaders(
	r *http.Request,
	auth *cliproxyauth.Auth,
	token string,
	stream bool,
	sessionID string,
	clientHeaders ...http.Header,
) {
	applyXAIDefaultHeaders(r, token, stream, sessionID)
	if !xaiUsingAPI(auth) && xaiIsCLIChatProxyRequestURL(r.URL) {
		xaiauth.ApplyShellIdentityHeaders(r.Header)
	}
	applyXAICustomHeaders(r, auth, clientHeaders...)
}

func xaiExecutionSessionID(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if value := cliproxysession.NormalizeExplicitID(
		xaiMetadataString(
			opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey,
		),
	); value != "" {
		return value
	}
	if value := cliproxysession.NormalizeExplicitID(
		xaiMetadataString(
			req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey,
		),
	); value != "" {
		return value
	}
	if value := cliproxysession.NormalizeExplicitID(
		gjson.GetBytes(
			req.Payload, "prompt_cache_key",
		).String(),
	); value != "" {
		return value
	}
	for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
		if value := cliproxysession.NormalizeExplicitID(xaiMetadataString(metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey)); value != "" {
			return cliproxysession.NormalizeToCanonicalUUID(value)
		}
	}
	return helps.DerivedSessionUUID("xai", opts.Metadata, req.Metadata)
}

func xaiRequiresIsolatedConversation(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), xaiComposerModelPrefix)
}

func xaiImageEndpointPath(opts cliproxyexecutor.Options) string {
	if opts.SourceFormat.String() != xaiImageHandlerType {
		return ""
	}

	path := xaiMetadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if strings.HasSuffix(path, "/images/edits") {
		return xaiImagesEditsPath
	}
	if strings.HasSuffix(path, "/images/generations") {
		return xaiImagesGenerationsPath
	}
	return xaiDefaultImageEndpointPath
}

// normalizeXAIImageRefs rewrites image.image_url aliases anywhere in the JSON
// tree while preserving chat content parts whose image_url is not nested below
// an image, images, or reference_images field.
func normalizeXAIImageRefs(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return body
	}
	if !normalizeXAIImageRefsValue(payload) {
		return body
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return normalized
}

func normalizeXAIImageRefsValue(value any) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			switch key {
			case "image":
				changed = normalizeXAIImageRef(child) || changed
			case "images", "reference_images":
				if refs, ok := child.([]any); ok {
					for _, ref := range refs {
						changed = normalizeXAIImageRef(ref) || changed
					}
				}
			}
			changed = normalizeXAIImageRefsValue(child) || changed
		}
	case []any:
		for _, child := range node {
			changed = normalizeXAIImageRefsValue(child) || changed
		}
	}
	return changed
}

func normalizeXAIImageRef(value any) bool {
	ref, ok := value.(map[string]any)
	if !ok {
		return false
	}
	originalURL, _ := ref["url"].(string)
	urlValue := strings.TrimSpace(originalURL)
	imageURL, hasImageURL := ref["image_url"]
	if urlValue == "" {
		switch typed := imageURL.(type) {
		case string:
			urlValue = strings.TrimSpace(typed)
		case map[string]any:
			urlValue, _ = typed["url"].(string)
			urlValue = strings.TrimSpace(urlValue)
		}
	}
	if urlValue == "" {
		return false
	}
	if urlValue == originalURL && !hasImageURL {
		return false
	}
	ref["url"] = urlValue
	delete(ref, "image_url")
	return true
}

func xaiIsVideoRequest(opts cliproxyexecutor.Options) bool {
	return opts.SourceFormat.String() == xaiVideoHandlerType
}

func xaiVideoEndpointPath(opts cliproxyexecutor.Options) string {
	if !xaiIsVideoRequest(opts) {
		return ""
	}
	path := xaiMetadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey)
	if strings.HasSuffix(path, "/videos/edits") {
		return xaiVideosEditsPath
	}
	if strings.HasSuffix(path, "/videos/extensions") {
		return xaiVideosExtensionsPath
	}
	if strings.HasSuffix(path, "/videos/generations") {
		return xaiVideosGenerationsPath
	}
	return ""
}

func xaiMetadataString(meta map[string]any, key string) string {
	if len(meta) == 0 || key == "" {
		return ""
	}
	value, ok := meta[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func xaiNormalizeReasoningSummaryEventLine(line []byte, eventName string) []byte {
	if eventName == "" && bytes.HasPrefix(line, xaiEventTag) {
		eventName = strings.TrimSpace(string(line[len(xaiEventTag):]))
	}
	eventName = xaiNormalizeReasoningSummaryEventName(eventName)
	if eventName == "" {
		return bytes.Clone(line)
	}
	return []byte("event: " + eventName)
}

func xaiNormalizeReasoningSummaryEventName(eventName string) string {
	switch eventName {
	case "response.reasoning_text.delta":
		return "response.reasoning_summary_text.delta"
	case "response.reasoning_text.done":
		return "response.reasoning_summary_part.done"
	default:
		return eventName
	}
}

func xaiNormalizeReasoningSummaryDataEvents(eventData []byte) [][]byte {
	if len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return [][]byte{eventData}
	}
	if gjson.GetBytes(eventData, "type").String() != "response.reasoning_text.done" {
		return [][]byte{xaiNormalizeReasoningSummaryData(eventData)}
	}

	textDone, _ := sjson.SetBytes(eventData, "type", "response.reasoning_summary_text.done")
	textDone = xaiNormalizeReasoningSummaryIndex(textDone)
	partDone := xaiNormalizeReasoningSummaryData(eventData)
	return [][]byte{textDone, partDone}
}

func xaiNormalizeReasoningSummaryData(eventData []byte) []byte {
	if len(eventData) == 0 || !gjson.ValidBytes(eventData) {
		return eventData
	}

	normalized := eventData
	switch gjson.GetBytes(normalized, "type").String() {
	case "response.reasoning_text.delta":
		normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_text.delta")
		normalized = xaiNormalizeReasoningSummaryIndex(normalized)
	case "response.reasoning_text.done":
		normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.done")
		normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
		if text := gjson.GetBytes(normalized, "text"); text.Exists() {
			normalized, _ = sjson.SetBytes(normalized, "part.text", text.String())
		}
		normalized, _ = sjson.DeleteBytes(normalized, "text")
		normalized = xaiNormalizeReasoningSummaryIndex(normalized)
	case "response.content_part.added":
		if gjson.GetBytes(normalized, "part.type").String() == "reasoning_text" {
			normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.added")
			normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
			normalized = xaiNormalizeReasoningSummaryIndex(normalized)
		}
	case "response.content_part.done":
		if gjson.GetBytes(normalized, "part.type").String() == "reasoning_text" {
			normalized, _ = sjson.SetBytes(normalized, "type", "response.reasoning_summary_part.done")
			normalized, _ = sjson.SetBytes(normalized, "part.type", "summary_text")
			normalized = xaiNormalizeReasoningSummaryIndex(normalized)
		}
	}
	return normalized
}

func xaiNormalizeReasoningSummaryIndex(eventData []byte) []byte {
	if gjson.GetBytes(eventData, "summary_index").Exists() {
		return eventData
	}
	contentIndex := gjson.GetBytes(eventData, "content_index")
	if !contentIndex.Exists() {
		return eventData
	}
	eventData, _ = sjson.SetRawBytes(eventData, "summary_index", []byte(contentIndex.Raw))
	eventData, _ = sjson.DeleteBytes(eventData, "content_index")
	return eventData
}

func preserveXAIResponsesOutputControls(body, source []byte, from sdktranslator.Format) []byte {
	var maxOutputTokens gjson.Result
	switch from {
	case sdktranslator.FormatOpenAI:
		maxOutputTokens = gjson.GetBytes(source, "max_completion_tokens")
		if !maxOutputTokens.Exists() || maxOutputTokens.Type == gjson.Null {
			maxOutputTokens = gjson.GetBytes(source, "max_tokens")
		}
	case sdktranslator.FormatOpenAIResponse:
		maxOutputTokens = gjson.GetBytes(source, "max_output_tokens")
	default:
		return body
	}
	if maxOutputTokens.Exists() && maxOutputTokens.Type != gjson.Null {
		body, _ = sjson.SetRawBytes(body, "max_output_tokens", []byte(maxOutputTokens.Raw))
	}
	for _, field := range []string{"temperature", "top_p", "top_k"} {
		value := gjson.GetBytes(source, field)
		if value.Exists() && value.Type != gjson.Null {
			body, _ = sjson.SetRawBytes(body, field, []byte(value.Raw))
		}
	}
	verbosity := gjson.GetBytes(source, "text.verbosity")
	if !verbosity.Exists() || verbosity.Type == gjson.Null {
		verbosity = gjson.GetBytes(source, "verbosity")
	}
	if verbosity.Exists() && verbosity.Type != gjson.Null {
		body, _ = sjson.SetRawBytes(body, "text.verbosity", []byte(verbosity.Raw))
	}
	body, _ = sjson.DeleteBytes(body, "verbosity")
	return body
}

var xaiGrokImageGenerationMinVersion = xaiGrokVersion{major: 4, minor: 6}

type xaiGrokVersion struct {
	major int
	minor int
}

func xaiSupportsNativeImageGeneration(model string) bool {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	if name == "" || !strings.HasPrefix(name, "grok-") {
		return false
	}
	rest := strings.TrimPrefix(name, "grok-")
	if rest == "4.20" || strings.HasPrefix(rest, "4.20-") {
		return false
	}
	version, ok := xaiParseGrokVersionPrefix(rest)
	if !ok {
		return false
	}
	return xaiCompareGrokVersion(version, xaiGrokImageGenerationMinVersion) >= 0
}

func xaiParseGrokVersionPrefix(value string) (xaiGrokVersion, bool) {
	majorEnd := 0
	for majorEnd < len(value) && value[majorEnd] >= '0' && value[majorEnd] <= '9' {
		majorEnd++
	}
	if majorEnd == 0 {
		return xaiGrokVersion{}, false
	}
	major, err := strconv.Atoi(value[:majorEnd])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	if majorEnd == len(value) || value[majorEnd] != '.' {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	minorEnd := majorEnd + 1
	for minorEnd < len(value) && value[minorEnd] >= '0' && value[minorEnd] <= '9' {
		minorEnd++
	}
	if minorEnd == majorEnd+1 {
		return xaiGrokVersion{major: major, minor: -1}, true
	}
	minor, err := strconv.Atoi(value[majorEnd+1 : minorEnd])
	if err != nil {
		return xaiGrokVersion{}, false
	}
	return xaiGrokVersion{major: major, minor: minor}, true
}

func xaiCompareGrokVersion(left, right xaiGrokVersion) int {
	if left.major < right.major {
		return -1
	}
	if left.major > right.major {
		return 1
	}
	leftMinor := max(left.minor, 0)
	rightMinor := max(right.minor, 0)
	if leftMinor < rightMinor {
		return -1
	}
	if leftMinor > rightMinor {
		return 1
	}
	return 0
}

func normalizeXAIImageGeneration(body []byte, model string) ([]byte, error) {
	hasImageGeneration := false
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if strings.TrimSpace(tool.Get("type").String()) == xaiImageGenerationToolType {
			hasImageGeneration = true
			break
		}
	}
	if !hasImageGeneration {
		return body, nil
	}
	if !xaiSupportsNativeImageGeneration(model) {
		return nil, statusErr{
			code: http.StatusBadRequest,
			msg:  fmt.Sprintf("xai model %q does not support the image_generation tool", model),
		}
	}
	return normalizeXAIForcedImageGenerationToolChoice(body), nil
}

func normalizeXAIForcedImageGenerationToolChoice(body []byte) []byte {
	return normalizeXAIForcedHostedToolChoice(body, xaiImageGenerationToolType)
}

func normalizeXAIForcedHostedToolChoice(body []byte, toolType string) []byte {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return body
	}
	choiceType := strings.TrimSpace(choice.Get("type").String())
	if choiceType == toolType {
		body = xaiKeepOnlyHostedTools(body, toolType)
		return xaiSetToolChoiceString(body, "required")
	}
	if choiceType != "allowed_tools" {
		return body
	}
	allowed := choice.Get("tools")
	if !allowed.IsArray() {
		return body
	}
	filtered := make([][]byte, 0, len(allowed.Array()))
	stripped := false
	for _, tool := range allowed.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolType {
			stripped = true
			continue
		}
		filtered = append(filtered, []byte(tool.Raw))
	}
	if !stripped {
		return body
	}
	if len(filtered) == 0 {
		mode := strings.TrimSpace(choice.Get("mode").String())
		if mode != "auto" {
			mode = "required"
		}
		body = xaiKeepOnlyHostedTools(body, toolType)
		return xaiSetToolChoiceString(body, mode)
	}
	updated, err := sjson.SetRawBytes(body, "tool_choice.tools", helps.JoinRawJSONArray(filtered))
	if err != nil {
		return body
	}
	return updated
}

func xaiKeepOnlyHostedTools(body []byte, toolType string) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	kept := make([][]byte, 0, 1)
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolType {
			kept = append(kept, []byte(tool.Raw))
		}
	}
	if len(kept) == 0 || len(kept) == len(tools.Array()) {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "tools", helps.JoinRawJSONArray(kept))
	if err != nil {
		return body
	}
	return updated
}

func xaiToolChoiceRequiresHostedToolOnly(body []byte) bool {
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Type != gjson.String || (choice.String() != "required" && choice.String() != "auto") {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return false
	}
	toolType := strings.TrimSpace(tools.Array()[0].Get("type").String())
	if toolType != xaiImageGenerationToolType && toolType != xaiWebSearchToolType {
		return false
	}
	for _, tool := range tools.Array()[1:] {
		if strings.TrimSpace(tool.Get("type").String()) != toolType {
			return false
		}
	}
	return true
}

func xaiSetToolChoiceString(body []byte, value string) []byte {
	updated, err := sjson.SetBytes(body, "tool_choice", value)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeXAIResponsesBody(body []byte, model string, req cliproxyexecutor.Request) []byte {
	body, _ = sjson.DeleteBytes(body, "stop")
	body = removeXAIEncryptedReasoningInclude(body)
	supportsReasoningEffort := xaiSupportsReasoningEffort(model)
	if info, ok := cliproxyauth.ResolvedModelInfo(req); ok {
		supportsReasoningEffort = info.Thinking != nil && len(info.Thinking.Levels) > 0
	}
	if !supportsReasoningEffort {
		if gjson.GetBytes(body, "reasoning.effort").Exists() {
			log.Debugf("xai: stripping reasoning.effort for model %s (no thinking levels in resolved capabilities)", model)
		}
		body, _ = sjson.DeleteBytes(body, "reasoning.effort")
		if reasoning := gjson.GetBytes(
			body, "reasoning",
		); reasoning.Exists() && reasoning.IsObject() && len(reasoning.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "reasoning")
		}
	}
	return body
}

type xaiNamespaceDispatcher struct {
	namespace string
	children  map[string]struct{}
}

type xaiNamespaceDispatcherState struct {
	dispatchers map[string]xaiNamespaceDispatcher
}

type xaiPreparedResponseNormalizer struct {
	identity            *xaiToolIdentityResponseNormalizer
	dispatcher          *xaiNamespaceDispatcherResponseNormalizer
	patch               *translatorcommon.ResponsesPatchBridge
	restorePatchTools   bool
	patchNamespaces     map[string]bool
	patchDispatcherCall map[string]*xaiPatchDispatcherCall
	sourceClosed        bool
	sentinelSeen        bool
	failed              bool
}

type xaiNamespaceDispatcherResponseNormalizer struct {
	state             *xaiNamespaceDispatcherState
	dispatcherItemIDs map[string]string
	terminal          bool
}

func (p *xaiPreparedRequest) newResponseNormalizer() *xaiPreparedResponseNormalizer {
	if p == nil {
		return &xaiPreparedResponseNormalizer{}
	}
	normalizer := &xaiPreparedResponseNormalizer{
		identity: p.toolIdentity.newResponseNormalizer(p.filterInternalXSearch),
		dispatcher: &xaiNamespaceDispatcherResponseNormalizer{
			state:             p.dispatcherState,
			dispatcherItemIDs: make(map[string]string),
		},
		patch:             translatorcommon.NewResponsesPatchBridge(p.patchDeclarations),
		restorePatchTools: p.responseFormat == sdktranslator.FormatOpenAIResponse || p.responseFormat == sdktranslator.FormatCodex,
	}
	if normalizer.patch != nil {
		normalizer.identity.allowDeferredNames = true
		normalizer.patchNamespaces = make(map[string]bool)
		normalizer.patchDispatcherCall = make(map[string]*xaiPatchDispatcherCall)
		for _, declaration := range translatorcommon.NewResponsesToolIdentity(p.patchDeclarations).Declarations() {
			if declaration.Name == "apply_patch" && declaration.Kind == translatorcommon.ResponsesCustomTool {
				normalizer.patchNamespaces[declaration.Namespace] = true
			}
		}
	}
	return normalizer
}

func (n *xaiPreparedResponseNormalizer) normalize(event []byte) [][]byte {
	if n == nil {
		return [][]byte{event}
	}
	if n.failed {
		return nil
	}
	if n.sourceClosed {
		switch gjson.GetBytes(event, "type").String() {
		case "response.failed", "response.error", "error":
			n.failed = true
			return [][]byte{event}
		}
		if !n.sentinelSeen && bytes.Equal(bytes.TrimSpace(event), []byte("[DONE]")) {
			n.sentinelSeen = true
			return [][]byte{event}
		}
		return nil
	}
	patchActive := n.patch != nil
	defer func() {
		if patchActive && (n.sourceClosed || n.failed) {
			n.identity = nil
			n.dispatcher = nil
			n.patch = nil
			n.patchNamespaces = nil
			n.patchDispatcherCall = nil
		}
	}()
	var normalized [][]byte
	for _, item := range n.normalizePatchDispatcher(event) {
		if n.identity != nil {
			item = n.identity.normalize(item)
		}
		if n.dispatcher != nil {
			item = n.dispatcher.normalize(item)
		}
		if len(item) == 0 {
			continue
		}
		items, err := n.patch.Transform(item)
		if err != nil {
			n.failed = true
			n.patchDispatcherCall = nil
			return [][]byte{invalidXAIToolCallEvent(err.Error())}
		}
		if !n.restorePatchTools {
			items = [][]byte{item}
		}
		for _, output := range items {
			kind := gjson.GetBytes(output, "type").String()
			if patchActive && kind == "response.done" {
				output, _ = sjson.SetBytes(output, "type", "response.completed")
				kind = "response.completed"
			}
			if patchActive && (kind == "response.completed" || kind == "response.incomplete") {
				n.sourceClosed = true
			}
			if kind == "response.failed" || kind == "response.error" || kind == "error" {
				n.failed = true
				n.patchDispatcherCall = nil
			}
			if kind == "response.custom_tool_call_input.delta" {
				call := n.patchDispatcherCall["item:"+gjson.GetBytes(output, "item_id").String()]
				if call != nil && call.dispatcher.namespace != "" {
					continue
				}
			}
			normalized = append(normalized, output)
		}
	}
	return normalized
}

func (n *xaiNamespaceDispatcherResponseNormalizer) normalize(event []byte) []byte {
	if n == nil || n.state == nil || len(n.state.dispatchers) == 0 || len(event) == 0 || !gjson.ValidBytes(event) {
		return event
	}
	if n.terminal {
		return nil
	}
	switch gjson.GetBytes(event, "type").String() {
	case "response.output_item.added":
		item := gjson.GetBytes(event, "item")
		if item.Get("type").String() != "function_call" {
			return event
		}
		name := strings.TrimSpace(item.Get("name").String())
		dispatcher, ok := n.state.dispatchers[name]
		if !ok {
			return event
		}
		if itemID := strings.TrimSpace(item.Get("id").String()); itemID != "" {
			n.dispatcherItemIDs[itemID] = dispatcher.namespace
		}
		updated, err := sjson.SetBytes(event, "item.namespace", dispatcher.namespace)
		if err == nil {
			return updated
		}
		return event
	case "response.function_call_arguments.done":
		itemID := strings.TrimSpace(gjson.GetBytes(event, "item_id").String())
		namespace, ok := n.dispatcherItemIDs[itemID]
		if !ok {
			return event
		}
		dispatcher := n.state.dispatchers[namespace]
		_, arguments, ok := unwrapXAINamespaceDispatcherArguments(
			gjson.GetBytes(event, "arguments").String(), dispatcher,
		)
		if !ok {
			n.terminal = true
			return invalidXAIToolCallEvent("xAI returned invalid namespace dispatcher arguments")
		}
		updated, err := sjson.SetBytes(event, "arguments", string(arguments))
		if err == nil {
			return updated
		}
		return event
	default:
		var valid bool
		event, valid = n.restoreAtPath(event, "item")
		if !valid {
			n.terminal = true
			return invalidXAIToolCallEvent("xAI returned an undeclared namespace dispatcher tool call")
		}
		for index := range gjson.GetBytes(event, "response.output").Array() {
			event, valid = n.restoreAtPath(event, fmt.Sprintf("response.output.%d", index))
			if !valid {
				n.terminal = true
				return invalidXAIToolCallEvent("xAI returned an undeclared namespace dispatcher tool call")
			}
		}
		return event
	}
}

func (n *xaiNamespaceDispatcherResponseNormalizer) restoreAtPath(data []byte, path string) ([]byte, bool) {
	if gjson.GetBytes(data, path+".type").String() != "function_call" {
		return data, true
	}
	name := strings.TrimSpace(gjson.GetBytes(data, path+".name").String())
	dispatcher, ok := n.state.dispatchers[name]
	if !ok {
		return data, true
	}
	updated, err := sjson.SetBytes(data, path+".namespace", dispatcher.namespace)
	if err != nil {
		return data, true
	}
	argumentsResult := gjson.GetBytes(data, path+".arguments")
	if !argumentsResult.Exists() || strings.TrimSpace(argumentsResult.String()) == "" {
		return updated, true
	}
	childName, childArguments, valid := unwrapXAINamespaceDispatcherArguments(argumentsResult.String(), dispatcher)
	if !valid {
		return data, false
	}
	updated, err = sjson.SetBytes(updated, path+".name", childName)
	if err != nil {
		return data, true
	}
	updated, err = sjson.SetBytes(updated, path+".arguments", string(childArguments))
	if err != nil {
		return data, true
	}
	return updated, true
}

func unwrapXAINamespaceDispatcherArguments(
	rawArguments string,
	dispatcher xaiNamespaceDispatcher,
) (string, []byte, bool) {
	if !gjson.Valid(rawArguments) {
		return "", nil, false
	}
	arguments := gjson.Parse(rawArguments)
	name := strings.TrimSpace(arguments.Get("name").String())
	if name == "" {
		return "", nil, false
	}
	if _, declared := dispatcher.children[name]; !declared {
		return "", nil, false
	}
	childArguments := []byte("{}")
	if nested := arguments.Get("arguments"); nested.Exists() {
		if nested.Type == gjson.String {
			childArguments = []byte(nested.String())
		} else {
			childArguments = []byte(nested.Raw)
		}
	} else {
		cleaned, err := sjson.DeleteBytes([]byte(rawArguments), "name")
		if err == nil && len(cleaned) > 0 && string(cleaned) != "{}" {
			childArguments = cleaned
		}
	}
	if len(childArguments) == 0 {
		childArguments = []byte("{}")
	}
	return name, childArguments, true
}

func xaiShouldFoldNamespaceTools(body []byte, willInjectXSearch bool) bool {
	count := xaiCountFlattenedTools(gjson.GetBytes(body, "tools"))
	if willInjectXSearch && !xaiRequestHasNativeXSearch(body) {
		count++
	}
	return count > xaiMaxTools
}

func xaiCountFlattenedTools(tools gjson.Result) int {
	if !tools.IsArray() {
		return 0
	}
	count := 0
	for _, tool := range tools.Array() {
		switch strings.TrimSpace(tool.Get("type").String()) {
		case xaiNamespaceToolType:
			count += xaiCountFlattenedTools(tool.Get("tools"))
		case xaiToolSearchType:
		default:
			count++
		}
	}
	return count
}

func foldXAINamespaceTools(body []byte) ([]byte, *xaiNamespaceDispatcherState, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil, nil
	}
	state := &xaiNamespaceDispatcherState{dispatchers: make(map[string]xaiNamespaceDispatcher)}
	folded := make([]json.RawMessage, 0, len(tools.Array()))
	changed := false
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != xaiNamespaceToolType {
			folded = append(folded, json.RawMessage(tool.Raw))
			continue
		}
		dispatcher, ref, err := buildXAINamespaceDispatcher(tool)
		if err != nil {
			return nil, nil, err
		}
		if len(dispatcher) == 0 {
			changed = true
			continue
		}
		folded = append(folded, json.RawMessage(dispatcher))
		state.dispatchers[ref.namespace] = ref
		changed = true
	}
	if !changed {
		return body, nil, nil
	}
	raw, err := json.Marshal(folded)
	if err != nil {
		return nil, nil, fmt.Errorf("xai: marshal namespace dispatchers: %w", err)
	}
	updated, err := sjson.SetRawBytes(body, "tools", raw)
	if err != nil {
		return nil, nil, fmt.Errorf("xai: set namespace dispatchers: %w", err)
	}
	if len(state.dispatchers) == 0 {
		return updated, nil, nil
	}
	return updated, state, nil
}

func buildXAINamespaceDispatcher(tool gjson.Result) ([]byte, xaiNamespaceDispatcher, error) {
	namespace := strings.TrimSpace(tool.Get("name").String())
	ref := xaiNamespaceDispatcher{namespace: namespace, children: make(map[string]struct{})}
	if namespace == "" {
		return nil, ref, nil
	}
	description := strings.TrimSpace(tool.Get("description").String())
	toolNames := make([]string, 0)
	toolDescriptions := make([]string, 0)
	for _, child := range tool.Get("tools").Array() {
		childType := strings.TrimSpace(child.Get("type").String())
		if childType != xaiFunctionToolType && childType != xaiCustomToolType {
			continue
		}
		childName := strings.TrimSpace(child.Get("name").String())
		if childName == "" {
			continue
		}
		ref.children[childName] = struct{}{}
		toolNames = append(toolNames, childName)
		entry := "- " + childName
		if childDescription := child.Get("description").String(); strings.TrimSpace(childDescription) != "" {
			entry += ": " + childDescription
		}
		parameters := child.Get("parameters")
		if !parameters.Exists() {
			parameters = child.Get("input_schema")
		}
		if parameters.IsObject() {
			inlined, _, err := inlineXAILocalRefs([]byte(parameters.Raw))
			if err != nil {
				return nil, ref, fmt.Errorf("xai: namespace %q tool %q: %w", namespace, childName, err)
			}
			inlined, _, err = normalizeXAIObjectFunctionSchema(inlined)
			if err != nil {
				return nil, ref, fmt.Errorf("xai: namespace %q tool %q: %w", namespace, childName, err)
			}
			trimmed := strings.TrimSpace(string(inlined))
			if trimmed != "" && trimmed != "{}" && trimmed != `{"type":"object","properties":{}}` {
				entry += "\n  Parameters: " + trimmed
			}
		}
		toolDescriptions = append(toolDescriptions, entry)
	}
	if len(toolDescriptions) > 0 {
		catalog := "Available tools in this namespace:\n" + strings.Join(toolDescriptions, "\n")
		if description == "" {
			description = fmt.Sprintf("Tools in namespace %s.\n\n%s", namespace, catalog)
		} else {
			description += "\n\n" + catalog
		}
	} else if description == "" {
		description = fmt.Sprintf("Tools in namespace %s.", namespace)
	}
	nameProperty := map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("Child tool name to execute in namespace %s", namespace),
	}
	if len(toolNames) > 0 {
		nameProperty["enum"] = toolNames
	}
	dispatcher := map[string]any{
		"type":        xaiFunctionToolType,
		"name":        namespace,
		"description": description,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": nameProperty,
				"arguments": map[string]any{
					"type":                 "object",
					"description":          "Arguments object matching the selected child tool schema",
					"additionalProperties": true,
				},
			},
			"required": []string{"name"},
		},
	}
	raw, err := json.Marshal(dispatcher)
	if err != nil {
		return nil, ref, fmt.Errorf("xai: marshal namespace %q: %w", namespace, err)
	}
	return raw, ref, nil
}

func (s *xaiNamespaceDispatcherState) registerDeclarations(normalizer *xaiToolIdentityNormalizer) {
	if s == nil || normalizer == nil {
		return
	}
	for name := range s.dispatchers {
		if name == xaiWebSearchToolType {
			normalizer.dispatcherWebSearch = true
		}
		normalizer.clientDeclaredTools[xaiClientToolKey{name: name, toolType: xaiFunctionToolType}] = struct{}{}
	}
}

func (s *xaiNamespaceDispatcherState) normalizeToolChoice(body []byte) []byte {
	if s == nil || len(s.dispatchers) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	normalizeAtPath := func(path string) {
		choice := gjson.GetBytes(body, path)
		if !choice.IsObject() || strings.TrimSpace(choice.Get("type").String()) != xaiFunctionToolType {
			return
		}
		namespace := strings.TrimSpace(choice.Get("namespace").String())
		if _, ok := s.dispatchers[namespace]; !ok {
			return
		}
		updated, err := sjson.SetBytes(body, path+".name", namespace)
		if err != nil {
			return
		}
		updated, err = sjson.DeleteBytes(updated, path+".namespace")
		if err == nil {
			body = updated
		}
	}
	normalizeAtPath("tool_choice")
	for index := range gjson.GetBytes(body, "tool_choice.tools").Array() {
		normalizeAtPath(fmt.Sprintf("tool_choice.tools.%d", index))
	}
	return body
}

func (s *xaiNamespaceDispatcherState) normalizeInputHistory(body []byte) []byte {
	if s == nil || len(s.dispatchers) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	for index, item := range gjson.GetBytes(body, "input").Array() {
		if strings.TrimSpace(item.Get("type").String()) != "function_call" {
			continue
		}
		namespace := strings.TrimSpace(item.Get("namespace").String())
		toolName := strings.TrimSpace(item.Get("name").String())
		dispatcher, ok := s.dispatchers[namespace]
		if !ok {
			continue
		}
		if _, declared := dispatcher.children[toolName]; !declared {
			continue
		}
		dispatcherArguments := map[string]any{"name": toolName}
		if rawArguments := item.Get("arguments").String(); rawArguments != "" {
			if json.Valid([]byte(rawArguments)) {
				dispatcherArguments["arguments"] = json.RawMessage(rawArguments)
			} else {
				dispatcherArguments["arguments"] = rawArguments
			}
		}
		encodedArguments, err := json.Marshal(dispatcherArguments)
		if err != nil {
			continue
		}
		path := fmt.Sprintf("input.%d", index)
		updated, err := sjson.SetBytes(body, path+".name", namespace)
		if err != nil {
			continue
		}
		updated, err = sjson.SetBytes(updated, path+".arguments", string(encodedArguments))
		if err != nil {
			continue
		}
		updated, err = sjson.DeleteBytes(updated, path+".namespace")
		if err == nil {
			body = updated
		}
	}
	return body
}

func clampXAIToolsLimit(body []byte, maxTools int, dispatcherState *xaiNamespaceDispatcherState) []byte {
	tools := gjson.GetBytes(body, "tools")
	if maxTools <= 0 || !tools.IsArray() || len(tools.Array()) <= maxTools {
		return body
	}
	dispatchers := make([]json.RawMessage, 0)
	regular := make([]json.RawMessage, 0)
	for _, tool := range tools.Array() {
		name := strings.TrimSpace(tool.Get("name").String())
		if dispatcherState != nil {
			if _, ok := dispatcherState.dispatchers[name]; ok {
				dispatchers = append(dispatchers, json.RawMessage(tool.Raw))
				continue
			}
		}
		regular = append(regular, json.RawMessage(tool.Raw))
	}
	capped := make([]json.RawMessage, 0, maxTools)
	if len(dispatchers) >= maxTools {
		capped = append(capped, dispatchers[:maxTools]...)
	} else {
		capped = append(capped, dispatchers...)
		remaining := maxTools - len(capped)
		if len(regular) > remaining {
			regular = regular[:remaining]
		}
		capped = append(capped, regular...)
	}
	raw, err := json.Marshal(capped)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "tools", raw)
	if err != nil {
		return body
	}
	updated = pruneXAIOrphanedToolChoice(updated)
	return normalizeXAIToolChoiceForTools(updated)
}

func normalizeXAITools(body []byte) ([]byte, error) {
	return normalizeXAIToolDeclarations(body)
}

func normalizeXAIToolChoiceForTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return body
	}
	if tools.Exists() {
		body, _ = sjson.DeleteBytes(body, "tools")
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
	}
	if gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	}
	return body
}

func inlineXAILocalRefs(schema []byte) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, fmt.Errorf("invalid parameters schema: %w", err)
	}
	resolved, changed, err := resolveXAILocalRefs(root, root, map[string]bool{"#": true})
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return schema, false, nil
	}
	if object, ok := resolved.(map[string]any); ok {
		defs, hasDefs := object["$defs"]
		definitions, hasDefinitions := object["definitions"]
		delete(object, "$defs")
		delete(object, "definitions")
		withoutDefinitions, err := json.Marshal(object)
		if err != nil {
			return nil, false, err
		}
		if bytes.Contains(withoutDefinitions, []byte(`"$ref"`)) ||
			bytes.Contains(withoutDefinitions, []byte(`"$dynamicRef"`)) ||
			bytes.Contains(withoutDefinitions, []byte(`"$recursiveRef"`)) {
			if hasDefs {
				object["$defs"] = defs
			}
			if hasDefinitions {
				object["definitions"] = definitions
			}
		}
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		return nil, false, fmt.Errorf("marshal resolved parameters schema: %w", err)
	}
	return raw, true, nil
}

func resolveXAILocalRefs(root, value any, active map[string]bool) (any, bool, error) {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && (ref == "#" || strings.HasPrefix(ref, "#/")) {
			target, found := lookupXAIJSONPointer(root, ref)
			if !found {
				return nil, false, fmt.Errorf("unresolved local reference %q", ref)
			}
			if active[ref] {
				return typed, false, nil
			}
			active[ref] = true
			resolvedTarget, _, err := resolveXAILocalRefs(root, target, active)
			if err != nil {
				delete(active, ref)
				return nil, false, err
			}
			siblings := make(map[string]any)
			for key, child := range typed {
				if key == "$ref" || key == "$defs" || key == "definitions" {
					continue
				}
				resolvedChild, _, err := resolveXAILocalRefs(root, child, active)
				if err != nil {
					delete(active, ref)
					return nil, false, err
				}
				siblings[key] = resolvedChild
			}
			delete(active, ref)
			resolvedMap, isObject := resolvedTarget.(map[string]any)
			var merged map[string]any
			if isObject {
				merged = make(map[string]any, len(resolvedMap)+len(typed))
				for key, child := range resolvedMap {
					merged[key] = child
				}
			}
			if len(siblings) > 0 {
				if !isObject {
					merged = map[string]any{"allOf": []any{resolvedTarget, siblings}}
				} else {
					allOf, exists := merged["allOf"]
					branches, valid := allOf.([]any)
					if exists && !valid {
						return nil, false, fmt.Errorf("referenced schema allOf must be an array")
					}
					merged["allOf"] = append(append([]any(nil), branches...), siblings)
				}
			}
			if merged != nil {
				for _, key := range []string{"$defs", "definitions"} {
					if definitions, exists := typed[key]; exists {
						merged[key] = definitions
					}
				}
				return merged, true, nil
			}
			return resolvedTarget, true, nil
		}
		out := make(map[string]any, len(typed))
		changed := false
		for key, child := range typed {
			if key == "$defs" || key == "definitions" {
				out[key] = child
				continue
			}
			resolvedChild, childChanged, err := resolveXAILocalRefs(root, child, active)
			if err != nil {
				return nil, false, err
			}
			out[key] = resolvedChild
			changed = changed || childChanged
		}
		return out, changed, nil
	case []any:
		out := make([]any, len(typed))
		changed := false
		for index, child := range typed {
			resolvedChild, childChanged, err := resolveXAILocalRefs(root, child, active)
			if err != nil {
				return nil, false, err
			}
			out[index] = resolvedChild
			changed = changed || childChanged
		}
		return out, changed, nil
	default:
		return value, false, nil
	}
}

func lookupXAIJSONPointer(root any, ref string) (any, bool) {
	if ref == "#" {
		return root, true
	}
	current := root
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var found bool
			current, found = node[segment]
			if !found {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) || strconv.Itoa(index) != segment {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func normalizeXAITool(tool gjson.Result, namespaceName string) ([]byte, bool, error) {
	toolType := tool.Get("type").String()
	changed := false
	if toolType == xaiToolSearchType {
		return nil, true, nil
	}
	if toolType == xaiCustomToolType && tool.Get("name").String() == "apply_patch" {
		return nil, true, nil
	}

	raw := []byte(tool.Raw)
	if toolType == xaiFunctionToolType || toolType == xaiCustomToolType {
		parameters := gjson.GetBytes(raw, "parameters")
		if parameters.IsObject() {
			inlined, refsChanged, err := inlineXAILocalRefs([]byte(parameters.Raw))
			if err != nil {
				return nil, false, fmt.Errorf("xai: inline local refs for tool %q: %w", tool.Get("name").String(), err)
			}
			if refsChanged {
				updated, err := sjson.SetRawBytes(raw, "parameters", inlined)
				if err != nil {
					return nil, false, fmt.Errorf("xai: inline local refs for tool %q: %w", tool.Get("name").String(), err)
				}
				raw = updated
				changed = true
			}
		}
		var schemaChanged bool
		var err error
		raw, schemaChanged, err = normalizeXAIFunctionParameters(raw, namespaceName, tool.Get("name").String())
		if err != nil {
			return nil, false, err
		}
		changed = changed || schemaChanged
	}
	if toolType == xaiCustomToolType {
		updatedTool, err := sjson.SetBytes(raw, "type", xaiFunctionToolType)
		if err != nil {
			return nil, false, fmt.Errorf("xai: normalize custom tool %q: %w", tool.Get("name").String(), err)
		}
		raw = updatedTool
		toolType = xaiFunctionToolType
		changed = true
	}
	if toolType == xaiWebSearchToolType && tool.Get("external_web_access").Exists() {
		updatedTool, err := sjson.DeleteBytes(raw, "external_web_access")
		if err != nil {
			return nil, false, fmt.Errorf("xai: normalize web search tool: %w", err)
		}
		raw = updatedTool
		changed = true
	}
	if toolType == xaiFunctionToolType && strings.TrimSpace(namespaceName) != "" {
		qualifiedName := qualifyXAINamespaceToolName(namespaceName, tool.Get("name").String())
		if qualifiedName == "" {
			return nil, false, fmt.Errorf("xai: function tool in namespace %q has no name", namespaceName)
		}
		updatedTool, err := sjson.SetBytes(raw, "name", qualifiedName)
		if err != nil {
			return nil, false, fmt.Errorf("xai: qualify function tool %q: %w", tool.Get("name").String(), err)
		}
		raw = updatedTool
		changed = true
	}
	return raw, changed, nil
}

func qualifyXAINamespaceToolName(namespaceName, toolName string) string {
	namespaceName = strings.TrimSpace(namespaceName)
	toolName = strings.TrimSpace(toolName)
	if namespaceName == "" || toolName == "" || strings.HasPrefix(toolName, "mcp__") {
		return toolName
	}
	prefix := strings.TrimSuffix(namespaceName, "__") + "__"
	if strings.HasPrefix(toolName, prefix) {
		return toolName
	}
	return prefix + toolName
}

func normalizeXAIFunctionParameters(tool []byte, namespaceName, toolName string) ([]byte, bool, error) {
	parameters := gjson.GetBytes(tool, "parameters")
	qualifiedName := qualifyXAINamespaceToolName(namespaceName, toolName)
	if qualifiedName == "" {
		qualifiedName = strings.TrimSpace(toolName)
	}
	if !parameters.Exists() || !parameters.IsObject() {
		return nil, false, fmt.Errorf("xai: function tool %q requires an object parameters schema", qualifiedName)
	}

	normalized, changed, err := normalizeXAIObjectFunctionSchema([]byte(parameters.Raw))
	if err != nil {
		return nil, false, fmt.Errorf("xai: function tool %q: %w", qualifiedName, err)
	}
	if !changed {
		return tool, false, nil
	}
	updated, err := sjson.SetRawBytes(tool, "parameters", normalized)
	if err != nil {
		return nil, false, fmt.Errorf("xai: function tool %q: set normalized parameters: %w", qualifiedName, err)
	}
	return updated, true, nil
}

func normalizeXAIObjectFunctionSchema(schema []byte) ([]byte, bool, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, false, fmt.Errorf("invalid parameters schema: %w", err)
	}

	changed := false
	rootType, hasRootType := root["type"]
	if hasRootType {
		normalizedType, objectAllowed, typeChanged := normalizeXAIObjectSchemaType(rootType)
		if !objectAllowed {
			return nil, false, fmt.Errorf("parameters schema has no valid object type")
		}
		if typeChanged {
			root["type"] = normalizedType
			changed = true
		}
	}

	unionFound := false
	for _, unionName := range []string{"anyOf", "oneOf"} {
		rawUnion, exists := root[unionName]
		if !exists {
			continue
		}
		branches, ok := rawUnion.([]any)
		if !ok || len(branches) == 0 {
			return nil, false, fmt.Errorf("parameters.%s must be a non-empty array", unionName)
		}
		unionFound = true
		objectBranches := make([]any, 0, len(branches))
		for _, branchValue := range branches {
			branch, ok := branchValue.(map[string]any)
			if !ok {
				continue
			}
			normalizedBranch, validObject, branchChanged := normalizeXAIObjectSchemaBranch(branch)
			if !validObject {
				continue
			}
			objectBranches = append(objectBranches, normalizedBranch)
			changed = changed || branchChanged
		}
		if len(objectBranches) == 0 {
			return nil, false, fmt.Errorf("parameters.%s has no valid object branch", unionName)
		}
		if len(objectBranches) != len(branches) {
			root[unionName] = objectBranches
			changed = true
		}
	}

	if !hasRootType {
		if unionFound || xaiSchemaUsesObjectKeywords(root) {
			root["type"] = "object"
			changed = true
		} else {
			return nil, false, fmt.Errorf("parameters schema has no valid object branch")
		}
	}
	if !changed {
		return schema, false, nil
	}
	normalized, err := json.Marshal(root)
	if err != nil {
		return nil, false, fmt.Errorf("marshal normalized parameters schema: %w", err)
	}
	return normalized, true, nil
}

func normalizeXAIObjectSchemaBranch(branch map[string]any) (map[string]any, bool, bool) {
	branchType, hasType := branch["type"]
	changed := false
	if hasType {
		normalizedType, objectAllowed, typeChanged := normalizeXAIObjectSchemaType(branchType)
		if !objectAllowed {
			return nil, false, false
		}
		if typeChanged {
			branch["type"] = normalizedType
			changed = true
		}
	}
	unionFound := false
	for _, unionName := range []string{"anyOf", "oneOf"} {
		rawUnion, exists := branch[unionName]
		if !exists {
			continue
		}
		branches, ok := rawUnion.([]any)
		if !ok || len(branches) == 0 {
			return nil, false, false
		}
		unionFound = true
		objectBranches := make([]any, 0, len(branches))
		for _, value := range branches {
			child, ok := value.(map[string]any)
			if !ok {
				continue
			}
			normalized, valid, childChanged := normalizeXAIObjectSchemaBranch(child)
			if valid {
				objectBranches = append(objectBranches, normalized)
				changed = changed || childChanged
			}
		}
		if len(objectBranches) == 0 {
			return nil, false, false
		}
		if len(objectBranches) != len(branches) {
			branch[unionName] = objectBranches
			changed = true
		}
	}
	if !hasType {
		if !unionFound && !xaiSchemaUsesObjectKeywords(branch) {
			return nil, false, false
		}
		branch["type"] = "object"
		changed = true
	}
	return branch, true, changed
}

func normalizeXAIObjectSchemaType(schemaType any) (string, bool, bool) {
	switch typed := schemaType.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "object") {
			return "object", true, typed != "object"
		}
		return "", false, false
	case []any:
		objectAllowed := false
		for _, item := range typed {
			value, ok := item.(string)
			if ok && strings.EqualFold(strings.TrimSpace(value), "object") {
				objectAllowed = true
			}
		}
		return "object", objectAllowed, objectAllowed
	default:
		return "", false, false
	}
}

func xaiSchemaUsesObjectKeywords(schema map[string]any) bool {
	for _, key := range []string{
		"properties", "required", "additionalProperties", "patternProperties", "propertyNames", "minProperties",
		"maxProperties", "dependentRequired", "dependentSchemas",
	} {
		if _, exists := schema[key]; exists {
			return true
		}
	}
	return false
}

func normalizeXAIInputReasoningItems(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}

	updated := body
	for i, item := range input.Array() {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		contentPath := fmt.Sprintf("input.%d.content", i)
		if content := gjson.GetBytes(updated, contentPath); content.Exists() && content.Type == gjson.Null {
			updatedBody, errDel := sjson.DeleteBytes(updated, contentPath)
			if errDel != nil {
				return body
			}
			updated = updatedBody
		}
		encryptedContentPath := fmt.Sprintf("input.%d.encrypted_content", i)
		if encryptedContent := gjson.GetBytes(
			updated, encryptedContentPath,
		); encryptedContent.Exists() && encryptedContent.Type == gjson.Null {
			updatedBody, errDel := sjson.DeleteBytes(updated, encryptedContentPath)
			if errDel != nil {
				return body
			}
			updated = updatedBody
		}
	}
	return mergeAdjacentXAIInputReasoningSummaries(updated)
}

func mergeAdjacentXAIInputReasoningSummaries(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}

	changed := false
	items := make([]json.RawMessage, 0, len(input.Array()))
	for _, item := range input.Array() {
		if len(items) > 0 && canMergeXAIReasoningSummary(items[len(items)-1], item) {
			merged, ok := appendXAIReasoningSummary(items[len(items)-1], item.Get("summary").Array())
			if ok {
				items[len(items)-1] = merged
				changed = true
				continue
			}
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	if !changed {
		return body
	}

	rawInput, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "input", rawInput)
	if errSet != nil {
		return body
	}
	return updated
}

func canMergeXAIReasoningSummary(previous json.RawMessage, current gjson.Result) bool {
	previousItem := gjson.ParseBytes(previous)
	if previousItem.Get("type").String() != "reasoning" || current.Get("type").String() != "reasoning" {
		return false
	}
	if !previousItem.Get("summary").IsArray() || !current.Get("summary").IsArray() {
		return false
	}
	if len(current.Get("summary").Array()) == 0 {
		return false
	}
	for name := range current.Map() {
		if name != "type" && name != "summary" {
			return false
		}
	}
	return true
}

func appendXAIReasoningSummary(previous json.RawMessage, currentSummary []gjson.Result) ([]byte, bool) {
	updated := []byte(previous)
	summary := gjson.GetBytes(updated, "summary")
	if !summary.IsArray() {
		return previous, false
	}
	nextIndex := len(summary.Array())
	for i, item := range currentSummary {
		updatedItem, errSet := sjson.SetRawBytes(updated, fmt.Sprintf("summary.%d", nextIndex+i), []byte(item.Raw))
		if errSet != nil {
			return previous, false
		}
		updated = updatedItem
	}
	return updated, true
}

func removeXAIEncryptedReasoningInclude(body []byte) []byte {
	include := gjson.GetBytes(body, "include")
	if !include.Exists() || !include.IsArray() {
		return body
	}
	kept := make([]string, 0, len(include.Array()))
	for _, item := range include.Array() {
		value := strings.TrimSpace(item.String())
		if value == "" || value == "reasoning.encrypted_content" {
			continue
		}
		kept = append(kept, value)
	}
	body, _ = sjson.SetBytes(body, "include", kept)
	return body
}

func xaiSupportsReasoningEffort(model string) bool {
	name := strings.ToLower(strings.TrimSpace(thinking.ParseSuffix(model).ModelName))
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if name == "" {
		return false
	}
	info := registry.LookupModelInfo(name, "xai")
	if info == nil || info.Thinking == nil {
		return false
	}
	return len(info.Thinking.Levels) > 0
}

func xaiCollectOutputItemDone(eventData []byte, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) {
	itemResult := gjson.GetBytes(eventData, "item")
	if !itemResult.Exists() || itemResult.Type != gjson.JSON {
		return
	}
	outputIndexResult := gjson.GetBytes(eventData, "output_index")
	if outputIndexResult.Exists() {
		outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
		return
	}
	*outputItemsFallback = append(*outputItemsFallback, []byte(itemResult.Raw))
}

func xaiPatchCompletedOutput(
	eventData []byte,
	outputItemsByIndex map[int64][]byte,
	outputItemsFallback [][]byte,
) []byte {
	if len(outputItemsByIndex) == 0 && len(outputItemsFallback) == 0 {
		return eventData
	}
	indexes := make([]int64, 0, len(outputItemsByIndex))
	for index := range outputItemsByIndex {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	items := make([][]byte, 0, len(indexes)+len(outputItemsFallback))
	for _, index := range indexes {
		items = append(items, outputItemsByIndex[index])
	}
	items = append(items, outputItemsFallback...)

	identities := func(item gjson.Result) [2]string {
		var keys [2]string
		if id := item.Get("id"); id.Type == gjson.String && id.String() != "" {
			keys[0] = "item:" + id.String()
		}
		if callID := item.Get("call_id"); callID.Type == gjson.String && callID.String() != "" {
			keys[1] = "call:" + callID.String()
		}
		return keys
	}
	output := gjson.GetBytes(eventData, "response.output").Array()
	positions := make(map[string]int, len(items))
	indexed := items[:0]
	for _, item := range items {
		keys := identities(gjson.ParseBytes(item))
		if len(output) > 0 && keys == [2]string{} {
			continue
		}
		for _, key := range keys {
			if key != "" {
				positions[key] = len(indexed)
			}
		}
		indexed = append(indexed, item)
	}
	items = indexed
	seen := make(map[int]bool, len(items))
	var additional [][]byte
	for _, item := range output {
		position, exists := 0, false
		for _, key := range identities(item) {
			if position, exists = positions[key]; exists {
				break
			}
		}
		if exists {
			items[position] = []byte(item.Raw)
			seen[position] = true
		} else {
			additional = append(additional, []byte(item.Raw))
		}
	}
	if len(seen) == len(items) {
		return eventData
	}
	items = append(items, additional...)
	patched, _ := sjson.SetRawBytes(eventData, "response.output", translatorcommon.JoinRawArray(items))
	return patched
}

const xaiFreeUsageExhaustedCooldown = 24 * time.Hour

func xaiStatusErr(code int, body []byte, headers ...http.Header) statusErr {
	err := statusErr{code: code, msg: string(body)}
	if len(headers) > 0 {
		err = err.withUpstreamResponse(headers[0])
	}
	if len(body) == 0 {
		return err
	}
	if code == http.StatusForbidden && isXAIBadCredentialsBody(body) {
		err.code = http.StatusUnauthorized
		return err
	}
	if code == http.StatusForbidden && isXAIContentSafetyBody(body) {
		err.requestScoped = true
		return err
	}
	if code != http.StatusTooManyRequests {
		return err
	}
	codeText := strings.ToLower(gjson.GetBytes(body, "code").String())
	if codeText == "" {
		codeText = strings.ToLower(gjson.GetBytes(body, "error.code").String())
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.ToLower(gjson.GetBytes(body, "error").String())
	}
	if message == "" {
		message = strings.ToLower(gjson.GetBytes(body, "message").String())
	}
	if message == "" {
		message = strings.ToLower(string(body))
	}
	if strings.Contains(codeText, "free-usage-exhausted") ||
		strings.Contains(message, "free-usage-exhausted") ||
		strings.Contains(message, "included free usage") {
		cooldown := xaiFreeUsageExhaustedCooldown
		err.retryAfter = &cooldown
	}
	return err
}

func isXAIBadCredentialsBody(body []byte) bool {
	for _, path := range []string{"code", "error.code", "body.error.code"} {
		if strings.Contains(strings.ToLower(gjson.GetBytes(body, path).String()), "bad-credentials") {
			return true
		}
	}
	for _, path := range []string{"error", "error.message", "message", "body.error", "body.error.message"} {
		if strings.Contains(
			strings.ToLower(gjson.GetBytes(body, path).String()),
			"access token could not be validated",
		) {
			return true
		}
	}
	raw := strings.ToLower(string(body))
	return strings.Contains(raw, "bad-credentials") ||
		strings.Contains(raw, "access token could not be validated")
}

func isXAIContentSafetyBody(body []byte) bool {
	raw := strings.ToLower(string(body))
	return strings.Contains(raw, "content violates usage guidelines") ||
		strings.Contains(raw, "safety_check_type_")
}
