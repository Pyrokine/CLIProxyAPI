package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeCompactionSummaryContextKey struct{}

func (e *ClaudeExecutor) prepareClaudeCompaction(
	auth *cliproxyauth.Auth,
	req *cliproxyexecutor.Request,
	opts *cliproxyexecutor.Options,
) (helps.AntigravityCompactionScope, bool, error) {
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		if opts.Alt == "responses/compact" {
			return helps.AntigravityCompactionScope{}, false, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact requires Responses format"}
		}
		return helps.AntigravityCompactionScope{}, false, nil
	}
	requested := opts.Alt == "responses/compact" ||
		helps.HasResponsesCompactionTrigger(req.Payload) ||
		helps.HasResponsesCompactionTrigger(opts.OriginalRequest)
	if !requested && !helps.HasResponsesCompactionItem(req.Payload) &&
		!helps.HasResponsesCompactionItem(opts.OriginalRequest) {
		return helps.AntigravityCompactionScope{}, false, nil
	}

	scope, err := e.claudeCompactionScope(auth, *req, *opts)
	if err != nil {
		return scope, false, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	if helps.HasResponsesCompactionItem(req.Payload) {
		req.Payload, err = helps.ExpandAntigravityCompactionCapsules(req.Payload, scope)
		if err != nil {
			return scope, false, statusErr{code: http.StatusBadRequest, msg: err.Error()}
		}
	}
	if helps.HasResponsesCompactionItem(opts.OriginalRequest) {
		opts.OriginalRequest, err = helps.ExpandAntigravityCompactionCapsules(opts.OriginalRequest, scope)
		if err != nil {
			return scope, false, statusErr{code: http.StatusBadRequest, msg: err.Error()}
		}
	}
	return scope, requested, nil
}

