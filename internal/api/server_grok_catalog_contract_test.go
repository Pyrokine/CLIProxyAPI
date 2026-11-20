package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/client/grokbuild"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/gin-gonic/gin"
)

func TestGrokCatalogReasoningLevelsFormatsStayIndependent(t *testing.T) {
	for _, sample := range []struct {
		name     string
		thinking any
		want     []string
	}{
		{
			name: "registered pointer", thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
			want: []string{"low", "high"},
		},
		{
			name: "registered value", thinking: registry.ThinkingSupport{Levels: []string{"low", "high"}},
			want: []string{"low", "high"},
		},
		{name: "JSON levels", thinking: map[string]any{"levels": []any{"low", "high"}}, want: []string{"low", "high"}},
		{
			name: "native levels", thinking: map[string]any{"levels": []string{"low", "high"}},
			want: []string{"low", "high"},
		},
		{name: "nil pointer", thinking: (*registry.ThinkingSupport)(nil)},
		{name: "absent thinking"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				model := map[string]any{"thinking": sample.thinking}
				levels := modelMapReasoningLevels(model)
				if !slices.Equal(levels, sample.want) {
					t.Fatalf("levels=%v, want %v", levels, sample.want)
				}
				if len(levels) != 0 {
					levels[0] = "changed"
					if current := modelMapReasoningLevels(model); !slices.Equal(current, sample.want) {
						t.Fatalf("returned levels changed source metadata: %v", current)
					}
				}
			},
		)
	}
}

func TestGrokCatalogAtRegisteredRouteKeepsScopeFieldsAndAPIKeyPolicy(t *testing.T) {
	for _, homeMode := range []bool{false, true} {
		name := "local"
		if homeMode {
			name = "Home"
		}
		t.Run(
			name, func(t *testing.T) {
				const visible, blocked, localOnly = "grok-contract-visible", "grok-contract-blocked", "grok-local-only"
				models := registry.GetGlobalRegistry()
				models.RegisterClient(
					t.Name(), "openai", []*registry.ModelInfo{
						{
							ID: visible, DisplayName: "Visible", ContextLength: 123456,
							Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
						},
						{ID: blocked}, {ID: localOnly},
					},
				)
				t.Cleanup(func() { models.UnregisterClient(t.Name()) })
				var client *home.Client
				if homeMode {
					client = newHomeCatalogTestClient(
						t, `{"xai":[
					{"id":"grok-contract-visible","display_name":"Visible","context_length":123456,
					 "thinking":{"levels":["low","high"]}},
					{"id":"grok-contract-blocked"}
				]}`,
					)
				}
				server := newTestServerWithOptions(t, WithHomeClientProvider(func() *home.Client { return client }))
				updated := *server.configSnapshot()
				updated.Home.Enabled = homeMode
				updated.APIKeyRules = map[string]config.APIKeyRule{
					"test-key": {BlockedModels: []string{blocked}},
				}
				server.setConfig(&updated)
				server.handlers.UpdateClients(effectiveSDKConfig(&updated))
				var route gin.HandlerFunc
				for _, registered := range server.engine.Routes() {
					if registered.Method == http.MethodGet && registered.Path == "/v1/models" {
						route = registered.HandlerFunc
					}
				}
				if route == nil {
					t.Fatal("model-list route is not registered")
				}
				for _, agent := range []string{"grok-shell/0.2.120", "grok-pager/1.0.5"} {
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.2.120", nil)
					ctx.Request.Header.Set("User-Agent", agent)
					ctx.Request.Header.Set("Authorization", "Bearer test-key")
					ctx.Set("apiKey", "test-key")
					route(ctx)
					if recorder.Code != http.StatusOK {
						t.Fatalf("agent=%s status=%d body=%s", agent, recorder.Code, recorder.Body.String())
					}
					var response grokbuild.Response
					if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					found := false
					for _, entry := range response.Data {
						if entry.ID == blocked || (homeMode && entry.ID == localOnly) {
							t.Fatalf("agent=%s exposed blocked or another scope's model: %#v", agent, entry)
						}
						if entry.ID != visible {
							continue
						}
						found = true
						if entry.Model != visible || entry.Name != "Visible" || entry.ContextWindow != 123456 ||
							entry.APIBackend != "responses" || !entry.SupportedInAPI ||
							len(entry.ReasoningEfforts) != 2 || entry.ReasoningEfforts[0].Value != "low" ||
							entry.ReasoningEfforts[1].Value != "high" {
							t.Fatalf("agent=%s lost catalog fields: %#v", agent, entry)
						}
					}
					if !found || response.Object != "list" {
						t.Fatalf("agent=%s lost the visible model: %s", agent, recorder.Body.String())
					}
				}
			},
		)
	}
}
