package executor

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type antigravityConfigurationUpdateHooks struct {
	*executorTranslatorHooks
}

func (antigravityConfigurationUpdateHooks) NormalizeRequest(
	_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool,
) []byte {
	return bytes.ReplaceAll(body, []byte(`"low"`), []byte(`"high"`))
}

func TestAntigravityRequestPairPublishesConfigurationUpdateIntent(t *testing.T) {
	body := []byte(`{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "synthetic-update", Payload: body}
	ctx := sdktranslator.WithPluginHooks(t.Context(), antigravityConfigurationUpdateHooks{&executorTranslatorHooks{}})
	executor := NewAntigravityExecutor(&config.Config{})
	_, working, err := executor.translateAntigravityRequestPair(ctx, &req, nil,
		sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, req.Model, body, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := req.Metadata["cliproxy.normalized_configuration_updates_changed"].(bool); !changed {
		t.Fatal("configuration update intent was not returned to the executor request")
	}
	if gjson.GetBytes(working, "input.0.reasoning.effort").String() != "high" ||
		gjson.GetBytes(body, "input.0.reasoning.effort").String() != "low" {
		t.Fatalf("working request or immutable original is incorrect: %s, original = %s", working, body)
	}
}

func TestAntigravitySelectedSearchCapabilityReachesTranslation(t *testing.T) {
	for _, supported := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, same := range []bool{false, true} {
				t.Run(fmt.Sprintf("supported=%t/stream=%t/same=%t", supported, stream, same), func(t *testing.T) {
					body := []byte(`{"model":"synthetic-search","instructions":"Keep the context","tools":[{"type":"web_search","filters":{"allowed_domains":["example.org"]}}],"input":[{"role":"user","content":"First question"},{"role":"assistant","content":"Earlier answer"},{"role":"user","content":"Next question"}]}`)
					original := body
					if !same {
						original = bytes.Clone(body)
					}
					before := bytes.Clone(body)
					info := &registry.ModelInfo{ID: "synthetic-search", NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported}}
					req := cliproxyexecutor.Request{Model: "synthetic-search", Payload: body, Metadata: map[string]any{
						"cliproxy.resolved_home_model_info": info,
					}}
					executor := NewAntigravityExecutor(&config.Config{})
					translatedOriginal, translated, err := executor.translateAntigravityRequestPair(
						sdktranslator.WithPluginHooks(t.Context(), nil), &req, nil,
						sdktranslator.FormatOpenAIResponse, sdktranslator.FormatAntigravity,
						req.Model, original, body, stream,
					)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, body) || !bytes.Equal(before, original) {
						t.Fatal("translation modified the source request")
					}
					for _, output := range [][]byte{translatedOriginal, translated} {
						if got := gjson.GetBytes(output, "requestType").String() == "web_search"; got != supported {
							t.Fatalf("web search route = %t, want %t: %s", got, supported, output)
						}
						search := gjson.GetBytes(output, "request.tools.0.googleSearch")
						if search.Exists() != supported {
							t.Fatalf("search tool = %t, want %t: %s", search.Exists(), supported, output)
						}
						if supported {
							if search.Get("enhancedContent.imageSearch.maxResultCount").Int() != 5 ||
								search.Get("includedDomains.0").String() != "example.org" {
								t.Fatalf("search options were not preserved: %s", output)
							}
							if gjson.GetBytes(output, "request.contents.#").Int() != 3 ||
								!bytes.Contains(output, []byte("Keep the context")) {
								t.Fatalf("search lost conversation or instructions: %s", output)
							}
						}
					}
					if len(translated) > 0 && len(translatedOriginal) > 0 && &translated[0] == &translatedOriginal[0] {
						t.Fatal("original and working translations share mutable bytes")
					}
				})
			}
		}
	}
}
