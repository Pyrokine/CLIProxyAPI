package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestManagementCooldownResetRequiresAuthenticationAndNeverNeedsCodexResetCredit(t *testing.T) {
	const password = "test-local-management-password"
	server := newTestServerWithOptions(t, WithLocalManagementPassword(password))
	manager := server.handlers.AuthManager
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID: "route-local-cooldown", Provider: "codex", Status: coreauth.StatusError,
		Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: next},
		ModelStates: map[string]*coreauth.ModelState{
			"transient": {
				Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: next,
				LastError: &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	request := func(authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v0/management/routing/cooldown/reset",
			strings.NewReader(`{"auth_index":"`+index+`"}`))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+password)
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	if recorder := request(false); recorder.Code == http.StatusOK {
		t.Fatalf("unauthenticated request succeeded: %s", recorder.Body.String())
	}
	if recorder := request(true); recorder.Code != http.StatusOK {
		t.Fatalf("local reset status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.Quota.Exceeded || updated.ModelStates["transient"].Unavailable {
		t.Fatalf("local cooldown not cleared: %#v", updated)
	}
}
