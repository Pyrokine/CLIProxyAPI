package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type alphaSearchTerminationRouter struct {
	phase  string
	status int
}

func (*alphaSearchTerminationRouter) HasRequestInterceptors() bool { return true }

func (r *alphaSearchTerminationRouter) intercept(
	phase string,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	if r.phase != phase {
		return pluginapi.RequestInterceptResponse{Headers: request.Headers, Body: request.Body}
	}
	return pluginapi.RequestInterceptResponse{
		Terminate: true, StatusCode: r.status,
		ResponseHeaders: http.Header{
			"Content-Type": {"text/plain"}, "Retry-After": {"7"}, "X-Plugin-Policy": {"declined"},
			"X-Cpa-Trace-Id": {"forged"},
		},
		ResponseBody: []byte("fixture plugin declined\n"),
	}
}

func (r *alphaSearchTerminationRouter) InterceptRequestBeforeAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return r.intercept("before", request)
}

func (r *alphaSearchTerminationRouter) InterceptRequestAfterAuth(
	_ context.Context,
	request pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return r.intercept("after", request)
}

func TestAlphaSearchPluginTerminationPreventsDispatch(t *testing.T) {
	for _, phase := range []string{"before", "after", "pass"} {
		for _, status := range []int{http.StatusServiceUnavailable, 0, http.StatusOK, 600} {
			for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
				t.Run(
					fmt.Sprintf("%s/%d%s", phase, status, path), func(t *testing.T) {
						server := newTestServer(t)
						executor := &c02SearchExecutor{}
						server.handlers.AuthManager.RegisterExecutor(executor)
						registerC02Credential(
							t, server, &auth.Auth{
								ID: "fixture-codex-auth", Provider: "codex", Status: auth.StatusActive,
								Metadata: map[string]any{"access_token": "fixture-codex-token"},
							}, "gpt-5.6-sol",
						)
						router := &alphaSearchTerminationRouter{phase: phase, status: status}
						engine := gin.New()
						engine.POST(path, func(c *gin.Context) { server.codexAlphaSearchWithRouter(c, router) })
						request := httptest.NewRequest(
							http.MethodPost, path, strings.NewReader(`{"model":"gpt-5.6-sol","query":"fixture"}`),
						)
						request.Header.Set("Authorization", "Bearer test-key")
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, request)
						if phase == "pass" {
							if response.Code != http.StatusOK || len(executor.selectedAuthIDs()) != 1 {
								t.Fatalf(
									"unchanged request: status=%d calls=%d body=%s", response.Code,
									len(executor.selectedAuthIDs()), response.Body,
								)
							}
							return
						}
						wantStatus := status
						if wantStatus < http.StatusOK || wantStatus > 599 {
							wantStatus = http.StatusForbidden
						}
						if response.Code != wantStatus || response.Body.String() != "fixture plugin declined\n" ||
							len(executor.selectedAuthIDs()) != 0 {
							t.Fatalf(
								"plugin termination ignored: status=%d body=%q upstream_calls=%d", response.Code,
								response.Body.String(), len(executor.selectedAuthIDs()),
							)
						}
						if response.Header().Get("Content-Type") != "text/plain" ||
							response.Header().Get("Retry-After") != "7" ||
							response.Header().Get("X-Plugin-Policy") != "declined" ||
							response.Header().Get("X-Cpa-Trace-Id") == "forged" {
							t.Fatalf("termination response headers=%v", response.Header())
						}
					},
				)
			}
		}
	}
}
