package handlers_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type publicContextExecutor struct {
	auth.ProviderExecutor
	calls atomic.Int32
}

func (*publicContextExecutor) Identifier() string { return "fixture-public-context" }

func (executor *publicContextExecutor) Execute(
	context.Context,
	*auth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	executor.calls.Add(1)
	return coreexecutor.Response{Payload: []byte(`{"choices":[]}`)}, nil
}

func TestPublicRegistryHTTPHonorsMaxContextMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name             string
		context, maximum int
	}{
		{"max only", 0, 1_000_000},
		{"low context with max", 200_000, 1_000_000},
		{"context only control", 1_000_000, 0},
		{"both high control", 1_000_000, 1_000_000},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				model := "fixture-public-context-" + strings.ReplaceAll(test.name, " ", "-")
				id := model + "-auth"
				manager := auth.NewManager(nil, nil, nil)
				executor := &publicContextExecutor{}
				manager.RegisterExecutor(executor)
				if _, err := manager.Register(
					t.Context(), &auth.Auth{
						ID: id, Provider: executor.Identifier(), Status: auth.StatusActive,
						Attributes: map[string]string{"api_key": "fixture-value"},
					},
				); err != nil {
					t.Fatal(err)
				}
				models := cliproxy.GlobalModelRegistry()
				models.RegisterClient(
					id, executor.Identifier(),
					[]*cliproxy.ModelInfo{{ID: model, ContextLength: test.context, MaxContextLength: test.maximum}},
				)
				t.Cleanup(func() { models.UnregisterClient(id) })
				manager.RefreshSchedulerEntry(id)
				if _, err := manager.Execute(
					t.Context(), []string{executor.Identifier()},
					coreexecutor.Request{Model: model + "[1m]", Payload: []byte(`{}`)}, coreexecutor.Options{},
				); err != nil || executor.calls.Load() != 1 {
					t.Fatalf("direct manager: error=%v calls=%d", err, executor.calls.Load())
				}
				executor.calls.Store(0)
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)
				router := gin.New()
				router.POST("/v1/chat/completions", openai.NewAPIHandler(base).ChatCompletions)
				body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"fixture"}]}`, model+"[1m]")
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusOK || executor.calls.Load() != 1 {
					t.Fatalf(
						"HTTP rejected trusted model metadata: status=%d calls=%d body=%s", response.Code,
						executor.calls.Load(), response.Body,
					)
				}
			},
		)
	}
}
