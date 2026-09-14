package executor

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAIStudioTranslateRequestPreservesSummaryFromOriginalRequest(t *testing.T) {
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)
	req := cliproxyexecutor.Request{
		Model:   "gemini-3.6-flash",
		Payload: []byte(`{"model":"gemini-3.6-flash","input":"hi"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
		OriginalRequest: []byte(`{"model":"gemini-3.6-flash","reasoning":{"summary":"auto"},"input":"hi"}`),
	}

	payload, _, errTranslate := executor.translateRequest(context.Background(), req, opts, false)
	if errTranslate != nil {
		t.Fatalf("translateRequest() error = %v", errTranslate)
	}
	if !gjson.GetBytes(payload, "generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("original request summary intent was lost: %s", payload)
	}
}
