package executor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type executorTranslatorHooks struct {
	requestCalls atomic.Int32
}

func (h *executorTranslatorHooks) NormalizeRequest(
	context.Context,
	sdktranslator.Format,
	sdktranslator.Format,
	string,
	[]byte,
	bool,
) []byte {
	h.requestCalls.Add(1)
	return []byte(`{"model":"gpt-5.5","input":[],"plugin_normalized":true}`)
}

func (*executorTranslatorHooks) TranslateRequest(
	_ context.Context,
	_, _ sdktranslator.Format,
	_ string,
	body []byte,
	_ bool,
) ([]byte, bool) {
	return body, true
}

func (*executorTranslatorHooks) NormalizeResponseBefore(
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

func (*executorTranslatorHooks) TranslateResponse(
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

func (*executorTranslatorHooks) NormalizeResponseAfter(
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

func TestCodexResponsesBodyUsesRequestScopedTranslatorHooks(t *testing.T) {
	hooks := &executorTranslatorHooks{}
	ctx := sdktranslator.WithPluginHooks(context.Background(), hooks)
	prepared, err := codexResponsesBody(
		ctx,
		&config.Config{},
		&cliproxyauth.Auth{},
		"gpt-5.5",
		"gpt-5.5",
		"codex",
		sdktranslator.FormatOpenAIResponse,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[]}`),
		},
		false,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody() error = %v", err)
	}
	if hooks.requestCalls.Load() != 2 {
		t.Fatalf("request hook calls = %d, want 2", hooks.requestCalls.Load())
	}
	if !gjson.GetBytes(prepared.Body, "plugin_normalized").Bool() {
		t.Fatalf("prepared body did not retain request hook output: %s", prepared.Body)
	}
}

func TestXAICountTokensUsesRequestScopedTranslatorHooks(t *testing.T) {
	hooks := &executorTranslatorHooks{}
	ctx := sdktranslator.WithPluginHooks(context.Background(), hooks)
	executor := NewXAIExecutor(&config.Config{})
	_, err := executor.CountTokens(
		ctx,
		nil,
		cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if hooks.requestCalls.Load() != 2 {
		t.Fatalf("request hook calls = %d, want 2", hooks.requestCalls.Load())
	}
}
