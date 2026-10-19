package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
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
	"github.com/tiktoken-go/tokenizer"
	"golang.org/x/sync/singleflight"
)

const metaUserAgent = "muse-build/1.3.0 (interactive; macos-aarch64; build ac7280f2aca67769d1455a8847bb502b617d50f6)"

var metaMintGroup singleflight.Group

var (
	_ cliproxyauth.ProviderExecutor    = (*MetaExecutor)(nil)
	_ cliproxyauth.RequestAuthPreparer = (*MetaExecutor)(nil)
)

// MetaExecutor executes requests against the Muse Responses endpoint.
type MetaExecutor struct {
	cfg *config.Config
}

func NewMetaExecutor(cfg *config.Config) *MetaExecutor {
	return &MetaExecutor{cfg: cfg}
}

func (e *MetaExecutor) Identifier() string {
	return "meta"
}

func (e *MetaExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return fmt.Errorf("meta executor: request is nil")
	}
	_, key := metaCredentials(auth)
	if key == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "meta executor: missing API key"}
	}
	util.ApplyCredentialHeader(req, "Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", metaUserAgent)
	req.Header.Set("X-Client-Id", "tbh:tui")
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

func (e *MetaExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("meta executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	preparedAuth, err := e.ensureAuth(ctx, auth)
	if err != nil {
		return nil, err
	}
	httpReq := req.WithContext(ctx)
	if err = e.PrepareRequest(httpReq, preparedAuth); err != nil {
		return nil, err
	}
	return newProxyAwareHTTPClient(ctx, e.cfg, preparedAuth, 0).Do(httpReq)
}

func (e *MetaExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || cliproxyauth.IsConfigAPIKeyAuth(auth) {
		return false
	}
	_, key := metaCredentials(auth)
	return key == "" && metaDCAToken(auth) != ""
}

func (e *MetaExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	return e.Refresh(ctx, auth)
}

func (e *MetaExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := refreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "meta executor: missing auth"}
	}
	dcaToken := metaDCAToken(auth)
	if dcaToken == "" {
		if _, key := metaCredentials(auth); key != "" {
			return auth, nil
		}
		return nil, statusErr{code: http.StatusUnauthorized, msg: "meta executor: missing API key or DCA token"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mintedValue, err, _ := metaMintGroup.Do(dcaToken, func() (any, error) {
		return metaauth.NewMetaAuthWithProxyURL(e.cfg, auth.ProxyURL).MintAPIKey(ctx, dcaToken)
	})
	if err != nil {
		return nil, fmt.Errorf("meta executor: API key mint failed: %w", err)
	}
	minted, ok := mintedValue.(*metaauth.MintedKeyResponse)
	if !ok || minted == nil || !usableMetaAPIKey(minted.APIKey) {
		return nil, fmt.Errorf("meta executor: API key mint returned an invalid key")
	}
	updated := auth.Clone()
	baseURL, _ := metaCredentials(updated)
	if value := strings.TrimSpace(minted.BaseURL); value != "" {
		baseURL = value
	}
	if updated.Attributes == nil {
		updated.Attributes = make(map[string]string)
	}
	updated.Attributes["api_key"] = minted.APIKey
	updated.Attributes["access_token"] = minted.APIKey
	updated.Attributes["base_url"] = baseURL
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["type"] = "meta"
	updated.Metadata["api_key"] = minted.APIKey
	updated.Metadata["access_token"] = minted.APIKey
	updated.Metadata["dca_token"] = dcaToken
	updated.Metadata["base_url"] = baseURL
	updated.Metadata["subs_tier_name"] = minted.SubsTierName
	updated.Metadata["subs_tier_id"] = minted.SubsTierID
	updated.Metadata["is_subs_active"] = minted.IsSubsActive
	updated.Metadata["has_payment_method"] = minted.HasPaymentMethod
	delete(updated.Metadata, "expired")
	if minted.UserEmail != "" {
		updated.Metadata["email"] = minted.UserEmail
	}
	if minted.UserFullName != "" {
		updated.Metadata["name"] = minted.UserFullName
	}
	refreshedAt := time.Now()
	updated.Metadata["last_refresh"] = refreshedAt.UTC().Format(time.RFC3339)
	if storage, ok := auth.Storage.(*metaauth.MetaTokenStorage); ok && storage != nil {
		copyStorage := *storage
		copyStorage.APIKey = minted.APIKey
		copyStorage.AccessToken = minted.APIKey
		copyStorage.DCAToken = dcaToken
		copyStorage.BaseURL = baseURL
		copyStorage.Expired = ""
		copyStorage.LastRefresh = refreshedAt.UTC().Format(time.RFC3339)
		if minted.UserEmail != "" {
			copyStorage.Email = minted.UserEmail
		}
		if minted.UserFullName != "" {
			copyStorage.Name = minted.UserFullName
		}
		copyStorage.Metadata = updated.Metadata
		updated.Storage = &copyStorage
	}
	updated.LastRefreshedAt = refreshedAt
	return updated, nil
}

func (e *MetaExecutor) ensureAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "meta executor: missing auth"}
	}
	_, key := metaCredentials(auth)
	if key == "" && e.ShouldPrepareRequestAuth(auth) {
		refreshed, err := e.Refresh(ctx, auth)
		if err != nil {
			return nil, err
		}
		auth = refreshed
		_, key = metaCredentials(auth)
	}
	if key == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "meta executor: Meta OAuth requires a minted API key; config requires an API key"}
	}
	return auth, nil
}

