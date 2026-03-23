package executor

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
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
	cfg *config.Config,
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
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, streaming)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, streaming)

	body, err := thinking.ApplyThinking(body, model, from.String(), to.String(), executorID)
	if err != nil {
		return codexResponsesBodyResult{}, err
	}

	requestedModel := payloadRequestedModel(opts, model)
	body = applyPayloadConfigWithRoot(cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	return codexResponsesBodyResult{
		Body:            body,
		OriginalPayload: originalPayload,
	}, nil
}
