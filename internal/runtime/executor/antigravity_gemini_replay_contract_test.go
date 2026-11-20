package executor

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestSanitizeAntigravityGeminiRequestSignaturesFinalizesParallelCalls(t *testing.T) {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, 0x34})
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, inner)
	nativeSignature := base64.StdEncoding.EncodeToString(encoded)

	tests := []struct {
		name               string
		firstSignature     string
		secondSignature    string
		wantFirstSignature string
	}{
		{
			name:               "synthetic",
			wantFirstSignature: "skip_thought_signature_validator",
		},
		{
			name:               "native",
			firstSignature:     nativeSignature,
			secondSignature:    "skip_thought_signature_validator",
			wantFirstSignature: nativeSignature,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				payload := []byte(`{"request":{"contents":[{"role":"model",` +
					`"parts":[{"functionCall":{"name":"first","args":{}}},` +
					`{"functionCall":{"name":"second","args":{}}}]},{"role":"user",` +
					`"parts":[{"functionResponse":{"name":"first",` +
					`"response":{"result":"ok"}}},` +
					`{"functionResponse":{"name":"second",` +
					`"response":{"result":"ok"}}}]}]}}`)
				if tt.firstSignature != "" {
					payload, _ = sjson.SetBytes(
						payload, "request.contents.0.parts.0.thoughtSignature", tt.firstSignature,
					)
				}
				if tt.secondSignature != "" {
					payload, _ = sjson.SetBytes(
						payload, "request.contents.0.parts.1.thoughtSignature", tt.secondSignature,
					)
				}

				output := sanitizeAntigravityGeminiRequestSignatures("gemini-3.5-flash", payload)
				if got := gjson.GetBytes(
					output, "request.contents.0.parts.0.thoughtSignature",
				).String(); got != tt.wantFirstSignature {
					t.Fatalf("first signature = %q, want %q; output=%s", got, tt.wantFirstSignature, output)
				}
				if signature := gjson.GetBytes(
					output, "request.contents.0.parts.1.thoughtSignature",
				); signature.Exists() {
					t.Fatalf("second parallel call should remain unsigned; output=%s", output)
				}
				if got := gjson.GetBytes(output, "request.contents.1.role").String(); got != "model" {
					t.Fatalf(
						"functionResponse role = %q, want native Antigravity model role; output=%s",
						got,
						output,
					)
				}
			},
		)
	}
}

