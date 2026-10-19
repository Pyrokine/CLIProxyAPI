package helps

import (
	"bytes"
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type summaryRemovingPluginHooks struct {
	t *testing.T
}

func (h summaryRemovingPluginHooks) NormalizeRequest(
	_ context.Context,
	_, _ sdktranslator.Format,
	_ string,
	body []byte,
	_ bool,
) []byte {
	h.t.Helper()
	const path = "generationConfig.thinkingConfig.includeThoughts"
	if !gjson.GetBytes(body, path).Bool() {
		h.t.Fatalf("request normalizer did not receive enabled summary: %s", body)
	}
	out, _ := sjson.DeleteBytes(body, path)
	return out
}

func (summaryRemovingPluginHooks) TranslateRequest(
	context.Context,
	sdktranslator.Format,
	sdktranslator.Format,
	string,
	[]byte,
	bool,
) ([]byte, bool) {
	return nil, false
}

func (summaryRemovingPluginHooks) NormalizeResponseBefore(
	context.Context,
	sdktranslator.Format,
	sdktranslator.Format,
	string,
	[]byte,
	[]byte,
	[]byte,
	bool,
) []byte {
	return nil
}

func (summaryRemovingPluginHooks) TranslateResponse(
	context.Context,
	sdktranslator.Format,
	sdktranslator.Format,
	string,
	[]byte,
	[]byte,
	[]byte,
	bool,
) ([]byte, bool) {
	return nil, false
}

func (summaryRemovingPluginHooks) NormalizeResponseAfter(
	context.Context,
	sdktranslator.Format,
	sdktranslator.Format,
	string,
	[]byte,
	[]byte,
	[]byte,
	bool,
) []byte {
	return nil
}

func TestTranslatedRequestSummaryConfigAuthority(t *testing.T) {
	tests := []struct {
		name                  string
		body                  string
		currentSourcePayload  string
		originalSourcePayload string
		fromFormat            string
		toFormat              string
		want                  thinking.SummaryMode
	}{
		{
			name:                  "target explicit visibility wins",
			body:                  `{"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`,
			currentSourcePayload:  `{"reasoning":{"summary":"auto"}}`,
			originalSourcePayload: `{"reasoning":{"summary":"auto"}}`,
			fromFormat:            "openai-response",
			toFormat:              "gemini",
			want:                  thinking.SummaryDisabled,
		},
		{
			name:                  "same format target keeps native visibility semantics",
			body:                  `{"reasoning_effort":"high"}`,
			currentSourcePayload:  `{"reasoning_effort":"none"}`,
			originalSourcePayload: `{"reasoning_effort":"none"}`,
			fromFormat:            "openai",
			toFormat:              "openai",
			want:                  thinking.SummaryEnabled,
		},
		{
			name:                  "Chat effort does not force Claude display",
			body:                  `{"thinking":{"type":"adaptive"}}`,
			currentSourcePayload:  `{"reasoning_effort":"high"}`,
			originalSourcePayload: `{"reasoning_effort":"high"}`,
			fromFormat:            "openai",
			toFormat:              "claude",
			want:                  thinking.SummaryUnspecified,
		},
		{
			name:                  "target normalizer removal remains final",
			body:                  `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`,
			currentSourcePayload:  `{"reasoning":{"summary":"auto"}}`,
			originalSourcePayload: `{"reasoning":{"summary":"auto"}}`,
			fromFormat:            "openai-response",
			toFormat:              "gemini",
			want:                  thinking.SummaryUnspecified,
		},
		{
			name:                  "current source overrides original source",
			body:                  `{}`,
			currentSourcePayload:  `{"reasoning":{"summary":null}}`,
			originalSourcePayload: `{"reasoning":{"summary":"auto"}}`,
			fromFormat:            "openai-response",
			toFormat:              "openai",
			want:                  thinking.SummaryDisabled,
		},
		{
			name:                  "missing current source recovers original source",
			body:                  `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`,
			currentSourcePayload:  `{"input":"hi"}`,
			originalSourcePayload: `{"reasoning":{"summary":"auto"}}`,
			fromFormat:            "openai-response",
			toFormat:              "gemini",
			want:                  thinking.SummaryEnabled,
		},
		{
			name:                  "provider specific Chat visibility remains available",
			body:                  `{}`,
			currentSourcePayload:  `{"reasoning":{"summary":"auto"}}`,
			originalSourcePayload: `{"reasoning":{"summary":"auto"}}`,
			fromFormat:            "openai-response",
			toFormat:              "openai",
			want:                  thinking.SummaryEnabled,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				got := translatedRequestSummaryConfig(
					[]byte(test.body),
					[]byte(test.currentSourcePayload),
					[]byte(test.originalSourcePayload),
					"test-model",
					test.fromFormat,
					test.toFormat,
				)
				if got.Mode != test.want {
					t.Fatalf("translatedRequestSummaryConfig() mode = %v, want %v", got.Mode, test.want)
				}
			},
		)
	}
}

