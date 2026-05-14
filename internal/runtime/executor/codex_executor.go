package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	codexClientVersion = "0.130.0"
	codexUserAgent     = "codex_cli_rs/0.130.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"
)

var dataTag = []byte("data:")

// codexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type codexExecutor struct {
	cfg *config.Config
}

func newCodexExecutor(cfg *config.Config) *codexExecutor { return &codexExecutor{cfg: cfg} }

func (e *codexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *codexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *codexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (
	*http.Response,
	error,
) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
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

func (e *codexExecutor) Execute(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(e.cfg, baseModel, req.Model, e.Identifier(), from, opts, req, false)
	if err != nil {
		return resp, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		httpReq, errReq := e.cacheHelper(ctx, from, url, req, body)
		if errReq != nil {
			return resp, errReq
		}
		applyCodexHeaders(httpReq, auth, apiKey, true, req.Model)
		recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return resp, errDo
		}
		data, readErr := func() ([]byte, error) {
			defer func() {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body error: %v", errClose)
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
				return nil, newCodexStatusErr(httpResp.StatusCode, b)
			}
			payload, errRead := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
			if errRead != nil {
				recordAPIResponseError(ctx, e.cfg, errRead)
				return nil, errRead
			}
			return payload, nil
		}()
		if readErr != nil {
			if statusErrCode(readErr) == http.StatusRequestTimeout && attempt == 0 {
				lastErr = readErr
				continue
			}
			return resp, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, data)

		recovered := newCodexNonStreamRecovery(req.Model)
		var streamErr error
		lines := bytes.SplitSeq(data, []byte("\n"))
		for line := range lines {
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}

			line = normalizeCodexCompletionEvent(bytes.TrimSpace(line[5:]))
			recovered.addEvent(line)
			if errEvent, ok := parseCodexEventError(line); ok {
				if streamErr == nil {
					streamErr = errEvent
				}
				continue
			}
			if gjson.GetBytes(line, "type").String() != "response.completed" {
				continue
			}

			if detail, ok := parseCodexUsage(line); ok {
				reporter.publish(ctx, detail)
			}

			var param any
			out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, line, &param)
			reporter.ensurePublished(ctx)
			resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
			return resp, nil
		}
		if streamErr != nil {
			return resp, streamErr
		}
		if recoveredPayload, ok := recovered.completedPayload(); ok {
			var param any
			out := sdktranslator.TranslateNonStream(
				ctx, to, from, req.Model, originalPayload, body, recoveredPayload, &param,
			)
			reporter.ensurePublished(ctx)
			resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
			return resp, nil
		}
		lastErr = statusErr{
			code: 408,
			msg:  "stream error: stream disconnected before completion: stream closed before response.completed",
		}
	}
	if lastErr != nil {
		return resp, lastErr
	}
	return resp, statusErr{
		code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed",
	}
}

