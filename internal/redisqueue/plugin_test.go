package redisqueue

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestUsageQueuePluginPayloadIncludesStableFieldsAndSuccess(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
			ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
			ctx = internallogging.WithResponseStatusHolder(ctx)
			ctx = internallogging.WithClientRequestMetadata(
				ctx, internallogging.ClientRequestMetadata{
					ClientIP:        "203.0.113.10",
					XForwardedFor:   "198.51.100.2, 203.0.113.10",
					UserAgent:       "redis-test-client/1.0",
					SessionID:       "slot:worker",
					ParentSessionID: "slot:root",
					NodeKind:        "compaction",
					IsCompaction:    true,
				},
			)
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			responseHeaders := http.Header{}
			responseHeaders.Add("X-Upstream-Request-Id", "upstream-req-1")
			responseHeaders.Add("Retry-After", "30")
			responseHeaders.Add("Set-Cookie", "session=response-secret")
			responseHeaders.Add("X-Api-Key", "sk-secret-value")
			responseHeaders.Add("X-Management-Key", "management-secret-value")

			plugin := &usageQueuePlugin{}
			plugin.HandleUsage(
				ctx, coreusage.Record{
					Provider:            "openai",
					BaseURL:             " https://upstream.example/v1 ",
					ExecutorType:        "OpenAICompatExecutor",
					Model:               "gpt-5.4",
					ResponseModel:       "gpt-5.4-preview",
					RequestID:           "inbound-request-id",
					ExecutionID:         "execution-attempt-id",
					Stream:              true,
					Alias:               "client-gpt",
					APIKey:              "test-key",
					AuthIndex:           "0",
					AuthType:            "apikey",
					Source:              "user@example.com",
					ReasoningEffort:     "medium",
					ServiceTier:         "flex",
					ResponseServiceTier: "default",
					Generate:            new(false),
					RequestedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
					Latency:             1500 * time.Millisecond,
					TimeToFirstByteMs:   250,
					Detail: coreusage.Detail{
						InputTokens:  10,
						OutputTokens: 20,
						CachedTokens: 5,
					},
					ResponseHeaders: responseHeaders.Clone(),
				},
			)
			responseHeaders.Set("Retry-After", "999")

			payload := popSinglePayload(t)
			requireStringField(t, payload, "provider", "openai")
			requireStringField(t, payload, "base_url", "https://upstream.example/v1")
			requireStringField(t, payload, "executor_type", "OpenAICompatExecutor")
			requireStringField(t, payload, "model", "gpt-5.4")
			requireStringField(t, payload, "response_model", "gpt-5.4-preview")
			requireBoolField(t, payload, "stream", true)
			requireStringField(t, payload, "alias", "client-gpt")
			requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
			requireStringField(t, payload, "auth_type", "apikey")
			requireStringField(t, payload, "api_key", util.HideAPIKey("test-key"))
			requireMissingField(t, payload, "user_api_key")
			requireStringField(t, payload, "request_id", "ctx-request-id")
			requireStringField(t, payload, "trace_id", "inbound-request-id")
			requireStringField(t, payload, "execution_id", "execution-attempt-id")
			requireStringField(t, payload, "reasoning_effort", "medium")
			requireStringField(t, payload, "service_tier", "flex")
			requireStringField(t, payload, "request_service_tier", "flex")
			requireStringField(t, payload, "response_service_tier", "default")
			requireStringField(t, payload, "client_ip", "203.0.113.10")
			requireStringField(t, payload, "x_forwarded_for", "198.51.100.2, 203.0.113.10")
			requireStringField(t, payload, "user_agent", "redis-test-client/1.0")
			requireStringField(t, payload, "session_id", coresession.NormalizeToCanonicalUUID("slot:worker"))
			requireStringField(t, payload, "parent_session_id", coresession.NormalizeToCanonicalUUID("slot:root"))
			requireStringField(t, payload, "node_kind", "compaction")
			requireBoolField(t, payload, "is_compaction", true)
			requireMissingField(t, payload, "is_fork")
			requireBoolField(t, payload, "generate", false)
			requireIntField(t, payload, "accounting_version", coreusage.TokenAccountingSchemaVersion)
			requireIntField(t, payload, "ttft_ms", 250)
			requireTokenField(t, payload, "total_tokens", 30)
			requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityComplete, 30, 0)
			requireTokenField(t, payload, "cached_tokens", 5)
			requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
			requireHeaderField(t, payload, "response_headers", "Retry-After", []string{"30"})
			requireHeaderNotPresent(t, payload, "response_headers", "Set-Cookie")
			requireHeaderFieldMasked(t, payload, "response_headers", "X-Api-Key", "sk-secret-value")
			requireHeaderFieldMasked(t, payload, "response_headers", "X-Management-Key", "management-secret-value")
			requireBoolField(t, payload, "failed", false)
			requireFailField(t, payload, http.StatusOK, "")
		},
	)
}

