package devin

import (
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCredentialsFromAuthUsesStableServerOnlyValues(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "devin.json",
		FileName: "devin.json",
		Index:    "auth-index",
		Attributes: map[string]string{
			"api_key":     "attribute-token",
			"base_url":    "https://server.example/",
			"device_seed": "attribute-seed",
		},
		Metadata: map[string]any{
			"api_key":     "metadata-token",
			"base_url":    "https://metadata.example",
			"device_seed": "metadata-seed",
		},
	}
	token, baseURL, seed := CredentialsFromAuth(auth)
	if token != "attribute-token" || baseURL != "https://server.example/" || seed != "attribute-seed" {
		t.Fatalf("credentials = %q/%q/%q", token, baseURL, seed)
	}

	auth.Attributes = nil
	token, baseURL, seed = CredentialsFromAuth(auth)
	if token != "metadata-token" || baseURL != "https://metadata.example" || seed != "metadata-seed" {
		t.Fatalf("metadata credentials = %q/%q/%q", token, baseURL, seed)
	}

	auth.Metadata = map[string]any{"api_key": "metadata-token"}
	_, baseURL, seed = CredentialsFromAuth(auth)
	if baseURL != DefaultServerURL || seed != "auth-index" {
		t.Fatalf("default identity = base %q seed %q", baseURL, seed)
	}
}
