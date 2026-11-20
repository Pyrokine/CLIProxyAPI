package executor

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestClassifyClaudeUpstreamRateLimitScope(t *testing.T) {
	const rateLimitBody = `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`
	tests := []struct {
		name              string
		headers           http.Header
		body              string
		wantScope         cliproxyexecutor.StateEffectScope
		wantRequestScoped bool
		wantCredential    bool
		wantRetry         bool
	}{
		{
			name:      "ordinary 429 is exact-model scoped",
			headers:   http.Header{"Retry-After": {"60"}},
			body:      rateLimitBody,
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
			wantRetry: true,
		},
		{
			name: "shared 5h rejection is credential scoped",
			headers: http.Header{
				"Retry-After":                           {"60"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			},
			body:           rateLimitBody,
			wantScope:      cliproxyexecutor.StateEffectScopeCredential,
			wantCredential: true,
			wantRetry:      true,
		},
		{
			name: "aggregate rejection with missing shared statuses is credential scoped",
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-Status": {"rejected"},
			},
			body:           rateLimitBody,
			wantScope:      cliproxyexecutor.StateEffectScopeCredential,
			wantCredential: true,
		},
		{
			name: "Fable-only rejection is exact-model scoped",
			headers: http.Header{
				"Retry-After":                              {"60"},
				"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			},
			body:      rateLimitBody,
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name: "Fast entitlement rejection is request scoped",
			body: `{"type":"error","error":{"type":"rate_limit_error","message":"Fast ` +
				`request rejected: usage credits are ` +
				`required"}}`,
			wantScope:         cliproxyexecutor.StateEffectScopeRequest,
			wantRequestScoped: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				err := classifyClaudeUpstreamError(
					http.StatusTooManyRequests,
					[]byte(test.body),
					false,
					test.headers,
				)
				if err == nil {
					t.Fatal("classifyClaudeUpstreamError() = nil")
				}
				var requestScoped cliproxyexecutor.RequestScopedError
				gotRequestScoped := errors.As(
					err, &requestScoped,
				) && requestScoped != nil && requestScoped.IsRequestScoped()
				if gotRequestScoped != test.wantRequestScoped {
					t.Fatalf("request scoped = %v, want %v", gotRequestScoped, test.wantRequestScoped)
				}
				var credentialScoped interface{ IsCredentialScoped() bool }
				gotCredential := errors.As(
					err, &credentialScoped,
				) && credentialScoped != nil && credentialScoped.IsCredentialScoped()
				if gotCredential != test.wantCredential {
					t.Fatalf("credential scoped = %v, want %v", gotCredential, test.wantCredential)
				}
				var dispositionErr cliproxyexecutor.ExecutionDispositionError
				if !errors.As(err, &dispositionErr) || dispositionErr == nil {
					t.Fatalf("error = %T, want ExecutionDispositionError", err)
				}
				disposition := dispositionErr.ExecutionDisposition()
				if disposition.EffectScope != test.wantScope {
					t.Fatalf("effect scope = %q, want %q", disposition.EffectScope, test.wantScope)
				}
				if test.wantRequestScoped {
					if disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
						t.Fatalf("retry directive = %q, want no_retry", disposition.RetryDirective)
					}
				} else if disposition.RetryDirective != cliproxyexecutor.RetryDirectiveRetryAfter {
					t.Fatalf("retry directive = %q, want retry_after", disposition.RetryDirective)
				}
				var retryAfter interface{ RetryAfter() *time.Duration }
				if !errors.As(err, &retryAfter) || retryAfter == nil {
					t.Fatalf("error = %T, want RetryAfter provider", err)
				}
				gotRetry := retryAfter.RetryAfter()
				if (gotRetry != nil) != test.wantRetry {
					t.Fatalf("RetryAfter() = %v, want present %v", gotRetry, test.wantRetry)
				}
				if gotRetry != nil && (*gotRetry < 61*time.Second || *gotRetry > 90*time.Second) {
					t.Fatalf("RetryAfter() = %v, want [61s, 90s]", *gotRetry)
				}
			},
		)
	}
}