func (e *codexExecutor) executeCompact(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (resp cliproxyexecutor.Response, err error) {
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	// noinspection DuplicatedCode
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return resp, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, req.Model)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
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
		err = newCodexStatusErr(httpResp.StatusCode, b)
		return resp, err
	}
	data, err := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publish(ctx, parseOpenAIUsage(data))
	reporter.ensurePublished(ctx)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *codexExecutor) ExecuteStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	baseModel, apiKey, baseURL := codexResolveBase(auth, req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	prepared, err := codexResponsesBody(e.cfg, baseModel, req.Model, e.Identifier(), from, opts, req, true)
	if err != nil {
		return nil, err
	}
	body := prepared.Body
	originalPayload := prepared.OriginalPayload

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, err := e.cacheHelper(ctx, from, url, req, body)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, req.Model)
	recordUpstreamRequest(ctx, e.cfg, url, httpReq.Header.Clone(), body, e.Identifier(), auth)

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 50<<20))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
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
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		recovered := newCodexNonStreamRecovery(req.Model)
		var streamErr error
		sawCompleted := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, dataTag) {
				data := normalizeCodexCompletionEvent(bytes.TrimSpace(line[5:]))
				recovered.addEvent(data)
				if errEvent, ok := parseCodexEventError(data); ok {
					if streamErr == nil {
						streamErr = errEvent
					}
					continue
				}
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					sawCompleted = true
					if detail, ok := parseCodexUsage(data); ok {
						reporter.publish(ctx, detail)
					}
					line = encodeCodexHTTPEvent(data)
				}
			}

			chunks := sdktranslator.TranslateStream(
				ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
		if streamErr != nil {
			recordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: streamErr}
			return
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !sawCompleted {
			if recoveredPayload, ok := recovered.completedPayload(); ok {
				translated := sdktranslator.TranslateStream(
					ctx, to, from, req.Model, originalPayload, body, encodeCodexHTTPEvent(recoveredPayload), &param,
				)
				for i := range translated {
					out <- cliproxyexecutor.StreamChunk{Payload: translated[i]}
				}
				reporter.ensurePublished(ctx)
				return
			}
			streamEOFErr := statusErr{
				code: 408,
				msg:  "stream error: stream disconnected before completion: stream closed before response.completed",
			}
			recordAPIResponseError(ctx, e.cfg, streamEOFErr)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: streamEOFErr}
			return
		}
		reporter.ensurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *codexExecutor) CountTokens(
	ctx context.Context,
	_ *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err := thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.SetBytes(body, "stream", false)
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(
		`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count,
	)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		tarr := tools.Array()
		for i := range tarr {
			tool := tarr[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *codexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := codexauth.NewAuth(e.cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func (e *codexExecutor) cacheHelper(
	ctx context.Context,
	from sdktranslator.Format,
	url string,
	req cliproxyexecutor.Request,
	rawJSON []byte,
) (*http.Request, error) {
	var cache codexCache
	if shouldUseImplicitCodexConversationCache(from, req.Model) {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = getCodexCache(key); !ok {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Conversation_id", cache.ID)
		httpReq.Header.Set("Session_id", cache.ID)
	}
	return httpReq, nil
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, model string) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := util.GinContextValue(r.Context()).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	misc.EnsureHeader(r.Header, ginHeaders, "Version", codexClientVersion)
	misc.EnsureHeader(r.Header, ginHeaders, "Session_id", uuid.NewString())
	misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", codexUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if !isAPIKey {
		r.Header.Set("Originator", "codex_cli_rs")
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
}

func newCodexStatusErr(statusCode int, body []byte) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if retryAfter := parseCodexRetryAfter(statusCode, body, time.Now()); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

func statusErrCode(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := err.(interface{ StatusCode() int }); ok {
		return se.StatusCode()
	}
	return 0
}

func parseCodexEventError(payload []byte) (error, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return nil, false
	}
	root := gjson.ParseBytes(payload)
	if strings.TrimSpace(root.Get("type").String()) != "error" {
		return nil, false
	}
	status := int(root.Get("status").Int())
	if status == 0 {
		status = int(root.Get("status_code").Int())
	}
	if status == 0 {
		status = http.StatusBadRequest
	}
	body := []byte(`{}`)
	if errNode := root.Get("error"); errNode.Exists() {
		raw := errNode.Raw
		if errNode.Type == gjson.String {
			raw = errNode.Raw
		}
		body, _ = sjson.SetRawBytes(body, "error", []byte(raw))
	} else {
		msg := strings.TrimSpace(root.Get("message").String())
		if msg == "" {
			msg = http.StatusText(status)
		}
		body, _ = sjson.SetBytes(body, "error.type", "server_error")
		body, _ = sjson.SetBytes(body, "error.message", msg)
	}
	return newCodexStatusErr(status, body), true
}

func parseCodexRetryAfter(statusCode int, errorBody []byte, now time.Time) *time.Duration {
	if statusCode != http.StatusTooManyRequests || len(errorBody) == 0 {
		return nil
	}
	if strings.TrimSpace(gjson.GetBytes(errorBody, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.GetBytes(errorBody, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			return new(resetAtTime.Sub(now))
		}
	}
	if resetsInSeconds := gjson.GetBytes(errorBody, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		return new(time.Duration(resetsInSeconds) * time.Second)
	}
	return nil
}

type codexRecoveredToolCall struct {
	callID    string
	name      string
	arguments string
}

type codexNonStreamRecovery struct {
	defaultModel string
	responseID   string
	model        string
	createdAt    int64
	reasoning    strings.Builder
	text         strings.Builder
	toolCalls    []codexRecoveredToolCall
}

func newCodexNonStreamRecovery(model string) *codexNonStreamRecovery {
	return &codexNonStreamRecovery{defaultModel: strings.TrimSpace(model)}
}

func (r *codexNonStreamRecovery) addEvent(payload []byte) {
	if r == nil || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	switch root.Get("type").String() {
	case "response.created":
		if id := strings.TrimSpace(root.Get("response.id").String()); id != "" {
			r.responseID = id
		}
		if model := strings.TrimSpace(root.Get("response.model").String()); model != "" {
			r.model = model
		}
		if createdAt := root.Get("response.created_at").Int(); createdAt > 0 {
			r.createdAt = createdAt
		}
	case "response.reasoning_summary_text.delta":
		r.reasoning.WriteString(root.Get("delta").String())
	case "response.output_text.delta":
		r.text.WriteString(root.Get("delta").String())
	case "response.output_item.added":
		item := root.Get("item")
		if item.Get("type").String() == "function_call" {
			r.upsertToolCall(item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments").String())
		}
	case "response.function_call_arguments.delta":
		r.appendToolArguments(root.Get("delta").String())
	case "response.function_call_arguments.done":
		r.completeToolArguments(root.Get("arguments").String())
	case "response.output_item.done":
		item := root.Get("item")
		switch item.Get("type").String() {
		case "function_call":
			r.upsertToolCall(item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments").String())
		case "message":
			if r.text.Len() == 0 {
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(
						func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								r.text.WriteString(part.Get("text").String())
							}
							return true
						},
					)
				}
			}
		}
	}
}

func (r *codexNonStreamRecovery) upsertToolCall(callID string, name string, arguments string) {
	if r == nil {
		return
	}
	callID = strings.TrimSpace(callID)
	name = strings.TrimSpace(name)
	for i := range r.toolCalls {
		if callID != "" && r.toolCalls[i].callID == callID {
			if name != "" {
				r.toolCalls[i].name = name
			}
			if arguments != "" {
				r.toolCalls[i].arguments = arguments
			}
			return
		}
	}
	r.toolCalls = append(r.toolCalls, codexRecoveredToolCall{callID: callID, name: name, arguments: arguments})
}

func (r *codexNonStreamRecovery) appendToolArguments(delta string) {
	if r == nil || len(r.toolCalls) == 0 || delta == "" {
		return
	}
	r.toolCalls[len(r.toolCalls)-1].arguments += delta
}

func (r *codexNonStreamRecovery) completeToolArguments(arguments string) {
	if r == nil || len(r.toolCalls) == 0 || arguments == "" {
		return
	}
	last := &r.toolCalls[len(r.toolCalls)-1]
	if last.arguments == "" {
		last.arguments = arguments
	}
}

func (r *codexNonStreamRecovery) completedPayload() ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	outputCount := 0
	payload := []byte(`{"type":"response.completed","response":{"id":"","model":"","created_at":0,"status":"completed","output":[]}}`)
	responseID := strings.TrimSpace(r.responseID)
	if responseID == "" {
		responseID = "resp_" + uuid.NewString()
	}
	payload, _ = sjson.SetBytes(payload, "response.id", responseID)
	model := strings.TrimSpace(r.model)
	if model == "" {
		model = thinking.ParseSuffix(r.defaultModel).ModelName
		if model == "" {
			model = r.defaultModel
		}
	}
	payload, _ = sjson.SetBytes(payload, "response.model", model)
	createdAt := r.createdAt
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload, _ = sjson.SetBytes(payload, "response.created_at", createdAt)
	if r.reasoning.Len() > 0 {
		reasoning := []byte(`{"type":"reasoning","summary":[{"type":"summary_text","text":""}]}`)
		reasoning, _ = sjson.SetBytes(reasoning, "summary.0.text", r.reasoning.String())
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", reasoning)
		outputCount++
	}
	if r.text.Len() > 0 {
		message := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", r.text.String())
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", message)
		outputCount++
	}
	for i := range r.toolCalls {
		tool := r.toolCalls[i]
		if strings.TrimSpace(tool.name) == "" {
			continue
		}
		call := []byte(`{"type":"function_call","call_id":"","name":"","arguments":""}`)
		call, _ = sjson.SetBytes(call, "call_id", tool.callID)
		call, _ = sjson.SetBytes(call, "name", tool.name)
		call, _ = sjson.SetBytes(call, "arguments", tool.arguments)
		payload, _ = sjson.SetRawBytes(payload, "response.output.-1", call)
		outputCount++
	}
	if outputCount == 0 {
		return nil, false
	}
	if len(r.toolCalls) > 0 {
		payload, _ = sjson.SetBytes(payload, "response.stop_reason", "tool_use")
	}
	return payload, true
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
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
