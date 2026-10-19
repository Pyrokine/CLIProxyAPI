package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiFinalPayloadConsumers(t *testing.T) {
	for _, route := range []string{"generate", "stream"} {
		t.Run("leading-user/"+route, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gemini-3.7-flash", Protocol: "gemini"}},
				Params: map[string]any{"contents.0.parts.0.text": "payload override"},
			}}}}
			payload := []byte(`{"contents":[{"role":"model","parts":[{"text":"prior output"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
			body := googlePayloadCall(t, "gemini", route, "gemini-3.7-flash", cfg, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
			contents := gjson.GetBytes(body, "contents").Array()
			if len(contents) != 3 || contents[0].Get("role").String() != "user" || contents[1].Get("role").String() != "model" || contents[2].Get("role").String() != "user" {
				t.Fatalf("roles=%s", body)
			}
			if text := contents[0].Get("parts.0.text"); !text.Exists() || text.String() != "payload override" {
				t.Fatalf("final leading user=%s", body)
			}
			if contents[1].Get("parts.0.text").String() != "prior output" || contents[2].Get("parts.0.text").String() != "continue" {
				t.Fatalf("history changed: %s", body)
			}
		})
	}
	for _, provider := range []string{"gemini", "vertex"} {
		t.Run("source-protocol/"+provider, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gemini-3.1-flash-lite", Protocol: "gemini", FromProtocol: "openai"}},
				Params: map[string]any{"contents.0.parts.0.text": "configured"},
			}}}}
			body := googlePayloadCall(t, provider, "generate", "gemini-3.1-flash-lite", cfg, []byte(`{"messages":[{"role":"user","content":"original"}]}`), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
			if got := gjson.GetBytes(body, "contents.0.parts.0.text").String(); got != "configured" {
				t.Fatalf("text=%q want configured", got)
			}
		})
		t.Run("token-content/"+provider, func(t *testing.T) {
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "gemini-3.1-flash-lite", Protocol: "gemini"}},
				Params: map[string]any{"contents.0.parts.0.text": "configured"},
			}}}}
			body := googlePayloadCall(t, provider, "count", "gemini-3.1-flash-lite", cfg, []byte(`{"contents":[{"role":"user","parts":[{"text":"original"}]}]}`), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini})
			if got := gjson.GetBytes(body, "contents.0.parts.0.text").String(); got != "configured" {
				t.Fatalf("text=%q want configured", got)
			}
		})
	}
}
