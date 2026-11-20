package redisqueue

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// noinspection GoFixInline
func TestUsageQueuePluginPreservesLegacyWireFields(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
			ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
			ctx = internallogging.WithClientRequestMetadata(
				ctx, internallogging.ClientRequestMetadata{
					ClientIP:         "192.0.2.10",
					ResolvedClientIP: "203.0.113.5",
					XForwardedFor:    "203.0.113.5, 198.51.100.8",
					UserAgent:        "test-client/1.0",
				},
			)
			ctx = internallogging.WithResponseStatusHolder(ctx)
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			responseHeaders := http.Header{}
			responseHeaders.Add("X-Upstream-Request-Id", "upstream-req-1")
			responseHeaders.Add("Retry-After", "30")
			(&usageQueuePlugin{}).HandleUsage(
				ctx, coreusage.Record{
					Provider:            "openai",
					ExecutorType:        "KimiExecutor",
					Model:               "gpt-5.4",
					Alias:               "client-gpt",
					APIKey:              "test-key",
					AuthIndex:           "0",
					AccessTokenSHA256:   "token-version-hash",
					AuthType:            "apikey",
					Source:              "user@example.com",
					ReasoningEffort:     "medium",
					ServiceTier:         "auto",
					ResponseServiceTier: "default",
					ResponseModel:       "gpt-5.6-luna",
					Generate:            coreusage.GenerateFlag(true),
					RequestedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
					Latency:             1500 * time.Millisecond,
					Detail:              coreusage.Detail{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
					ResponseHeaders:     responseHeaders.Clone(),
				},
			)
			responseHeaders.Set("Retry-After", "999")
			payload := popSinglePayload(t)
			for key, want := range map[string]string{
				"provider": "openai", "executor_type": "KimiExecutor", "model": "gpt-5.4",
				"alias": "client-gpt", "endpoint": "POST /v1/chat/completions", "auth_type": "apikey",
				"access_token_sha256": "token-version-hash", "request_id": "ctx-request-id",
				"client_ip": "192.0.2.10", "resolved_client_ip": "203.0.113.5",
				"x_forwarded_for": "203.0.113.5, 198.51.100.8", "user_agent": "test-client/1.0",
				"reasoning_effort": "medium", "service_tier": "auto",
				"response_service_tier": "default", "response_model": "gpt-5.6-luna",
			} {
				requireStringField(t, payload, key, want)
			}
			requireMissingField(t, payload, "user_api_key")
			requireMissingField(t, payload, "request_service_tier")
			requireIntField(t, payload, "accounting_version", coreusage.TokenAccountingSchemaVersion)
			requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityComplete, 30, 0)
			var tokens map[string]json.RawMessage
			if err := json.Unmarshal(payload["tokens"], &tokens); err != nil {
				t.Fatal(err)
			}
			requireBoolField(t, tokens, "cache_read_tokens_present", true)
			requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
			requireHeaderField(t, payload, "response_headers", "Retry-After", []string{"30"})
			requireBoolField(t, payload, "failed", false)
			requireBoolField(t, payload, "generate", true)
			requireBoolField(t, payload, "stream", false)
			requireFailField(t, payload, http.StatusOK, "")
		},
	)
}

func TestUsageQueuePluginPreservesLegacyCachedOnlyUsage(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithResponseStatusHolder(context.Background())
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			(&usageQueuePlugin{}).HandleUsage(
				ctx, coreusage.Record{
					Provider: "openai", Model: "gpt-5.4", Detail: coreusage.Detail{CachedTokens: 13},
				},
			)
			payload := popSinglePayload(t)
			var tokens map[string]json.RawMessage
			if err := json.Unmarshal(payload["tokens"], &tokens); err != nil {
				t.Fatal(err)
			}
			requireBoolField(t, tokens, "cache_read_tokens_present", true)
			requireIntField(t, tokens, "cache_read_tokens", 13)
			requireIntField(t, tokens, "total_tokens", 13)
			requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityUnclassified, 13, 13)
		},
	)
}

func TestUsageQueuePluginEmitsSingleCanonicalAutoTier(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := coreusage.WithServiceTier(context.Background(), "auto")
			ctx = internallogging.WithResponseStatusHolder(ctx)
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			(&usageQueuePlugin{}).HandleUsage(
				ctx, coreusage.Record{
					Provider: "openai", Model: "gpt-5.4", Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
				},
			)
			payload := popSinglePayload(t)
			requireStringField(t, payload, "service_tier", "auto")
			requireMissingField(t, payload, "request_service_tier")
		},
	)
}

func TestUsageQueuePluginAcceptsDeprecatedRequestTierRecordField(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithResponseStatusHolder(context.Background())
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			(&usageQueuePlugin{}).HandleUsage(
				ctx, coreusage.Record{
					Provider: "openai", Model: "gpt-5.4", RequestServiceTier: "priority",
					Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
				},
			)
			payload := popSinglePayload(t)
			requireStringField(t, payload, "service_tier", "priority")
			requireMissingField(t, payload, "request_service_tier")
		},
	)
}

func TestUsageQueuePluginCanonicalTierPrecedesDeprecatedAlias(t *testing.T) {
	withEnabledQueue(
		t, func() {
			(&usageQueuePlugin{}).HandleUsage(
				coreusage.WithServiceTier(context.Background(), "default"), coreusage.Record{
					Provider: "openai", Model: "gpt-5.4", ServiceTier: "auto", RequestServiceTier: "priority",
					ResponseServiceTier: "flex", Detail: coreusage.Detail{InputTokens: 1, TotalTokens: 1},
				},
			)
			payload := popSinglePayload(t)
			requireStringField(t, payload, "service_tier", "auto")
			requireStringField(t, payload, "response_service_tier", "flex")
			requireMissingField(t, payload, "request_service_tier")
		},
	)
}