func usableMetaAPIKey(key string) bool {
	key = strings.TrimSpace(key)
	return key != "" && !strings.HasPrefix(strings.ToLower(key), "dca:")
}

func metaDCAToken(auth *cliproxyauth.Auth) string {
	if auth == nil || cliproxyauth.IsConfigAPIKeyAuth(auth) {
		return ""
	}
	if auth.Attributes != nil {
		if token := strings.TrimSpace(auth.Attributes["dca_token"]); token != "" {
			return token
		}
		if token := strings.TrimSpace(auth.Attributes["access_token"]); strings.HasPrefix(token, "dca:") {
			return token
		}
	}
	if auth.Metadata != nil {
		if token, ok := auth.Metadata["dca_token"].(string); ok && strings.TrimSpace(token) != "" {
			return strings.TrimSpace(token)
		}
		if token, ok := auth.Metadata["access_token"].(string); ok && strings.HasPrefix(strings.TrimSpace(token), "dca:") {
			return strings.TrimSpace(token)
		}
	}
	if storage, ok := auth.Storage.(*metaauth.MetaTokenStorage); ok && storage != nil {
		if storage.DCAToken != "" {
			return storage.DCAToken
		}
		if strings.HasPrefix(storage.AccessToken, "dca:") {
			return storage.AccessToken
		}
	}
	return ""
}

func metaCredentials(auth *cliproxyauth.Auth) (baseURL, key string) {
	baseURL = metaauth.DefaultAPIBaseURL
	if auth == nil {
		return baseURL, ""
	}
	dcaToken := metaDCAToken(auth)
	usableKey := func(value string) bool {
		return usableMetaAPIKey(value) && strings.TrimSpace(value) != dcaToken
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			baseURL = value
		}
		if value := strings.TrimSpace(auth.Attributes["api_key"]); usableKey(value) {
			key = value
		} else if value := strings.TrimSpace(auth.Attributes["access_token"]); !cliproxyauth.IsConfigAPIKeyAuth(auth) && usableKey(value) {
			key = value
		}
	}
	if auth.Metadata != nil {
		if baseURL == metaauth.DefaultAPIBaseURL {
			if value, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(value) != "" {
				baseURL = strings.TrimSpace(value)
			} else if value, ok := auth.Metadata["api_base_url"].(string); ok && strings.TrimSpace(value) != "" {
				baseURL = strings.TrimSpace(value)
			}
		}
		if key == "" {
			if value, ok := auth.Metadata["api_key"].(string); ok && usableKey(value) {
				key = strings.TrimSpace(value)
			} else if value, ok := auth.Metadata["access_token"].(string); ok && !cliproxyauth.IsConfigAPIKeyAuth(auth) && usableKey(value) {
				key = strings.TrimSpace(value)
			}
		}
	}
	if storage, ok := auth.Storage.(*metaauth.MetaTokenStorage); ok && storage != nil {
		if baseURL == metaauth.DefaultAPIBaseURL && storage.BaseURL != "" {
			baseURL = strings.TrimSpace(storage.BaseURL)
		}
		if key == "" {
			if usableKey(storage.APIKey) {
				key = strings.TrimSpace(storage.APIKey)
			} else if !cliproxyauth.IsConfigAPIKeyAuth(auth) && usableKey(storage.AccessToken) {
				key = strings.TrimSpace(storage.AccessToken)
			}
		}
	}
	return baseURL, key
}