func TestClaudeFastDirectResponsePreservesRateLimitScope(t *testing.T) {
	const body = `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`
	tests := []struct {
		name              string
		headers           http.Header
		body              string
		wantScope         cliproxyexecutor.StateEffectScope
		wantRequestScoped bool
		wantCredential    bool
		modelLevelCooling bool
	}{
		{
			name:      "ordinary 429",
			headers:   http.Header{"Retry-After": {"60"}, "X-Upstream": {"preserved"}},
			body:      body,
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name: "shared 429",
			headers: http.Header{
				"Retry-After":                           {"60"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"X-Upstream":                            {"preserved"},
			},
			body:           body,
			wantScope:      cliproxyexecutor.StateEffectScopeCredential,
			wantCredential: true,
		},
		{
			name: "shared 429 with model-level cooling",
			headers: http.Header{
				"Retry-After": {"60"}, "Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
				"X-Upstream": {"preserved"},
			},
			body:              body,
			wantScope:         cliproxyexecutor.StateEffectScopeExactModel,
			modelLevelCooling: true,
		},
		{
			name:              "Fast entitlement 429",
			headers:           http.Header{"Retry-After": {"60"}, "X-Upstream": {"preserved"}},
			body:              `{"type":"error","error":{"type":"rate_limit_error","message":"Fast request rejected"}}`,
			wantScope:         cliproxyexecutor.StateEffectScopeRequest,
			wantRequestScoped: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				response := &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     test.headers,
				}
				err := newClaudeFastDirectResponseError(response, []byte(test.body), test.modelLevelCooling)
				var direct *cliproxyexecutor.RequestTerminatedError
				if !errors.As(err, &direct) || direct == nil {
					t.Fatalf("error = %T, want RequestTerminatedError", err)
				}
				if got := string(direct.ResponseBody()); got != test.body {
					t.Fatalf("direct body = %q, want %q", got, test.body)
				}
				if got := direct.ResponseHeaders().Get("X-Upstream"); got != "preserved" {
					t.Fatalf("X-Upstream = %q, want preserved", got)
				}
				var requestScoped cliproxyexecutor.RequestScopedError
				gotRequestScoped := errors.As(
					err, &requestScoped,
				) && requestScoped != nil && requestScoped.IsRequestScoped()
				if gotRequestScoped != test.wantRequestScoped {
					t.Fatalf("request scoped = %v, want %v", gotRequestScoped, test.wantRequestScoped)
				}
				var credentialScoped interface{ IsCredentialScoped() bool }
				gotCredential := errors.As(
					err, &credentialScoped,
				) && credentialScoped != nil && credentialScoped.IsCredentialScoped()
				if gotCredential != test.wantCredential {
					t.Fatalf("credential scoped = %v, want %v", gotCredential, test.wantCredential)
				}
				var dispositionErr cliproxyexecutor.ExecutionDispositionError
				if !errors.As(err, &dispositionErr) || dispositionErr == nil {
					t.Fatalf("error = %T, want disposition", err)
				}
				if got := dispositionErr.ExecutionDisposition().EffectScope; got != test.wantScope {
					t.Fatalf("effect scope = %q, want %q", got, test.wantScope)
				}
			},
		)
	}
}

