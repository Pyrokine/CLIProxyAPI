package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementBrowserSessionRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	server := newTestServer(t)
	cfg := *server.configSnapshot()
	cfg.RemoteManagement.AllowRemote = true
	cfg.TLS.RequireForAuth = true
	setKey := func(key string) {
		t.Helper()
		hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		next := cfg
		next.RemoteManagement.SecretKey = string(hash)
		server.mgmt.SetConfig(&next)
	}
	setKey("initial-test-key")
	server.registerManagementRoutes()
	server.managementRoutesEnabled.store(true)
	request := func(method, target, key, token, peer string, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = peer
		if key != "" {
			req.Header.Set("X-Management-Key", key)
		}
		if token != "" {
			req.Header.Set(management.ManagementSessionTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d", method, req.URL.Path, rec.Code, want)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s cache control: %q", method, req.URL.Path, rec.Header().Get("Cache-Control"))
		}
		return rec
	}
	const loopback = "127.0.0.1:12345"
	const remote = "192.0.2.1:12345"
	const sessionPath = "/v0/management/session"
	const configPath = "/v0/management/config"
	login := func(key string) string {
		t.Helper()
		rec := request(http.MethodPost, sessionPath, key, "", loopback, http.StatusCreated)
		var session struct {
			Token string `json:"session_token"`
		}
		if err := json.Unmarshal(
			rec.Body.Bytes(), &session,
		); err != nil || len(session.Token) != 64 || session.Token == key {
			t.Fatal("login did not issue an opaque browser session")
		}
		return session.Token
	}
	request(http.MethodPost, sessionPath, "incorrect-test-key", "", loopback, http.StatusUnauthorized)
	request(http.MethodPost, sessionPath, "initial-test-key", "", remote, http.StatusMisdirectedRequest)
	first, other := login("initial-test-key"), login("initial-test-key")
	if first == other {
		t.Fatal("two logins received the same session")
	}
	request(http.MethodGet, configPath, "", first, loopback, http.StatusOK)
	request(http.MethodGet, configPath, "", first, loopback, http.StatusOK)
	request(http.MethodGet, configPath, "", first, remote, http.StatusMisdirectedRequest)
	request(http.MethodPost, sessionPath, "", first, loopback, http.StatusUnauthorized)
	request(http.MethodGet, "/v0/management/codex-auth-url", "", first, loopback, http.StatusBadRequest)
	request(http.MethodDelete, sessionPath, "", first, loopback, http.StatusNoContent)
	request(http.MethodGet, configPath, "", first, loopback, http.StatusUnauthorized)
	request(http.MethodGet, configPath, "", other, loopback, http.StatusOK)
	request(http.MethodGet, configPath, "initial-test-key", "", loopback, http.StatusOK)

	setKey("replacement-test-key")
	request(http.MethodGet, configPath, "", other, loopback, http.StatusUnauthorized)
	request(http.MethodPost, sessionPath, "initial-test-key", "", loopback, http.StatusUnauthorized)
	request(http.MethodGet, configPath, "", login("replacement-test-key"), loopback, http.StatusOK)
}