func TestUsageQueuePluginSessionParentFallbackIsSameOrigin(t *testing.T) {
	for _, test := range []struct {
		name   string
		record coreusage.Record
	}{
		{
			name:   "independent record does not inherit stale parent",
			record: coreusage.Record{SessionID: "session:new-root"},
		},
		{
			name: "self parent is removed",
			record: coreusage.Record{
				SessionID:       "session:self",
				ParentSessionID: "session:self",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			withEnabledQueue(t, func() {
				ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
					SessionID:       "session:old-child",
					ParentSessionID: "session:old-parent",
					NodeKind:        "fork",
					IsFork:          true,
				})
				test.record.Provider = "openai"
				test.record.Model = "gpt-test"
				(&usageQueuePlugin{}).HandleUsage(ctx, test.record)
				payload := popSinglePayload(t)
				requireStringField(t, payload, "session_id", coresession.NormalizeToCanonicalUUID(test.record.SessionID))
				requireMissingField(t, payload, "parent_session_id")
				requireMissingField(t, payload, "node_kind")
				requireMissingField(t, payload, "is_fork")
				requireMissingField(t, payload, "is_compaction")
			})
		})
	}
}

func TestUsageQueuePluginAsyncUsesRecordResponseHeaders(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithRequestID(context.Background(), "ctx-request-id")
			ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
			ctx = internallogging.WithResponseStatusHolder(ctx)
			ctx = internallogging.WithResponseHeadersHolder(ctx)
			internallogging.SetResponseStatus(ctx, http.StatusOK)
			initialHeaders := http.Header{}
			initialHeaders.Set("X-Upstream-Request-Id", "upstream-req-1")
			internallogging.SetResponseHeaders(ctx, initialHeaders)

			mgr := coreusage.NewManager(16)
			defer mgr.Stop()

			mgr.Register(
				pluginFunc(
					func(ctx context.Context, _ coreusage.Record) {
						nextHeaders := http.Header{}
						nextHeaders.Set("X-Upstream-Request-Id", "upstream-req-2")
						internallogging.SetResponseHeaders(ctx, nextHeaders)
					},
				),
			)
			mgr.Register(&usageQueuePlugin{})

			if errPublish := mgr.Publish(
				ctx, coreusage.Record{
					Provider:    "openai",
					Model:       "gpt-5.4",
					Alias:       "client-gpt",
					APIKey:      "test-key",
					AuthIndex:   "0",
					AuthType:    "apikey",
					Source:      "user@example.com",
					RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
					Latency:     1500 * time.Millisecond,
					Detail: coreusage.Detail{
						InputTokens:  10,
						OutputTokens: 20,
						TotalTokens:  30,
					},
					ResponseHeaders: internallogging.GetResponseHeaders(ctx),
				},
			); errPublish != nil {
				t.Fatalf("publish usage record: %v", errPublish)
			}

			payload := waitForSinglePayload(t, 2*time.Second)
			requireHeaderField(t, payload, "response_headers", "X-Upstream-Request-Id", []string{"upstream-req-1"})
		},
	)
}