func (e *ClaudeExecutor) claudeCompactionScope(
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (helps.AntigravityCompactionScope, error) {
	session := antigravityCompactionSessionID(req, opts)
	for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
		derived := metadataString(metadata, cliproxyexecutor.DerivedSessionIDMetadataKey)
		if derived != "" && session == cliproxysession.BoundSessionIdentity("derived:"+derived) {
			session = ""
		}
	}
	if session == "" {
		return helps.AntigravityCompactionScope{}, fmt.Errorf("compaction requires a stable session identity")
	}
	caller := metadataString(opts.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
	if caller == "" {
		caller = metadataString(req.Metadata, cliproxyexecutor.CallerScopeMetadataKey)
	}
	if caller != "" {
		session = cliproxysession.BoundSessionIdentity(caller + "\x00" + session)
	}

	apiKey, baseURL := claudeCreds(auth)
	key := ""
	if auth != nil {
		key = strings.TrimSpace(auth.Attributes["api_key"])
		if key == "" {
			key = strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token"))
		}
	}
	if key == "" && e != nil && e.cfg != nil {
		key = strings.TrimSpace(e.cfg.RemoteManagement.SecretKey)
	}
	if key == "" {
		key = strings.TrimSpace(apiKey)
	}
	if key == "" {
		return helps.AntigravityCompactionScope{}, fmt.Errorf("compaction requires a credential key")
	}
	owner := ""
	if auth != nil {
		if id := strings.TrimSpace(auth.ID); id != "" {
			owner = "id:" + id
		}
	}
	if owner == "" {
		owner = antigravityCredentialScope("key:", key+"\x00"+baseURL)
	}
	return helps.AntigravityCompactionScope{
		Provider:    "claude",
		Model:       thinking.ParseSuffix(req.Model).ModelName,
		Session:     session,
		Owner:       owner,
		KeyMaterial: []byte(key),
	}, nil
}

func (e *ClaudeExecutor) executeClaudeCompaction(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	scope helps.AntigravityCompactionScope,
) (cliproxyexecutor.Response, error) {
	payload := req.Payload
	if !helps.HasResponsesCompactionTrigger(payload) && helps.HasResponsesCompactionTrigger(opts.OriginalRequest) {
		payload = opts.OriginalRequest
	}
	if len(payload) == 0 {
		payload = opts.OriginalRequest
	}
	tools := gjson.GetBytes(payload, "tools")
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(payload, scope.Model)
	if tools.Exists() {
		summaryPayload, _ = sjson.SetRawBytes(summaryPayload, "tools", []byte(tools.Raw))
	}
	summaryReq := cliproxyexecutor.Request{Model: req.Model, Payload: summaryPayload, Metadata: req.Metadata}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatClaude

	summary, err := e.Execute(context.WithValue(ctx, claudeCompactionSummaryContextKey{}, true), auth, summaryReq, summaryOpts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	text, err := helps.ExtractAntigravitySummaryText(summary.Payload)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("extract compaction summary: %w", err)
	}
	capsule, err := helps.SealAntigravityCompaction(text, scope)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("seal compaction capsule: %w", err)
	}
	usage := gjson.GetBytes(summary.Payload, "usage")
	inputTokens := int(usage.Get("input_tokens").Int() + usage.Get("cache_creation_input_tokens").Int() + usage.Get("cache_read_input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	cachedTokens := int(usage.Get("cache_read_input_tokens").Int())
	response := helps.BuildAntigravityCompactionResponse(scope.Model, capsule, inputTokens, outputTokens, inputTokens+outputTokens)
	response, _ = sjson.SetBytes(response, "usage.input_tokens_details.cached_tokens", cachedTokens)
	return cliproxyexecutor.Response{Payload: response, Headers: summary.Headers}, nil
}

func (e *ClaudeExecutor) executeClaudeCompactionStream(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	scope helps.AntigravityCompactionScope,
) (*cliproxyexecutor.StreamResult, error) {
	summary, err := e.executeClaudeCompaction(ctx, auth, req, opts, scope)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	capsule := gjson.GetBytes(summary.Payload, "output.0.encrypted_content").String()
	usage := gjson.GetBytes(summary.Payload, "usage")
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	totalTokens := int(usage.Get("total_tokens").Int())
	cachedTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
	frames := helps.BuildAntigravityCompactionStreamChunks(scope.Model, capsule, inputTokens, outputTokens, totalTokens)
	out := make(chan cliproxyexecutor.StreamChunk, len(frames))
	for _, frame := range frames {
		if bytes.Contains(frame, []byte(`"usage":`)) {
			frame = claudeCompactionStreamCachedTokens(frame, cachedTokens)
		}
		out <- cliproxyexecutor.StreamChunk{Payload: frame}
	}
	close(out)
	headers := summary.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func claudeCompactionStreamCachedTokens(frame []byte, cachedTokens int) []byte {
	idx := bytes.Index(frame, []byte("data: "))
	if idx < 0 {
		return frame
	}
	payload := bytes.TrimSpace(frame[idx+len("data: "):])
	updated, err := sjson.SetBytes(payload, "response.usage.input_tokens_details.cached_tokens", cachedTokens)
	if err != nil {
		return frame
	}
	out := make([]byte, 0, idx+len("data: ")+len(updated)+2)
	out = append(out, frame[:idx+len("data: ")]...)
	out = append(out, updated...)
	return append(out, '\n', '\n')
}

func finalizeClaudeCompactionSummaryBody(ctx context.Context, body []byte) []byte {
	if ctx == nil || ctx.Value(claudeCompactionSummaryContextKey{}) != true {
		return body
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"none"}`))
		return body
	}
	body, _ = sjson.DeleteBytes(body, "tool_choice")
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	for i, message := range messages.Array() {
		parts := message.Get("content")
		if !parts.IsArray() {
			continue
		}
		for j, part := range parts.Array() {
			var text string
			switch part.Get("type").String() {
			case "tool_use":
				text = fmt.Sprintf("Tool call %s (%s): %s", part.Get("name").String(), part.Get("id").String(), part.Get("input").Raw)
			case "tool_result":
				text = fmt.Sprintf("Tool result %s: %s", part.Get("tool_use_id").String(), part.Get("content").Raw)
			default:
				continue
			}
			replacement, _ := sjson.SetBytes([]byte(`{"type":"text"}`), "text", text)
			body, _ = sjson.SetRawBytes(body, fmt.Sprintf("messages.%d.content.%d", i, j), replacement)
		}
	}
	return body
}
