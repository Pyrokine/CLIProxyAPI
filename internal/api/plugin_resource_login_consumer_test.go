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
)

func TestPluginResourceSessionFrontendLoginConsumer(t *testing.T) {
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
				original := issueAuditResourceCookie(t, server)
				other := issueAuditResourceCookie(t, server)
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
					if req.URL.Path == pluginResourceSessionPath && req.Method == http.MethodPost && !invalidLogin &&
						!injectedRotation {
						cookies := rec.Result().Cookies()
						if rec.Code != http.StatusNoContent || len(cookies) != 1 {
							t.Fatalf("rotation response: %d cookies=%v", rec.Code, cookies)
						}
						previous := original
						if len(rotations) > 0 {
							previous = rotations[len(rotations)-1]
						}
						if resourceCookieAuthenticated(server, previous) || !resourceCookieAuthenticated(
							server, cookies[0],
						) {
							t.Fatal("login retained the previous cookie or failed to publish the new cookie")
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
				wantRotations := 3
				current := original
				if rotationStatus != 0 {
					wantRotations = 0
				} else if len(rotations) == 3 {
					current = rotations[2]
				}
				if !completed || len(rotations) != wantRotations {
					t.Fatalf(
						"login/relogin/restore consumer incomplete: rotations=%d completed=%v stderr=%s",
						len(rotations), completed, stderr.String(),
					)
				}
				if rotationStatus == http.StatusInternalServerError {
					if !resourceCookieAuthenticated(server, original) || len(methods) != 1 {
						t.Fatal("rotation failure changed the original session or continued login")
					}
				} else if resourceCookieAuthenticated(server, current) {
					t.Fatal("logout retained the current login session")
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
