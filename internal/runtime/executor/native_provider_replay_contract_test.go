package executor

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestNativeSWEAndLegacyField1SignaturesReachEveryExecutorEntry(t *testing.T) {
	legacy := base64.StdEncoding.EncodeToString([]byte{0x0a, 0x02, 0x01, 0x8f, 0x0a, 0x03, 0x01, 0x90, 0x91})
	for _, tc := range []struct {
		name, provider, model, sig, signaturePath string
		format                                    translator.Format
	}{
		{"SWE", "claude", "swe-1.5", "sealed.v1.test", "messages.0.content.0.signature", translator.FormatClaude},
		{
			"Gemini-field1", "gemini", "gemini-2.5-pro", legacy, "contents.1.parts.0.thoughtSignature",
			translator.FormatGemini,
		},
	} {
		for _, operation := range []string{"Execute", "ExecuteStream", "CountTokens"} {
			t.Run(
				tc.name+"/"+operation, func(t *testing.T) {
					cfg := &config.Config{}
					auth := &coreauth.Auth{
						ID: "synthetic-replay", Provider: tc.provider, Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://replay.synthetic.invalid",
						},
					}
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.Host != "replay.synthetic.invalid" {
								return nil, fmt.Errorf("unexpected synthetic route %s", request.URL)
							}
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							if got := gjson.GetBytes(body, tc.signaturePath).String(); got != tc.sig {
								return nil, fmt.Errorf(
									"native %s replay signature changed: got=%q want=%q", tc.name, got, tc.sig,
								)
							}
							response := `{"id":"fixture","type":"message","model":"swe-1.5","role":"` +
								`assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
								`"usage":{"input_tokens":1,"output_tokens":1}}`
							if tc.provider == "gemini" {
								response = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"fi` +
									`nishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidates` +
									`TokenCount":1,"totalTokenCount":2}}`
							}
							headers := http.Header{"Content-Type": {"application/json"}}
							switch operation {
							case "ExecuteStream":
								headers.Set("Content-Type", "text/event-stream")
								if tc.provider == "gemini" {
									response = "data: " + response + "\n\n"
								} else {
									response = ("event: message_start\ndata: {\"type\":\"message_start\"," +
										"\"message\":" +
										"{") +
										"\"id\":\"fixture\",\"model\":\"swe-1.5\",\"role\":\"assistant\",\"cont" +
										"ent\":[]}}\n\n" +
										("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"" +
											"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n") +
										("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"," +
											"\"index\":" +
											"0}\n\n") +
										("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":" +
											"{\"stop_reason\":" +
											"\"end_turn\"}}\n\n") +
										"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
								}
							case "CountTokens":
								response = `{"input_tokens":1}`
								if tc.provider == "gemini" {
									response = `{"totalTokens":1}`
								}
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: headers,
								Body: io.NopCloser(strings.NewReader(response)), Request: request,
							}, nil
						},
					}
					ctx := util.WithRoundTripper(t.Context(), transport)
					claude := NewClaudeExecutor(cfg)
					execute, executeStream, count := claude.Execute, claude.ExecuteStream, claude.CountTokens
					payload := (`{"model":"swe-1.5","max_tokens":16,"messages":[{"role":"assistant","content":` +
						`[{"type":"thinking","thinking":"cached thought","signature":"`) +
						tc.sig +
						`"}]},{"role":"user","content":"continue"}]}`
					if tc.provider == "gemini" {
						gemini := NewGeminiExecutor(cfg)
						execute, executeStream, count = gemini.Execute, gemini.ExecuteStream, gemini.CountTokens
						payload = `{"contents":[{"role":"model","parts":[{"thought":true,"text":"cached ` +
							`thought","thoughtSignature":` +
							`"` +
							tc.sig +
							`"}]},{"role":"user","parts":[{"text":"continue"}]}]}`
						if client := newProxyAwareHTTPClient(ctx, cfg, auth, 0); client.Transport != transport {
							t.Fatal("Gemini replay transport is not isolated")
						}
					} else {
						ctx = auditClaudeContractContext(t, cfg, auth, transport)
					}
					request := coreexecutor.Request{Model: tc.model, Payload: []byte(payload)}
					opts := coreexecutor.Options{SourceFormat: tc.format, ResponseFormat: tc.format}
					var err error
					wantCalls := 1
					switch operation {
					case "Execute":
						_, err = execute(ctx, auth, request, opts)
					case "CountTokens":
						var response coreexecutor.Response
						response, err = count(ctx, auth, request, opts)
						if tc.name == "SWE" {
							wantCalls = 0
							if err == nil && gjson.GetBytes(response.Payload, "input_tokens").Int() <= 0 {
								t.Fatalf("local SWE token count=%s", response.Payload)
							}
						}
					case "ExecuteStream":
						var result *coreexecutor.StreamResult
						result, err = executeStream(ctx, auth, request, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					}
					if err != nil || calls != wantCalls {
						t.Fatalf(
							"native %s operation=%s calls=%d want=%d error=%v",
							tc.name,
							operation,
							calls,
							wantCalls,
							err,
						)
					}
					if string(request.Payload) != payload {
						t.Fatal("executor changed the caller's replay payload")
					}
				},
			)
		}
	}
}
