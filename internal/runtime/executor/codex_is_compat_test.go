package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexResponsesBodyUsesResolvedModelIsCompat(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Headers:      http.Header{"User-Agent": []string{"codex-tui/0.145.0"}},
	}
	payload := []byte(`{"model":"compat-model","input":[{"type":"agent_message",` +
		`"author":"/root","recipient":"/root/worker",` +
		`"internal_chat_message_metadata_passthrough":{"turn_id":"one"},` +
		`"content":[{"type":"encrypted_content","encrypted_content":"task"}]},` +
		`{"type":"message","role":"user","author":"/root/worker","recipient":"/root",` +
		`"internal_chat_message_metadata_passthrough":{"turn_id":"two"},"content":"reply"}]}`)

	build := func(isCompat bool) codexResponsesBodyResult {
		t.Helper()
		req := cliproxyexecutor.Request{
			Model:   "compat-model",
			Payload: payload,
			Metadata: map[string]any{
				"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: isCompat},
			},
		}
		prepared, err := codexResponsesBody(
			context.Background(),
			cfg,
			&cliproxyauth.Auth{},
			"compat-model",
			"compat-model",
			"codex",
			sdktranslator.FormatOpenAIResponse,
			opts,
			req,
			false, false,
		)
		if err != nil {
			t.Fatalf("codexResponsesBody() error = %v", err)
		}
		return prepared
	}

	native := build(false)
	if got := gjson.GetBytes(native.Body, "input.0.type").String(); got != "agent_message" {
		t.Fatalf("non-compat input type = %q, want agent_message", got)
	}
	if !gjson.GetBytes(native.Body, "input.0.author").Exists() ||
		!gjson.GetBytes(native.Body, "input.1.internal_chat_message_metadata_passthrough").Exists() {
		t.Fatalf("native Codex metadata was removed: %s", native.Body)
	}
	compat := build(true)
	if got := gjson.GetBytes(compat.Body, "input.0.type").String(); got != "message" {
		t.Fatalf("compat input type = %q, want message; output=%s", got, compat.Body)
	}
	if got := gjson.GetBytes(compat.Body, "input.0.content.0.text").String(); got != "task" {
		t.Fatalf("compat input text = %q, want task; output=%s", got, compat.Body)
	}
	for _, path := range []string{
		"input.0.author", "input.0.recipient", "input.0.internal_chat_message_metadata_passthrough",
		"input.1.author", "input.1.recipient", "input.1.internal_chat_message_metadata_passthrough",
	} {
		if gjson.GetBytes(compat.Body, path).Exists() {
			t.Fatalf("compat request retained %s: %s", path, compat.Body)
		}
	}

	cfg.Codex.OptimizeMultiAgentV2 = false
	compatWithoutOptimization := build(true)
	if got := gjson.GetBytes(compatWithoutOptimization.Body, "input.0.type").String(); got != "message" {
		t.Fatalf("disabled optimization skipped compat normalization: type=%q body=%s", got, compatWithoutOptimization.Body)
	}
	if gjson.GetBytes(compatWithoutOptimization.Body, "input.0.role").String() != "user" ||
		gjson.GetBytes(compatWithoutOptimization.Body, "input.0.content.0.type").String() != "input_text" ||
		gjson.GetBytes(compatWithoutOptimization.Body, "input.0.content.0.text").String() != "task" ||
		gjson.GetBytes(compatWithoutOptimization.Body, "input.0.content.0.encrypted_content").Exists() {
		t.Fatalf("disabled optimization lost compat task content: %s", compatWithoutOptimization.Body)
	}
	if gjson.GetBytes(compatWithoutOptimization.Body, "input.0.author").Exists() ||
		gjson.GetBytes(compatWithoutOptimization.Body, "input.1.internal_chat_message_metadata_passthrough").Exists() {
		t.Fatalf("disabled optimization retained compat metadata: %s", compatWithoutOptimization.Body)
	}
	nativeWithoutOptimization := build(false)
	if gjson.GetBytes(nativeWithoutOptimization.Body, "input.0.type").String() != "agent_message" ||
		!gjson.GetBytes(nativeWithoutOptimization.Body, "input.0.author").Exists() {
		t.Fatalf("disabled optimization changed native Codex input: %s", nativeWithoutOptimization.Body)
	}
}

func TestCodexResponsesBodyAppliesIndependentOrphanDelegationCompatibility(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OrphanDelegationCompatibility: true}}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Headers:      http.Header{"X-Openai-Subagent": []string{"collab_spawn"}},
	}
	payload := []byte(`{
		"model":"compat-model",
		"input":[{
			"type":"function_call_output",
			"namespace":"codex_app",
			"name":"create_thread",
			"call_id":"orphan-call",
			"output":"worker result"
		}]
	}`)
	req := cliproxyexecutor.Request{
		Model:   "compat-model",
		Payload: payload,
		Metadata: map[string]any{
			"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
		},
	}

	prepared, err := codexResponsesBody(
		context.Background(),
		cfg,
		&cliproxyauth.Auth{},
		"compat-model",
		"compat-model",
		"codex",
		sdktranslator.FormatOpenAIResponse,
		opts,
		req,
		false, false,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody() error = %v", err)
	}
	if got := gjson.GetBytes(prepared.Body, "input.0.type").String(); got != "message" {
		t.Fatalf("orphan input type = %q, want message; output=%s", got, prepared.Body)
	}
	wantText := "Tool output from codex_app__create_thread:\nworker result"
	if got := gjson.GetBytes(prepared.Body, "input.0.content.0.text").String(); got != wantText {
		t.Fatalf("orphan input text = %q, want %q; output=%s", got, wantText, prepared.Body)
	}
	if prepared.OptimizedMultiAgentV2 {
		t.Fatal("orphan compatibility enabled multi-agent v2 optimization")
	}
}
