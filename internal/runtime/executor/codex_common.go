package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexDefaultBaseURL = "https://chatgpt.com/backend-api/codex"

// codexResolveBase extracts the base model name, API key and base URL from auth
// and the raw model string. If no base URL is configured, it falls back to the
// default ChatGPT Codex endpoint.
func codexResolveBase(auth *cliproxyauth.Auth, model string) (baseModel, apiKey, baseURL string) {
	baseModel = thinking.ParseSuffix(model).ModelName
	apiKey, baseURL = codexCreds(auth)
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}
	return
}

func shouldUseImplicitCodexConversationCache(from sdktranslator.Format, model string) bool {
	return sourceFormatEqual(from, sdktranslator.FormatClaude) && codexPromptCacheModelName(model) != ""
}

func codexPromptCacheFromClaudeRequest(
	ctx context.Context,
	model string,
	payload []byte,
	headers http.Header,
) (codexCache, bool) {
	modelName := codexPromptCacheModelName(model)
	if modelName == "" {
		return codexCache{}, false
	}
	executionScope, ok := helps.ClaudeCodeExecutionScope(ctx, payload, headers)
	if !ok {
		executionScope = codexPromptCacheClaudeScope(payload)
		if executionScope == "" {
			return codexCache{}, false
		}
		executionScope += ":agent:" + helps.ExtractClaudeCodeAgentID(ctx, headers)
	}
	identity := strings.Join(
		[]string{"cli-proxy-api:codex:claude-code", modelName, executionScope},
		"\x00",
	)
	return codexCache{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()}, true
}

func codexPromptCacheFromOpenAIAPIKey(ctx context.Context) (codexCache, bool) {
	apiKey := strings.TrimSpace(apiKeyFromContext(ctx))
	if apiKey == "" {
		return codexCache{}, false
	}
	return codexCache{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String(),
	}, true
}

func codexPromptCacheClaudeScope(payload []byte) string {
	if sessionID := helps.ExtractClaudeCodeSessionID(context.Background(), payload, nil); sessionID != "" {
		return "claude-session:" + sessionID
	}
	userID := cliproxysession.NormalizeExplicitID(gjson.GetBytes(payload, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	return "claude-user:" + userID
}

func codexPromptCacheModelName(model string) string {
	modelName := strings.TrimSpace(strings.ToLower(thinking.ParseSuffix(model).ModelName))
	if modelName != "" {
		return modelName
	}
	return strings.TrimSpace(strings.ToLower(model))
}

// codexResponsesBodyResult holds the outputs of codexResponsesBody.
type codexResponsesBodyResult struct {
	Body                   []byte
	OriginalPayload        []byte
	OptimizedMultiAgentV2  bool
	MultiAgentV2Conflict   bool
	Native                 bool
	baseModel              string
	requestedModel         string
	from                   sdktranslator.Format
	originalTranslated     []byte
	imageGenerationEnabled bool
}

// codexResponsesBody translates and prepares the request body for a Codex /responses call.
//
// It resolves the original payload source, translates the request into Codex format,
// applies thinking parameters, payload configuration rules, sets the model field, and
// cleans up fields that the Codex /responses endpoint does not accept.
func codexResponsesBody(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	baseModel string,
	model string,
	executorID string,
	from sdktranslator.Format,
	opts cliproxyexecutor.Options,
	req cliproxyexecutor.Request,
	streaming bool,
	deferPayloadRules bool,
) (codexResponsesBodyResult, error) {
	to := sdktranslator.FromString("codex")
	isCompat := helps.APIKeyModelIsCompat(req)
	nativeRequest := helps.IsNativeCodexRequest(req.Payload, opts)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	reasoningSummaryDelivery := gjson.GetBytes(req.Payload, "stream_options.reasoning_summary_delivery")
	if !reasoningSummaryDelivery.Exists() {
		reasoningSummaryDelivery = gjson.GetBytes(originalPayloadSource, "stream_options.reasoning_summary_delivery")
	}
	originalPayload := originalPayloadSource
	originalTranslated, body, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
		ctx, opts.Headers, cfg, from, to, baseModel, originalPayload, req.Payload, streaming, isCompat, &req,
	)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}

	body, err = helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, model, from.String(), to.String(), executorID, req,
	)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}

	requestedModel := payloadRequestedModel(opts, model)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(
		ctx,
		"codex",
		body,
		isCompat,
	)
	body, err = preserveCodexPreviousResponseID(body, req.Payload)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}
	body = normalizeCodexInstructions(body, nativeRequest)
	if streaming && reasoningSummaryDelivery.Exists() {
		body, _ = sjson.SetBytes(body, "stream_options.reasoning_summary_delivery", reasoningSummaryDelivery.Value())
	}
	prepared := codexResponsesBodyResult{
		Body:                   body,
		OriginalPayload:        originalPayload,
		Native:                 nativeRequest,
		baseModel:              baseModel,
		requestedModel:         requestedModel,
		from:                   from,
		originalTranslated:     bytes.Clone(originalTranslated),
		imageGenerationEnabled: (cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff) && !isCodexFreePlanAuth(auth),
	}
	if !deferPayloadRules {
		if err := prepared.finalize(ctx, cfg, req, opts, executorID); err != nil {
			return codexResponsesBodyResult{}, err
		}
	}
	return prepared, nil
}

