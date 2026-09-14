package executor

import (
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
	Body                  []byte
	OriginalPayload       []byte
	OptimizedMultiAgentV2 bool
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
) (codexResponsesBodyResult, error) {
	to := sdktranslator.FromString("codex")
	isCompat := helps.APIKeyModelIsCompat(req)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	reasoningSummaryDelivery := gjson.GetBytes(req.Payload, "stream_options.reasoning_summary_delivery")
	if !reasoningSummaryDelivery.Exists() {
		reasoningSummaryDelivery = gjson.GetBytes(originalPayloadSource, "stream_options.reasoning_summary_delivery")
	}
	originalPayload := originalPayloadSource
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		cfg,
		from,
		to,
		baseModel,
		originalPayload,
		streaming,
		isCompat,
	)
	body := helps.TranslateRequestWithAPIKeyModelCompatibility(
		ctx,
		opts.Headers,
		cfg,
		from,
		to,
		baseModel,
		req.Payload,
		streaming,
		isCompat,
	)

	body, err := helps.ApplyThinkingWithSourcePayload(
		body, req.Payload, originalPayloadSource, model, from.String(), to.String(), executorID, req,
	)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}

	requestedModel := payloadRequestedModel(opts, model)
	body = applyPayloadConfigWithRoot(cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
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
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = normalizeCodexInstructions(body)
	if streaming && reasoningSummaryDelivery.Exists() {
		body, _ = sjson.SetBytes(body, "stream_options.reasoning_summary_delivery", reasoningSummaryDelivery.Value())
	}
	if cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body, optimizedMultiAgentV2 := helps.OptimizeCodexMultiAgentV2Request(
		ctx,
		opts.Headers,
		body,
		cfg,
		isCompat,
	)
	if err = validateCodexResponsesToolNames(body, sourceFormatEqual(from, sdktranslator.FormatClaude)); err != nil {
		return codexResponsesBodyResult{}, statusErr{code: http.StatusBadRequest, msg: "codex executor: " + err.Error()}
	}

	return codexResponsesBodyResult{
		Body:                  body,
		OriginalPayload:       originalPayload,
		OptimizedMultiAgentV2: optimizedMultiAgentV2,
	}, nil
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
