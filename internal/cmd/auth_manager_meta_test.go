package cmd

import (
	"context"
	"testing"
)

func TestDefaultAuthManagerRegistersMeta(t *testing.T) {
	_, _, err := newAuthManager().Login(context.Background(), "meta", nil, nil)
	if err == nil || err.Error() != "meta authentication requires a context and configuration" {
		t.Fatalf("Meta login with nil config = %v, want pre-request validation", err)
	}
}