func TestApplyThinkingWithSourcePayloadPreservesNormalizerSummaryRemoval(t *testing.T) {
	hooks := summaryRemovingPluginHooks{t: t}
	ctx := sdktranslator.WithPluginHooks(context.Background(), hooks)
	source := []byte(`{"model":"gemini-3.6-flash","reasoning":{"effort":"high","summary":"auto"},"input":"hi"}`)
	translated := sdktranslator.TranslateRequestWithContext(
		ctx,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatGemini,
		"gemini-3.6-flash",
		source,
		false,
	)
	const summaryPath = "generationConfig.thinkingConfig.includeThoughts"
	if gjson.GetBytes(translated, summaryPath).Exists() {
		t.Fatalf("request normalizer did not remove summary: %s", translated)
	}

	out, err := ApplyThinkingWithSourcePayload(
		translated,
		source,
		source,
		"gemini-3.6-flash",
		sdktranslator.FormatOpenAIResponse.String(),
		sdktranslator.FormatGemini.String(),
		"gemini",
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSourcePayload() error = %v", err)
	}
	if gjson.GetBytes(out, summaryPath).Exists() {
		t.Fatalf("executor restored summary removed by request normalizer: %s", out)
	}
}

func TestApplyThinkingWithSourcePayloadPreservesOriginalOnlySummary(t *testing.T) {
	currentSource := []byte(`{"model":"gemini-3.6-flash","input":"hi"}`)
	originalSource := []byte(`{"model":"gemini-3.6-flash","reasoning":{"summary":null},"input":"hi"}`)
	body := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)

	out, err := ApplyThinkingWithSourcePayload(
		body,
		currentSource,
		originalSource,
		"gemini-3.6-flash",
		sdktranslator.FormatOpenAIResponse.String(),
		sdktranslator.FormatGemini.String(),
		"gemini",
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSourcePayload() error = %v", err)
	}
	include := gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts")
	if !include.Exists() || include.Bool() {
		t.Fatalf("original disabled summary was not preserved: %s", out)
	}
}

func TestApplyThinkingWithSourcePayloadAntigravityResponsesSummary(t *testing.T) {
	source := []byte(`{"model":"gemini-3.8-flash-high","reasoning":{"effort":"low","summary":"auto"},"input":"hi"}`)
	translated := sdktranslator.TranslateRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatAntigravity,
		"gemini-3.8-flash-high",
		source,
		false,
	)

	out, err := ApplyThinkingWithSourcePayload(
		translated,
		source,
		source,
		"gemini-3.8-flash-high",
		sdktranslator.FormatOpenAIResponse.String(),
		sdktranslator.FormatAntigravity.String(),
		"antigravity",
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithSourcePayload() error = %v", err)
	}
	include := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
	if !include.Exists() || !include.Bool() {
		t.Fatalf("includeThoughts not enabled: %s", out)
	}
	if level := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String(); level != "low" {
		t.Fatalf("thinkingLevel = %q, want low: %s", level, out)
	}
}

