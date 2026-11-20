package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

// noinspection GoErrorStringFormat
func TestRequestCodexTokenCompletionKeepsConcurrentSessionPending(t *testing.T) {
	for _, completeIndex := range []int{0, 1} {
		t.Run(
			[]string{"first completes", "second completes"}[completeIndex], func(t *testing.T) {
				previousSessions := oauthSessions
				oauthSessions = newOAuthSessionStore(time.Minute)
				t.Cleanup(func() { oauthSessions = previousSessions })
				originalTransport := http.DefaultTransport
				codes := []string{"first-code", "second-code"}
				exchanged := make(chan string, 2)
				http.DefaultTransport = roundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						if req.URL.String() != "https://auth.openai.com/oauth/token" ||
							req.Method != http.MethodPost {
							return nil, errors.New("unexpected Codex OAuth request")
						}
						if err := req.ParseForm(); err != nil {
							return nil, err
						}
						code := req.PostForm.Get("code")
						if code != codes[completeIndex] {
							return nil, errors.New("Codex code belongs to the other session")
						}
						if req.PostForm.Get("code_verifier") == "" {
							return nil, errors.New("Codex PKCE verifier is missing")
						}
						data, err := json.Marshal(
							map[string]any{
								"id_token": "invalid-test-id-token", "access_token": "access-" + code,
								"refresh_token": "refresh-" + code, "expires_in": 3600,
							},
						)
						if err != nil {
							return nil, err
						}
						exchanged <- code
						return &http.Response{
							StatusCode: http.StatusOK, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(string(data))),
						}, nil
					},
				)
				t.Cleanup(func() { http.DefaultTransport = originalTransport })

				authDir := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(authDir)
				manager := coreauth.NewManager(nil, nil, nil)
				handler := &Handler{
					cfg:         &config.Config{AuthDir: authDir},
					authManager: manager,
					tokenStore:  store,
				}
				router := gin.New()
				router.GET("/codex-auth-url", handler.RequestCodexToken)
				states := make([]string, 0, 2)
				t.Cleanup(
					func() {
						for _, state := range states {
							if isOAuthSessionPending(state, "codex") {
								if _, err := WriteOAuthCallbackFileForPendingSession(
									"codex", state, "", "access_denied",
								); err != nil {
									t.Errorf("retire pending Codex session: %v", err)
								}
							}
							waitForOAuthSessionDone(t, state)
						}
					},
				)
				for range 2 {
					request := httptest.NewRequest(http.MethodGet, "/codex-auth-url", nil)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					if response.Code != http.StatusOK {
						t.Fatalf("start Codex OAuth status=%d body=%s", response.Code, response.Body.String())
					}
					var payload struct {
						State string `json:"state"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
						t.Fatal(err)
					}
					if payload.State == "" {
						t.Fatal("Codex authorization response omitted state")
					}
					states = append(states, payload.State)
				}
				if states[0] == states[1] {
					t.Fatal("concurrent Codex entries share a state")
				}
				for _, state := range states {
					if !isOAuthSessionPending(state, "codex") {
						t.Fatalf("Codex session %s is not pending", state)
					}
				}
				completed, pending := states[completeIndex], states[1-completeIndex]
				if _, err := WriteOAuthCallbackFileForPendingSession(
					"codex", completed, codes[completeIndex], "",
				); err != nil {
					t.Fatalf("write completed callback: %v", err)
				}
				waitForOAuthSessionDone(t, completed)
				if _, status, complete, ok := getOAuthSessionDetails(completed); !ok || !complete ||
					status != "" {
					t.Fatalf("first completion: exists=%v completed=%v status=%q", ok, complete, status)
				}
				if !isOAuthSessionPending(pending, "codex") {
					t.Fatalf(
						"expected concurrent Codex session %s to remain pending after %s completed",
						pending,
						completed,
					)
				}
				select {
				case code := <-exchanged:
					if code != codes[completeIndex] {
						t.Fatalf("exchanged code=%q", code)
					}
				default:
					t.Fatal("completed OAuth session did not exchange its code")
				}
				select {
				case code := <-exchanged:
					t.Fatalf("pending session exchanged code %q", code)
				default:
				}
				auths := manager.List()
				if len(auths) != 1 {
					t.Fatalf("completed OAuth session published %d credentials", len(auths))
				}
				data, err := os.ReadFile(auths[0].Attributes[coreauth.AttributePath])
				if err != nil {
					t.Fatal(err)
				}
				var persisted map[string]any
				if err = json.Unmarshal(data, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted["access_token"] != "access-"+codes[completeIndex] ||
					persisted["refresh_token"] !=
						"refresh-"+
							codes[completeIndex] ||
					persisted["id_token"] !=
						("invalid-test-i"+
							"d-token") {
					t.Fatalf("completed callback saved the wrong tokens: %#v", persisted)
				}
			},
		)
	}
}

func waitForOAuthSessionDone(t *testing.T, state string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		_, status, completed, ok := getOAuthSessionDetails(state)
		if !ok || completed || status != "" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for Codex session %s to finish", state)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
