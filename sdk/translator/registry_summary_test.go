package translator

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type summaryPluginHooks struct {
	normalizeRequest     func([]byte) []byte
	requestTranslateBody []byte
	requestTranslateOK   bool
}

func (h summaryPluginHooks) NormalizeRequest(
	_ context.Context,
	_, _ Format,
	_ string,
	body []byte,
	_ bool,
) []byte {
	if h.normalizeRequest == nil {
		return body
	}
	return h.normalizeRequest(body)
}

func (h summaryPluginHooks) TranslateRequest(
	_ context.Context,
	_, _ Format,
	_ string,
	_ []byte,
	_ bool,
) ([]byte, bool) {
	return h.requestTranslateBody, h.requestTranslateOK
}

func (summaryPluginHooks) NormalizeResponseBefore(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	return body
}

func (summaryPluginHooks) TranslateResponse(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, _ []byte,
	_ bool,
) ([]byte, bool) {
	return nil, false
}

func (summaryPluginHooks) NormalizeResponseAfter(
	_ context.Context,
	_, _ Format,
	_ string,
	_, _, body []byte,
	_ bool,
) []byte {
	return body
}

func TestRegistryTranslateRequestAppliesSummaryIntent(t *testing.T) {
	registry := NewRegistry()
	registry.Register(
		FormatOpenAIResponse,
		FormatGemini,
		func(_ string, _ []byte, _ bool) []byte {
			return []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)
		},
		ResponseTransform{},
	)

	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatGemini,
		"gemini-3-flash",
		[]byte(`{"reasoning":{"effort":"high","summary":"auto"}}`),
		false,
	)
	if !gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("translated request lost enabled summary: %s", out)
	}
}

func TestRegistryChatEffortDoesNotForceClaudeSummaryDisplay(t *testing.T) {
	registry := NewRegistry()
	registry.Register(FormatOpenAI, FormatClaude, func(_ string, _ []byte, _ bool) []byte {
		return []byte(`{"model":"claude-sonnet-5","thinking":{"type":"adaptive"}}`)
	}, ResponseTransform{})
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "effort only", body: `{"reasoning_effort":"high"}`},
		{name: "explicit summary", body: `{"reasoning_effort":"high","reasoning":{"summary":"auto"}}`, want: "summarized"},
		{name: "explicit exclusion", body: `{"reasoning_effort":"high","reasoning":{"exclude":true}}`, want: "omitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := registry.TranslateRequest(FormatOpenAI, FormatClaude, "claude-sonnet-5", []byte(tc.body), false)
			if got := gjson.GetBytes(out, "thinking.display").String(); got != tc.want {
				t.Fatalf("thinking.display = %q, want %q; body=%s", got, tc.want, out)
			}
		})
	}
}

func TestRegistryTranslateRequestDoesNotActivateClaudeForDisabledSummary(t *testing.T) {
	registry := NewRegistry()
	registry.Register(
		FormatOpenAIResponse,
		FormatClaude,
		func(_ string, _ []byte, _ bool) []byte {
			return []byte(`{"model":"claude-opus-5","max_tokens":32000}`)
		},
		ResponseTransform{},
	)

	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatClaude,
		"claude-opus-5",
		[]byte(`{"reasoning":{"summary":null},"input":"hi"}`),
		false,
	)
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("disabled summary activated Claude thinking: %s", out)
	}
}

func TestRegistryTranslateRequestDoesNotMixSummaryIntoFallback(t *testing.T) {
	registry := NewRegistry()
	body := []byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`)
	out := registry.TranslateRequest(
		FormatOpenAIResponse,
		FormatGemini,
		"gemini-3-flash",
		body,
		false,
	)
	if !bytes.Equal(out, body) {
		t.Fatalf("missing translator changed payload: got %s, want %s", out, body)
	}
}

func TestRegistryTranslateRequestPluginNormalizerOwnsSummaryIntent(t *testing.T) {
	registry := NewRegistry()
	hooks := summaryPluginHooks{
		normalizeRequest: func(body []byte) []byte {
			out, _ := sjson.DeleteBytes(body, "reasoning.summary")
			return out
		},
		requestTranslateBody: []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`),
		requestTranslateOK:   true,
	}
	ctx := WithPluginHooks(context.Background(), hooks)

	out := registry.TranslateRequestWithContext(
		ctx,
		FormatOpenAIResponse,
		FormatGemini,
		"gemini-3-flash",
		[]byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`),
		false,
	)
	if gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts").Exists() {
		t.Fatalf("source normalizer removal was overridden: %s", out)
	}
}

func TestRegistryTranslateRequestTargetNormalizerOwnsFinalSummary(t *testing.T) {
	registry := NewRegistry()
	registry.Register(
		FormatOpenAIResponse,
		FormatGemini,
		func(_ string, _ []byte, _ bool) []byte {
			return []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`)
		},
		ResponseTransform{},
	)
	hooks := summaryPluginHooks{
		normalizeRequest: func(body []byte) []byte {
			if !gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Bool() {
				t.Fatalf("normalizer did not receive canonical summary: %s", body)
			}
			out, _ := sjson.DeleteBytes(body, "generationConfig.thinkingConfig.includeThoughts")
			return out
		},
	}
	ctx := WithPluginHooks(context.Background(), hooks)

	out := registry.TranslateRequestWithContext(
		ctx,
		FormatOpenAIResponse,
		FormatGemini,
		"gemini-3-flash",
		[]byte(`{"reasoning":{"summary":"auto"},"input":"hi"}`),
		false,
	)
	if gjson.GetBytes(out, "generationConfig.thinkingConfig.includeThoughts").Exists() {
		t.Fatalf("target normalizer removal was overridden: %s", out)
	}
}
