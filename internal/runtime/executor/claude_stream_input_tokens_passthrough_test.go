package executor

import (
	"bytes"
	"context"
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

func TestClaudeInputEstimatePreservesNativeStreams(t *testing.T) {
	for _, provider := range []string{"claude", "kimi"} {
		for _, initial := range []int64{0, 73} {
			t.Run(
				fmt.Sprintf("%s/initial=%d", provider, initial), func(t *testing.T) {
					cfg := &config.Config{}
					credential := &coreauth.Auth{
						ID: t.Name(), Provider: provider,
						Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://native-stream.invalid",
						},
					}
					var implementation coreauth.ProviderExecutor = NewClaudeExecutor(cfg)
					if provider == "kimi" {
						implementation = NewKimiExecutor(cfg)
					}
					wire := fmt.Sprintf(
						"event: message_start\ndata: "+
							`{"type":"message_start","message":{"id":"msg-native","model":"claude-test",`+
							`"usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n\n", initial,
					) +
						"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
						`"usage":{"input_tokens":7,"output_tokens":5}}` + "\n\n" +
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.Host != "native-stream.invalid" || req.URL.Path != "/v1/messages" {
								return nil, fmt.Errorf("unexpected native route %s", req.URL)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: req,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body:   io.NopCloser(strings.NewReader(wire)),
							}, nil
						},
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					isolated := auditClaudeContractContext(t, cfg, credential, transport)
					ctx = util.WithRoundTripper(ctx, util.RoundTripperValue(isolated))
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
					registerTestUsagePlugin(t, plugin)
					request, opts := claudeInputEstimateRequest("claude-test", false)
					before := bytes.Clone(request.Payload)
					result, err := implementation.ExecuteStream(ctx, credential, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					assertClaudeInputEstimateDelivery(t, ctx, result, plugin.records, initial)
					if calls != 1 || !bytes.Equal(before, request.Payload) {
						t.Fatalf("native calls=%d, caller request changed", calls)
					}
				},
			)
		}
	}
}

func TestClaudeInputEstimateSkipsNonClaudeSourceAtConsumer(t *testing.T) {
	cfg := &config.Config{}
	credential := &coreauth.Auth{
		ID: t.Name(), Provider: "compat",
		Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://non-claude.invalid"},
	}
	wire := "data: " + `{"id":"chat-native","model":"synthetic-model","choices":[{"index":0,` +
		`"delta":{"role":"assistant","content":"answer"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"chat-native","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}` + "\n\ndata: [DONE]\n\n"
	transport := &auditClaudeContractTransport{
		respond: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "non-claude.invalid" {
				return nil, fmt.Errorf("unexpected isolated route %s", req.URL)
			}
			return &http.Response{
				StatusCode: http.StatusOK, Request: req,
				Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader(wire)),
			}, nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = util.WithRoundTripper(ctx, transport)
	if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
		t.Fatal("actual upstream transport is not memory-isolated")
	}
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)
	request := core.Request{
		Model: "synthetic-model", Payload: []byte(`{"model":"synthetic-model",` +
			`"stream":true,"messages":[{"role":"user","content":"Hello."}]}`),
	}
	opts := core.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatClaude, Stream: true}
	result, err := NewOpenAICompatExecutor("compat", cfg).ExecuteStream(ctx, credential, request, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertClaudeInputEstimateDelivery(t, ctx, result, plugin.records, 0)
}

func TestClaudeInputEstimateSkipsNonClaudeResponseAtConsumer(t *testing.T) {
	cfg := &config.Config{}
	credential := &coreauth.Auth{
		ID: t.Name(), Provider: "compat",
		Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://non-claude.invalid"},
	}
	const start = `{"type":"message_start","message":{"usage":{"input_tokens":0}}}`
	wire := "data: " + start + "\n\ndata: " +
		`{"id":"chat-native","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}` + "\n\ndata: [DONE]\n\n"
	transport := &auditClaudeContractTransport{
		respond: func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "non-claude.invalid" {
				return nil, fmt.Errorf("unexpected isolated route %s", req.URL)
			}
			return &http.Response{
				StatusCode: http.StatusOK, Request: req,
				Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader(wire)),
			}, nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = util.WithRoundTripper(ctx, transport)
	if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
		t.Fatal("actual upstream transport is not memory-isolated")
	}
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)
	request, opts := claudeInputEstimateRequest("synthetic-model", false)
	opts.ResponseFormat = translator.FormatOpenAI
	result, err := NewOpenAICompatExecutor("compat", cfg).ExecuteStream(ctx, credential, request, opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		for line := range bytes.SplitSeq(chunk.Payload, []byte("\n")) {
			payload := jsonPayload(line)
			if gjson.GetBytes(payload, "type").String() == "message_start" {
				found = true
				if !bytes.Equal(payload, []byte(start)) {
					t.Fatalf("non-Claude response event changed: %s", payload)
				}
			}
		}
	}
	if !found {
		t.Fatal("original non-Claude response event was not delivered")
	}
	record := testWaitForDevinUsageRecord(t, plugin.records)
	if record.Failed || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 5 ||
		record.Detail.TotalTokens != 12 {
		t.Fatalf("recorded billable usage changed: %+v", record)
	}
}
