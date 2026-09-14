package helps

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/gemini"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
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
