package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestC04XAIAPIKeyAndOAuthRoutingHeaders(t *testing.T) {
	apiKeyAuth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":   "xai-key",
			"base_url":  "https://api.x.ai/v1",
			"auth_kind": "apikey",
		},
	}
	if got := xaiChatBaseURL(apiKeyAuth); got != "https://api.x.ai/v1" {
		t.Fatalf("API-key base URL = %q", got)
	}
	apiReq, _ := http.NewRequest(http.MethodPost, "https://api.x.ai/v1/responses", nil)
	applyXAIChatHeaders(apiReq, apiKeyAuth, "xai-key", false, "")
	if got := apiReq.Header.Get("Authorization"); got != "Bearer xai-key" {
		t.Fatalf("API-key Authorization = %q", got)
	}
	if got := apiReq.Header.Get(xaiTokenAuthHeader); got != "" {
		t.Fatalf("API-key request received CLI auth header %q", got)
	}

	oauth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
	cliURL := xaiChatBaseURL(oauth)
	cliReq, _ := http.NewRequest(http.MethodPost, cliURL+"/responses", nil)
	applyXAIChatHeaders(cliReq, oauth, "oauth-token", false, "")
	if got := cliReq.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("OAuth Authorization = %q", got)
	}
	if got := cliReq.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
		t.Fatalf("OAuth CLI auth header = %q, want %q", got, xaiTokenAuthValue)
	}
}
