package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
)

type alphaSearchBusyDispatcher struct{ homeSearchDispatcher }

func (*alphaSearchBusyDispatcher) RPopAuthWithPolicy(context.Context, string, string, http.Header, int, string) (
	[]byte,
	error,
) {
	return []byte(
		`{"error":{"type":"credential_concurrency_exceeded","message":"fixture busy","retry_after_ms":750}}`,
	), nil
}

func TestAlphaSearchBusyResponseExposesRetryAfter(t *testing.T) {
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		t.Run(
			path, func(t *testing.T) {
				server := newTestServer(t)
				manager := server.handlers.AuthManager
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				registry := executionregistry.New()
				t.Cleanup(func() { _ = registry.Close() })
				manager.PublishHomeDispatch(&alphaSearchBusyDispatcher{}, registry, 1)
				request := httptest.NewRequest(
					http.MethodPost, path, strings.NewReader(`{"model":"gpt-5-codex","query":"fixture"}`),
				)
				request.Header.Set("Authorization", "Bearer test-key")
				request.Header.Set("Origin", "https://fixture-management.invalid")
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" ||
					response.Header().Get("Access-Control-Allow-Origin") != "*" {
					t.Fatalf(
						"busy response status=%d headers=%v body=%s", response.Code, response.Header(), response.Body,
					)
				}
				exposed := false
				for header := range strings.SplitSeq(response.Header().Get("Access-Control-Expose-Headers"), ",") {
					exposed = exposed || strings.EqualFold(strings.TrimSpace(header), "Retry-After")
				}
				if !exposed {
					t.Fatalf(
						"cross-origin retry delay hidden: %q", response.Header().Get("Access-Control-Expose-Headers"),
					)
				}
			},
		)
	}
}
