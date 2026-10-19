package helps

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type geminiSearchNormalizeHooks struct {
	sdktranslator.PluginHooks
	t      *testing.T
	search bool
	calls  int
}

func (h *geminiSearchNormalizeHooks) NormalizeRequest(_ context.Context, from, to sdktranslator.Format, model string, body []byte, _ bool) []byte {
	h.calls++
	if from != sdktranslator.FormatOpenAIResponse || model != "gemini-private-search-pipeline-fixture" {
		h.t.Fatalf("normalizer received wrong route: %s -> %s, model=%s", from, to, model)
	}
	path := "tools.0.googleSearch"
	if to == sdktranslator.FormatGeminiCLI {
		path = "request." + path
	}
	if gjson.GetBytes(body, path).Exists() != h.search {
		h.t.Fatalf("normalizer did not receive the selected native capability: %s", body)
	}
	out, err := sjson.SetBytes(body, "fixture_normalized", true)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestGeminiSelectedSearchCapabilityPreservesRegisteredPipeline(t *testing.T) {
	for _, to := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		for _, supported := range []bool{false, true} {
			for _, compat := range []bool{false, true} {
				t.Run(to.String()+"/search="+map[bool]string{false: "false", true: "true"}[supported]+"/compat="+map[bool]string{false: "false", true: "true"}[compat], func(t *testing.T) {
					const model = "gemini-private-search-pipeline-fixture"
					payload := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`)
					request := cliproxyexecutor.Request{Model: model, Payload: payload, Metadata: map[string]any{
						"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{ID: model, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported}},
					}}
					hooks := &geminiSearchNormalizeHooks{t: t, search: supported}
					ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
					original, working, err := TranslateRequestPairWithAPIKeyModelCompatibilityChecked(ctx, nil, &config.Config{},
						sdktranslator.FormatOpenAIResponse, to, model, payload, payload, false, compat, &request)
					if err != nil {
						t.Fatal(err)
					}
					if hooks.calls != 1 || !gjson.GetBytes(working, "fixture_normalized").Bool() || string(original) != string(working) {
						t.Fatalf("registered pipeline bypassed or duplicated: normalizer calls=%d, original=%s, working=%s", hooks.calls, original, working)
					}
					original[0] = '['
					if working[0] != '{' {
						t.Fatal("original translation shares mutable bytes with working translation")
					}
				})
			}
		}
	}
}

func TestGeminiSearchPairRejectsOnlyWorkingPayload(t *testing.T) {
	for _, to := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		for _, invalidWorking := range []bool{false, true} {
			t.Run(to.String()+"/invalid-working="+map[bool]string{false: "false", true: "true"}[invalidWorking], func(t *testing.T) {
				const model = "gemini-private-search-pipeline-fixture"
				valid := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`)
				invalid := []byte(`{"input":"search","tools":[{"type":"web_search","filters":{"allowed_domains":[]}}]}`)
				original, working := invalid, valid
				if invalidWorking {
					original, working = valid, invalid
				}
				supported := true
				request := cliproxyexecutor.Request{Model: model, Payload: working, Metadata: map[string]any{
					"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{ID: model, NativeCapabilities: &registry.NativeCapabilities{WebSearch: &supported}},
				}}
				hooks := &geminiSearchNormalizeHooks{t: t, search: true}
				ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
				_, body, err := TranslateRequestPairWithAPIKeyModelCompatibilityChecked(ctx, nil, &config.Config{},
					sdktranslator.FormatOpenAIResponse, to, model, original, working, false, false, &request)
				if hooks.calls != 1 {
					t.Fatalf("normalizer calls=%d, want only the valid payload normalized", hooks.calls)
				}
				if invalidWorking {
					var status cliproxyexecutor.StatusError
					var scoped cliproxyexecutor.RequestScopedError
					if body != nil || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
						t.Fatalf("invalid working payload accepted: body=%s, error=%v", body, err)
					}
				} else if err != nil || !gjson.GetBytes(body, "fixture_normalized").Bool() {
					t.Fatalf("invalid original payload rejected valid working payload: body=%s, error=%v", body, err)
				}
			})
		}
	}
}