func TestAntigravityExecutorCountTokensSanitizesGeminiToolHistory(t *testing.T) {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, 0x34})
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, inner)
	nativeSignature := base64.StdEncoding.EncodeToString(encoded)

	var upstreamBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != antigravityCountTokensPath {
					t.Fatalf("path = %q, want %q", r.URL.Path, antigravityCountTokensPath)
				}
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read countTokens body: %v", errRead)
				}
				upstreamBody = append([]byte(nil), body...)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"totalTokens":42}`))
			},
		),
	)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash-high","messages":[{"role":"assistant",` +
		`"content":[{"type":"tool_use","id":"call-1","name":"read",` +
		(`"input":{"file":"one"},"signature":` +
			`"`) +
		nativeSignature +
		(`"},{"type":"tool_use","id":"call-2",` +
			`"name":"read",`) +
		`"input":{"file":"two"},` +
		`"signature":"skip_thought_signature_validator"}]},{"role":"user",` +
		`"content":[{"type":"tool_result","tool_use_id":"call-2",` +
		`"content":"two"},` +
		`{"type":"tool_result","tool_use_id":"call-1","content":"one"}]}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	_, errCount := exec.CountTokens(
		context.Background(),
		antigravityCountTokensAuth(server.URL, "original-count-token-replay"),
		cliproxyexecutor.Request{
			Model:   "gemini-3.6-flash-high",
			Payload: payload,
		},
		cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatClaude,
			ResponseFormat:  sdktranslator.FormatClaude,
			OriginalRequest: payload,
		},
	)
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if len(upstreamBody) == 0 {
		t.Fatal("countTokens upstream body was not captured")
	}
	if gjson.GetBytes(upstreamBody, "request.contents.#").Int() != 3 ||
		gjson.GetBytes(upstreamBody, "request.contents.0.role").String() != "user" {
		t.Fatalf("native leading user boundary is missing: %s", upstreamBody)
	}
	if got := gjson.GetBytes(
		upstreamBody, "request.contents.1.parts.0.thoughtSignature",
	).String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; body=%s", got, upstreamBody)
	}
	if signature := gjson.GetBytes(upstreamBody, "request.contents.1.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("second sibling bypass was not removed: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "request.contents.2.role").String(); got != "model" {
		t.Fatalf("functionResponse role = %q, want model; body=%s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "request.contents.2.parts.0.functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; body=%s", got, upstreamBody)
	}
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(upstreamBody); errPairing != nil {
		t.Fatalf("countTokens tool history is invalid: %v; body=%s", errPairing, upstreamBody)
	}
}

func TestAntigravityExecutorCountTokensReconstructsCompactedClaudeToolCall(t *testing.T) {
	cache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(cache.ClearAntigravityReasoningReplayCache)

	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, 0x34})
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, inner)
	nativeSignature := base64.StdEncoding.EncodeToString(encoded)
	const nativeID = "native-count-token-call"
	const nativeArgs = `{"command":"true"}`
	clientID := util.GeminiClaudeToolUseID(nativeID, "Bash", nativeArgs)
	item := []byte(`{"type":"function_call_part","contentIndex":0,"partIndex":0,` +
		`"targetOccurrence":0,"call_id":"` +
		nativeID +
		`","name":"Bash","args":` +
		nativeArgs +
		`,"thoughtSignature":"` +
		nativeSignature +
		`"}`)
	if !cache.AntigravityReasoningReplayItems(
		"gemini-3.6-flash-high",
		"responses:count-token-replay",
		[][]byte{item},
	) {
		t.Fatal("failed to cache native tool provenance")
	}

	var upstreamBody []byte
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read countTokens body: %v", errRead)
				}
				upstreamBody = append([]byte(nil), body...)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"totalTokens":42}`))
			},
		),
	)
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"session_id":"count-token-replay",` +
		`"messages":[{"role":"user","content":[{"type":"tool_result",` +
		`"tool_use_id":"` + clientID + `","content":"ok"}]}],"tools":[{"name":"Bash",` +
		`"input_schema":{"type":"object",` +
		`"properties":{"command":{"type":"string"}}}}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	_, errCount := exec.CountTokens(
		context.Background(),
		antigravityCountTokensAuth(server.URL, "original-count-token-replay"),
		cliproxyexecutor.Request{
			Model:   "gemini-3.6-flash-high",
			Payload: payload,
		},
		cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatClaude,
			ResponseFormat:  sdktranslator.FormatClaude,
			OriginalRequest: payload,
		},
	)
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if len(upstreamBody) == 0 {
		t.Fatal("countTokens upstream body was not captured")
	}
	if gjson.GetBytes(upstreamBody, "request.contents.#").Int() != 3 ||
		gjson.GetBytes(upstreamBody, "request.contents.0.role").String() != "user" {
		t.Fatalf("native leading user boundary is missing: %s", upstreamBody)
	}
	call := gjson.GetBytes(upstreamBody, "request.contents.1.parts.0")
	if call.Get("functionCall.id").String() != nativeID || call.Get("functionCall.name").String() != "Bash" ||
		call.Get("thoughtSignature").String() != nativeSignature {
		t.Fatalf("native function call provenance was not reconstructed: %s", upstreamBody)
	}
	response := gjson.GetBytes(upstreamBody, "request.contents.2.parts.0.functionResponse")
	if response.Get("id").String() != nativeID || response.Get("name").String() != "Bash" {
		t.Fatalf("native function response provenance was not reconstructed: %s", upstreamBody)
	}
	if strings.Contains(string(upstreamBody), clientID) {
		t.Fatalf("Claude opaque provenance ID leaked upstream: %s", upstreamBody)
	}
	if errPairing := internalsignature.ValidateGeminiFunctionCallPairing(upstreamBody); errPairing != nil {
		t.Fatalf("countTokens compacted tool history is invalid: %v; body=%s", errPairing, upstreamBody)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesLeavesMixedUserContent(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user",` +
		`"parts":[{"functionResponse":{"name":"run",` +
		`"response":{"result":"ok"}}},` +
		`{"text":"user follow-up"}]}]}}`)
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := gjson.GetBytes(output, "request.contents.0.role").String(); got != "user" {
		t.Fatalf("mixed functionResponse/user content role = %q, want user; output=%s", got, output)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesOrdersParallelResponses(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"model",` +
		`"parts":[{"functionCall":{"id":"call-1",` +
		`"name":"read","args":{"file":"one"}}},` +
		`{"functionCall":{"id":"call-2",` +
		`"name":"read","args":{"file":"two"}}}]},{"role":" Model ",` +
		`"parts":[{"functionResponse":{"id":"call-2","name":"read",` +
		`"response":{"result":"two"}}},` +
		`{"functionResponse":{"id":"call-1","name":"read",` +
		`"response":{"result":"one"}}}]}]}}`)
	output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
	if got := gjson.GetBytes(output, "request.contents.1.role").String(); got != "model" {
		t.Fatalf("functionResponse role = %q, want model; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.1.parts.0.functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first functionResponse.id = %q, want call-1; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.1.parts.1.functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second functionResponse.id = %q, want call-2; output=%s", got, output)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate != nil {
		t.Fatalf("normalized parallel responses are invalid: %v; output=%s", errValidate, output)
	}
}

func TestNormalizeAntigravityGeminiFunctionResponseRolesDoesNotCrossEmptyContentBoundary(t *testing.T) {
	for _, boundary := range []string{
		`{"role":"user","parts":[]}`,
		`{"role":"user"}`,
		`{"role":"user","parts":null}`,
	} {
		payload := []byte(`{"request":{"contents":[{"role":"model",` +
			`"parts":[{"functionCall":{"id":"call-1",` +
			`"name":"read","args":{}}},{"functionCall":{"id":"call-2",` +
			`"name":"read",` +
			`"args":{}}}]},` + boundary + `,{"role":"user","parts":[{"functionResponse":{"id":"call-2",` +
			`"name":"read",` +
			`"response":{"result":"two"}}},` +
			`{"functionResponse":{"id":"call-1","name":"read",` +
			`"response":{"result":"one"}}}]}]}}`)
		output := normalizeAntigravityGeminiFunctionResponseRoles(payload)
		if got := gjson.GetBytes(output, "request.contents.2.role").String(); got != "model" {
			t.Fatalf("pure functionResponse role = %q, want model; output=%s", got, output)
		}
		if got := gjson.GetBytes(output, "request.contents.2.parts.0.functionResponse.id").String(); got != "call-2" {
			t.Fatalf(
				"response crossed content boundary %s and was reordered: first id=%q; output=%s",
				boundary,
				got,
				output,
			)
		}
		if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(output); errValidate == nil {
			t.Fatalf("responses crossing content boundary %s were accepted: %s", boundary, output)
		}
	}
}

func TestAntigravityExecutor_GeminiTargetPreservesGeminiThinkingCarrier(t *testing.T) {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, 0x34})
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, inner)
	validSignature := base64.StdEncoding.EncodeToString(encoded)
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text",` +
		`"text":"answer"},` +
		`{"type":"thinking","thinking":"","signature":"` +
		validSignature +
		`"},{"type":"thinking","thinking":"","signature":"invalid"}]}]}`)

	output, err := validateAntigravityRequestSignatures(
		context.Background(),
		"gemini-3.6-flash-high",
		sdktranslator.FormatClaude,
		payload,
	)
	if err != nil {
		t.Fatalf("validateAntigravityRequestSignatures() error = %v", err)
	}
	content := gjson.GetBytes(output, "messages.0.content").Array()
	if len(content) != 2 {
		t.Fatalf("content length = %d, want text plus valid Gemini carrier: %s", len(content), output)
	}
	if got := content[1].Get("signature").String(); got != validSignature {
		t.Fatalf("preserved signature = %q, want Gemini carrier", got)
	}
}
