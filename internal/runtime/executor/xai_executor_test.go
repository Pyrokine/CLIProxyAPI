package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIExecutorAppliesThinkingSuffix(t *testing.T) {
	executor := NewXAIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3(low)",
		Payload: []byte(`{"model":"grok-4.3(low)","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	prepared, err := executor.prepareResponsesRequest(req, opts, true)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	if got := gjson.GetBytes(prepared.body, "model").String(); got != "grok-4.3" {
		t.Fatalf("model = %q, want %q, body=%s", got, "grok-4.3", prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want %q, body=%s", got, "low", prepared.body)
	}
}
