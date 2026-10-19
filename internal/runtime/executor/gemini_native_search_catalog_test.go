package executor

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestGeminiResponsesWebSearchUsesCatalogMetadata(t *testing.T) {
	for _, provider := range []struct {
		name   string
		models func() []*registry.ModelInfo
	}{
		{name: "gemini", models: registry.GetGeminiModels},
		{name: "vertex", models: registry.GetGeminiVertexModels},
		{name: "gemini-cli", models: registry.GetGeminiCLIModels},
		{name: "aistudio", models: registry.GetAIStudioModels},
	} {
		for _, search := range []bool{true, false} {
			t.Run(provider.name+"/"+map[bool]string{true: "native", false: "explicit-false"}[search], func(t *testing.T) {
				var info *registry.ModelInfo
				for _, model := range provider.models() {
					if model.ID == "gemini-2.5-flash" {
						info = model
						break
					}
				}
				if info == nil {
					t.Fatal("catalog model missing")
				}
				if !search {
					info.NativeCapabilities = &registry.NativeCapabilities{WebSearch: &search}
				}
				clientID := "native-search-catalog-" + provider.name
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(clientID, provider.name, []*registry.ModelInfo{info})
				t.Cleanup(func() { reg.UnregisterClient(clientID) })

				request := cliproxyexecutor.Request{
					Model:   info.ID,
					Payload: []byte(`{"model":"gemini-2.5-flash","input":"search","tools":[{"type":"web_search"}]}`),
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: request.Payload}
				ctx := context.Background()
				cfg := &config.Config{}
				var body []byte
				format := sdktranslator.FormatGemini
				searchPath := "tools.0.googleSearch"
				switch provider.name {
				case "gemini":
					prepared, err := NewGeminiExecutor(cfg).prepareGeminiRequest(ctx,
						&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture-key"}}, request, opts, false)
					if err != nil {
						t.Fatal(err)
					}
					body, err = io.ReadAll(prepared.httpReq.Body)
					if err != nil {
						t.Fatal(err)
					}
					_ = prepared.httpReq.Body.Close()
				case "vertex":
					prepared, err := NewGeminiVertexExecutor(cfg).prepareVertexGeminiBody(ctx, request, opts, info.ID, false)
					if err != nil {
						t.Fatal(err)
					}
					body = prepared.body
				case "gemini-cli":
					prepared, err := NewGeminiCLIExecutor(cfg).prepareGeminiCLIRequest(ctx,
						&cliproxyauth.Auth{Metadata: map[string]any{
							"access_token": "fixture-access", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
						}}, request, opts, false)
					if err != nil {
						t.Fatal(err)
					}
					body = prepared.basePayload
					format = sdktranslator.FormatGeminiCLI
					searchPath = "request.tools.0.googleSearch"
				case "aistudio":
					var err error
					body, _, err = NewAIStudioExecutor(cfg, "aistudio", nil).translateRequest(ctx, request, opts, false)
					if err != nil {
						t.Fatal(err)
					}
				}
				if got := gjson.GetBytes(body, searchPath).Exists(); got != search {
					t.Fatalf("native search = %t, want %t: %s", got, search, body)
				}
				if capability := reg.GetResponsesWebSearchCapability(info.ID); capability == nil || *capability != search {
					t.Fatalf("advertised capability = %v, want %t", capability, search)
				}
				upstream := []byte(`{"responseId":"catalog-search","candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"answer"}]},"groundingMetadata":{"webSearchQueries":["query"],"groundingChunks":[{"web":{"uri":"https://example.com/source","title":"Source"}}]}}]}`)
				if !search {
					var err error
					upstream, err = sjson.DeleteBytes(upstream, "candidates.0.groundingMetadata")
					if err != nil {
						t.Fatal(err)
					}
				}
				if format == sdktranslator.FormatGeminiCLI {
					upstream = append(append([]byte(`{"response":`), upstream...), '}')
				}
				var param any
				result := sdktranslator.TranslateNonStream(ctx, format, opts.SourceFormat, info.ID, request.Payload, body, upstream, &param)
				if got := gjson.GetBytes(result, "output.0.type").String(); (got == "web_search_call") != search {
					t.Fatalf("search result = %q, want search=%t: %s", got, search, result)
				}
				if search && (gjson.GetBytes(result, "output.0.action.sources.0.url").String() != "https://example.com/source" ||
					gjson.GetBytes(result, "tool_usage.web_search.num_requests").Int() != 1) {
					t.Fatalf("search sources or usage missing: %s", result)
				}
			})
		}
	}
}
