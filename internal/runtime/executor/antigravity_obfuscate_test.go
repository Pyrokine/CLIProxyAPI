package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAntigravitySensitiveWordsObfuscatesSystemInstructionOnly(t *testing.T) {
	executor := NewAntigravityExecutor(
		&config.Config{
			Antigravity: config.AntigravityConfig{SensitiveWords: []string{"proxy"}},
		},
	)
	payload := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"Use proxy safely"}]},"system_instruction":"proxy fallback","contents":[{"role":"user","parts":[{"text":"proxy remains unchanged"}]}]}}`)

	got := executor.obfuscateSensitiveWords(payload)
	if text := gjson.GetBytes(
		got, "request.systemInstruction.parts.0.text",
	).String(); text != "Use p"+zeroWidthSpace+"roxy safely" {
		t.Fatalf("system instruction = %q, want zero-width obfuscation", text)
	}
	if text := gjson.GetBytes(got, "request.system_instruction").String(); text != "p"+zeroWidthSpace+"roxy fallback" {
		t.Fatalf("snake-case system instruction = %q, want zero-width obfuscation", text)
	}
	if text := gjson.GetBytes(got, "request.contents.0.parts.0.text").String(); text != "proxy remains unchanged" {
		t.Fatalf("content text = %q, want unchanged", text)
	}
}

func TestAntigravityStreamObfuscatesSensitiveSystemInstruction(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Errorf("read request body: %v", errRead)
					return
				}
				captured <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
			},
		),
	)
	defer server.Close()

	executor := NewAntigravityExecutor(
		&config.Config{
			Antigravity:  config.AntigravityConfig{SensitiveWords: []string{"Hermes", "Nous Research"}},
			RequestRetry: 1,
		},
	)
	result, errExecute := executor.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{
			Metadata: map[string]any{
				"access_token": "token-123",
				"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
				"project_id":   "project-1",
			},
			Attributes: map[string]string{"base_url": server.URL},
		},
		cliproxyexecutor.Request{
			Model:   "gemini-3.6-flash-high",
			Payload: []byte(`{"model":"gemini-3.6-flash-high","instructions":"You are Hermes Agent, an intelligent AI assistant created by Nous Research.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatOpenAIResponse,
			ResponseFormat: sdktranslator.FormatOpenAIResponse,
			Stream:         true,
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	body := <-captured
	got := gjson.GetBytes(body, "request.systemInstruction.parts.0.text").String()
	want := "You are H" + zeroWidthSpace + "ermes Agent, an intelligent AI assistant created by N" + zeroWidthSpace + "ous Research."
	if got != want {
		t.Fatalf("system instruction = %q, want %q; body=%s", got, want, body)
	}
}
