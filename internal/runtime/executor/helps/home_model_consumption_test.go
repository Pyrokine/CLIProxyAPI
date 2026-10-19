package helps

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/codex"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestHomeModelCapabilitiesOverrideLocalThinkingAndCompatibility(t *testing.T) {
	body := []byte(`{"model":"test-model","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":"hi"},{"type":"configuration_update","reasoning":{"effort":"low"}}]}`)
	local := &registry.ModelInfo{ID: "test-model", IsCompat: true, SupportConfigurationUpdate: true}
	home := &registry.ModelInfo{
		ID: "test-model", Type: "codex", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
		IsCompat: false, SupportConfigurationUpdate: false,
	}
	request := cliproxyexecutor.Request{
		Model: "test-model",
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": local,
			"cliproxy.resolved_home_model_info":    home,
		},
	}
	if APIKeyModelIsCompat(request) {
		t.Fatal("local is-compat leaked into Home attempt")
	}
	out, err := ApplyThinkingWithSourcePayload(
		body, body, body, "test-model", "codex", "codex", "codex", request,
	)
	if err != nil {
		t.Fatal(err)
	}
	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 1 || items[0].Get("type").String() != "message" {
		t.Fatalf("unsupported Home configuration_update left in request: %s", out)
	}
	if !local.SupportConfigurationUpdate || !local.IsCompat {
		t.Fatal("Home metadata mutated local model capabilities")
	}
}
