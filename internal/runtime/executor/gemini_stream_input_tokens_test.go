package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiStreamEstimatesInitialClaudeInputTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	for _, provider := range []string{"gemini", "vertex", "vertex-service-account", "gemini-cli", "gemini-cli-json"} {
		for _, explicitOriginal := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/original=%t", provider, explicitOriginal), func(t *testing.T) {
					cfg := &config.Config{}
					auth := googlePayloadAuth(provider)
					delete(auth.Attributes, "interactions")
					if provider == "vertex-service-account" {
						delete(auth.Attributes, "api_key")
						delete(auth.Metadata, "access_token")
						auth.Provider = "vertex"
						auth.Metadata["location"] = "global"
						auth.Metadata["service_account"] = map[string]any{
							"type": "service_account", "project_id": "synthetic-project",
							"client_email": "stream@synthetic.invalid", "private_key": privateKey,
							"token_uri": "https://stream-token.invalid/token",
						}
					}
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
					registerTestUsagePlugin(t, plugin)
					calls, tokenCalls := 0, 0
					transport := &auditClaudeContractTransport{
						respond: func(req *http.Request) (*http.Response, error) {
							if req.URL.Host == "stream-token.invalid" {
								tokenCalls++
								return googlePayloadResponse(
									req,
									`{"access_token":"synthetic-vertex-bearer","expires_in":3600,`+
										`"token_type":`+
										`"Bearer"}`,
								), nil
							}
							calls++
							if _, err := io.ReadAll(req.Body); err != nil {
								return nil, err
							}
							frames := geminiUsageTailFrames(false)
							if strings.HasPrefix(provider, "gemini-cli") {
								frames = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"" +
									"text\":\"answer\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"" +
									"promptTokenCount\":10,\"candidatesTokenCount\":2,\"thoughtsTokenCount\":" +
									"3,\"cachedContentTokenCount\":4,\"totalTokenCount\":15}}}\n\n"
							}
							if provider == "gemini-cli-json" {
								frames = strings.TrimSpace(strings.TrimPrefix(frames, "data: "))
							}
							return googlePayloadResponse(req, frames), nil
						},
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					ctx = util.WithRoundTripper(ctx, transport)
					if newProxyAwareHTTPClient(ctx, cfg, auth, 0).Transport != transport ||
						strings.HasPrefix(provider, "gemini-cli") && newHTTPClient(
							ctx, cfg, auth, 0,
						).Transport != transport {
						t.Fatal("actual upstream transport is not memory-isolated")
					}
					if !tokenExpiry(auth.Metadata).After(time.Now().Add(refreshSkew)) {
						t.Fatal("synthetic credential would enter refresh")
					}
					original := []byte(
						`{"model":"gemini-2.5-flash","max_tokens":32,"messages":[{"role":"user"` +
							`,"content":"Please explain the deterministic synthetic stream input to` +
							`ken estimate contract."}]}`,
					)
					snapshot := bytes.Clone(original)
					opts := core.Options{
						SourceFormat: translator.FormatClaude, ResponseFormat: translator.FormatClaude, Stream: true,
					}
					if provider == "gemini-cli-json" {
						opts.Alt = "json"
					}
					if explicitOriginal {
						opts.OriginalRequest = original
					}
					expected, err := countClaudeInputTokens(original)
					if err != nil || expected != 13 {
						t.Fatalf("original estimate=%d error=%v, want 13", expected, err)
					}
					var implementation coreauth.ProviderExecutor = NewGeminiExecutor(cfg)
					if strings.HasPrefix(provider, "vertex") {
						implementation = NewGeminiVertexExecutor(cfg)
					}
					if strings.HasPrefix(provider, "gemini-cli") {
						implementation = NewGeminiCLIExecutor(cfg)
					}
					stream, err := implementation.ExecuteStream(
						ctx, auth, core.Request{Model: "gemini-2.5-flash", Payload: original}, opts,
					)
					if err != nil {
						t.Fatal(err)
					}
					starts, stops := 0, 0
					var initial int64
					var finalUsage gjson.Result
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						for line := range strings.SplitSeq(string(chunk.Payload), "\n") {
							if !strings.HasPrefix(line, "data:") {
								continue
							}
							event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
							switch event.Get("type").String() {
							case "message_start":
								starts++
								initial = event.Get("message.usage.input_tokens").Int()
							case "message_delta":
								finalUsage = event.Get("usage")
							case "message_stop":
								stops++
							}
						}
					}
					if calls != 1 || starts != 1 || initial != expected || stops != 1 {
						t.Errorf(
							"calls=%d starts=%d initial=%d want=%d stops=%d",
							calls,
							starts,
							initial,
							expected,
							stops,
						)
					}
					if finalUsage.Get("input_tokens").Int() != 6 ||
						finalUsage.Get("output_tokens").Int() != 5 ||
						finalUsage.Get("cache_read_input_tokens").Int() != 4 {
						t.Errorf("final real upstream usage changed: %s", finalUsage.Raw)
					}
					select {
					case record := <-plugin.records:
						if record.Failed || record.Detail.InputTokens != 10 ||
							record.Detail.OutputTokens != 2 ||
							record.Detail.ReasoningTokens != 3 ||
							record.Detail.CachedTokens != 4 ||
							record.Detail.TotalTokens != 15 {
							t.Errorf("recorded billable usage changed: %+v", record)
						}
					case <-ctx.Done():
						t.Fatal("usage was not published")
					}
					if (provider == "vertex-service-account") != (tokenCalls == 1) {
						t.Errorf("synthetic service-account token calls=%d", tokenCalls)
					}
					if !bytes.Equal(original, snapshot) {
						t.Fatal("caller request changed")
					}
				},
			)
		}
	}
}

