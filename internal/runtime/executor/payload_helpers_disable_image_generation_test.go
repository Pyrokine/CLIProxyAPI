package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigDisableImageGenerationPassthroughKeepsTools(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"search"}},{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationPassthrough},
	}

	out := applyPayloadConfigWithRequest(cfg, "", "", "", "", payload, nil, "", "/v1/responses", nil)

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
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationChat},
	}

	out := applyPayloadConfigWithRequest(cfg, "", "", "", "", payload, nil, "", "/v1/images/generations", nil)

	if !gjson.GetBytes(out, `tools.#(type=="image_generation")`).Exists() {
		t.Fatalf("image_generation tool was removed for images endpoint: %s", out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; payload=%s", got, out)
	}
}

func TestApplyPayloadConfigDisableImageGenerationAllStripsTools(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"function","function":{"name":"search"}},{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll},
	}

	out := applyPayloadConfigWithRequest(cfg, "", "", "", "", payload, nil, "", "/v1/images/generations", nil)

	if gjson.GetBytes(out, `tools.#(type=="image_generation")`).Exists() {
		t.Fatalf("image_generation tool still exists: %s", out)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice still exists: %s", out)
	}
}
