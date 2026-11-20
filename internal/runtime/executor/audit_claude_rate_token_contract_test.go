package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type auditClaudeContractTransport struct {
	respond func(*http.Request) (*http.Response, error)
}

func (t *auditClaudeContractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.respond(req)
}

func auditClaudeContractContext(
	t *testing.T,
	cfg *config.Config,
	auth *coreauth.Auth,
	transport *auditClaudeContractTransport,
) context.Context {
	t.Helper()
	ctx := util.WithRoundTripper(t.Context(), transport)
	client := newClaudeUtlsHTTPClient(ctx, cfg, auth, 0)
	selected, ok := client.Transport.(*claudeFallbackRoundTripper)
	if !ok || selected.anthropic != transport || selected.chrome != transport ||
		selected.fallback != transport {
		t.Fatal("actual Claude executor transport is not isolated; no request issued")
	}
	return ctx
}

func TestAuditClaudeResetScopeAndRetryAcrossRealEntrypoints(t *testing.T) {
	for _, tc := range []struct {
		name, window, status, aggregateStatus string
		aggregate, cooling, fast, entitlement bool
		scope                                 cliproxyexecutor.StateEffectScope
	}{
		{name: "missing-5h-status", window: "5h", scope: cliproxyexecutor.StateEffectScopeExactModel},
		{name: "missing-7d-status", window: "7d", scope: cliproxyexecutor.StateEffectScopeExactModel},
		{name: "aggregate-only", aggregate: true, scope: cliproxyexecutor.StateEffectScopeExactModel},
		{name: "standalone-7d-oi-reset", window: "7d_oi", scope: cliproxyexecutor.StateEffectScopeExactModel},
		{
			name: "standalone-oi-with-allowed-aggregate", window: "7d_oi", status: "rejected", aggregate: true,
			aggregateStatus: "allowed", scope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name: "standalone-oi-with-unknown-aggregate", window: "7d_oi", status: "rejected", aggregate: true,
			aggregateStatus: "unknown", scope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name: "shared-aggregate-latest", window: "5h", status: "rejected", aggregate: true,
			scope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "shared-fast-model-cooling", window: "5h", status: "rejected", cooling: true, fast: true,
			scope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name: "fast-entitlement-has-no-retry-hint", fast: true, entitlement: true,
			scope: cliproxyexecutor.StateEffectScopeRequest,
		},
	} {
		for _, entry := range []string{"Execute", "ExecuteStream", "CountTokens"} {
			t.Run(
				tc.name+"/"+entry, func(t *testing.T) {
					cfg := &config.Config{Claude: config.ClaudeConfig{ModelLevelCooling: tc.cooling}}
					auth := &coreauth.Auth{
						ID: "synthetic-claude-rate", Provider: "claude",
						Attributes: map[string]string{"api_key": "synthetic-key"},
					}
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.Host != "api.anthropic.com" ||
								(req.URL.Path != "/v1/messages" && req.URL.Path != "/v1/messages/count_tokens") {
								return nil, fmt.Errorf("unexpected isolated route %s", req.URL)
							}
							h := make(http.Header)
							if tc.window != "" {
								h.Set(
									"Anthropic-Ratelimit-Unified-"+tc.window+"-Reset",
									fmt.Sprint(time.Now().Add(600*time.Second).Unix()),
								)
								if tc.status != "" {
									h.Set("Anthropic-Ratelimit-Unified-"+tc.window+"-Status", tc.status)
								}
							}
							if tc.aggregate {
								h.Set(
									"Anthropic-Ratelimit-Unified-Reset",
									fmt.Sprint(time.Now().Add(900*time.Second).Unix()),
								)
							}
							if tc.aggregateStatus != "" {
								h.Set("Anthropic-Ratelimit-Unified-Status", tc.aggregateStatus)
							}
							message := "rate limit exceeded"
							if tc.fast {
								message = "Fast request rejected: usage credits are required"
							}
							if tc.entitlement {
								h.Set("Retry-After", "60")
							}
							return &http.Response{
								StatusCode: http.StatusTooManyRequests, Header: h,
								Body: io.NopCloser(
									strings.NewReader(
										`{"type":"error","error":{"type":"rate_limit_error","message":"` +
											message +
											`"}}`,
									),
								),
								Request: req,
							}, nil
						},
					}
					ctx := auditClaudeContractContext(t, cfg, auth, transport)
					payload := `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":` +
						`"user","content":` +
						`"hello"}]}`
					if tc.fast {
						payload = `{"model":"claude-sonnet-4-6","max_tokens":16,"speed":"fast",` +
							`"messages":[{"role":"user","content":` +
							`"hello"}]}`
					}
					req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(payload)}
					opts := cliproxyexecutor.Options{
						SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
					}
					exec := NewClaudeExecutor(cfg)
					var err error
					switch entry {
					case "Execute":
						_, err = exec.Execute(ctx, auth, req, opts)
					case "ExecuteStream":
						_, err = exec.ExecuteStream(ctx, auth, req, opts)
					case "CountTokens":
						_, err = exec.CountTokens(ctx, auth, req, opts)
					}
					if err == nil || calls != 1 {
						t.Fatalf("upstream error=%v calls=%d", err, calls)
					}
					var disposition cliproxyexecutor.ExecutionDispositionError
					if !errors.As(err, &disposition) ||
						disposition.ExecutionDisposition().EffectScope != tc.scope {
						t.Fatalf("error=%T %v, want scope=%s", err, err, tc.scope)
					}
					var retry interface{ RetryAfter() *time.Duration }
					if !errors.As(err, &retry) {
						t.Fatalf("error has no retry contract: %T", err)
					}
					if tc.entitlement {
						if retry.RetryAfter() != nil ||
							disposition.ExecutionDisposition().RetryDirective !=
								cliproxyexecutor.RetryDirectiveNoRetry {
							t.Fatalf(
								"request entitlement carries retry state: %+v retry=%v",
								disposition.ExecutionDisposition(), retry.RetryAfter(),
							)
						}
					} else {
						want := 600 * time.Second
						if tc.aggregate {
							want = 900 * time.Second
						}
						if retry.RetryAfter() ==
							nil ||
							*retry.RetryAfter() <
								want-
									time.Second ||
							*retry.RetryAfter() >
								want+
									30*
										time.Second ||
							disposition.ExecutionDisposition().RetryDirective !=
								cliproxyexecutor.RetryDirectiveRetryAfter {
							t.Fatalf(
								"scope=%s retry=%v want=[%s,%s]", tc.scope, retry.RetryAfter(), want-time.Second,
								want+30*time.Second,
							)
						}
					}
				},
			)
		}
	}
}

