package auth

import (
	"context"
	"errors"
	"testing"
)

func TestQwenLoginEmailCancellationWinsAfterPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	options := &LoginOptions{
		Prompt: func(string) (string, error) {
			cancel()
			return "cancelled@example.com", nil
		},
	}

	email, err := qwenLoginEmail(ctx, options)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("qwenLoginEmail error = %v, want context.Canceled", err)
	}
	if email != "" {
		t.Fatalf("qwenLoginEmail email = %q, want empty", email)
	}
}
