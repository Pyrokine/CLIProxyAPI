package management

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestSetLatestReleaseRequestHeaders(t *testing.T) {
	for _, tc := range []struct{ name, token, authorization string }{
		{"sets GitHub authorization", "release-token", "Bearer release-token"},
		{"omits authorization without token", "", ""},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				t.Setenv("GITHUB_TOKEN", tc.token)
				t.Setenv("github_token", "ignored-lower-token")
				t.Setenv("GH_TOKEN", "ignored-gh-token")
				t.Setenv("GITSTORE_GIT_TOKEN", "ignored-store-token")
				t.Setenv("GITSTORE_GIT_URL", "https://github.com/synthetic/auth-store.git")
				previous := http.DefaultTransport
				calls := 0
				http.DefaultTransport = releaseRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						if request.Method != http.MethodGet ||
							request.URL.String() !=
								("https://api.github.com/repos/synthetic/header-test/"+
									"releases/"+
									"latest") {
							t.Fatalf("unexpected latest release request: %s %s", request.Method, request.URL)
						}
						if got := request.Header.Get("Authorization"); got != tc.authorization {
							t.Fatalf("Authorization=%q want %q", got, tc.authorization)
						}
						if got := request.Header.Get("Accept"); got != "application/vnd.github+json" {
							t.Fatalf("Accept=%q", got)
						}
						if got := request.Header.Get("User-Agent"); got != latestReleaseUserAgent {
							t.Fatalf("User-Agent=%q want %q", got, latestReleaseUserAgent)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
							Body: io.NopCloser(
								strings.NewReader(
									`{"tag_name":"v9.9.9","published_at":"2026-11-12T00:00:00Z"}`,
								),
							),
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = previous })
				cfg := &config.Config{}
				cfg.RemoteManagement.CPAGitHubRepository = "synthetic/header-test"
				h := &Handler{cfg: cfg}
				engine := gin.New()
				engine.GET("/v0/management/latest-version", h.GetLatestVersion)
				response := httptest.NewRecorder()
				engine.ServeHTTP(
					response,
					httptest.NewRequest(http.MethodGet, "/v0/management/latest-version", nil),
				)
				var payload map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusOK || calls != 1 || payload["latest-version"] != "v9.9.9" ||
					payload["published-at"] != "2026-11-12T00:00:00Z" {
					t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
				}
			},
		)
	}
}
