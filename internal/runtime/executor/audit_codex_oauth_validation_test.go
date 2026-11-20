package executor

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	codexauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuditCodexAutoRefreshRejectsInvalidOAuthBeforeMutation(t *testing.T) {
	for i, payload := range []string{
		`{}`, `null`, `{"access_token":"   "}`,
		`{"access_token":"synthetic-rotated-access","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`,
	} {
		t.Run(
			fmt.Sprint(i), func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.String() != "https://auth.openai.com/oauth/token" {
								return nil, fmt.Errorf("unexpected token route %s", req.URL)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(payload)), Request: req,
							}, nil
						},
					),
				)
				auth := &coreauth.Auth{
					ID: "synthetic-codex", Provider: "codex", Metadata: map[string]any{
						"access_token": "synthetic-old-access", "refresh_token": "synthetic-old-refresh",
						"last_refresh": "synthetic-original-time",
					}, Storage: &codexauth.TokenStorage{
						AccessToken: "synthetic-old-access", RefreshToken: "synthetic-old-refresh",
						LastRefresh: "synthetic-original-time",
					},
				}
				before := auth.Clone()
				result, err := NewCodexAutoExecutor(&config.Config{}).Refresh(ctx, auth)
				if calls != 1 {
					t.Fatalf("refresh requests=%d, want 1", calls)
				}
				if i < 3 {
					if err == nil || result != nil || !reflect.DeepEqual(auth, before) {
						t.Fatalf("invalid 200 changed auth: error=%v result=%#v auth=%#v", err, result, auth)
					}
				} else if err !=
					nil ||
					result ==
						nil ||
					result.Metadata["access_token"] !=
						("synthetic-rotate"+
							"d-access") ||
					result.Storage.(*codexauth.TokenStorage).AccessToken !=
						("synthetic-rota"+
							"ted-access") ||
					result.Metadata["last_refresh"] ==
						"synthetic-original-time" {
					t.Fatalf("valid control failed: result=%#v error=%v", result, err)
				}
			},
		)
	}
}
