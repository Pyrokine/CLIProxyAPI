package executor

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuditQwenRefreshRejectsEmptyOAuthBeforeMutatingCredential(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, payload := range []string{`{}`, `{"expires_in":3600}`, `{"access_token":"   "}`} {
		t.Run(
			payload, func(t *testing.T) {
				calls := 0
				http.DefaultTransport = devinTestRoundTripper(
					func(request *http.Request) (*http.Response, error) {
						calls++
						if request.URL.Host != "chat.qwen.ai" || request.URL.Path != "/api/v1/oauth2/token" {
							t.Fatalf("unexpected synthetic token route: %s", request.URL)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(payload)), Request: request,
						}, nil
					},
				)
				credential := &auth.Auth{
					ID: "qwen-synthetic", Provider: "qwen", Metadata: map[string]any{
						"access_token": "synthetic-existing-access", "refresh_token": "synthetic-refresh",
						"last_refresh": "original-refresh-time",
					},
				}
				before := credential.Clone()
				result, err := NewQwenExecutor(&config.Config{}).Refresh(t.Context(), credential)
				if err == nil || result != nil || calls != 1 || !reflect.DeepEqual(credential, before) {
					t.Fatalf(
						"empty OAuth result reached credential mutation: error=%v result=%#v calls=%d credential=%#v",
						err, result, calls, credential,
					)
				}
			},
		)
	}
}