func preserveCodexPreviousResponseID(body []byte, requestPayload []byte) ([]byte, error) {
	previousID := strings.TrimSpace(gjson.GetBytes(requestPayload, "previous_response_id").String())
	if previousID == "" {
		updated, errDelete := sjson.DeleteBytes(body, "previous_response_id")
		if errDelete != nil {
			return nil, fmt.Errorf("codex executor: remove previous_response_id: %w", errDelete)
		}
		return updated, nil
	}
	updated, errSet := sjson.SetBytes(body, "previous_response_id", previousID)
	if errSet != nil {
		return nil, fmt.Errorf("codex executor: preserve previous_response_id: %w", errSet)
	}
	if got := strings.TrimSpace(gjson.GetBytes(updated, "previous_response_id").String()); got != previousID {
		return nil, fmt.Errorf("codex executor: previous_response_id was not preserved")
	}
	return updated, nil
}

func codexPayloadFinalizer(
	ctx context.Context,
	cfg *config.Config,
	prepared *codexResponsesBodyResult,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	executorID string,
) func([]byte) ([]byte, error) {
	opts.Headers = opts.Headers.Clone()
	return func(body []byte) ([]byte, error) {
		prepared.Body = body
		if err := prepared.finalize(ctx, cfg, req, opts, executorID); err != nil {
			return nil, err
		}
		return prepared.Body, nil
	}
}

func (p *codexResponsesBodyResult) finalize(
	ctx context.Context,
	cfg *config.Config,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	executorID string,
) error {
	to := sdktranslator.FormatCodex
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FormatOpenAIResponse
	}
	body, touched := applyPayloadConfigWithRequestTracked(cfg, p.baseModel, to.String(), p.from.String(), "", p.Body, p.originalTranslated,
		p.requestedModel, payloadRequestPath(opts), opts.Headers, executorID, "tools")
	if p.imageGenerationEnabled && !touched["tools"] {
		body = ensureImageGenerationTool(body, gjson.GetBytes(body, "model").String(), nil, opts.Headers)
	}
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex", body, helps.APIKeyModelIsCompat(req))
	if opts.Alt == "responses/compact" {
		body, _ = sjson.DeleteBytes(body, "stream")
	} else {
		body, _ = sjson.SetBytes(body, "stream", true)
		body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
		body, _ = sjson.DeleteBytes(body, "safety_identifier")
		var err error
		body, err = preserveCodexPreviousResponseID(body, req.Payload)
		if err != nil {
			return err
		}
	}
	body = helps.NormalizeCodexToolSchemas(body)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	conflict := helps.HasCodexMultiAgentV2NamespaceConflict(body)
	body, optimized := helps.OptimizeCodexMultiAgentV2Request(ctx, opts.Headers, body, cfg, helps.APIKeyModelIsCompat(req))
	if err := validateCodexResponsesToolNames(body, sourceFormatEqual(p.from, sdktranslator.FormatClaude)); err != nil {
		return statusErr{code: http.StatusBadRequest, msg: "codex executor: " + err.Error()}
	}
	p.Body = body
	p.OptimizedMultiAgentV2 = optimized
	p.MultiAgentV2Conflict = conflict
	return nil
}
