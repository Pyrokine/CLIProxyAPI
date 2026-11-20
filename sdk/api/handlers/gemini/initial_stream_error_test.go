package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type initialStreamFailureExecutor struct {
	coreauth.ProviderExecutor
	dropFirst bool
}

func (initialStreamFailureExecutor) Identifier() string { return "gemini-initial-error-fixture" }

func (e initialStreamFailureExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 2)
	if e.dropFirst {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[]}`)}
		chunks <- coreexecutor.StreamChunk{Terminal: true}
	} else {
		chunks <- coreexecutor.StreamChunk{Err: errors.New("fixture stream failed before its first payload")}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type firstPayloadDropHost struct{}

func (firstPayloadDropHost) InterceptRequestBeforeAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (firstPayloadDropHost) InterceptRequestAfterAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (firstPayloadDropHost) InterceptResponse(
	context.Context,
	pluginapi.ResponseInterceptRequest,
) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}

func (firstPayloadDropHost) InterceptStreamChunk(
	_ context.Context,
	request pluginapi.StreamChunkInterceptRequest,
) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{DropChunk: len(request.Body) > 0}
}

func TestGeminiFirstChunkErrorNeverReturnsEmptySuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		alt       string
		dropFirst bool
	}{{"", false}, {"json", false}, {"", true}, {"json", true}} {
		t.Run(
			"alt="+tc.alt+"/drop-first="+strconv.FormatBool(tc.dropFirst), func(t *testing.T) {
				for attempt := range 32 {
					executor := initialStreamFailureExecutor{dropFirst: tc.dropFirst}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(executor)
					model := "gemini-first-error-" + strconv.Itoa(attempt)
					auth := &coreauth.Auth{
						ID: t.Name() + model, Provider: executor.Identifier(), Status: coreauth.StatusActive,
					}
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					models := registry.GetGlobalRegistry()
					models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
					handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
					if tc.dropFirst {
						handler.SetPluginHost(firstPayloadDropHost{})
					}
					router := gin.New()
					router.POST("/v1beta/models/*action", handler.GeminiHandler)
					recorder := httptest.NewRecorder()
					request := httptest.NewRequest(
						http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent?alt="+tc.alt,
						strings.NewReader(`{"contents":[]}`),
					)
					request = request.WithContext(t.Context())
					router.ServeHTTP(recorder, request)
					models.UnregisterClient(auth.ID)
					if recorder.Code < http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"error"`) {
						t.Fatalf(
							"attempt %d returned status=%d body=%q", attempt, recorder.Code, recorder.Body.String(),
						)
					}
				}
			},
		)
	}
}