func TestUsageQueuePluginPayloadIncludesStableFieldsAndFailureAndGinRequestID(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ctx := internallogging.WithRequestID(context.Background(), "gin-request-id")
			ctx = internallogging.WithEndpoint(ctx, "GET /v1/responses")
			ctx = internallogging.WithResponseStatusHolder(ctx)
			internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

			plugin := &usageQueuePlugin{}
			plugin.HandleUsage(
				ctx, coreusage.Record{
					Provider:    "openai",
					Model:       "gpt-5.4-mini",
					Alias:       "client-mini",
					APIKey:      "test-key",
					AuthIndex:   "0",
					AuthType:    "apikey",
					Source:      "user@example.com",
					RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
					Latency:     2500 * time.Millisecond,
					Fail: coreusage.Failure{
						StatusCode: http.StatusInternalServerError,
						Body:       "upstream failed",
					},
					Detail: coreusage.Detail{
						InputTokens:  10,
						OutputTokens: 20,
						TotalTokens:  30,
					},
				},
			)

			payload := popSinglePayload(t)
			requireStringField(t, payload, "provider", "openai")
			requireStringField(t, payload, "model", "gpt-5.4-mini")
			requireStringField(t, payload, "alias", "client-mini")
			requireStringField(t, payload, "endpoint", "GET /v1/responses")
			requireStringField(t, payload, "auth_type", "apikey")
			requireStringField(t, payload, "api_key", util.HideAPIKey("test-key"))
			requireMissingField(t, payload, "user_api_key")
			requireStringField(t, payload, "request_id", "gin-request-id")
			requireStringField(t, payload, "trace_id", "gin-request-id")
			requireExecutionUUIDField(t, payload)
			requireMissingField(t, payload, "response_model")
			requireBoolField(t, payload, "failed", true)
			requireFailField(t, payload, http.StatusInternalServerError, "upstream failed")
		},
	)
}

func TestUsageQueuePluginAsyncIgnoresRecycledGinContext(t *testing.T) {
	withEnabledQueue(
		t, func() {
			ginCtx := newTestGinContext(t, http.MethodPost, "/v1/chat/completions", http.StatusOK)
			ctx := util.WithGinContext(context.Background(), ginCtx)
			ctx = internallogging.WithRequestID(ctx, "ctx-request-id")
			ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
			ctx = internallogging.WithResponseStatusHolder(ctx)
			internallogging.SetResponseStatus(ctx, http.StatusInternalServerError)

			mgr := coreusage.NewManager(16)
			defer mgr.Stop()

			mgr.Register(
				pluginFunc(
					func(_ context.Context, _ coreusage.Record) {
						ginCtx.Request = httptest.NewRequest(http.MethodGet, "http://example.com/v1/responses", nil)
						ginCtx.Status(http.StatusOK)
					},
				),
			)
			mgr.Register(&usageQueuePlugin{})

			if errPublish := mgr.Publish(
				ctx, coreusage.Record{
					Provider:    "openai",
					Model:       "gpt-5.4",
					Alias:       "client-gpt",
					APIKey:      "test-key",
					AuthIndex:   "0",
					AuthType:    "apikey",
					Source:      "user@example.com",
					RequestedAt: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
					Latency:     1500 * time.Millisecond,
					Fail: coreusage.Failure{
						StatusCode: http.StatusBadGateway,
						Body:       "bad gateway",
					},
					Detail: coreusage.Detail{
						InputTokens:  10,
						OutputTokens: 20,
						TotalTokens:  30,
					},
				},
			); errPublish != nil {
				t.Fatalf("publish usage record: %v", errPublish)
			}

			payload := waitForSinglePayload(t, 2*time.Second)
			requireStringField(t, payload, "endpoint", "POST /v1/chat/completions")
			requireStringField(t, payload, "alias", "client-gpt")
			requireStringField(t, payload, "api_key", util.HideAPIKey("test-key"))
			requireMissingField(t, payload, "user_api_key")
			requireStringField(t, payload, "request_id", "ctx-request-id")
			requireBoolField(t, payload, "failed", true)
			requireFailField(t, payload, http.StatusBadGateway, "bad gateway")
		},
	)
}