func TestAuditClaudeFinalWirePreservesExplicitMaxTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{
			"explicit-above-limit", "default-registry", "default-1024", "payload-override", "payload-filter",
			"final-model-default", "upstream-explicit-rejection",
		} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
					clientID := "audit-max-" + t.Name()
					baseModel, finalModel := "claude-audit-max-base", "claude-audit-max-final"
					limit := 4096
					if mode == "default-1024" {
						limit = 0
					}
					registry.GetGlobalRegistry().RegisterClient(
						clientID, "claude", []*registry.ModelInfo{
							{ID: baseModel, Type: "claude", MaxCompletionTokens: limit},
							{ID: finalModel, Type: "claude", MaxCompletionTokens: 8192},
						},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
					cfg := &config.Config{}
					models := []config.PayloadModelRule{{Name: "*", Protocol: "claude"}}
					payload := `{"model":"` + baseModel + `","messages":[{"role":"user","content":"hello"}]}`
					want := "4096"
					switch mode {
					case "explicit-above-limit", "upstream-explicit-rejection":
						payload = `{"model":"` +
							baseModel +
							`","max_tokens":7000,"messages":[{"role":"user","content":"hello"}]}`
						want = "7000"
					case "default-1024":
						want = "1024"
					case "payload-override":
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{"max_tokens": 7000},
							},
						}
						want = "7000"
					case "payload-filter":
						payload = `{"model":"` +
							baseModel +
							`","max_tokens":7000,"messages":[{"role":"user","content":"hello"}]}`
						cfg.Payload.Filter = []config.PayloadFilterRule{
							{
								Models: models, Params: []string{"max_tokens"},
							},
						}
					case "final-model-default":
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{"model": finalModel},
							},
						}
						want = "8192"
					}
					auth := &coreauth.Auth{
						ID: "synthetic-claude-max", Provider: "claude",
						Attributes: map[string]string{"api_key": "synthetic-key"},
					}
					var wire []byte
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.Host != "api.anthropic.com" || req.URL.Path != "/v1/messages" {
								return nil, fmt.Errorf("unexpected isolated route %s", req.URL)
							}
							var err error
							wire, err = io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							if mode == "upstream-explicit-rejection" {
								return googlePayloadResponse(
									req,
									`{"type":"error","error":{"type":"invalid_request_error","message":`+
										`"max_tokens exceeds the synthetic model `+
										`limit"}}`,
								), nil
							}
							body := `{"id":"synthetic-msg","type":"message","role":"assistant","model":"` +
								baseModel +
								(`","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{` +
									`"input_tokens":1,"output_tokens":1}}`)
							if gjson.GetBytes(wire, "stream").Bool() {
								body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" +
									"\"id\":\"synthetic-msg\",\"type\":\"message\",\"role\":\"assistant\",\"" +
									"model\":\"claude-audit-max-base\",\"content\":[],\"usage\":{\"" +
									"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {" +
									"\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"" +
									"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":" +
									"\"message_stop\"}\n\n"
							}
							return googlePayloadResponse(req, body), nil
						},
					}
					if mode == "upstream-explicit-rejection" {
						original := transport.respond
						transport.respond = func(req *http.Request) (*http.Response, error) {
							resp, err := original(req)
							if resp != nil {
								resp.StatusCode = http.StatusBadRequest
							}
							return resp, err
						}
					}
					ctx := auditClaudeContractContext(t, cfg, auth, transport)
					req := cliproxyexecutor.Request{Model: baseModel, Payload: []byte(payload)}
					opts := cliproxyexecutor.Options{
						SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
					}
					exec := NewClaudeExecutor(cfg)
					var err error
					if stream {
						var result *cliproxyexecutor.StreamResult
						result, err = exec.ExecuteStream(ctx, auth, req, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = exec.Execute(ctx, auth, req, opts)
					}
					if calls != 1 || gjson.GetBytes(wire, "max_tokens").Raw != want {
						t.Fatalf(
							"calls=%d max_tokens=%s want=%s error=%v wire=%s", calls,
							gjson.GetBytes(wire, "max_tokens").Raw, want, err, wire,
						)
					}
					if mode == "upstream-explicit-rejection" {
						var status interface{ StatusCode() int }
						if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
							t.Fatalf("explicit upstream rejection lost: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				},
			)
		}
	}
}

func TestAuditClaudeExplicitMaxTokensKeepsOriginalBytes(t *testing.T) {
	model := "claude-audit-explicit-literals"
	registry.GetGlobalRegistry().RegisterClient(
		t.Name(), "claude", []*registry.ModelInfo{{ID: model, Type: "claude", MaxCompletionTokens: 4096}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(t.Name()) })
	for _, literal := range []string{"9007199254740993", "7000", "4097", "0", "-1", "null", `"7000"`} {
		body := []byte(`{"max_tokens":` + literal + `,"messages":[{"role":"user","content":"hello <>&"}]}`)
		if output := capMaxTokens(body, model); string(output) != string(body) {
			t.Fatalf("explicit %s changed: %s", literal, output)
		}
	}
}
