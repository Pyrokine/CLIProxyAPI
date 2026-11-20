package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"testing"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPluginExecutorCheckedAttachmentsAndToolResultsReachEveryEntry(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	const inlineDocument = "JVBERi0xLjQK"
	for _, sample := range []struct {
		name, payload, path, want string
		reject                    bool
	}{
		{
			name: "unsupported-file", reject: true,
			payload: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":` +
				`[{"type":"container_upload","file_id":` +
				`"file-1"}]}]}`,
		},
		{
			name: "text", path: "contents.0.parts.0.text", want: "keep me",
			payload: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":` +
				`[{"type":"text","text":"keep ` +
				`me"}]}]}`,
		},
		{
			name: "inline-document", path: "contents.0.parts.1.inline_data.data", want: inlineDocument,
			payload: `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":` +
				`[{"type":"text","text":"document"},{"type":"document","source":{"type":"base64",` +
				`"media_type":"application/pdf","data":"` + inlineDocument + `"}}]}]}`,
		},
		{
			name: "tool-result", path: "contents.1.parts.0.functionResponse.response.result", want: "found",
			payload: `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":` +
				`[{"type":"tool_use","id":"call-1","name":"lookup","input":{"key":"value"}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"found"}]}]}`,
		},
	} {
		for _, entry := range []string{"Execute", "ExecuteStream", "CountTokens"} {
			t.Run(
				fmt.Sprintf("%s/%s", sample.name, entry), func(t *testing.T) {
					calls := 0
					upstreamErr := errors.New("synthetic plugin response")
					capture := func(request pluginapi.ExecutorRequest) {
						calls++
						if request.Model != "m" ||
							request.SourceFormat != sdktranslator.FormatGemini.String() ||
							string(request.OriginalRequest) != sample.payload {
							t.Errorf("plugin request metadata or original body changed: %+v", request)
						}
						if got := gjson.GetBytes(request.Payload, sample.path).String(); got != sample.want {
							t.Errorf(
								"plugin payload %s=%q, want %q: %s",
								sample.path,
								got,
								sample.want,
								request.Payload,
							)
						}
						if sample.name == "inline-document" && gjson.GetBytes(
							request.Payload, "contents.0.parts.1.inline_data.mime_type",
						).String() != "application/pdf" {
							t.Error("plugin document MIME type changed")
						}
						if sample.name == "tool-result" && gjson.GetBytes(
							request.Payload, "contents.1.parts.0.functionResponse.name",
						).String() != "lookup" {
							t.Error("plugin tool result lost its function name")
						}
					}
					ordinary := func(_ context.Context, request pluginapi.ExecutorRequest) (
						pluginapi.ExecutorResponse,
						error,
					) {
						capture(request)
						return pluginapi.ExecutorResponse{}, upstreamErr
					}
					adapter := &executorAdapter{
						host: New(), pluginID: "attachment-contract", provider: "attachment-provider",
						inputFormats:  []sdktranslator.Format{sdktranslator.FormatGemini},
						outputFormats: []sdktranslator.Format{sdktranslator.FormatGemini},
						executor: &fakeExecutor{
							execute: ordinary, countTokens: ordinary,
							executeStream: func(
								_ context.Context,
								request pluginapi.ExecutorRequest,
							) (pluginapi.ExecutorStreamResponse, error) {
								capture(request)
								return pluginapi.ExecutorStreamResponse{}, upstreamErr
							},
						},
					}
					auth := &coreauth.Auth{ID: "synthetic-attachment", Provider: adapter.provider}
					request := coreexecutor.Request{Model: "m", Payload: []byte(sample.payload)}
					opts := coreexecutor.Options{
						SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatGemini,
						Stream: entry == "ExecuteStream",
					}
					var err error
					switch entry {
					case "Execute":
						_, err = adapter.Execute(t.Context(), auth, request, opts)
					case "ExecuteStream":
						_, err = adapter.ExecuteStream(t.Context(), auth, request, opts)
					case "CountTokens":
						_, err = adapter.CountTokens(t.Context(), auth, request, opts)
					}
					if sample.reject {
						var part *translatorcommon.UnsupportedPartError
						if !errors.As(
							err, &part,
						) ||
							part.Type !=
								"container_upload" ||
							part.StatusCode() !=
								400 ||
							!part.IsRequestScoped() ||
							calls !=
								0 {
							t.Fatalf("unsupported attachment error=%v plugin calls=%d", err, calls)
						}
					} else if !errors.Is(err, upstreamErr) || calls != 1 {
						t.Fatalf("valid request error=%v plugin calls=%d", err, calls)
					}
				},
			)
		}
	}
}