func TestUsageQueuePluginPublishesInvalidTokenTotalsAsInconsistent(t *testing.T) {
	tests := []struct {
		name   string
		detail coreusage.Detail
	}{
		{
			name: "negative token",
			detail: coreusage.Detail{
				InputTokens: -1,
			},
		},
		{
			name: "cached token overflow",
			detail: coreusage.Detail{
				CacheReadTokens:     math.MaxInt64,
				CacheCreationTokens: 1,
			},
		},
		{
			name: "total token overflow",
			detail: coreusage.Detail{
				InputTokens:  math.MaxInt64,
				OutputTokens: 1,
			},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				withEnabledQueue(
					t, func() {
						plugin := &usageQueuePlugin{}
						plugin.HandleUsage(
							context.Background(),
							coreusage.Record{Model: "m", Detail: test.detail},
						)
						payload := popSinglePayload(t)
						requireIntField(t, payload, "accounting_version", coreusage.TokenAccountingSchemaVersion)
						requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityInconsistent, 0, 0)
					},
				)
			},
		)
	}
}

func TestUsageQueuePluginExecutionID(t *testing.T) {
	for _, async := range []bool{false, true} {
		for _, test := range []struct {
			name, recordID, contextID, want string
		}{
			{name: "generated"},
			{name: "whitespace", recordID: " \t ", contextID: " \n "},
			{name: "context", contextID: " context-execution ", want: "context-execution"},
			{name: "record", recordID: " record-execution ", want: "record-execution"},
			{name: "record overrides context", recordID: "record-execution", contextID: "context-execution", want: "record-execution"},
		} {
			path := "direct/"
			if async {
				path = "publisher/"
			}
			t.Run(path+test.name, func(t *testing.T) {
				withEnabledQueue(t, func() {
					ctx := internallogging.WithRequestID(context.Background(), "http-request-id")
					ctx = coreusage.WithExecutionID(ctx, test.contextID)
					plugin := NewUsagePluginForQueue(defaultQueue)
					var publish func(coreusage.Record) error
					if async {
						manager := coreusage.NewManager(16)
						defer manager.Stop()
						manager.Register(plugin)
						ctx = coreusage.WithManager(ctx, manager)
						publish = func(record coreusage.Record) error { return coreusage.PublishRecord(ctx, record) }
					} else {
						publish = func(record coreusage.Record) error {
							plugin.HandleUsage(ctx, record)
							return nil
						}
					}
					var previous string
					for range 2 {
						if err := publish(coreusage.Record{Provider: "fixture", Model: "fixture-model", RequestID: "trace-id", ExecutionID: test.recordID}); err != nil {
							t.Fatal(err)
						}
						payload := waitForSinglePayload(t, 2*time.Second)
						requireStringField(t, payload, "request_id", "http-request-id")
						requireStringField(t, payload, "trace_id", "trace-id")
						if test.want != "" {
							requireStringField(t, payload, "execution_id", test.want)
							continue
						}
						id := requireExecutionUUIDField(t, payload)
						if id == previous {
							t.Fatal("separate records reused an execution ID")
						}
						previous = id
					}
				})
			})
		}
	}
}

