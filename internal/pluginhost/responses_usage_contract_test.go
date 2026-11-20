package pluginhost

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPluginNativeResponsesKeepUsageDetails(t *testing.T) {
	for _, format := range []sdk.Format{sdk.FormatOpenAIResponse, sdk.FormatCodex, sdk.FormatOpenAI, ""} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
					payload := []byte(`{"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100}}`)
					if stream {
						payload = append(append([]byte("data: "), payload...), '\n', '\n')
					}
					adapter := &executorAdapter{}
					prepared := preparedExecutorCall{requestedFormat: format, outputFormat: format}
					var output []byte
					if stream {
						in := make(chan pluginapi.ExecutorStreamChunk, 1)
						in <- pluginapi.ExecutorStreamChunk{Payload: payload}
						close(in)
						for chunk := range adapter.translateExecutorStreamChunks(t.Context(), prepared, in) {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							output = append(output, chunk.Payload...)
						}
					} else {
						output = adapter.translateExecutorResponse(t.Context(), prepared, payload, false, nil)
					}
					if format != sdk.FormatOpenAIResponse && format != sdk.FormatCodex {
						if !bytes.Equal(output, payload) {
							t.Fatalf("unrelated native output changed: %s", output)
						}
						return
					}
					output = bytes.TrimSpace(bytes.TrimPrefix(output, []byte("data: ")))
					for _, field := range []string{
						"usage.input_tokens_details.cached_tokens", "usage.output_tokens_details.reasoning_tokens",
					} {
						if value := gjson.GetBytes(output, field); !value.Exists() || value.Int() != 0 {
							t.Fatalf("native plugin lost %s: %s", field, output)
						}
					}
					if gjson.GetBytes(output, "usage.total_tokens").Int() != 100 {
						t.Fatalf("native plugin token total changed: %s", output)
					}
				},
			)
		}
	}
}

func TestResponsesUsageNormalizationPreservesPluginFallbackDetection(t *testing.T) {
	payload := []byte(`{"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100}}`)
	prepared := preparedExecutorCall{outputFormat: sdk.FormatOpenAI, requestedFormat: sdk.FormatOpenAIResponse}
	if !executorNativeStreamResponseTranslatorExists(prepared.outputFormat, prepared.requestedFormat) {
		t.Fatal("original registered response translator is unavailable")
	}
	frames := [][]byte{util.EnsureResponsesUsageDetails(payload)}
	if !executorStreamTranslationFellBack(prepared, payload, frames) {
		t.Fatalf("schema-only normalization hid untranslated plugin output: %s", frames)
	}
}