type metaPreparedRequest struct {
	baseModel       string
	from            sdktranslator.Format
	responseFormat  sdktranslator.Format
	to              sdktranslator.Format
	originalPayload []byte
	body            []byte
}

func (e *MetaExecutor) prepareResponsesRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*metaPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalSource = opts.OriginalRequest
	}
	isCompat := helps.APIKeyModelIsCompat(req)
	originalPayload := bytes.Clone(originalSource)
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, bytes.Clone(req.Payload), stream, isCompat,
	)
	if err != nil {
		return nil, err
	}
	body, err = helps.ApplyThinkingWithSourcePayload(body, req.Payload, originalSource, req.Model, from.String(), to.String(), e.Identifier(), req)
	if err != nil {
		return nil, err
	}
	body = applyPayloadConfigWithRequest(e.cfg, baseModel, e.Identifier(), from.String(), "", body, originalTranslated, payloadRequestedModel(opts, req.Model), payloadRequestPath(opts), opts.Headers, e.Identifier())
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.SetBoolIfDifferent(body, "stream", stream)
	for _, field := range []string{"generate", "prompt_cache_retention", "safety_identifier", "stream_options", "client_metadata"} {
		body, err = sjson.DeleteBytes(body, field)
		if err != nil {
			return nil, fmt.Errorf("meta executor: remove unsupported field %s: %w", field, err)
		}
	}
	body = normalizeCodexInstructions(body)
	body = sanitizeOpenAIResponsesReasoningEncryptedContentKeepForeign(ctx, e.Identifier(), body)
	body, err = sanitizeMetaWebSearchTools(body)
	if err != nil {
		return nil, err
	}
	return &metaPreparedRequest{
		baseModel:       baseModel,
		from:            from,
		responseFormat:  cliproxyexecutor.ResponseFormatOrSource(opts),
		to:              to,
		originalPayload: originalPayload,
		body:            body,
	}, nil
}

func sanitizeMetaWebSearchTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}
	for index, tool := range tools.Array() {
		if tool.Get("type").String() == "web_search" && tool.Get("search_content_types").Exists() {
			var err error
			body, err = sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.search_content_types", index))
			if err != nil {
				return nil, fmt.Errorf("meta executor: sanitize web search tool: %w", err)
			}
		}
		if tool.Get("type").String() == "namespace" {
			for subIndex, subtool := range tool.Get("tools").Array() {
				if subtool.Get("type").String() == "web_search" && subtool.Get("search_content_types").Exists() {
					var err error
					body, err = sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.tools.%d.search_content_types", index, subIndex))
					if err != nil {
						return nil, fmt.Errorf("meta executor: sanitize namespace web search tool: %w", err)
					}
				}
			}
		}
	}
	return body, nil
}

func (e *MetaExecutor) request(ctx context.Context, auth *cliproxyauth.Auth, prepared *metaPreparedRequest, opts cliproxyexecutor.Options, reporter *usageReporter) (*http.Response, error) {
	baseURL, key := metaCredentials(auth)
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	if opts.Headers != nil && auth != nil {
		util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes, opts.Headers)
	}
	util.ApplyCredentialHeader(httpReq, "Authorization", "Bearer "+key)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), prepared.body, e.Identifier(), auth)
	httpClient := reporter.trackHTTPClient(newProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	return httpClient.Do(httpReq)
}

func (e *MetaExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "meta executor: response compaction is unsupported"}
	}
	auth, err = e.ensureAuth(ctx, auth)
	if err != nil {
		return resp, err
	}
	prepared, err := e.prepareResponsesRequest(ctx, req, opts, true)
	if err != nil {
		return resp, err
	}
	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	var observedUsage streamUsageBuffer
	defer func() {
		if err != nil {
			reporter.publishFailureWithDetail(ctx, observedUsage.detail, err)
		}
	}()
	reporter.setTranslatedReasoningEffort(prepared.body, prepared.to.String())
	httpResp, err := e.request(ctx, auth, prepared, opts, reporter)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("meta executor: close response body: %v", errClose)
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
		return resp, metaResponseError(httpResp.StatusCode, data, httpResp.Header)
	}
	completed, err := metaCompletedEvent(data, reporter, &observedUsage)
	if err != nil {
		return resp, err
	}
	var param any
	payload := sdktranslator.TranslateNonStream(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, completed, &param)
	if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
		return resp, markResponseStarted(statusErr{code: http.StatusBadGateway, msg: errTranslate.Error()})
	}
	if len(payload) == 0 {
		return resp, markResponseStarted(fmt.Errorf("meta executor: response translation produced no content"))
	}
	if errContext := scannerOrContextError(ctx, nil); errContext != nil {
		return resp, errContext
	}
	reporter.publish(ctx, observedUsage.detail)
	return cliproxyexecutor.Response{Payload: payload, Headers: httpResp.Header.Clone()}, nil
}