func requireExecutionUUIDField(t *testing.T, payload map[string]json.RawMessage) string {
	t.Helper()
	var id string
	if err := json.Unmarshal(payload["execution_id"], &id); err != nil {
		t.Fatalf("decode execution_id: %v", err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || parsed.String() != id {
		t.Fatalf("execution_id = %q, want a canonical UUID v4: %v", id, err)
	}
	return id
}

func withEnabledQueue(t *testing.T, fn func()) {
	t.Helper()

	prevQueueEnabled := Enabled()
	prevUsageEnabled := usageStatisticsEnabled()

	SetEnabled(false)
	SetEnabled(true)
	SetUsageStatisticsEnabled(true)

	defer func() {
		SetEnabled(false)
		SetEnabled(prevQueueEnabled)
		SetUsageStatisticsEnabled(prevUsageEnabled)
	}()

	fn()
}

func newTestGinContext(t *testing.T, method, path string, status int) *gin.Context {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(method, "http://example.com"+path, nil)
	if status != 0 {
		ginCtx.Status(status)
	}
	return ginCtx
}

func popSinglePayload(t *testing.T) map[string]json.RawMessage {
	t.Helper()

	items := PopOldest(10)
	if len(items) != 1 {
		t.Fatalf("PopOldest() items = %d, want 1", len(items))
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(items[0], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

func waitForSinglePayload(t *testing.T, timeout time.Duration) map[string]json.RawMessage {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		items := PopOldest(10)
		if len(items) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if len(items) != 1 {
			t.Fatalf("PopOldest() items = %d, want 1", len(items))
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(items[0], &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		return payload
	}
	t.Fatalf("timeout waiting for queued payload")
	return nil
}

func requireStringField(t *testing.T, payload map[string]json.RawMessage, key, want string) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func requireMissingField(t *testing.T, payload map[string]json.RawMessage, key string) {
	t.Helper()

	if _, ok := payload[key]; ok {
		t.Fatalf("payload unexpectedly contains %q", key)
	}
}

type pluginFunc func(context.Context, coreusage.Record)

func (fn pluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) {
	fn(ctx, record)
}

func requireBoolField(t *testing.T, payload map[string]json.RawMessage, key string, want bool) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %t, want %t", key, got, want)
	}
}

func requireIntField(t *testing.T, payload map[string]json.RawMessage, key string, want int64) {
	t.Helper()

	raw, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q", key)
	}
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", key, got, want)
	}
}

func requireTokenField(t *testing.T, payload map[string]json.RawMessage, key string, want int64) {
	t.Helper()

	raw, ok := payload["tokens"]
	if !ok {
		t.Fatalf("payload missing %q", "tokens")
	}
	var tokens map[string]int64
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatalf("unmarshal tokens: %v", err)
	}
	if got := tokens[key]; got != want {
		t.Fatalf("tokens[%q] = %d, want %d", key, got, want)
	}
}

func requireTokenBreakdown(
	t *testing.T,
	payload map[string]json.RawMessage,
	wantQuality coreusage.TokenAccountingQuality,
	wantTotal int64,
	wantUnclassified int64,
) {
	t.Helper()
	raw, ok := payload["token_breakdown"]
	if !ok {
		t.Fatal("payload missing token_breakdown")
	}
	var got coreusage.TokenBreakdown
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal token_breakdown: %v", err)
	}
	if !got.Valid() || got.Quality != wantQuality || got.TotalTokens != wantTotal || got.UnclassifiedTokens != wantUnclassified {
		t.Fatalf(
			"token_breakdown = %+v, want quality=%s total=%d unclassified=%d", got, wantQuality, wantTotal,
			wantUnclassified,
		)
	}
}

func requireFailField(t *testing.T, payload map[string]json.RawMessage, wantStatus int, wantBody string) {
	t.Helper()

	raw, ok := payload["fail"]
	if !ok {
		t.Fatalf("payload missing %q", "fail")
	}
	var got struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal fail: %v", err)
	}
	if got.StatusCode != wantStatus || got.Body != wantBody {
		t.Fatalf(
			"fail = {status_code:%d body:%q}, want {status_code:%d body:%q}", got.StatusCode, got.Body, wantStatus,
			wantBody,
		)
	}
}

func requireHeaderField(t *testing.T, payload map[string]json.RawMessage, field, key string, want []string) {
	t.Helper()

	headers := requireHeadersField(t, payload, field)
	got, ok := headers[key]
	if !ok {
		t.Fatalf("%s missing header %q", field, key)
	}
	if len(got) != len(want) {
		t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%q] = %v, want %v", field, key, got, want)
		}
	}
}

func requireHeaderNotPresent(t *testing.T, payload map[string]json.RawMessage, field, key string) {
	t.Helper()

	headers := requireHeadersField(t, payload, field)
	if got, ok := headers[key]; ok {
		t.Fatalf("%s[%q] = %v, want missing header", field, key, got)
	}
}

func requireHeaderFieldMasked(t *testing.T, payload map[string]json.RawMessage, field, key, rawValue string) {
	t.Helper()

	headers := requireHeadersField(t, payload, field)
	got, ok := headers[key]
	if !ok || len(got) == 0 {
		t.Fatalf("%s missing header %q", field, key)
	}
	for _, value := range got {
		if value == rawValue {
			t.Fatalf("%s[%q] = %v, want masked value", field, key, got)
		}
	}
}

func requireHeadersField(t *testing.T, payload map[string]json.RawMessage, field string) map[string][]string {
	t.Helper()

	raw, ok := payload[field]
	if !ok {
		t.Fatalf("payload missing %q", field)
	}
	var headers map[string][]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		t.Fatalf("unmarshal %q: %v", field, err)
	}
	return headers
}
