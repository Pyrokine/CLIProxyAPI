package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func issueAuditResourceCookie(t *testing.T, server *Server, key string) *http.Cookie {
	t.Helper()
	server.managementRoutesEnabled.store(true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, pluginResourceSessionPath, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Management-Key", key)
	server.engine.ServeHTTP(recorder, request)
	cookies := recorder.Result().Cookies()
	if recorder.Code != http.StatusNoContent || len(cookies) != 1 {
		t.Fatalf("resource session issuance failed: status=%d, cookies=%d", recorder.Code, len(cookies))
	}
	return cookies[0]
}

func resourceCookieAuthenticated(server *Server, cookie *http.Cookie) bool {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/resource/plugins/example", nil)
	ctx.Request.RemoteAddr = "127.0.0.1:12345"
	ctx.Request.AddCookie(cookie)
	return server.pluginResourceSessionAuthenticated(ctx)
}

func TestPluginResourceSessionPreservesManagementAddressScope(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		allowRemote      bool
		wantRemote       bool
	}{
		{name: "local password with remote management", credential: "local", allowRemote: true},
		{name: "management key with local management", credential: "key"},
		{name: "management key with remote management", credential: "key", allowRemote: true, wantRemote: true},
		{name: "internal credential with remote management", credential: "internal", allowRemote: true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				t.Setenv("MANAGEMENT_PASSWORD", "")
				server := newTestServer(t)
				cfg := *server.configSnapshot()
				cfg.RemoteManagement.AllowRemote = tc.allowRemote
				hash, err := bcrypt.GenerateFromPassword([]byte("fixture-management-key"), bcrypt.MinCost)
				if err != nil {
					t.Fatal(err)
				}
				cfg.RemoteManagement.SecretKey = string(hash)
				server.mgmt.SetConfig(&cfg)
				server.mgmt.SetLocalPassword("fixture-local-password")
				keys := map[string]string{
					"key":      "fixture-management-key",
					"local":    "fixture-local-password",
					"internal": server.mgmt.InternalToken(),
				}
				cookie := issueAuditResourceCookie(t, server, keys[tc.credential])
				revoke := func(peer string, want int) {
					t.Helper()
					req := httptest.NewRequest(http.MethodDelete, "https://cpa.invalid"+pluginResourceSessionPath, nil)
					req.RemoteAddr = peer
					req.AddCookie(cookie)
					rec := httptest.NewRecorder()
					server.engine.ServeHTTP(rec, req)
					if rec.Code != want {
						t.Errorf("resource session response = %d, want %d", rec.Code, want)
					}
				}
				if tc.wantRemote {
					revoke("192.0.2.1:12345", http.StatusNoContent)
				} else {
					revoke("192.0.2.1:12345", http.StatusUnauthorized)
					revoke("127.0.0.1:12345", http.StatusNoContent)
				}
			},
		)
	}
}