func TestCurrentSourceConfigurationUpdateIntent(t *testing.T) {
	const original = `{"reasoning":{"effort":"xhigh","summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"ok"}]}`
	for _, tc := range []struct {
		name, model, format, current, body, path, want string
		supported, same                                bool
	}{
		{name: "unknown source entry", model: "synthetic-unknown", format: "codex", current: original, body: `{"reasoning":{"effort":"xhigh","summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"medium"}},{"role":"user","content":"ok"}]}`, path: "reasoning.effort", want: "low"},
		{name: "unknown cross protocol", model: "synthetic-unknown", format: "openai", current: original, body: `{"reasoning_effort":"xhigh","messages":[{"role":"user","content":"ok"}]}`, path: "reasoning_effort", want: "low"},
		{name: "unknown nonarray target input remains intact", model: "synthetic-unknown", format: "codex", current: original, body: `{"reasoning":{"summary":"auto"},"input":{"type":"configuration_update"}}`, path: "reasoning.effort", want: "low"},
		{name: "current source overrides original", model: "synthetic-unknown", format: "openai", current: `{"reasoning":{"effort":"xhigh"},"input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":"ok"}]}`, body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}]}`, path: "reasoning_effort", want: "high"},
		{name: "missing current recovers original", model: "synthetic-unknown", format: "openai", body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}]}`, path: "reasoning_effort", want: "low"},
		{name: "suffix overrides source", model: "synthetic-unknown(high)", format: "openai", current: original, body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}]}`, path: "reasoning_effort", want: "high"},
		{name: "registry native body preserved", model: "gpt-6-astra", format: "codex", current: original, body: original, supported: true, same: true, path: "reasoning.effort", want: "xhigh"},
		{name: "source effort absent does not restore original", model: "synthetic-unknown", format: "openai", current: `{"input":[{"role":"user","content":"ok"}]}`, body: `{"reasoning_effort":"medium","messages":[{"role":"user","content":"ok"}]}`, path: "reasoning_effort", want: "medium"},
	} {
		for _, bound := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
				request := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.current)}
				if bound {
					request.Metadata = map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{
						ID: "synthetic", Type: tc.format, SupportConfigurationUpdate: tc.supported,
						Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}},
					}}
				}
				out, err := ApplyThinkingWithSourcePayload([]byte(tc.body), []byte(tc.current), []byte(original), tc.model, "openai-response", tc.format, tc.format, request)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(out, tc.path).String(); got != tc.want {
					t.Fatalf("effort = %q, want %q; body=%s", got, tc.want, out)
				}
				if tc.same && string(out) != tc.body {
					t.Fatalf("native body changed: %s", out)
				}
				if input := gjson.GetBytes([]byte(tc.body), "input"); input.IsObject() && gjson.GetBytes(out, "input").Raw != input.Raw {
					t.Fatalf("nonarray target input changed: %s", out)
				}
			})
		}
	}
}

type configurationUpdateEditingHooks struct {
	summaryRemovingPluginHooks
	effort string
	calls  int
}

func (h *configurationUpdateEditingHooks) NormalizeRequest(_ context.Context, _, _ sdktranslator.Format, _ string, body []byte, _ bool) []byte {
	h.calls++
	if gjson.GetBytes(body, "input.0.type").String() != "configuration_update" {
		return body
	}
	if h.effort == "" {
		out, _ := sjson.DeleteBytes(body, "input.0")
		return out
	}
	out, _ := sjson.SetBytes(body, "input.0.reasoning.effort", h.effort)
	return out
}

func TestConfigurationUpdateNormalizerIntentReachesThinking(t *testing.T) {
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hi"}]}`
	for _, tc := range []struct{ name, edit, want string }{
		{name: "delete", want: "medium"},
		{name: "rewrite", edit: "high", want: "high"},
	} {
		for _, bound := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
				hooks := &configurationUpdateEditingHooks{effort: tc.edit}
				ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
				request := cliproxyexecutor.Request{Model: "synthetic", Payload: []byte(source)}
				if bound {
					request.Metadata = map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{ID: "synthetic", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}}}
				}
				before := bytes.Clone(request.Payload)
				_, translated, err := TranslateRequestPairWithAPIKeyModelCompatibilityChecked(ctx, nil, nil, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, request.Model, request.Payload, request.Payload, false, false, &request)
				if err != nil {
					t.Fatal(err)
				}
				out, err := ApplyThinkingWithSourcePayload(translated, request.Payload, request.Payload, request.Model, "openai-response", "codex", "codex", request)
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(out, "reasoning.effort").String(); got != tc.want {
					t.Fatalf("normalizer effort = %q, want %q: %s", got, tc.want, out)
				}
				if !bytes.Equal(request.Payload, before) {
					t.Fatal("source payload mutated")
				}
			})
		}
	}
}

