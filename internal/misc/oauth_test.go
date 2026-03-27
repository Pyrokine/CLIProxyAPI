package misc

import "testing"

func TestParseOAuthCallback_ValidURL(t *testing.T) {
	cb, err := ParseOAuthCallback("http://localhost/callback?code=abc&state=xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}
	if cb.Code != "abc" {
		t.Fatalf("Code = %q, want %q", cb.Code, "abc")
	}
	if cb.State != "xyz" {
		t.Fatalf("State = %q, want %q", cb.State, "xyz")
	}
}

func TestParseOAuthCallback_EmptyInput(t *testing.T) {
	cb, err := ParseOAuthCallback("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb != nil {
		t.Fatalf("expected nil for empty input, got %+v", cb)
	}
}

func TestParseOAuthCallback_MissingCode(t *testing.T) {
	_, err := ParseOAuthCallback("http://localhost/callback?state=xyz")
	if err == nil {
		t.Fatal("expected error for missing code")
	}
}

func TestParseOAuthCallback_ErrorResponse(t *testing.T) {
	cb, err := ParseOAuthCallback("http://localhost/callback?error=access_denied&error_description=user+denied")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Fatal("expected non-nil callback")
	}
	if cb.Error != "access_denied" {
		t.Fatalf("Error = %q, want %q", cb.Error, "access_denied")
	}
	if cb.ErrorDescription != "user denied" {
		t.Fatalf("ErrorDescription = %q, want %q", cb.ErrorDescription, "user denied")
	}
}
