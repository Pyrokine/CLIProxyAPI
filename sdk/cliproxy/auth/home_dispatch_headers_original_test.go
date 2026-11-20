package auth

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

func TestOriginalHomeDispatchHeaderInputs(t *testing.T) {
	for _, sample := range []struct {
		name, path, source, apiKey, want string
		headers                          http.Header
	}{
		{name: "query key", path: "/?key=12345", headers: http.Header{"User-Agent": {"client"}}, want: "12345"},
		{
			name: "access metadata", path: "/", source: "query-key", apiKey: "12345",
			headers: http.Header{"User-Agent": {"client"}}, want: "12345",
		},
		{
			name: "existing credential", path: "/?key=query-key",
			headers: http.Header{"X-Goog-Api-Key": {"header-key"}}, want: "header-key",
		},
		{
			name: "authorization source", path: "/", source: "authorization", apiKey: "12345",
			headers: http.Header{"Authorization": {"Bearer 12345"}},
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Request = httptest.NewRequest(http.MethodGet, sample.path, nil)
				if sample.source != "" {
					ginCtx.Set("accessMetadata", map[string]string{"source": sample.source})
					ginCtx.Set("apiKey", sample.apiKey)
				}
				before := sample.headers.Clone()
				got := homeDispatchHeaders(util.WithGinContext(t.Context(), ginCtx), sample.headers)
				if got.Get("X-Goog-Api-Key") != sample.want || !reflect.DeepEqual(sample.headers, before) {
					t.Fatalf("Home headers=%v original=%v want API key=%q", got, sample.headers, sample.want)
				}
				if got.Get("Authorization") != before.Get("Authorization") ||
					got.Get("User-Agent") != before.Get("User-Agent") {
					t.Fatal("Home dispatch changed an existing authorization or user-agent header")
				}
				if ginCtx.Query("key") != "" || sample.source == "query-key" {
					got.Set("X-Goog-Api-Key", "changed-by-caller")
					if !reflect.DeepEqual(sample.headers, before) {
						t.Fatal("Home dispatch shared a mutable query-credential header map")
					}
				}
			},
		)
	}
}

func TestHomeDispatchedUserAPIKeyReachesRealGinContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Set("apiKey", "original-client-key")
	manager := NewManager(nil, nil, nil)
	dispatcher := &testHomeDispatcher{
		heartbeat: true, payload: []byte(`{"user_api_key":"12345","auth":{"id":"cred-1","provider":"codex"}}`),
	}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "codex"})
	selection, err := manager.pickHomeDispatchSelection(util.WithGinContext(t.Context(), ginCtx), "gpt", core.Options{})
	if err != nil || selection == nil {
		t.Fatalf("Home selection=%v error=%v", selection, err)
	}
	defer selection.End("test_complete")
	if got := ginCtx.GetString("apiKey"); got != "12345" {
		t.Fatalf("Home user API key=%q want=12345", got)
	}
}
