package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	proxyconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestRealtimeModelPolicyUsesFixedSessionAndActualBootstrapModel(t *testing.T) {
	server := newTestServer(t)
	updated := *server.configSnapshot()
	updated.APIKeyRules = map[string]proxyconfig.APIKeyRule{
		"test-key": {Providers: []string{"codex"}, CanonicalModels: []string{"future-live-model"}},
	}
	server.UpdateClients(&updated)
	issued := httptest.NewRecorder()
	issueRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/realtime/client_secrets",
		strings.NewReader(`{"session":{"model":"future-live-model"}}`),
	)
	issueRequest.Header.Set("Authorization", "Bearer test-key")
	issueRequest.Header.Set("Content-Type", "application/json")
	server.engine.ServeHTTP(issued, issueRequest)
	secret := gjson.GetBytes(issued.Body.Bytes(), "value").String()
	if issued.Code != http.StatusOK || secret == "" {
		t.Fatalf("restricted custom-model issuance=%d: %s", issued.Code, issued.Body.String())
	}
	for _, test := range []struct {
		name, method, path, contentType, body string
		ephemeral                             bool
		want                                  int
	}{
		{
			name: "fixed websocket without query", method: http.MethodGet, path: "/v1/realtime", ephemeral: true,
			want: http.StatusUpgradeRequired,
		},
		{
			name: "fixed websocket explicit model", method: http.MethodGet,
			path: "/v1/realtime?model=future-live-model", ephemeral: true, want: http.StatusUpgradeRequired,
		},
		{
			name: "standard websocket default remains restricted", method: http.MethodGet, path: "/v1/realtime",
			want: http.StatusForbidden,
		},
		{
			name: "fixed websocket forbidden explicit model", method: http.MethodGet,
			path: "/v1/realtime?model=other-model", ephemeral: true, want: http.StatusForbidden,
		},
		{
			name: "fixed SDP bootstrap", method: http.MethodPost, path: "/v1/realtime", contentType: "application/sdp",
			body: "v=0\r\n", ephemeral: true, want: http.StatusServiceUnavailable,
		},
		{
			name: "fixed SDP ignores query model", method: http.MethodPost, path: "/v1/realtime?model=other-model",
			contentType: "application/sdp", body: "v=0\r\n", ephemeral: true, want: http.StatusServiceUnavailable,
		},
		{
			name: "standard JSON bootstrap uses body model", method: http.MethodPost, path: "/v1/realtime",
			contentType: "application/json", body: `{"model":"future-live-model","sdp":"v=0\r\n"}`,
			want: http.StatusServiceUnavailable,
		},
		{
			name: "standard forbidden body remains restricted", method: http.MethodPost, path: "/v1/realtime",
			contentType: "application/" +
				"json", body: `{"model":"other-model","sdp":` +
				`"v=0\r\n"}`, want: http.StatusForbidden,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
				key := "test-key"
				if test.ephemeral {
					key = secret
				}
				request.Header.Set("Authorization", "Bearer "+key)
				if test.contentType != "" {
					request.Header.Set("Content-Type", test.contentType)
				}
				response := httptest.NewRecorder()
				server.engine.ServeHTTP(response, request)
				if response.Code != test.want {
					t.Fatalf(
						"actual route status=%d, want=%d, body=%s",
						response.Code,
						test.want,
						response.Body.String(),
					)
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("Realtime response lost no-store")
				}
			},
		)
	}
}
