package management

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestQwenManagementRejectsInvalidSuccessfulTokenBeforePersistence(t *testing.T) {
	for _, test := range []struct {
		name  string
		body  string
		valid bool
	}{
		{"empty", `{}`, false},
		{"null", `null`, false},
		{"blank access token", `{"access_token":" \t","refresh_token":"synthetic-not-persisted"}`, false},
		{
			"valid control",
			`{"access_token":"synthetic-qwen-access","refresh_token":"synthetic-ref` +
				`resh","resource_url":"https://resource.invalid","token_type":"Bearer",` +
				`"expires_in":3600}`,
			true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				previousSessions, previousTransport := oauthSessions, http.DefaultTransport
				oauthSessions = newOAuthSessionStore(time.Minute)
				t.Cleanup(func() { oauthSessions, http.DefaultTransport = previousSessions, previousTransport })
				var tokenCalls, publications atomic.Int32
				http.DefaultTransport = roundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						if req.Method != http.MethodPost {
							return nil, errors.New("unexpected synthetic Qwen method")
						}
						body := ""
						switch req.URL.String() {
						case "https://chat.qwen.ai/api/v1/oauth2/device/code":
							body = `{"device_code":"synthetic-device","user_code":"SYNTHETIC","` +
								`verification_uri_complete":"https://verification.invalid","expires_in":60}`
						case "https://chat.qwen.ai/api/v1/oauth2/token":
							tokenCalls.Add(1)
							body = test.body
						default:
							return nil, errors.New("unexpected synthetic Qwen endpoint")
						}
						return &http.Response{
							StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
						}, nil
					},
				)
				dir := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(dir)
				manager := coreauth.NewManager(store, nil, nil)
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store, authManager: manager}
				h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { publications.Add(1); return nil }
				response := performOAuthSessionRequest(
					t, http.MethodGet, "/qwen-auth-url?nonce=synthetic-qwen", h.RequestQwenToken,
				)
				if response.Code != 200 {
					t.Fatalf("start status=%d body=%s", response.Code, response.Body.String())
				}
				state, _ := decodeOAuthSessionResponse(t, response)["state"].(string)
				if state == "" {
					t.Fatal("device flow has no state")
				}
				t.Cleanup(func() { cancelOAuthSession(state) })
				deadline := time.After(3 * time.Second)
				for {
					_, status, completed, exists := getOAuthSessionDetails(state)
					if exists && (status != "" || completed) {
						if test.valid != completed || test.valid && status != "" ||
							!test.valid && status != "Authentication failed" {
							t.Fatalf("session completed=%t status=%q", completed, status)
						}
						break
					}
					select {
					case <-deadline:
						t.Fatal("synthetic Qwen device flow did not finish")
					case <-time.After(time.Millisecond):
					}
				}
				files, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if test.valid {
					want = 1
				}
				if tokenCalls.Load() != 1 || len(files) != want || len(manager.List()) != want ||
					int(publications.Load()) != want {
					t.Fatalf(
						"token calls=%d files=%d auths=%d publications=%d want effects=%d", tokenCalls.Load(),
						len(files), len(manager.List()), publications.Load(), want,
					)
				}
			},
		)
	}
}
