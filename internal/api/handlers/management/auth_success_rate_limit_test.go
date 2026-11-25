package management

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestManagementAuthSuccessKeepsSharedIPFailures(t *testing.T) {
	for _, mode := range []string{"http", "resp-auth-hook"} {
		t.Run(
			mode, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						t.Setenv("MANAGEMENT_PASSWORD", "")
						cfg := &config.Config{AuthDir: t.TempDir()}
						cfg.RemoteManagement.AllowRemote = true
						handler := NewHandler(cfg, "", nil, withEnvSecret("correct"))
						defer func() { _ = handler.Close() }()
						engine := gin.New()
						engine.Use(handler.middleware())
						engine.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })
						const ip = "203.0.113.1"
						success := func() {
							t.Helper()
							if mode == "http" {
								recorder := httptest.NewRecorder()
								engine.ServeHTTP(
									recorder,
									newRemoteRequest(http.MethodGet, "/test", "Bearer correct"),
								)
								if recorder.Code != http.StatusOK {
									t.Fatalf(
										"successful HTTP auth: %d %s",
										recorder.Code,
										recorder.Body.String(),
									)
								}
							} else if allowed, status, message := handler.AuthenticateManagementKey(
								ip, false, "correct",
							); !allowed || status != 0 || message != "" {
								t.Fatalf(
									"successful RESP hook auth: allowed=%t status=%d message=%s", allowed, status,
									message,
								)
							}
						}
						failure := func(value string) {
							t.Helper()
							if allowed, status, _ := handler.AuthenticateManagementKey(
								ip, false, value,
							); allowed || status != http.StatusUnauthorized {
								t.Fatalf("invalid credential: allowed=%t status=%d", allowed, status)
							}
						}
						failure("wrong-0")
						success()
						handler.attemptsMu.Lock()
						attempt := handler.failedAttempts[ip]
						count, pending, fingerprint, observed := len(
							attempt.failures,
						), attempt.pendingNoticeFailures, attempt.lastFailureFingerprint,
							!attempt.lastSuccessfulAuthAt.IsZero()
						handler.attemptsMu.Unlock()
						if count != 1 || pending != 1 || fingerprint == "" || !observed {
							t.Errorf(
								"success lost failures: count=%d pending=%d fingerprint=%t success_time=%t",
								count,
								pending,
								fingerprint != "",
								observed,
							)
						}
						failure("wrong-0")
						handler.attemptsMu.Lock()
						count, pending = len(attempt.failures), attempt.pendingNoticeFailures
						handler.attemptsMu.Unlock()
						if count != 1 || pending != 1 {
							t.Errorf(
								"duplicate after success was counted twice: count=%d pending=%d",
								count,
								pending,
							)
						}
						for i := 1; i < managementMaxFailures; i++ {
							failure(fmt.Sprintf("wrong-%d", i))
							if i+1 < managementMaxFailures {
								success()
							}
						}
						if _, active := handler.currentRateLimitBan(ip, time.Now()); !active {
							t.Fatal("expected shared IP failure state to remain active")
						}
						success()
						if allowed, status, _ := handler.AuthenticateManagementKey(
							ip, false, "unrecognized",
						); allowed || status != http.StatusTooManyRequests {
							t.Errorf("unverified key: allowed=%t status=%d", allowed, status)
						}
						recorder := httptest.NewRecorder()
						engine.ServeHTTP(recorder, newRemoteRequest(http.MethodGet, "/test", "Bearer unrecognized"))
						if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") == "" {
							t.Errorf(
								"HTTP failure cooldown: status=%d Retry-After=%s", recorder.Code,
								recorder.Header().Get("Retry-After"),
							)
						}
					},
				)
			},
		)
	}
}