func metaCompletedEvent(data []byte, reporter *usageReporter, observedUsage *streamUsageBuffer) ([]byte, error) {
	outputItems := make(map[int64][]byte)
	var fallback [][]byte
	for len(data) > 0 {
		frameLen := sseFrameLen(data)
		if frameLen == 0 {
			break
		}
		frame := data[:frameLen]
		data = data[frameLen:]
		payload, hasData, valid := sseDataPayload(frame)
		if !hasData {
			continue
		}
		if !valid {
			return nil, markResponseStarted(fmt.Errorf("meta executor: malformed stream event"))
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		reporter.observeResponseModel(payload)
		if detail, ok := parseCodexUsage(payload); ok {
			observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
		}
		if err := metaEventError(payload); err != nil {
			return nil, err
		}
		switch gjson.GetBytes(payload, "type").String() {
		case "response.output_item.done":
			xaiCollectOutputItemDone(payload, outputItems, &fallback)
		case "response.completed", "response.incomplete":
			return patchCodexCompletedOutput(payload, outputItems, fallback), nil
		}
	}
	if len(bytes.TrimSpace(data)) > 0 && bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		payload := bytes.TrimSpace(data)
		reporter.observeResponseModel(payload)
		if gjson.ValidBytes(payload) {
			if openAIStyleUsageTokenFieldsExist(gjson.GetBytes(payload, "usage")) {
				detail := parseOpenAIUsage(payload)
				observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
			} else if detail, ok := parseCodexUsage(payload); ok {
				observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
			}
			if err := metaEventError(payload); err != nil {
				return nil, err
			}
			switch gjson.GetBytes(payload, "type").String() {
			case "response.completed", "response.incomplete":
				return patchCodexCompletedOutput(payload, outputItems, fallback), nil
			}
			if gjson.GetBytes(payload, "object").String() == "response" || gjson.GetBytes(payload, "output").Exists() {
				wrapped, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", payload)
				if err != nil {
					return nil, fmt.Errorf("meta executor: wrap response: %w", err)
				}
				return patchCodexCompletedOutput(wrapped, outputItems, fallback), nil
			}
		}
	}
	return nil, markResponseStarted(fmt.Errorf("meta executor: stream ended without a completed response"))
}

func (e *MetaExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "meta executor: response compaction is unsupported"}
	}
	auth, err = e.ensureAuth(ctx, auth)
	if err != nil {
		return nil, err
	}
	prepared, err := e.prepareResponsesRequest(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := newUsageReporter(ctx, e, prepared.baseModel, auth)
	reporter.markStream()
	defer reporter.trackFailure(ctx, &err)
	reporter.setTranslatedReasoningEffort(prepared.body, prepared.to.String())
	httpResp, err := e.request(ctx, auth, prepared, opts, reporter)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := readXAIResponseBody(httpResp.Body, maxXAIResponseBodyBytes)
		if closeErr := httpResp.Body.Close(); closeErr != nil {
			log.Errorf("meta executor: close response body: %v", closeErr)
		}
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		return nil, metaResponseError(httpResp.StatusCode, data, httpResp.Header)
	}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer func() {
			if closeErr := httpResp.Body.Close(); closeErr != nil {
				log.Errorf("meta executor: close response body: %v", closeErr)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		scanner.Split(scanSSEFrame)
		var param any
		var observedUsage streamUsageBuffer
		outputItems := make(map[int64][]byte)
		var fallback [][]byte
		for scanner.Scan() {
			frame := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, frame)
			if !isCompleteSSEFrame(frame) {
				sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, fmt.Errorf("meta executor: incomplete stream event"))
				return
			}
			payload, hasData, valid := sseDataPayload(frame)
			terminal := false
			if hasData {
				if !valid {
					sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, fmt.Errorf("meta executor: malformed stream event"))
					return
				}
				if !bytes.Equal(payload, []byte("[DONE]")) {
					observeResponseTokenEvent(ctx, payload)
					reporter.observeResponseModel(payload)
					if detail, ok := parseCodexUsage(payload); ok {
						observedUsage.observe(detail, true, detail.ResponseServiceTier != "")
					}
					if eventErr := metaEventError(payload); eventErr != nil {
						sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, eventErr)
						return
					}
					switch gjson.GetBytes(payload, "type").String() {
					case "response.output_item.done":
						xaiCollectOutputItemDone(payload, outputItems, &fallback)
					case "response.completed", "response.incomplete":
						terminal = true
						payload = patchCodexCompletedOutput(payload, outputItems, fallback)
						frame = encodeSSEDataFrame(payload)
					}
				}
			}
			translated := sdktranslator.TranslateStream(ctx, prepared.to, prepared.responseFormat, req.Model, prepared.originalPayload, prepared.body, frame, &param)
			if errTranslate := sdktranslator.ResponseTranslationError(&param); errTranslate != nil {
				sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, statusErr{code: http.StatusBadGateway, msg: errTranslate.Error()})
				return
			}
			if !sendStreamPayloads(ctx, chunks, translated) {
				reporter.publishFailureWithDetail(ctx, observedUsage.detail, ctx.Err())
				return
			}
			if terminal {
				if errContext := scannerOrContextError(ctx, nil); errContext != nil {
					sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, errContext)
					return
				}
				reporter.publish(ctx, observedUsage.detail)
				return
			}
		}
		streamErr := scannerOrContextError(ctx, scanner)
		if streamErr == nil {
			streamErr = fmt.Errorf("meta executor: stream ended without a completed response")
		}
		sendMetaStreamError(ctx, chunks, reporter, e.cfg, &observedUsage, streamErr)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: chunks}, nil
}

