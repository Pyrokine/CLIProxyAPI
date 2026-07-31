package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
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

func codexPromptCacheFromClaudeRequest(model string, payload []byte) (codexCache, bool) {
	if len(payload) == 0 {
		return codexCache{}, false
	}
	scope := codexPromptCacheClaudeScope(payload)
	if scope == "" {
		return codexCache{}, false
	}
	key := "codex-prompt-cache:" + codexPromptCacheModelName(model) + ":" + scope
	if cache, ok := getCodexCache(key); ok {
		return cache, true
	}
	cache := codexCache{
		ID:     uuid.New().String(),
		Expire: time.Now().Add(time.Hour),
	}
	setCodexCache(key, cache)
	return cache, true
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
	if sessionID := extractClaudeCodeSessionIDForCodexReplay(payload); sessionID != "" {
		return "claude-session:" + sessionID
	}
	userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
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
	Body            []byte
	OriginalPayload []byte
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

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequestWithContext(ctx, from, to, baseModel, originalPayload, streaming)
	body := sdktranslator.TranslateRequestWithContext(ctx, from, to, baseModel, req.Payload, streaming)

	body, err := thinking.ApplyThinking(body, model, from.String(), to.String(), executorID)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}

	requestedModel := payloadRequestedModel(opts, model)
	body = applyPayloadConfigWithRoot(cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex", body)
	body, err = preserveCodexPreviousResponseID(body, req.Payload)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body = normalizeCodexInstructions(body)
	if cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}

	return codexResponsesBodyResult{
		Body:            body,
		OriginalPayload: originalPayload,
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
