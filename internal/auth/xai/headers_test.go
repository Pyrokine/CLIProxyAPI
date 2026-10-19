package xai

import (
	"net/http"
	"testing"
)

func TestApplyShellIdentityHeaders(t *testing.T) {
	headers := make(http.Header)
	ApplyShellIdentityHeaders(headers)

	want := map[string]string{
		"X-XAI-Token-Auth":         "xai-grok-cli",
		"x-grok-client-version":    "1.0.46",
		"User-Agent":               "xai-grok-workspace/1.0.46",
		"x-grok-client-identifier": "grok-shell",
		"x-authenticateresponse":   "authenticate-response",
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("header %q = %q, want %q", name, got, value)
		}
	}
}

func TestShellIdentityHeaderValuesReturnsIndependentCopy(t *testing.T) {
	first := ShellIdentityHeaderValues()
	first["User-Agent"] = "changed"

	second := ShellIdentityHeaderValues()
	if got := second["User-Agent"]; got != "xai-grok-workspace/1.0.46" {
		t.Fatalf("User-Agent = %q", got)
	}
}
