package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestRequestScopedErrorSDKHelpersAndStructuredRawMessage(t *testing.T) {
	if (*Error)(nil).MarkRequestScoped() != nil {
		t.Fatal("nil MarkRequestScoped() returned non-nil")
	}
	explicit := NewRequestScopedError("explicit request fault", http.StatusUnauthorized)
	if !explicit.IsRequestScoped() || explicit.Code != ErrorCodeRequestScoped ||
		explicit.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("NewRequestScopedError() = %#v", explicit)
	}
	marked := (&Error{Message: "custom request fault", HTTPStatus: http.StatusUnauthorized}).MarkRequestScoped()
	if !marked.IsRequestScoped() || marked.Code != ErrorCodeRequestScoped {
		t.Fatalf("MarkRequestScoped() = %#v", marked)
	}

	for index, test := range []struct {
		name         string
		err          *Error
		wantCooldown bool
	}{
		{name: "explicit helper", err: explicit},
		{name: "marked helper", err: marked},
		{
			name: "structured raw message",
			err: &Error{
				Code:       "custom_upstream_code",
				HTTPStatus: http.StatusUnauthorized,
				Message:    `{"error":{"type":"invalid_request_error","message":"invalid parameter"}}`,
			},
		},
		{
			name: "real authentication failure",
			err: &Error{
				HTTPStatus: http.StatusUnauthorized,
				Message:    `{"error":{"type":"authentication_error","message":"invalid API key"}}`,
			},
			wantCooldown: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:       fmt.Sprintf("request-helper-auth-%d", index),
				Provider: "request-helper-provider",
				Status:   StatusActive,
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}
			manager.MarkResult(context.Background(), Result{
				AuthID:   auth.ID,
				Provider: auth.Provider,
				Model:    "request-helper-model",
				Error:    test.err,
			})
			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth missing")
			}
			if updated.Unavailable != test.wantCooldown {
				t.Fatalf("auth unavailable = %t, want %t: %#v", updated.Unavailable, test.wantCooldown, updated)
			}
			state := updated.ModelStates["request-helper-model"]
			if test.wantCooldown {
				if updated.NextRetryAfter.IsZero() {
					t.Fatalf("authentication failure credential state = %#v", updated)
				}
				if state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("credential failure created model state = %#v", state)
				}
			} else if state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
				t.Fatalf("request fault model state = %#v", state)
			}
		})
	}
}
