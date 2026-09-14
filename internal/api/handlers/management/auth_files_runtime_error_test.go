package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestBuildAuthFileEntryProjectsLatestModelRuntimeError(t *testing.T) {
	observedAt := time.Date(2026, time.September, 10, 8, 30, 0, 0, time.UTC)
	nextRetryAfter := observedAt.Add(2 * time.Hour)
	rawMessage := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"prolite","token":"provider-secret","resets_at":1789371604,"resets_in_seconds":578762}}`
	auth := &coreauth.Auth{
		ID:               "codex-runtime-error",
		Provider:         "codex",
		FileName:         "codex.json",
		Status:           coreauth.StatusError,
		StatusMessage:    rawMessage,
		Attributes:       map[string]string{"runtime_only": "true"},
		UpdatedAt:        observedAt.Add(-time.Minute),
		LastError:        &coreauth.Error{Code: "older_error", Message: "older error"},
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		StateEffectKey:   "codex-runtime-error",
		StateGeneration:  3,
		NextRetryAfter:   observedAt.Add(time.Minute),
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.6-sol": {
				Status:         coreauth.StatusError,
				StatusMessage:  rawMessage,
				Unavailable:    true,
				NextRetryAfter: nextRetryAfter,
				LastError: &coreauth.Error{
					Code:            "quota_exhausted",
					Message:         rawMessage,
					HTTPStatus:      http.StatusTooManyRequests,
					EffectScope:     cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:       "codex-sol",
					StateGeneration: 9,
				},
				EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
				EffectKey:   "codex-sol",
				Generation:  7,
				UpdatedAt:   observedAt,
			},
		},
	}

	encoded, err := json.Marshal((&Handler{}).buildAuthFileEntry(auth))
	if err != nil {
		t.Fatalf("marshal auth file entry: %v", err)
	}
	if strings.Contains(string(encoded), "provider-secret") {
		t.Fatalf("auth file entry leaked provider secret: %s", encoded)
	}

	var entry map[string]any
	if err = json.Unmarshal(encoded, &entry); err != nil {
		t.Fatalf("decode auth file entry: %v", err)
	}
	statusMessage, _ := entry["status_message"].(string)
	if strings.Contains(statusMessage, "provider-secret") {
		t.Fatalf("status_message leaked provider secret: %s", statusMessage)
	}
	runtimeError, ok := entry["runtime_error"].(map[string]any)
	if !ok {
		t.Fatalf("runtime_error = %#v", entry["runtime_error"])
	}
	if runtimeError["code"] != "quota_exhausted" || runtimeError["http_status"] != float64(http.StatusTooManyRequests) ||
		runtimeError["model"] != "gpt-5.6-sol" || runtimeError["effect_scope"] != "quota_family" ||
		runtimeError["effect_key"] != "codex-sol" || runtimeError["state_generation"] != float64(9) {
		t.Fatalf("runtime_error = %#v", runtimeError)
	}
	if runtimeError["next_retry_after"] != nextRetryAfter.Format(time.RFC3339) ||
		runtimeError["observed_at"] != observedAt.Format(time.RFC3339) {
		t.Fatalf("runtime_error times = %#v", runtimeError)
	}
	for _, forbidden := range []string{"metadata", "attributes", "storage", "token", "cookie", "authorization"} {
		if _, exists := runtimeError[forbidden]; exists {
			t.Fatalf("runtime_error contains forbidden field %q: %#v", forbidden, runtimeError)
		}
	}
}

func TestBuildAuthFileRuntimeErrorUsesModelObservationForAggregatedModelError(t *testing.T) {
	observedAt := time.Date(2026, time.September, 10, 8, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:               "aggregated-model-error",
		Status:           coreauth.StatusError,
		StatusMessage:    "quota exhausted",
		UpdatedAt:        observedAt.Add(time.Minute),
		StateEffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
		StateEffectKey:   "codex-sol",
		LastError: &coreauth.Error{
			Code:            "quota_exhausted",
			Message:         "quota exhausted",
			HTTPStatus:      http.StatusTooManyRequests,
			EffectScope:     cliproxyexecutor.StateEffectScopeQuotaFamily,
			EffectKey:       "codex-sol",
			StateGeneration: 4,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-5.6-sol": {
				Status:        coreauth.StatusError,
				StatusMessage: "quota exhausted",
				LastError: &coreauth.Error{
					Code:            "quota_exhausted",
					Message:         "quota exhausted",
					HTTPStatus:      http.StatusTooManyRequests,
					EffectScope:     cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:       "codex-sol",
					StateGeneration: 4,
				},
				EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
				EffectKey:   "codex-sol",
				Generation:  4,
				UpdatedAt:   observedAt,
			},
		},
	}

	runtimeError := buildAuthFileRuntimeError(auth)
	if runtimeError == nil {
		t.Fatal("runtime error is nil")
	}
	if runtimeError.Model != "gpt-5.6-sol" || runtimeError.ObservedAt == nil ||
		!runtimeError.ObservedAt.Equal(observedAt) {
		t.Fatalf("runtime error = %#v", runtimeError)
	}
}

func TestBuildAuthFileRuntimeErrorKeepsNewerCredentialScope(t *testing.T) {
	modelObservedAt := time.Date(2026, time.September, 10, 8, 0, 0, 0, time.UTC)
	credentialObservedAt := modelObservedAt.Add(time.Minute)
	auth := &coreauth.Auth{
		ID:               "credential-error",
		Status:           coreauth.StatusError,
		StatusMessage:    "invalid grant",
		UpdatedAt:        credentialObservedAt,
		NextRetryAfter:   credentialObservedAt.Add(time.Hour),
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		StateEffectKey:   "credential-error",
		StateGeneration:  12,
		LastError: &coreauth.Error{
			Code:            "unauthorized",
			Message:         "invalid grant",
			HTTPStatus:      http.StatusUnauthorized,
			EffectScope:     cliproxyexecutor.StateEffectScopeCredential,
			EffectKey:       "credential-error",
			StateGeneration: 12,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"old-model": {
				Status:      coreauth.StatusError,
				LastError:   &coreauth.Error{Code: "old_model_error", Message: "old model error"},
				EffectScope: cliproxyexecutor.StateEffectScopeExactModel,
				EffectKey:   "old-model",
				UpdatedAt:   modelObservedAt,
			},
		},
	}

	runtimeError := buildAuthFileRuntimeError(auth)
	if runtimeError == nil {
		t.Fatal("runtime error is nil")
	}
	if runtimeError.Code != "unauthorized" || runtimeError.EffectScope != "credential" ||
		runtimeError.EffectKey != auth.ID || runtimeError.Model != "" || runtimeError.ObservedAt == nil ||
		!runtimeError.ObservedAt.Equal(credentialObservedAt) {
		t.Fatalf("runtime error = %#v", runtimeError)
	}
}
