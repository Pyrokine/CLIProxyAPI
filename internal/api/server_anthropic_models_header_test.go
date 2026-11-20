package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestModelsListRecognizesAnthropicVersionWithoutCLIUserAgent(t *testing.T) {
	clientID := "fixture-anthropic-header-models"
	registry.GetGlobalRegistry().RegisterClient(
		clientID, "claude", []*registry.ModelInfo{{ID: "fixture-header-model", Object: "model"}},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
	for _, homeMode := range []bool{false, true} {
		name := "local"
		if homeMode {
			name = "Home"
		}
		t.Run(
			name, func(t *testing.T) {
				var options []ServerOption
				if homeMode {
					client := newHomeCatalogTestClient(
						t,
						`{"claude":[{"id":"fixture-header-model","context_length":4096,"max_completion_tokens":1024}]}`,
					)
					options = append(options, WithHomeClientProvider(func() *home.Client { return client }))
				}
				server := newTestServerWithOptions(t, options...)
				server.cfg.Home.Enabled = homeMode
				var registered gin.HandlerFunc
				for _, route := range server.engine.Routes() {
					if route.Method == http.MethodGet && route.Path == "/v1/models" {
						registered = route.HandlerFunc
					}
				}
				if registered == nil {
					t.Fatal("models route is not registered")
				}
				for _, test := range []struct {
					name, userAgent, version, query string
					claude                          bool
				}{
					{name: "header only", userAgent: "Zed/1.0", version: "2023-06-01", claude: true},
					{name: "legacy CLI", userAgent: "claude-cli/test", claude: true},
					{name: "generic client", userAgent: "Zed/1.0"},
					{name: "blank header", userAgent: "Zed/1.0", version: " "},
					{
						name: "explicit Codex client", userAgent: "Zed/1.0", version: "2023-06-01",
						query: "?client_version=cpa",
					},
				} {
					t.Run(
						test.name, func(t *testing.T) {
							request := httptest.NewRequest(http.MethodGet, "/v1/models"+test.query, nil)
							request.Header.Set("Authorization", "Bearer test-key")
							request.Header.Set("User-Agent", test.userAgent)
							request.Header.Set("Anthropic-Version", test.version)
							response := httptest.NewRecorder()
							if homeMode {
								ctx, _ := gin.CreateTestContext(response)
								ctx.Request = request
								ctx.Set("apiKey", "test-key")
								registered(ctx)
							} else {
								server.engine.ServeHTTP(response, request)
							}
							if response.Code != http.StatusOK {
								t.Fatalf("model response status=%d body=%s", response.Code, response.Body)
							}
							body := response.Body.String()
							if got := gjson.Get(body, "has_more").Exists(); got != test.claude {
								t.Fatalf("Claude format=%t want=%t body=%s", got, test.claude, body)
							}
							if test.claude && (!gjson.Get(body, "first_id").Exists() || !gjson.Get(
								body, "last_id",
							).Exists()) {
								t.Fatalf("Claude pagination fields missing: %s", body)
							}
						},
					)
				}
			},
		)
	}
}
