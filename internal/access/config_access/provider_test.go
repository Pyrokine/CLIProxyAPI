package configaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
)

func newTestProvider(keys []string, allowQueryAuth bool) *provider {
	return newProvider(sdkaccess.DefaultAccessProviderName, keys, allowQueryAuth)
}

func TestAuthenticate_ValidBearerToken(t *testing.T) {
	p := newTestProvider([]string{"sk-test-key-123"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test-key-123")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success, got error: %v", authErr)
	}
	if result.Principal != "sk-test-key-123" {
		t.Fatalf("expected principal sk-test-key-123, got %s", result.Principal)
	}
	if result.Metadata["source"] != "authorization" {
		t.Fatalf("expected source authorization, got %s", result.Metadata["source"])
	}
}

func TestAuthenticate_ValidGoogleAPIKey(t *testing.T) {
	p := newTestProvider([]string{"google-key-abc"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Goog-Api-Key", "google-key-abc")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success, got error: %v", authErr)
	}
	if result.Principal != "google-key-abc" {
		t.Fatalf("expected principal google-key-abc, got %s", result.Principal)
	}
	if result.Metadata["source"] != "x-goog-api-key" {
		t.Fatalf("expected source x-goog-api-key, got %s", result.Metadata["source"])
	}
}

func TestAuthenticate_ValidAnthropicKey(t *testing.T) {
	p := newTestProvider([]string{"anthropic-key-xyz"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Api-Key", "anthropic-key-xyz")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success, got error: %v", authErr)
	}
	if result.Metadata["source"] != "x-api-key" {
		t.Fatalf("expected source x-api-key, got %s", result.Metadata["source"])
	}
}

func TestAuthenticate_InvalidKey(t *testing.T) {
	p := newTestProvider([]string{"valid-key"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")

	_, authErr := p.Authenticate(context.Background(), req)
	if authErr == nil {
		t.Fatal("expected error for invalid key")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("expected InvalidCredential error, got %s", authErr.Code)
	}
}

func TestAuthenticate_NoCredentials(t *testing.T) {
	p := newTestProvider([]string{"valid-key"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	// No auth headers or query params.

	_, authErr := p.Authenticate(context.Background(), req)
	if authErr == nil {
		t.Fatal("expected error for no credentials")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials) {
		t.Fatalf("expected NoCredentials error, got %s", authErr.Code)
	}
}

func TestAuthenticate_EmptyKeys_NotHandled(t *testing.T) {
	p := newTestProvider(nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer something")

	_, authErr := p.Authenticate(context.Background(), req)
	if authErr == nil {
		t.Fatal("expected error for empty keys provider")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNotHandled) {
		t.Fatalf("expected NotHandled error, got %s", authErr.Code)
	}
}

func TestAuthenticate_QueryAuth_Disabled(t *testing.T) {
	p := newTestProvider([]string{"query-key-val"}, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?key=query-key-val", nil)

	_, authErr := p.Authenticate(context.Background(), req)
	if authErr == nil {
		t.Fatal("expected error: query auth should be disabled")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials) {
		t.Fatalf("expected NoCredentials error when query auth disabled, got %s", authErr.Code)
	}
}

func TestAuthenticate_QueryAuth_Enabled_KeyParam(t *testing.T) {
	p := newTestProvider([]string{"qkey-123"}, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?key=qkey-123", nil)

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success with query key, got error: %v", authErr)
	}
	if result.Principal != "qkey-123" {
		t.Fatalf("expected principal qkey-123, got %s", result.Principal)
	}
	if result.Metadata["source"] != "query-key" {
		t.Fatalf("expected source query-key, got %s", result.Metadata["source"])
	}
}

func TestAuthenticate_QueryAuth_Enabled_AuthTokenParam(t *testing.T) {
	p := newTestProvider([]string{"at-token-456"}, true)
	req := httptest.NewRequest(http.MethodGet, "/v1/models?auth_token=at-token-456", nil)

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success with auth_token query, got error: %v", authErr)
	}
	if result.Principal != "at-token-456" {
		t.Fatalf("expected principal at-token-456, got %s", result.Principal)
	}
	if result.Metadata["source"] != "query-auth-token" {
		t.Fatalf("expected source query-auth-token, got %s", result.Metadata["source"])
	}
}

func TestAuthenticate_ConstantTimeComparison_AllKeysIterated(t *testing.T) {
	// With multiple keys, even if first matches, all should be iterated
	// (constant-time guarantee). We test correctness: last key matches too.
	keys := []string{"key-first", "key-second", "key-third"}
	p := newTestProvider(keys, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer key-third")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success matching last key, got error: %v", authErr)
	}
	if result.Principal != "key-third" {
		t.Fatalf("expected principal key-third, got %s", result.Principal)
	}
}

func TestAuthenticate_NilProvider(t *testing.T) {
	var p *provider
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	_, authErr := p.Authenticate(context.Background(), req)
	if authErr == nil {
		t.Fatal("expected error for nil provider")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNotHandled) {
		t.Fatalf("expected NotHandled error, got %s", authErr.Code)
	}
}

func TestIdentifier(t *testing.T) {
	p := newTestProvider([]string{"k"}, false)
	if id := p.Identifier(); id != sdkaccess.DefaultAccessProviderName {
		t.Fatalf("expected default name, got %s", id)
	}

	p2 := newProvider("custom-name", []string{"k"}, false)
	if id := p2.Identifier(); id != "custom-name" {
		t.Fatalf("expected custom-name, got %s", id)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"Bearer abc123", "abc123"},
		{"bearer   xyz  ", "xyz"},
		{"Basic dXNlcjpwYXNz", "Basic dXNlcjpwYXNz"},
		{"raw-token-no-space", "raw-token-no-space"},
	}
	for _, tt := range tests {
		got := extractBearerToken(tt.input)
		if got != tt.want {
			t.Errorf("extractBearerToken(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeKeys(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  int
	}{
		{"nil", nil, 0},
		{"empty strings", []string{"", "  "}, 0},
		{"dedup", []string{"a", "b", "a"}, 2},
		{"trim", []string{"  x  ", "x"}, 1},
	}
	for _, tt := range tests {
		got := normalizeKeys(tt.input)
		if len(got) != tt.want {
			t.Errorf("%s: normalizeKeys got %d keys, want %d", tt.name, len(got), tt.want)
		}
	}
}