func TestPluginResourceSessionRejectsOlderAuthenticationGeneration(t *testing.T) {
	server := newTestServer(t)
	server.mgmt.SetLocalPassword("fixture-original-password")
	var current *http.Cookie
	router := gin.New()
	router.POST(
		"/session", server.mgmt.MiddlewareWithConfig(server.managementConfigSnapshot), func(c *gin.Context) {
			server.mgmt.SetLocalPassword("fixture-replacement-password")
			current = issueAuditResourceCookie(t, server, "fixture-replacement-password")
			if server.issuePluginResourceSession(c) {
				t.Error("an older authentication generation issued a resource session")
			}
		},
	)
	request := httptest.NewRequest(http.MethodPost, "/session", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Management-Key", "fixture-original-password")
	record := httptest.NewRecorder()
	router.ServeHTTP(record, request)
	if record.Code != http.StatusUnauthorized || len(record.Result().Cookies()) != 0 {
		t.Fatal("older authentication generation must reject without setting a cookie")
	}
	if current == nil || !resourceCookieAuthenticated(server, current) {
		t.Fatal("rejected issuance revoked a current resource session")
	}
}

func TestPluginResourceSessionCredentialChangesRevokeOldCookies(t *testing.T) {
	server := newTestServer(t)
	server.mgmt.SetLocalPassword("first-password")
	original := issueAuditResourceCookie(t, server, "first-password")
	updated := *server.configSnapshot()
	updated.Debug = !updated.Debug
	server.mgmt.SetConfig(&updated)
	if !resourceCookieAuthenticated(server, original) {
		t.Fatal("unrelated config update revoked session")
	}
	server.mgmt.SetLocalPassword("first-password")
	if !resourceCookieAuthenticated(server, original) {
		t.Fatal("unchanged password revoked session")
	}
	server.mgmt.SetLocalPassword("second-password")
	if resourceCookieAuthenticated(server, original) {
		t.Fatal("old password session remained authenticated")
	}
	server.mgmt.SetLocalPassword("first-password")
	if resourceCookieAuthenticated(server, original) {
		t.Fatal("rotating back revived old session")
	}
	current := issueAuditResourceCookie(t, server, "first-password")
	rotated := updated
	rotated.RemoteManagement.SecretKey = "changed-management-key-hash"
	server.mgmt.SetConfig(&rotated)
	if resourceCookieAuthenticated(server, current) {
		t.Fatal("management key change retained old session")
	}
}

func TestPluginResourceSessionLoginRotatesSameCredentials(t *testing.T) {
	for _, home := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "management", true: "home"}[home], func(t *testing.T) {
				server := newTestServer(t)
				server.mgmt.SetLocalPassword("same-login-password")
				cfg := *server.configSnapshot()
				cfg.Home.Enabled = home
				server.setConfig(&cfg)
				original := issueAuditResourceCookie(t, server, "same-login-password")
				other := issueAuditResourceCookie(t, server, "same-login-password")
				request := func(key string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, pluginResourceSessionPath, nil)
					req.RemoteAddr = "127.0.0.1:12345"
					req.AddCookie(original)
					if key != "" {
						req.Header.Set("Authorization", "Bearer "+key)
					}
					rec := httptest.NewRecorder()
					server.engine.ServeHTTP(rec, req)
					return rec
				}
				for _, key := range []string{"", "invalid-login-password"} {
					if rec := request(key); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
						t.Fatalf(
							"rotation without management authentication: status=%d cookies=%v", rec.Code,
							rec.Result().Cookies(),
						)
					}
				}
				if !resourceCookieAuthenticated(server, original) {
					t.Fatal("rejected rotation revoked the original session")
				}
				for range 2 {
					rec := request("same-login-password")
					cookies := rec.Result().Cookies()
					if rec.Code != http.StatusNoContent || len(cookies) != 1 || cookies[0].Value == "" ||
						cookies[0].Value == original.Value {
						t.Fatalf("same-credential rotation: status=%d cookies=%v", rec.Code, cookies)
					}
					current := cookies[0]
					if current.Path != original.Path || current.HttpOnly != original.HttpOnly ||
						current.SameSite != original.SameSite ||
						current.Secure != original.Secure {
						t.Fatalf("rotation changed cookie protection: original=%v current=%v", original, current)
					}
					if resourceCookieAuthenticated(server, original) || !resourceCookieAuthenticated(
						server, current,
					) || !resourceCookieAuthenticated(server, other) {
						t.Fatal("rotation must replace only the presented session")
					}
					original = current
				}
			},
		)
	}
}

func TestPluginResourceLogoutRevokesOnlyCurrentCookie(t *testing.T) {
	server := newTestServer(t)
	server.mgmt.SetLocalPassword("logout-password")
	first := issueAuditResourceCookie(t, server, "logout-password")
	other := issueAuditResourceCookie(t, server, "logout-password")
	request := func(cookie *http.Cookie, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, pluginResourceSessionPath, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if key != "" {
			req.Header.Set("X-Management-Key", key)
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	if response := request(nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated logout=%d", response.Code)
	}
	response := request(first, "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout=%d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Value != "" || cookies[0].Path != first.Path ||
		cookies[0].HttpOnly != first.HttpOnly ||
		cookies[0].Secure != first.Secure ||
		cookies[0].SameSite != first.SameSite {
		t.Fatalf("deletion cookie=%v, original=%v", cookies, first)
	}
	if resourceCookieAuthenticated(server, first) {
		t.Fatal("revoked cookie authenticated")
	}
	if !resourceCookieAuthenticated(server, other) {
		t.Fatal("logout revoked a different session")
	}
	for range 2 {
		if response := request(first, "logout-password"); response.Code != http.StatusNoContent {
			t.Fatalf("repeated logout=%d body=%s", response.Code, response.Body.String())
		}
	}
	if !resourceCookieAuthenticated(server, other) {
		t.Fatal("repeated logout revoked another session")
	}
}
