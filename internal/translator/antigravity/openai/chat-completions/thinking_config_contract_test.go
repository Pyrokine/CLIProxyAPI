package chat_completions_test

import (
	"context"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/openai/chat-completions"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravityChatSDKThinkingObjectAliases(t *testing.T) {
	for _, tc := range []struct {
		fields  string
		budget  int64
		include bool
	}{
		{
			`"generation_config":{"maxOutputTokens":400,"thinking_config":{"thinking_budget":8192,"` +
				`include_thoughts":true,"retained_field":"value"}}`,
			8192, true,
		},
		{
			`"generationConfig":{"maxOutputTokens":400,"thinking_config":{"thinking_budget":8192,"` +
				`include_thoughts":true},"thinkingConfig":{"thinkingBudget":4096,"includeThoughts":false,"` +
				`retained_field":"value"}}`,
			4096, false,
		},
		{
			`"generation_config":{"thinkingConfig":{"thinkingBudget":1}},"generationConfig":{"` +
				`maxOutputTokens":400,"thinkingConfig":{"thinkingBudget":4096,"includeThoughts":false,"` +
				`retained_field":"value"}}`,
			4096, false,
		},
	} {
		body := []byte(`{"messages":[{"role":"user","content":"query"}],` + tc.fields + `}`)
		out, err := sdk.TranslateRequestChecked(
			context.Background(), sdk.FormatOpenAI, sdk.FormatAntigravity, "gemini-test", body, false,
		)
		if err != nil {
			t.Fatal(err)
		}
		config := gjson.GetBytes(out, "request.generationConfig")
		if config.Get("maxOutputTokens").Int() != 400 ||
			config.Get("thinkingConfig.thinkingBudget").Int() != tc.budget ||
			config.Get("thinkingConfig.includeThoughts").Bool() != tc.include ||
			config.Get("thinkingConfig.retained_field").String() != "value" ||
			config.Get("thinking_config").Exists() {
			t.Fatalf("camel/snake object contract changed: %s", out)
		}
	}
}