func TestPluginResourceSessionFrontendLoginConsumer(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	fixture := os.Getenv("CPAM_LOGIN_SESSION_FIXTURE")
	if fixture == "" {
		t.Skip("set CPAM_LOGIN_SESSION_FIXTURE to the management center loginResourceSession.ts fixture")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	for _, rotationStatus := range []int{
		0, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusInternalServerError,
	} {
		t.Run(
			"rotation-status-"+strconv.Itoa(rotationStatus), func(t *testing.T) {
				server := newTestServer(t)
				server.mgmt.SetLocalPassword("synthetic-login-password")
				server.registerManagementRoutes()
				server.managementRoutesEnabled.store(true)
				original := issueAuditResourceCookie(t, server, "synthetic-login-password")
				other := issueAuditResourceCookie(t, server, "synthetic-login-password")
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, bun, fixture)
				var stderr bytes.Buffer
				child.Stderr = &stderr
				input, err := child.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				output, err := child.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err = child.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cancel(); _ = child.Wait() })
				encoder := json.NewEncoder(input)
				if err = encoder.Encode(
					map[string]any{
						"apiBase": "http://127.0.0.1", "managementKey": "synthetic-login-password",
						"cookie": original.Name + "=" + original.Value, "rotationStatus": rotationStatus,
					},
				); err != nil {
					t.Fatal(err)
				}
				var rotations []*http.Cookie
				var managementTokens []string
				var methods []string
				completed := false
				scanner := bufio.NewScanner(output)
				for scanner.Scan() {
					var message struct {
						Method  string            `json:"method"`
						URL     string            `json:"url"`
						Headers map[string]string `json:"headers"`
						Body    string            `json:"body"`
						Done    bool              `json:"done"`
					}
					if err = json.Unmarshal(scanner.Bytes(), &message); err != nil {
						t.Fatalf("fixture transport: %s: %v", scanner.Text(), err)
					}
					if message.Done {
						completed = true
						break
					}
					req := httptest.NewRequest(message.Method, message.URL, strings.NewReader(message.Body))
					req.RemoteAddr = "127.0.0.1:12345"
					for key, value := range message.Headers {
						req.Header.Set(key, value)
					}
					rec := httptest.NewRecorder()
					injectedRotation := rotationStatus != 0 && req.URL.Path == pluginResourceSessionPath &&
						req.Method == http.MethodPost
					if injectedRotation {
						rec.WriteHeader(rotationStatus)
					} else {
						server.engine.ServeHTTP(rec, req)
					}
					invalidLogin := req.Header.Get("Authorization") == "Bearer synthetic-invalid-password"
					if rec.Code >= 400 && !injectedRotation && !(invalidLogin && rec.Code == http.StatusUnauthorized) {
						t.Fatalf("%s %s returned %d: %s", message.Method, req.URL.Path, rec.Code, rec.Body.String())
					}
					methods = append(methods, message.Method+" "+req.URL.Path)
					if req.URL.Path == "/v0/management/session" && req.Method == http.MethodPost && !invalidLogin {
						var created struct {
							Token string `json:"session_token"`
						}
						if rec.Code != http.StatusCreated || json.Unmarshal(
							rec.Body.Bytes(), &created,
						) != nil || len(created.Token) != 64 {
							t.Fatalf("expected opaque management session, status=%d", rec.Code)
						}
						managementTokens = append(managementTokens, created.Token)
					}
					if req.URL.Path == pluginResourceSessionPath && req.Method == http.MethodPost && !invalidLogin &&
						!injectedRotation {
						cookies := rec.Result().Cookies()
						if rec.Code != http.StatusNoContent || len(cookies) != 1 {
							t.Fatalf("rotation response: %d cookies=%v", rec.Code, cookies)
						}
						if resourceCookieAuthenticated(server, original) || !resourceCookieAuthenticated(
							server, cookies[0],
						) {
							t.Fatal("login retained its legacy resource cookie or failed to publish the new cookie")
						}
						rotations = append(rotations, cookies[0])
					}
					if err = encoder.Encode(
						map[string]any{
							"status": rec.Code, "headers": rec.Header(), "body": rec.Body.String(),
						},
					); err != nil {
						t.Fatal(err)
					}
				}
				_ = input.Close()
				if err = scanner.Err(); err != nil {
					t.Fatal(err)
				}
				if err = child.Wait(); err != nil {
					t.Fatalf("frontend login: %v\n%s", err, stderr.String())
				}
				wantRotations, wantManagementSessions := 2, 2
				if rotationStatus != 0 {
					wantRotations = 0
				}
				if rotationStatus == http.StatusInternalServerError {
					wantManagementSessions = 1
				}
				if !completed || len(rotations) != wantRotations || len(managementTokens) != wantManagementSessions {
					t.Fatalf(
						"browser login/restore/logout incomplete: rotations=%d sessions=%d completed=%v stderr=%s",
						len(rotations), len(managementTokens), completed, stderr.String(),
					)
				}
				for _, token := range managementTokens {
					req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
					req.RemoteAddr = "127.0.0.1:12345"
					req.Header.Set(management.ManagementSessionTokenHeader, token)
					rec := httptest.NewRecorder()
					server.engine.ServeHTTP(rec, req)
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("browser session retained after logout or failed login: %d", rec.Code)
					}
				}
				for _, cookie := range rotations {
					if resourceCookieAuthenticated(server, cookie) {
						t.Fatal("derived resource cookie retained after browser session revocation")
					}
				}
				if rotationStatus != 0 && !resourceCookieAuthenticated(server, original) {
					t.Fatal("a failed resource rotation revoked its prior cookie")
				}
				if !resourceCookieAuthenticated(server, other) {
					t.Fatal("login or logout revoked another session")
				}
				t.Logf(
					"actual frontend login/relogin/restore -> management API -> server cookie -> logout: %v", methods,
				)
			},
		)
	}
}
