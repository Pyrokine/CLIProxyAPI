package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type alwaysDueRefresh struct{}

func (alwaysDueRefresh) ShouldRefresh(time.Time, *Auth) bool { return true }

type invalidGrantBackoffExecutor struct {
	routingRefreshTestExecutor
	succeed bool
}

func (e *invalidGrantBackoffExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	if !e.succeed {
		return nil, e.refreshErr
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new-access"
	updated.Metadata["refresh_token"] = "new-refresh"
	return updated, nil
}

func TestInvalidGrantBackoffRetainsValidAccessTokenAndReschedules(t *testing.T) {
	executor := &invalidGrantBackoffExecutor{routingRefreshTestExecutor: routingRefreshTestExecutor{
		provider:   "test-refresh-backoff",
		refreshErr: &Error{Code: "authentication_error", HTTPStatus: http.StatusUnauthorized, Message: "invalid_grant"},
	}}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "invalid-grant-valid", Provider: executor.Identifier(), Status: StatusActive, Runtime: alwaysDueRefresh{},
		Metadata: map[string]any{
			"access_token": "old-access", "refresh_token": "expired-refresh",
			"expired": time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if _, errRefresh := manager.refreshAuthForRequest(context.Background(), registered.ID, ""); errRefresh == nil {
			t.Fatalf("attempt %d: expected invalid_grant", index+1)
		}
		current, _ := manager.GetByID(registered.ID)
		if current.Unavailable || current.Status != StatusActive || current.RefreshFailures != min(index+1, 6) {
			t.Fatalf("attempt %d: state = %+v", index+1, current)
		}
		if remaining := time.Until(current.NextRefreshAfter); remaining < want-time.Second || remaining > want+time.Second {
			t.Fatalf("attempt %d: retry after %v, want %v", index+1, remaining, want)
		}
		if manager.shouldRefresh(current, time.Now()) || !manager.shouldRefresh(current, current.NextRefreshAfter.Add(time.Millisecond)) {
			t.Fatalf("attempt %d: refresh scheduling ignored backoff", index+1)
		}
	}
	executor.refreshErr = context.DeadlineExceeded
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), registered.ID, ""); errRefresh == nil {
		t.Fatal("expected non-invalid-grant refresh error")
	}
	current, _ := manager.GetByID(registered.ID)
	if current.RefreshFailures != 0 || time.Until(current.NextRefreshAfter) < refreshFailureBackoff-time.Second {
		t.Fatalf("non-invalid-grant failure did not reset backoff: %+v", current)
	}
	executor.refreshErr = &Error{HTTPStatus: http.StatusUnauthorized, Message: "invalid_grant"}
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), registered.ID, ""); errRefresh == nil {
		t.Fatal("expected invalid_grant after reset")
	}
	executor.succeed = true
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), registered.ID, ""); errRefresh != nil {
		t.Fatalf("refresh success: %v", errRefresh)
	}
	current, _ = manager.GetByID(registered.ID)
	if current.RefreshFailures != 0 || current.LastError != nil || current.Metadata["access_token"] != "new-access" {
		t.Fatalf("successful refresh did not clear failure state: %+v", current)
	}
	wire, errMarshal := json.Marshal(current)
	if errMarshal != nil || strings.Contains(string(wire), "refresh_failures") {
		t.Fatalf("refresh backoff was serialized: %v %s", errMarshal, wire)
	}
}

func TestInvalidGrantRefreshDoesNotScheduleDisabledCredential(t *testing.T) {
	executor := &invalidGrantBackoffExecutor{routingRefreshTestExecutor: routingRefreshTestExecutor{
		provider:   "test-disabled-refresh",
		refreshErr: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"},
	}}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registered, err := manager.Register(context.Background(), &Auth{
		ID: "invalid-grant-disabled", Provider: executor.Identifier(), Disabled: true, Status: StatusDisabled,
		Metadata: map[string]any{"access_token": "old-access", "refresh_token": "invalid-refresh",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, errRefresh := manager.refreshAuthForRequest(context.Background(), registered.ID, ""); errRefresh == nil {
		t.Fatal("expected invalid_grant for disabled credential")
	}
	current, _ := manager.GetByID(registered.ID)
	if !current.Disabled || current.Status != StatusDisabled || !current.NextRefreshAfter.IsZero() ||
		current.RefreshFailures != 0 || manager.shouldRefresh(current, time.Now().Add(time.Hour)) {
		t.Fatalf("disabled invalid_grant scheduled refresh: %+v", current)
	}
}
