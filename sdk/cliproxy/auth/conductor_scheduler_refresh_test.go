package auth

import (
	"context"
	"testing"
)

func TestManager_RegisterAndUpdate(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)

	auth := &Auth{
		ID:       "refresh-test-register",
		Provider: "gemini",
	}

	registered, err := manager.Register(ctx, auth)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registered == nil || registered.ID != auth.ID {
		t.Fatalf("Register() returned unexpected auth: %v", registered)
	}

	updated := auth.Clone()
	updated.Metadata = map[string]any{"updated": true}
	result, err := manager.Update(ctx, updated)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result == nil || result.ID != auth.ID {
		t.Fatalf("Update() returned unexpected auth: %v", result)
	}
}
