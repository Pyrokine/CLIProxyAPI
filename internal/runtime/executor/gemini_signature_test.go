package executor

import (
	"context"
	"encoding/base64"
	"io"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validGeminiExecutorThoughtSignature() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestPrepareGeminiRequestSanitizesThoughtSignatures(t *testing.T) {
	payload := []byte(`{
		"model":"gemini-3-pro",
		"contents":[{
			"role":"model",
			"parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"not-gemini"}]
		}]
	}`)
	executor := NewGeminiExecutor(&config.Config{})

	prepared, err := executor.prepareGeminiRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "gemini-3-pro", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")},
		false,
	)
	if err != nil {
		t.Fatalf("prepareGeminiRequest error: %v", err)
	}
	body, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}

	if got := gjson.GetBytes(
		body, "contents.0.parts.0.thoughtSignature",
	).String(); got != "skip_thought_signature_validator" {
		t.Fatalf("thoughtSignature = %q, want skip sentinel. Body: %s", got, string(body))
	}
}

func TestPrepareGeminiRequestPreservesValidThoughtSignatures(t *testing.T) {
	signature := validGeminiExecutorThoughtSignature()
	payload := []byte(`{
		"model":"gemini-3-pro",
		"contents":[{
			"role":"model",
			"parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"` + signature + `"}]
		}]
	}`)
	executor := NewGeminiExecutor(&config.Config{})

	prepared, err := executor.prepareGeminiRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "gemini-3-pro", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")},
		false,
	)
	if err != nil {
		t.Fatalf("prepareGeminiRequest error: %v", err)
	}
	body, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}

	if got := gjson.GetBytes(body, "contents.0.parts.0.thoughtSignature").String(); got != signature {
		t.Fatalf("thoughtSignature = %q, want %q. Body: %s", got, signature, string(body))
	}
}

func TestAIStudioTranslateRequestSanitizesThoughtSignatures(t *testing.T) {
	payload := []byte(`{
		"model":"gemini-3-pro",
		"contents":[{
			"role":"model",
			"parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"not-gemini"}]
		}]
	}`)
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", nil)

	translated, _, err := executor.translateRequest(
		context.Background(),
		cliproxyexecutor.Request{Model: "gemini-3-pro", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")},
		false,
	)
	if err != nil {
		t.Fatalf("translateRequest error: %v", err)
	}

	if got := gjson.GetBytes(
		translated, "contents.0.parts.0.thoughtSignature",
	).String(); got != "skip_thought_signature_validator" {
		t.Fatalf("thoughtSignature = %q, want skip sentinel. Body: %s", got, string(translated))
	}
}

func TestPrepareVertexGeminiBodySanitizesThoughtSignatures(t *testing.T) {
	payload := []byte(`{
		"model":"gemini-3-pro",
		"contents":[{
			"role":"model",
			"parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"not-gemini"}]
		}]
	}`)
	executor := NewGeminiVertexExecutor(&config.Config{})

	prepared, err := executor.prepareVertexGeminiBody(
		context.Background(),
		cliproxyexecutor.Request{Model: "gemini-3-pro", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")},
		"gemini-3-pro",
		false,
	)
	if err != nil {
		t.Fatalf("prepareVertexGeminiBody error: %v", err)
	}

	if got := gjson.GetBytes(
		prepared.body, "contents.0.parts.0.thoughtSignature",
	).String(); got != "skip_thought_signature_validator" {
		t.Fatalf("thoughtSignature = %q, want skip sentinel. Body: %s", got, string(prepared.body))
	}
}
