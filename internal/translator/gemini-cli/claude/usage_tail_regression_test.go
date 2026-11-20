package claude

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiCLISDKRetainsSplitUsageThroughTerminal(t *testing.T) {
	for _, registry := range []string{"package", "default"} {
		for _, canceled := range []bool{false, true} {
			name := registry + "/completed"
			if canceled {
				name = registry + "/canceled"
			}
			t.Run(
				name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var state any
					translate := func(frame string) [][]byte {
						if registry == "default" {
							return sdktranslator.Default().TranslateStream(
								ctx, sdktranslator.FormatGeminiCLI, sdktranslator.FormatClaude, "fixture", nil, nil,
								[]byte(frame), &state,
							)
						}
						return sdktranslator.TranslateStream(
							ctx, sdktranslator.FormatGeminiCLI, sdktranslator.FormatClaude, "fixture", nil, nil,
							[]byte(frame), &state,
						)
					}
					var output strings.Builder
					for _, frame := range []string{
						`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],` +
							`"usageMetadata":{"promptTokenCount":` +
							`10}}}`,
						`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],` +
							`"usageMetadata":{"candidatesTokenCount":` +
							`2}}}`,
						`{"response":{"usageMetadata":{"cachedContentTokenCount":4,` +
							`"thoughtsTokenCount":3,"totalTokenCount":` +
							`15}}}`,
						`{"response":{"usageMetadata":{"promptTokenCount":"invalid",` +
							`"cachedContentTokenCount":-1,"thoughtsTokenCount":` +
							`null}}}`,
					} {
						for _, chunk := range translate(frame) {
							output.Write(chunk)
						}
					}
					if canceled {
						cancel()
					}
					for _, chunk := range translate("[DONE]") {
						output.Write(chunk)
					}
					for _, frame := range []string{
						"[DONE]", `{"response":{"usageMetadata":{"promptTokenCount":999}}}`,
						`{"response":{"candidates":[{"content":{"parts":[{"text":"post-terminal"}]}}]}}`,
					} {
						for _, chunk := range translate(frame) {
							if len(chunk) != 0 {
								t.Errorf("event after terminal or cancellation: %s", chunk)
							}
						}
					}
					starts, stops, finishes, blockStarts, blockStops := 0, 0, 0, 0, 0
					var finalUsage gjson.Result
					for line := range strings.SplitSeq(output.String(), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
						switch event.Get("type").String() {
						case "message_start":
							starts++
						case "content_block_start":
							blockStarts++
						case "content_block_stop":
							blockStops++
						case "message_delta":
							finalUsage = event.Get("usage")
							if event.Get("delta.stop_reason").Exists() {
								finishes++
								if event.Get("delta.stop_reason").String() != "max_tokens" {
									t.Errorf("finish reason changed: %s", event.Raw)
								}
							}
						case "message_stop":
							stops++
						}
					}
					wantStops := 1
					if canceled {
						wantStops = 0
					}
					if starts != 1 || stops != wantStops || finishes != 1 || blockStarts != 1 ||
						blockStops != 1 ||
						finalUsage.Get("input_tokens").Int() != 6 ||
						finalUsage.Get("output_tokens").Int() != 5 ||
						finalUsage.Get("cache_read_input_tokens").Int() != 4 {
						t.Fatalf(
							"starts=%d stops=%d finishes=%d blocks=%d/%d usage=%s", starts, stops, finishes,
							blockStarts, blockStops, finalUsage.Raw,
						)
					}
				},
			)
		}
	}
}
