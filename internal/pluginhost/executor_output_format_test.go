package pluginhost

import (
	"context"
	"fmt"
	"testing"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestExecutorOutputFormatRequiresMatchingResponseTransformer(t *testing.T) {
	streamTransform := func(context.Context, string, []byte, []byte, []byte, *any) [][]byte { return nil }
	nonStreamTransform := func(context.Context, string, []byte, []byte, []byte, *any) []byte { return nil }
	for _, test := range []struct {
		name     string
		response sdktranslator.ResponseTransform
	}{
		{name: "empty"},
		{name: "token-count", response: sdktranslator.ResponseTransform{TokenCount: func(context.Context, int64) []byte { return nil }}},
		{name: "stream-only", response: sdktranslator.ResponseTransform{Stream: streamTransform}},
		{name: "non-stream-only", response: sdktranslator.ResponseTransform{NonStream: nonStreamTransform}},
		{name: "both", response: sdktranslator.ResponseTransform{Stream: streamTransform, NonStream: nonStreamTransform}},
	} {
		for _, stream := range []bool{false, true} {
			for _, alternative := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/alternative=%t", test.name, stream, alternative), func(t *testing.T) {
					requested := sdktranslator.Format(t.Name() + "-requested")
					output := sdktranslator.Format(t.Name() + "-output")
					fallback := sdktranslator.Format(t.Name() + "-alternative")
					sdktranslator.Register(requested, output, func(_ string, body []byte, _ bool) []byte { return body }, test.response)
					formats := []sdktranslator.Format{output}
					if alternative {
						sdktranslator.Register(requested, fallback, nil, sdktranslator.ResponseTransform{
							Stream: streamTransform, NonStream: nonStreamTransform,
						})
						formats = append(formats, fallback)
					}
					adapter := &executorAdapter{
						host: newHostWithRecords(), provider: "response-capability",
						inputFormats: []sdktranslator.Format{requested}, outputFormats: formats,
					}
					prepared, err := adapter.prepareExecutorCall(context.Background(), coreexecutor.Request{
						Model: "model", Payload: []byte(`{}`),
					}, coreexecutor.Options{SourceFormat: requested, ResponseFormat: requested, Stream: stream})
					supported := (!stream && test.response.NonStream != nil) || (stream && test.response.Stream != nil)
					if !supported && !alternative {
						if err == nil {
							t.Fatalf("unsupported response format was selected: %q", prepared.outputFormat)
						}
						return
					}
					want := output
					if !supported {
						want = fallback
					}
					if err != nil || prepared.outputFormat != want {
						t.Fatalf("selected response format = %q, error = %v, want %q", prepared.outputFormat, err, want)
					}
				})
			}
		}
	}
}
