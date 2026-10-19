package management

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRequestCodexTokenPersistsSubscriptionPlan(t *testing.T) {
	for _, tc := range []struct {
		name, claim, want string
	}{
		{name: "missing claim", want: "free"},
		{name: "verified plan", claim: "pro", want: "pro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousSessions := oauthSessions
			oauthSessions = newOAuthSessionStore(time.Minute)
			t.Cleanup(func() { oauthSessions = previousSessions })

			claims := map[string]any{"email": "fixture@example.test"}
			if tc.claim != "" {
				claims["https://api.openai.com/auth"] = map[string]string{"chatgpt_plan_type": tc.claim}
			}
			claimsJSON, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			fixtureIDToken := "fixture." + base64.RawURLEncoding.EncodeToString(claimsJSON) + ".fixture"
			responseJSON, err := json.Marshal(map[string]any{
				"id_token": fixtureIDToken, "access_token": "fixture-access-token",
				"refresh_token": "fixture-refresh-token", "expires_in": 3600,
			})
			if err != nil {
				t.Fatal(err)
			}
			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://auth.openai.com/oauth/token" || req.Method != http.MethodPost {
					return nil, errors.New("unexpected Codex OAuth request")
				}
				if err := req.ParseForm(); err != nil || req.PostForm.Get("code") != "fixture-callback" {
					return nil, errors.New("unexpected Codex callback code")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(responseJSON)))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })

			authDir := t.TempDir()
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(authDir)
			manager := coreauth.NewManager(nil, nil, nil)
			handler := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager, tokenStore: store}
			response := performOAuthSessionRequest(t, http.MethodGet, "/codex-auth-url?nonce=fixture-nonce", handler.RequestCodexToken)
			if response.Code != http.StatusOK {
				t.Fatalf("start Codex OAuth: status %d body %s", response.Code, response.Body.String())
			}
			state, _ := decodeOAuthSessionResponse(t, response)["state"].(string)
			if state == "" {
				t.Fatal("Codex OAuth session state is missing")
			}
			if _, err := WriteOAuthCallbackFileForPendingSession("codex", state, "fixture-callback", ""); err != nil {
				cancelOAuthSession(state)
				t.Fatalf("submit fixture Codex callback: %v", err)
			}

			deadline := time.After(5 * time.Second)
			for {
				_, status, completed, ok := getOAuthSessionDetails(state)
				if !ok || completed || status != "" {
					if !ok || !completed || status != "" {
						t.Fatalf("Codex OAuth did not complete: exists=%v completed=%v status=%q", ok, completed, status)
					}
					break
				}
				select {
				case <-deadline:
					cancelOAuthSession(state)
					t.Fatal("Codex OAuth callback did not finish")
				case <-time.After(10 * time.Millisecond):
				}
			}

			auths := manager.List()
			if len(auths) != 1 || auths[0].Provider != "codex" || auths[0].Metadata["plan_type"] != tc.want ||
				auths[0].Attributes["plan_type"] != tc.want {
				t.Fatalf("runtime Codex plan does not match OAuth claim: %+v", auths)
			}
			path := auths[0].Attributes[coreauth.AttributePath]
			if !strings.HasSuffix(filepath.Base(path), "-"+tc.want+".json") {
				t.Fatalf("Codex credential name does not reflect plan: %q", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["plan_type"] != tc.want || saved["id_token"] != fixtureIDToken ||
				saved["access_token"] != "fixture-access-token" {
				t.Fatalf("saved Codex credentials do not match OAuth result: %#v", saved)
			}
		})
	}
}