func TestClaudeStreamInputTokenEstimateBoundaries(t *testing.T) {
	original := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	count, err := countClaudeInputTokens(original)
	if err != nil || count == 0 {
		t.Fatalf("local estimate=%d error=%v", count, err)
	}
	for _, test := range []struct {
		name, prefix, payload, suffix string
		original                      []byte
		handled, canceled             bool
		want                          int64
		wantHandled                   bool
	}{
		{
			name: "zero", prefix: "event: message_start\ndata: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\n\n",
			original: original, want: count, wantHandled: true,
		},
		{
			name: "missing", prefix: "data: ", payload: `{"type":"message_start","message":{}}`, suffix: "\n\n",
			original: original, want: count, wantHandled: true,
		},
		{
			name: "nonzero", prefix: "data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":42}}}`, suffix: "\n\n",
			original: original, want: 42, wantHandled: true,
		},
		{
			name: "CRLF", prefix: "event: ping\r\ndata: {\"type\":\"ping\"}\r\n\r\nevent: message_start\r\n  data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\r\n\r\n",
			original: original, want: count, wantHandled: true,
		},
		{
			name: "empty original", prefix: "data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\n\n",
			wantHandled: true,
		},
		{
			name: "invalid original", prefix: "data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\n\n",
			original: []byte(`{"messages":`), wantHandled: true,
		},
		{
			name: "already handled", prefix: "data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\n\n",
			original: original, handled: true, wantHandled: true,
		},
		{
			name: "canceled", prefix: "data: ",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`, suffix: "\n\n",
			original: original, canceled: true,
		},
		{
			name: "other event", prefix: "data: ",
			payload: `{"type":"message_delta","usage":{"input_tokens":7,"cache_read_input_tokens":3}}`, suffix: "\n\n",
			original: original,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if test.canceled {
					cancel()
				}
				chunk := []byte(test.prefix + test.payload + test.suffix)
				snapshot := bytes.Clone(chunk)
				out := estimateClaudeStreamInputTokens(ctx, [][]byte{chunk}, test.original, &test.handled)
				want := snapshot
				if test.want == count {
					want = []byte(strings.Replace(
						string(snapshot),
						test.payload,
						fmt.Sprintf(
							`{"type":"message_start","message":{"usage":{"input_tokens":%d}}}`,
							count,
						),
						1,
					))
				}
				if !bytes.Equal(out[0], want) || test.handled != test.wantHandled ||
					!bytes.Equal(chunk, snapshot) {
					t.Fatalf(
						"out=%s want=%s handled=%t want=%t input unchanged=%t", out[0], want, test.handled,
						test.wantHandled, bytes.Equal(chunk, snapshot),
					)
				}
				second := []byte(
					"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n",
				)
				if test.handled && !bytes.Equal(
					estimateClaudeStreamInputTokens(
						ctx, [][]byte{second}, original, &test.handled,
					)[0], second,
				) {
					t.Fatal("estimate repeated after the first message_start")
				}
			},
		)
	}
}
