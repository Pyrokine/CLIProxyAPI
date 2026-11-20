package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestErrorEventProjectionPreservesFieldsAndRedactsSecrets(t *testing.T) {
	next := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	quota := QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2}
	snapshot := &Auth{
		ID: "auth-event-projection", Index: "opaque-event-index", Provider: "codex",
		Status: StatusError, Disabled: true, Unavailable: true,
		StatusMessage:  "Authorization: Bearer SYNTHETIC_AUTH_STATUS_SECRET",
		NextRetryAfter: next, Quota: quota,
		Metadata: map[string]any{"access_token": "SYNTHETIC_PRIVATE_METADATA_SECRET"},
		ModelStates: map[string]*ModelState{
			"gpt-5.3-codex": {
				Status: StatusError, Unavailable: true, NextRetryAfter: next, Quota: quota,
				StatusMessage: "Authorization: Bearer SYNTHETIC_MODEL_STATUS_SECRET",
			},
		},
	}
	payload, errBuild := buildErrorEventPayload(
		Result{
			AuthID: snapshot.ID, Provider: "codex", Model: "gpt-5.3-codex(high)",
			Error: &Error{
				HTTPStatus: http.StatusTooManyRequests, Code: "rate_limit", Retryable: true,
				Message: `{"error":"quota","access_token":"SYNTHETIC_ERROR_BODY_SECRET"}`,
			},
		}, snapshot,
	)
	if errBuild != nil {
		t.Fatal(errBuild)
	}
	for _, field := range []struct{ path, want string }{
		{"provider", "codex"}, {"model", "gpt-5.3-codex(high)"},
		{"auth_id", snapshot.ID}, {"auth_index", snapshot.Index},
		{"status_code", "429"}, {"code", "rate_limit"}, {"retryable", "true"},
		{"auth_status.status", string(StatusError)}, {"auth_status.disabled", "true"},
		{"auth_status.unavailable", "true"}, {"auth_status.next_retry_after", next.Format(time.RFC3339)},
		{"auth_status.quota.exceeded", "true"}, {"auth_status.quota.reason", "quota"},
		{"auth_status.quota.next_recover_at", next.Format(time.RFC3339)}, {"auth_status.quota.backoff_level", "2"},
		{"auth_status.model.name", "gpt-5.3-codex"}, {"auth_status.model.status", string(StatusError)},
		{"auth_status.model.unavailable", "true"}, {"auth_status.model.next_retry_after", next.Format(time.RFC3339)},
		{"auth_status.model.quota.exceeded", "true"}, {"auth_status.model.quota.reason", "quota"},
		{"auth_status.model.quota.next_recover_at", next.Format(time.RFC3339)},
		{"auth_status.model.quota.backoff_level", "2"},
	} {
		if got := gjson.GetBytes(payload, field.path); !got.Exists() || got.String() != field.want {
			t.Fatalf("event field %s = %s, want %s", field.path, got, field.want)
		}
	}
	if _, errTimestamp := time.Parse(
		time.RFC3339Nano, gjson.GetBytes(payload, "timestamp").String(),
	); errTimestamp != nil {
		t.Fatalf("event timestamp: %v", errTimestamp)
	}
	if !json.Valid([]byte(gjson.GetBytes(payload, "body").String())) {
		t.Fatalf("redacted error body is not JSON: %s", payload)
	}
	for _, secret := range []string{
		"SYNTHETIC_AUTH_STATUS_SECRET", "SYNTHETIC_MODEL_STATUS_SECRET",
		"SYNTHETIC_PRIVATE_METADATA_SECRET", "SYNTHETIC_ERROR_BODY_SECRET",
	} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("event contains synthetic secret %s", secret)
		}
	}
}

func TestManagerErrorEventsPreserveNeutralStateAndRejectStaleResults(t *testing.T) {
	queue := newErrorEventQueue(t)
	subscriber, unsubscribe := queue.SubscribeErrors()
	defer unsubscribe()
	manager := NewManager(nil, nil, nil)
	manager.SetErrorQueue(queue)
	auth, errRegister := manager.Register(
		WithSkipPersist(t.Context()), &Auth{
			ID: "auth-event-generation", Provider: "codex", Status: StatusActive,
			Metadata: map[string]any{"access_token": "synthetic-old-token"},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	failure := executionResult(
		auth, "codex", "gpt-5",
		NewRequestScopedError("request rejected", http.StatusBadRequest),
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(t.Context(), failure)
	payload := requireErrorSubscriberPayload(t, subscriber)
	if gjson.GetBytes(payload, "status_code").Int() != http.StatusBadRequest ||
		gjson.GetBytes(payload, "auth_status.status").String() != string(StatusActive) ||
		gjson.GetBytes(payload, "auth_status.unavailable").Bool() ||
		gjson.GetBytes(payload, "auth_status.model").Exists() {
		t.Fatalf("availability-neutral event altered the auth state: %s", payload)
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "synthetic-new-token"
	if _, errUpdate := manager.Update(WithSkipPersist(t.Context()), updated); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	manager.MarkResult(t.Context(), failure)
	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("registered credential disappeared")
	}
	manager.MarkResult(
		t.Context(), executionResult(
			current, "codex", "gpt-5", nil,
			cliproxyexecutor.DispatchPhaseResponseStarted,
		),
	)
	manager.SetErrorQueue(nil)
	manager.MarkResult(
		t.Context(), executionResult(
			current, "codex", "gpt-5",
			NewRequestScopedError("explicitly disabled", http.StatusBadRequest),
			cliproxyexecutor.DispatchPhaseResponseStarted,
		),
	)
	select {
	case got := <-subscriber:
		t.Fatalf("received a stale, successful or explicitly disabled event: %q", got)
	default:
	}
}
