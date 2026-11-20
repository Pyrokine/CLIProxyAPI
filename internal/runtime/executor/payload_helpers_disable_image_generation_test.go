package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigDisableImageGenerationPassthroughKeepsTools(t *testing.T) {
	payload := []byte((`{"tools":[{"type":"function","function":{"name":"search"}},{"type":"im` +
		`age_generation"}],"tool_choice":{"type":"image_generation"}}`))
	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationPassthrough,
	}

	out := applyPayloadConfigWithRequest(cfg, "", "", "", "", payload, nil, "", "/v1/responses", nil, "openai-compat")

	if !gjson.GetBytes(out, `tools.#(type=="image_generation")`).Exists() {
		t.Fatalf("image_generation tool was removed: %s", out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; payload=%s", got, out)
	}
}

func TestApplyPayloadConfigDisableImageGenerationChatKeepsImagesEndpoint(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationChat,
	}

	out := applyPayloadConfigWithRequest(
		cfg, "", "", "", "", payload, nil, "", "/v1/images/generations", nil, "openai-compat",
	)

	if !gjson.GetBytes(out, `tools.#(type=="image_generation")`).Exists() {
		t.Fatalf("image_generation tool was removed for images endpoint: %s", out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; payload=%s", got, out)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGeneration_RemovesToolsEntryWithRoot(t *testing.T) {
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	payload := []byte(`{"request":{"tools":[{"type":"image_generation"},{"type":"web_search"}]}}`)

	out := applyPayloadConfigWithRequest(
		cfg, "gpt-5.4", "antigravity", "", "request", payload, nil, "", "", nil, "antigravity",
	)

	tools := gjson.GetBytes(out, "request.tools")
	if !tools.Exists() || !tools.IsArray() {
		t.Fatalf("expected request.tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 1 {
		t.Fatalf("expected 1 tool after removal, got %d", len(arr))
	}
	if got := arr[0].Get("type").String(); got != "web_search" {
		t.Fatalf("expected remaining tool type=web_search, got %q", got)
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGeneration_RemovesToolChoiceByNameWithRoot(t *testing.T) {
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
	payload := []byte(`{"request":{"tools":[{"type":"image_generation"},{"type":"web_search"}],` +
		`"tool_choice":{"type":"tool","name":"image_generation"}}}`)

	out := applyPayloadConfigWithRequest(
		cfg, "gpt-5.4", "antigravity", "", "request", payload, nil, "", "", nil, "antigravity",
	)

	if gjson.GetBytes(out, "request.tool_choice").Exists() {
		t.Fatal("expected request.tool_choice to be removed")
	}
}

func TestApplyPayloadConfigWithRoot_DisableImageGeneration_PayloadOverrideCanRestoreImageGeneration(t *testing.T) {
	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationAll,
		Payload: config.PayloadConfig{
			OverrideRaw: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "openai-response"}},
					Params: map[string]any{
						"tools":       `[{"type":"image_generation"},{"type":"function","name":"f1"}]`,
						"tool_choice": `{"type":"image_generation"}`,
					},
				},
			},
		},
	}
	payload := []byte(`{"tools":[{"type":"image_generation"},{"type":"function","name":"f1"}],` +
		`"tool_choice":{"type":"image_generation"}}`)

	out := applyPayloadConfigWithRequest(
		cfg, "gpt-5.4", "openai-response", "", "", payload, nil, "", "", nil, "openai-compat",
	)

	tools := gjson.GetBytes(out, "tools")
	if !tools.Exists() || !tools.IsArray() {
		t.Fatalf("expected tools array, got %v", tools.Type)
	}
	arr := tools.Array()
	if len(arr) != 2 {
		t.Fatalf("expected 2 tools after payload override, got %d", len(arr))
	}
	if got := arr[0].Get("type").String(); got != "image_generation" {
		t.Fatalf("expected first tool type=image_generation, got %q", got)
	}
	if !gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatal("expected tool_choice to be restored by payload override")
	}
}

func TestApplyPayloadConfigDisableImageGenerationAllStripsTools(t *testing.T) {
	payload := []byte((`{"tools":[{"type":"function","function":{"name":"search"}},{"type":"im` +
		`age_generation"}],"tool_choice":{"type":"image_generation"}}`))
	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationAll,
	}

	out := applyPayloadConfigWithRequest(
		cfg, "", "", "", "", payload, nil, "", "/v1/images/generations", nil, "openai-compat",
	)

	if gjson.GetBytes(out, `tools.#(type=="image_generation")`).Exists() {
		t.Fatalf("image_generation tool still exists: %s", out)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice still exists: %s", out)
	}
}