func TestClaudeRateLimitParityAcrossEntrypoints(t *testing.T) {
	scenarios := []struct {
		name              string
		model             string
		requestBody       string
		responseBody      string
		headers           http.Header
		wantScope         cliproxyexecutor.StateEffectScope
		wantCredential    bool
		wantRetry         bool
		modelLevelCooling bool
	}{
		{
			name:  "ordinary model rejection",
			model: "claude-opus-4-7",
			requestBody: `{"model":"claude-opus-4-7","max_tokens":16,"messages":[{"role":"user",` +
				`"content":"reply ` +
				`OK"}]}`,
			responseBody: `{"type":"error","error":{"type":"rate_limit_error","message":"model rate limit exceeded"}}`,
			headers: http.Header{
				"Content-Type":                          {"application/json"},
				"Retry-After":                           {"60"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status": {"allowed_warning"},
			},
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
			wantRetry: true,
		},
		{
			name:  "shared rejection overrides Fast entitlement text",
			model: "claude-opus-4-7",
			requestBody: `{"model":"claude-opus-4-7","max_tokens":16,"speed":"fast","messages":` +
				`[{"role":"user","content":"reply ` +
				`OK"}]}`,
			responseBody: `{"type":"error","error":{"type":"rate_limit_error","message":"Fast request rejected"}}`,
			headers: http.Header{
				"Content-Type":                          {"application/json"},
				"Retry-After":                           {"60"},
				"Anthropic-Ratelimit-Unified-Status":    {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			},
			wantScope:      cliproxyexecutor.StateEffectScopeCredential,
			wantCredential: true,
			wantRetry:      true,
		},
		{
			name:  "shared rejection with model-level cooling",
			model: "claude-opus-4-7",
			requestBody: `{"model":"claude-opus-4-7","max_tokens":16,"messages":[{"role":"user",` +
				`"content":"reply ` +
				`OK"}]}`,
			responseBody: `{"type":"error","error":{"type":"rate_limit_error","message":"shared rate limit"}}`,
			headers: http.Header{
				"Content-Type": {"application/json"}, "Retry-After": {"60"},
				"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"},
			},
			wantScope:         cliproxyexecutor.StateEffectScopeExactModel,
			wantRetry:         true,
			modelLevelCooling: true,
		},
		{
			name:  "overage spend cap with omitted 5h window",
			model: "claude-sonnet-4-5",
			requestBody: `{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":` +
				`"user","content":"reply ` +
				`OK"}]}`,
			responseBody: `{"type":"error","error":{"type":"rate_limit_error","message":"overage spend cap reached"}}`,
			headers: http.Header{
				"Content-Type":                                     {"application/json"},
				"Retry-After":                                      {"60"},
				"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
				"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.00"},
				"Anthropic-Ratelimit-Unified-Overage-Status":       {"rejected"},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
			},
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
		},
		{
			name:  "Fable-only rejection",
			model: "claude-fable-5-1",
			requestBody: `{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":` +
				`"user","content":"reply ` +
				`OK"}]}`,
			responseBody: `{"type":"error","error":{"type":"rate_limit_error","message":"Fable ` +
				`usage window ` +
				`rejected"}}`,
			headers: http.Header{
				"Content-Type":                             {"application/json"},
				"Retry-After":                              {"60"},
				"Anthropic-Ratelimit-Unified-Status":       {"rejected"},
				"Anthropic-Ratelimit-Unified-5h-Status":    {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Status":    {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"},
			},
			wantScope: cliproxyexecutor.StateEffectScopeExactModel,
		},
	}
	entrypoints := []string{"Execute", "ExecuteStream", "CountTokens"}

	for _, scenario := range scenarios {
		t.Run(
			scenario.name, func(t *testing.T) {
				for _, entrypoint := range entrypoints {
					t.Run(
						entrypoint, func(t *testing.T) {
							transport := protectedUtlsRoundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									return &http.Response{
										StatusCode: http.StatusTooManyRequests,
										Header:     scenario.headers.Clone(),
										Body:       io.NopCloser(bytes.NewBufferString(scenario.responseBody)),
										Request:    req,
									}, nil
								},
							)
							ctx := util.WithRoundTripper(t.Context(), transport)
							auth := &cliproxyauth.Auth{
								ID:         "claude-rate-limit-" + strings.ToLower(entrypoint),
								Attributes: map[string]string{"api_key": "sk-ant-oat-rate-limit-parity"},
								Metadata:   claudeOAuthWireTestMetadata(),
							}
							request := cliproxyexecutor.Request{
								Model:   scenario.model,
								Payload: []byte(scenario.requestBody),
							}
							options := cliproxyexecutor.Options{
								SourceFormat:   sdktranslator.FormatClaude,
								ResponseFormat: sdktranslator.FormatClaude,
							}
							executor := NewClaudeExecutor(
								&config.Config{
									Claude: config.ClaudeConfig{
										ModelLevelCooling: scenario.modelLevelCooling,
									},
								},
							)

							var err error
							switch entrypoint {
							case "Execute":
								_, err = executor.Execute(ctx, auth, request, options)
							case "ExecuteStream":
								options.Stream = true
								_, err = executor.ExecuteStream(ctx, auth, request, options)
							case "CountTokens":
								_, err = executor.CountTokens(ctx, auth, request, options)
							default:
								t.Fatalf("unknown entrypoint %q", entrypoint)
							}
							if err == nil {
								t.Fatal("error = nil")
							}
							var dispositionErr cliproxyexecutor.ExecutionDispositionError
							if !errors.As(err, &dispositionErr) || dispositionErr == nil {
								t.Fatalf("error = %T %v, want disposition", err, err)
							}
							disposition := dispositionErr.ExecutionDisposition()
							if disposition.EffectScope != scenario.wantScope {
								t.Fatalf(
									"effect scope = %q, want %q",
									disposition.EffectScope,
									scenario.wantScope,
								)
							}
							if disposition.RetryDirective != cliproxyexecutor.RetryDirectiveRetryAfter {
								t.Fatalf("retry directive = %q, want retry_after", disposition.RetryDirective)
							}
							var credentialScoped interface{ IsCredentialScoped() bool }
							gotCredential := errors.As(
								err, &credentialScoped,
							) && credentialScoped != nil && credentialScoped.IsCredentialScoped()
							if gotCredential != scenario.wantCredential {
								t.Fatalf(
									"credential scoped = %v, want %v",
									gotCredential,
									scenario.wantCredential,
								)
							}
							var retryAfter interface{ RetryAfter() *time.Duration }
							if !errors.As(err, &retryAfter) || retryAfter == nil {
								t.Fatalf("error = %T %v, want RetryAfter provider", err, err)
							}
							gotRetry := retryAfter.RetryAfter()
							if (gotRetry != nil) != scenario.wantRetry {
								t.Fatalf("RetryAfter() = %v, want present %t", gotRetry, scenario.wantRetry)
							}
							if gotRetry != nil && (*gotRetry < 61*time.Second || *gotRetry > 90*time.Second) {
								t.Fatalf("RetryAfter() = %v, want [61s, 90s]", *gotRetry)
							}
						},
					)
				}
			},
		)
	}
}
