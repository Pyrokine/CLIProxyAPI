package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAIStudioTranslateRequestNormalizesThinkingLevel(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model: "gemini-3.7-flash",
			Payload: []byte(`{
				"contents":[{"role":"user","parts":[{"text":"hi"}]}],
				"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	if got := gjson.GetBytes(payload, "generationConfig.thinkingConfig.thinkingLevel").String(); got != "HIGH" {
		t.Fatalf("thinkingLevel = %q, want HIGH; payload=%s", got, payload)
	}
	if !gjson.GetBytes(payload, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("includeThoughts was lost: %s", payload)
	}
}

func TestNormalizeAIStudioThinkingLevel(t *testing.T) {
	tests := []struct {
		name      string
		levelJSON string
		want      string
		unchanged bool
	}{
		{name: "minimal", levelJSON: `"minimal"`, want: "MINIMAL"},
		{name: "low", levelJSON: `"low"`, want: "LOW"},
		{name: "medium", levelJSON: `"medium"`, want: "MEDIUM"},
		{name: "mixed case high", levelJSON: `"hIgH"`, want: "HIGH"},
		{name: "already uppercase", levelJSON: `"HIGH"`, want: "HIGH", unchanged: true},
		{name: "unsupported max", levelJSON: `"max"`, want: "max", unchanged: true},
		{name: "non-string", levelJSON: `1`, want: "1", unchanged: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":%s}},"marker":true}`, test.levelJSON))
			output := normalizeAIStudioThinkingLevel(input)
			if got := gjson.GetBytes(output, "generationConfig.thinkingConfig.thinkingLevel").String(); got != test.want {
				t.Fatalf("thinkingLevel = %q, want %q; payload=%s", got, test.want, output)
			}
			if test.unchanged && string(output) != string(input) {
				t.Fatalf("payload changed from %s to %s", input, output)
			}
		})
	}
}

func TestAIStudioTranslateRequestNormalizesGeminiBoundaryTurns(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model:   "gemini-3.7-flash",
			Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	assertGeminiBoundaryRoles(t, payload, []string{"user", "model", "user"})
}

func TestAIStudioTranslateRequestCountTokensPreservesTrailingModelTurn(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	payload, _, errTranslate := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{
			Model: "gemini-3.7-flash",
			Payload: []byte(`{"contents":[` +
				`{"role":"user","parts":[{"text":"hello"}]},` +
				`{"role":"model","parts":[{"text":"answer"}]}` +
				`]}`),
			Metadata: map[string]any{"action": "countTokens"},
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	assertGeminiBoundaryRoles(t, payload, []string{"user", "model"})
}

func assertGeminiBoundaryRoles(t *testing.T, payload []byte, want []string) {
	t.Helper()
	contents := gjson.GetBytes(payload, "contents").Array()
	if len(contents) != len(want) {
		t.Fatalf("contents len = %d, want %d; payload=%s", len(contents), len(want), payload)
	}
	for index, role := range want {
		if got := contents[index].Get("role").String(); got != role {
			t.Fatalf("contents[%d].role = %q, want %q; payload=%s", index, got, role, payload)
		}
	}
}
