package redisqueue_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestUsageQueueMetadataReachesAsyncConsumer(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("trusted=%t/stream=%t/failed=%t", trusted, streaming, failed), func(t *testing.T) {
						queue := redisqueue.NewQueue()
						queue.SetEnabled(true)
						queue.SetUsageStatisticsEnabled(true)
						t.Cleanup(queue.Close)
						manager := coreusage.NewManager(1)
						manager.Register(redisqueue.NewUsagePluginForQueue(queue))
						manager.Start(context.Background())
						t.Cleanup(manager.Stop)

						request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						request.RemoteAddr = "127.0.0.1:4321"
						request.Header.Set("X-Forwarded-For", "203.0.113.5")
						request.Header.Set("User-Agent", "metadata-client/1.0")
						var trustedProxies []string
						wantIP := "127.0.0.1"
						if trusted {
							trustedProxies = []string{"127.0.0.1/32"}
							wantIP = "203.0.113.5"
						}
						client := access.ResolveClientAddress(request, trustedProxies)
						request = request.WithContext(access.WithClientAddress(request.Context(), client))
						ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
						ginContext.Request = request
						handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
						ctx, cancel := handler.GetContextWithCancel(nil, ginContext, t.Context())
						defer cancel()
						ctx = coreusage.WithManager(ctx, manager)
						ctx = coreusage.WithServiceTier(ctx, "auto")
						headers := http.Header{
							"X-Upstream-Request-Id": {"upstream-req-1"},
							"X-Credits-Remaining":   {"17", "19"},
							"Retry-After":           {"30"},
							"Set-Cookie":            {"session=synthetic-secret"},
							"X-Api-Key":             {"sk-synthetic-secret"},
						}
						internallogging.SetResponseHeaders(ctx, headers)
						reporter := runtimeexecutor.NewPluginExecutorUsageReporter(
							ctx, "openai", "metadata-model", &coreauth.Auth{ID: t.Name(), Provider: "openai"},
						)
						if streaming {
							reporter.MarkStream()
						}
						detail := coreusage.Detail{InputTokens: 1, TotalTokens: 1, ResponseServiceTier: "priority"}
						if failed {
							reporter.PublishFailureWithDetail(ctx, detail, errors.New("synthetic upstream failure"))
						} else {
							reporter.Publish(ctx, detail)
						}
						headers["X-Credits-Remaining"][0] = "changed"
						internallogging.SetResponseHeaders(ctx, http.Header{"X-Upstream-Request-Id": {"later-attempt"}})
						ginContext.Request.Header.Set("User-Agent", "recycled-context")
						manager.Stop()
						items := queue.PopOldest(2)
						if len(items) != 1 {
							t.Fatalf("queued records = %d, want one", len(items))
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(items[0], &payload); err != nil {
							t.Fatal(err)
						}
						for field, want := range map[string]string{
							"client_ip": wantIP, "resolved_client_ip": wantIP,
							"x_forwarded_for": "203.0.113.5", "user_agent": "metadata-client/1.0",
							"service_tier": "auto", "response_service_tier": "priority",
						} {
							var got string
							if err := json.Unmarshal(payload[field], &got); err != nil || got != want {
								t.Fatalf("%s = %q, want %q; error=%v", field, got, want, err)
							}
						}
						if _, exists := payload["request_service_tier"]; exists {
							t.Fatal("deprecated request tier leaked into Redis schema")
						}
						for field, want := range map[string]bool{"stream": streaming, "failed": failed} {
							var got bool
							if err := json.Unmarshal(payload[field], &got); err != nil || got != want {
								t.Fatalf("%s = %t, want %t; error=%v", field, got, want, err)
							}
						}
						var captured http.Header
						if err := json.Unmarshal(payload["response_headers"], &captured); err != nil {
							t.Fatal(err)
						}
						for field, want := range map[string][]string{
							"X-Upstream-Request-Id": {"upstream-req-1"},
							"X-Credits-Remaining":   {"17", "19"},
							"Retry-After":           {"30"},
						} {
							if !reflect.DeepEqual(captured.Values(field), want) {
								t.Fatalf("%s = %v, want %v", field, captured.Values(field), want)
							}
						}
						if captured.Get("Set-Cookie") != "" || captured.Get("X-Api-Key") == "" ||
							captured.Get("X-Api-Key") == "sk-synthetic-secret" {
							t.Fatal("sensitive response headers were not sanitized")
						}
					},
				)
			}
		}
	}
}
