package helps

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/openai"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/openai/claude"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeEnabledEffortThroughRegisteredThinkingPipeline(t *testing.T) {
	for _, test := range []struct {
		name, config, want string
	}{
		{"explicit effort", `"thinking":{"type":"enabled"},"output_config":{"effort":"high"}`, "high"},
		{"budget priority", `"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}`, "medium"},
		{"missing effort", `"thinking":{"type":"enabled"}`, "auto"},
		{"empty effort", `"thinking":{"type":"enabled"},"output_config":{"effort":""}`, "auto"},
		{"blank effort", `"thinking":{"type":"enabled"},"output_config":{"effort":"   "}`, "auto"},
		{"non-string effort", `"thinking":{"type":"enabled"},"output_config":{"effort":123}`, "auto"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(test.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				source := []byte(`{"model":"custom-openai","messages":[{"role":"user","content":"hi"}],` + test.config + `}`)
				translated, err := sdktranslator.TranslateRequestChecked(context.Background(), sdktranslator.FormatClaude,
					sdktranslator.FormatOpenAI, "custom-openai", source, stream)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(translated, "reasoning_effort").String(); got != test.want {
					t.Fatalf("translated effort = %q, want %q; body=%s", got, test.want, translated)
				}
				applied, err := thinking.ApplyThinking(translated, "custom-openai", "claude", "openai", "openai")
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(applied, "reasoning_effort").String(); got != test.want {
					t.Fatalf("public thinking effort = %q, want %q; body=%s", got, test.want, applied)
				}
				final, err := ApplyThinkingWithSourcePayload(translated, source, source, "custom-openai", "claude", "openai", "openai")
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(final, "reasoning_effort").String(); got != test.want {
					t.Fatalf("executor thinking effort = %q, want %q; body=%s", got, test.want, final)
				}
			})
		}
	}
}
