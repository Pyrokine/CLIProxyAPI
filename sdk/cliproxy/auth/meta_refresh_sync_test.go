package auth

import (
	"context"
	"net/http"
	"testing"
)

type metaUnauthorizedRefreshExecutor struct {
	schedulerTestExecutor
	refreshCalls int
}

func (*metaUnauthorizedRefreshExecutor) Identifier() string { return "meta" }

func (e *metaUnauthorizedRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls++
	updated := auth.Clone()
	updated.Metadata["api_key"] = "new-key"
	updated.Metadata["access_token"] = "new-key"
	return updated, nil
}

func TestMetaDCAAllowsRefreshAfterUnauthorized(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &metaUnauthorizedRefreshExecutor{}
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(t.Context(), &Auth{
		ID: "meta-oauth", Provider: "meta", Status: StatusActive,
		Metadata: map[string]any{"api_key": "old-key", "access_token": "old-key", "dca_token": "dca:test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if authHasRefreshCredential(&Auth{Provider: "other", Metadata: map[string]any{"dca_token": "dca:test"}}) {
		t.Fatal("unrelated provider used Meta device token as a refresh credential")
	}
	if !authHasRefreshCredential(&Auth{Provider: "meta", Attributes: map[string]string{"dca_token": "dca:test"}}) {
		t.Fatal("Meta file credential did not use its device token for refresh")
	}
	if authHasRefreshCredential(&Auth{Provider: "meta", Attributes: map[string]string{
		"auth_kind": "apikey", "source_backend": "config", "dca_token": "dca:test",
	}}) {
		t.Fatal("configured Meta API key must not use a device token for refresh")
	}
	updated, refreshed := manager.tryRefreshAfterUnauthorized(t.Context(), registered, executor,
		&Error{HTTPStatus: http.StatusUnauthorized, Message: "expired API key"}, false)
	if !refreshed || executor.refreshCalls != 1 || updated.Metadata["api_key"] != "new-key" {
		t.Fatalf("Meta key not refreshed: refreshed=%v calls=%d auth=%#v", refreshed, executor.refreshCalls, updated)
	}
}