func TestConfigurationUpdateRequestPairWorkingIntentAndBufferOwnership(t *testing.T) {
	const original = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hi"}]}`
	for _, stream := range []bool{false, true} {
		for _, compat := range []bool{false, true} {
			for _, distinct := range []bool{false, true} {
				hooks := &configurationUpdateEditingHooks{}
				ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
				payload := []byte(original)
				if distinct {
					payload = []byte(`{"input":[{"role":"user","content":"different"}]}`)
				}
				shared := map[string]any{configurationUpdatesChangedMetadataKey: true, "marker": "source"}
				request := cliproxyexecutor.Request{Model: "synthetic", Payload: payload, Metadata: shared}
				before := bytes.Clone(payload)
				baseline, work, err := TranslateRequestPairWithAPIKeyModelCompatibilityChecked(ctx, nil, nil, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, request.Model, []byte(original), payload, stream, compat, &request)
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if distinct {
					wantCalls = 2
				}
				if hooks.calls != wantCalls || request.Metadata[configurationUpdatesChangedMetadataKey] != !distinct {
					t.Fatalf("stream=%v compat=%v distinct=%v: calls=%d, intent=%v", stream, compat, distinct, hooks.calls, request.Metadata[configurationUpdatesChangedMetadataKey])
				}
				if !distinct && !bytes.Equal(baseline, work) {
					t.Fatal("same input produced different plugin results")
				}
				baselineBefore := bytes.Clone(baseline)
				work[0] = 'X'
				if !bytes.Equal(baseline, baselineBefore) || !bytes.Equal(payload, before) || shared[configurationUpdatesChangedMetadataKey] != true || request.Metadata["marker"] != "source" {
					t.Fatal("working request changed source buffer, baseline or shared metadata")
				}
			}
		}
	}
}

func TestUnknownClaudeConfigurationUpdateReachesPreparedRequest(t *testing.T) {
	const model = "unlisted-claude-configuration-update"
	const source = `{"model":"unlisted-claude-configuration-update","reasoning":{"effort":"xhigh"},"input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":"hello"}]}`
	if registry.LookupModelInfo(model, "claude") != nil {
		t.Fatal("fixture requires a Claude model without registered capabilities")
	}
	for _, target := range []string{"translated-manual", "adaptive"} {
		t.Run(target, func(t *testing.T) {
			body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"max_tokens":4096}`)
			if target == "translated-manual" {
				_, translated, err := TranslateRequestPairWithAPIKeyModelCompatibilityChecked(
					t.Context(), nil, nil, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude,
					model, []byte(source), []byte(source), false, false,
				)
				if err != nil {
					t.Fatal(err)
				}
				body = translated
				if got := gjson.GetBytes(body, "thinking.type").String(); got != "enabled" {
					t.Fatalf("translated thinking type = %s, want enabled", got)
				}
			}
			out, err := ApplyThinkingWithSourcePayload(body, []byte(source), []byte(source), model,
				sdktranslator.FormatOpenAIResponse.String(), sdktranslator.FormatClaude.String(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			if target == "adaptive" {
				if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
					t.Fatalf("adaptive effort = %s, want high from current configuration update", got)
				}
				if gjson.GetBytes(out, "thinking.type").String() != "adaptive" || gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
					t.Fatal("adaptive configuration update changed thinking mode")
				}
			} else if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != 24576 {
				t.Fatalf("manual budget = %d, want 24576 from current configuration update", got)
			}
		})
	}
}