func sendMetaStreamError(ctx context.Context, chunks chan<- cliproxyexecutor.StreamChunk, reporter *usageReporter, cfg *config.Config, observedUsage *streamUsageBuffer, err error) {
	err = markResponseStarted(err)
	recordAPIResponseError(ctx, cfg, err)
	reporter.publishFailureWithDetail(ctx, observedUsage.detail, err)
	_ = sendStreamChunk(ctx, chunks, cliproxyexecutor.StreamChunk{Err: err})
}

func (e *MetaExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if _, err := e.ensureAuth(ctx, auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	prepared, err := e.prepareResponsesRequest(ctx, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("meta executor: token codec: %w", err)
	}
	count, err := countCodexInputTokens(enc, prepared.body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("meta executor: count input: %w", err)
	}
	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	out := sdktranslator.TranslateTokenCount(ctx, prepared.to, prepared.responseFormat, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: out}, nil
}

func metaResponseError(status int, body []byte, headers http.Header) error {
	err := newUpstreamStatusErrMessage(status, fmt.Sprintf("meta upstream returned HTTP %d", status), headers)
	if status == http.StatusTooManyRequests || status == http.StatusNotFound {
		if reset := gjson.GetBytes(body, "error.resets_at").Int(); reset > 0 {
			if delay := time.Until(time.Unix(reset, 0)); delay > 0 {
				err.retryAfter = &delay
			}
		}
		if status == http.StatusNotFound && err.retryAfter == nil {
			delay := 5 * time.Minute
			err.retryAfter = &delay
		}
	}
	if status == http.StatusTooManyRequests {
		message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
		code := strings.ToLower(gjson.GetBytes(body, "error.code").String())
		if strings.Contains(message, "subscription quota") || strings.Contains(message, "quota exhausted") || (strings.Contains(code, "quota") && gjson.GetBytes(body, "error.resets_at").Exists()) {
			return metaQuotaError{statusErr: err}
		}
	}
	return err
}

type metaQuotaError struct {
	statusErr
}

func (e metaQuotaError) IsCredentialScoped() bool {
	return true
}

func (e metaQuotaError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	disposition := e.statusErr.ExecutionDisposition()
	disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
	return disposition
}

func (e metaQuotaError) Unwrap() error {
	return e.statusErr
}

func metaEventError(payload []byte) error {
	root := gjson.ParseBytes(payload)
	eventType := root.Get("type").String()
	if eventType != "error" && eventType != "response.error" && eventType != "response.failed" &&
		root.Get("status").String() != "failed" && root.Get("response.status").String() != "failed" {
		return nil
	}
	errNode := root.Get("error")
	if !errNode.Exists() || errNode.Type == gjson.Null {
		errNode = root.Get("response.error")
		if errNode.Exists() && errNode.Type != gjson.Null {
			payload, _ = sjson.SetRawBytes(payload, "error", []byte(errNode.Raw))
		}
	}
	status := codexEventFailureStatus(root, errNode)
	return markResponseStarted(metaResponseError(status, payload, nil))
}
