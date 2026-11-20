package executor

import (
	"context"
	"io"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestModelCatalogSpeedReachesFinalProviderPayload(t *testing.T) {
	for _, test := range []struct {
		model, channel, mode, protocol, field, expected string
	}{
		{"gpt-6.2-astra", "codex", "ultrafast", "codex", "service_tier", "ultrafast"},
		{"gpt-6.2-sol", "codex", "fast", "codex", "service_tier", "priority"},
		{"gpt-6.2-astra", "openai-compatibility", "ultrafast", "openai-response", "service_tier", "ultrafast"},
		{"gpt-6.2-astra", "openai-compatibility", "ultrafast", "openai", "service_tier", "original"},
		{"gpt-5.5", "codex", "fast", "openai-response", "service_tier", "priority"},
		{"gpt-6.1-terra", "codex", "standard", "codex", "service_tier", "default"},
		{"gpt-6.2-astra", "openai-compatibility", "standard", "openai-response", "service_tier", "default"},
		{"gpt-6.2-astra", "codex", "inherit", "codex", "service_tier", "original"},
		{"claude-opus-5-5", "claude", "fast", "claude", "speed", "fast"},
		{"claude-fable-5-2", "claude", "standard", "claude", "speed", "standard"},
		{"claude-haiku-5-5", "claude", "fast", "claude", "speed", "original"},
	} {
		t.Run(
			test.model+"/"+test.mode, func(t *testing.T) {
				cfg := &config.Config{
					SDKConfig: config.SDKConfig{
						ModelCatalog: config.ModelCatalogConfig{
							Models: []config.ModelCatalogModel{
								{
									Name: test.model, Channel: test.channel, Speed: test.mode,
								},
							},
						},
					},
				}
				body := []byte(`{"service_tier":"original","speed":"original"}`)
				out := applyPayloadConfigWithRequest(
					cfg, test.model, test.protocol, "openai", "", body, body,
					test.model, "/v1/responses", nil, test.channel,
				)
				if got := gjson.GetBytes(out, test.field).String(); got != test.expected {
					t.Fatalf("final payload = %s; %s = %q, want %q", out, test.field, got, test.expected)
				}
			},
		)
	}
}

func TestModelCatalogSpeedKeepsExplicitPayloadOverridePriority(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ModelCatalog: config.ModelCatalogConfig{
				Models: []config.ModelCatalogModel{{Name: "gpt-6.2-astra", Channel: "codex", Speed: "ultrafast"}},
			},
		}, Payload: config.PayloadConfig{
			Override: []config.PayloadRule{
				{
					Models: []config.PayloadModelRule{{Name: "gpt-6.2-astra"}},
					Params: map[string]any{"service_tier": "priority"},
				},
			},
		},
	}
	out := applyPayloadConfigWithRequest(
		cfg, "gpt-6.2-astra", "codex", "openai-response", "", []byte(`{}`), nil,
		"gpt-6.2-astra", "/v1/responses", nil, "codex",
	)
	if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
		t.Fatalf("explicit payload override was changed: %s", out)
	}
}

func TestModelCatalogSpeedSkipsNonInferenceEndpoints(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ModelCatalog: config.ModelCatalogConfig{
				Models: []config.ModelCatalogModel{{Name: "gpt-6.2-astra", Channel: "codex", Speed: "ultrafast"}},
			},
		},
	}
	for _, path := range []string{
		"/v1/messages/count_tokens", "/v1/models/model:countTokens", "/v1/responses/compact",
	} {
		body := []byte(`{"service_tier":"original"}`)
		out := applyPayloadConfigWithRequest(
			cfg, "gpt-6.2-astra", "codex", "codex", "", body, body,
			"gpt-6.2-astra", path, nil, "codex",
		)
		if string(out) != string(body) {
			t.Fatalf("%s received inference speed: %s", path, out)
		}
	}
}

func TestModelCatalogSpeedReachesPreparedClaudeRequest(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ModelCatalog: config.ModelCatalogConfig{
				Models: []config.ModelCatalogModel{{Name: "claude-opus-5-5", Channel: "claude", Speed: "fast"}},
			},
		},
	}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "synthetic-speed", "base_url": "https://example.com",
		},
	}
	for _, stream := range []bool{false, true} {
		body := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
		request, err := executor.prepareClaudeRequest(
			context.Background(), auth,
			cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: body},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, stream,
		)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(request.httpReq.Body)
		if err != nil {
			t.Fatal(err)
		}
		if speed := gjson.GetBytes(payload, "speed").String(); speed != "fast" {
			t.Fatalf("stream=%t final speed = %q; body=%s", stream, speed, payload)
		}
	}
}
