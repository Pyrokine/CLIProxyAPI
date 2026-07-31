package iflow

import (
	"strings"
	"testing"
)

func TestSanitizeIFlowResponseBodyMasksSecrets(t *testing.T) {
	body := []byte(`{"apiKey":"sk-secret-value","accessToken":"access-secret-value","message":"token=message-secret-value"}`)

	got := sanitizeIFlowResponseBody(body)
	for _, secret := range []string{"sk-secret-value", "access-secret-value", "message-secret-value"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitizeIFlowResponseBody kept secret %q in %q", secret, got)
		}
	}
}

func TestSanitizeIFlowTextMasksURLTokens(t *testing.T) {
	text := "Get https://iflow.cn/api/oauth/getUserInfo?accessToken=access-secret-value&model=gpt failed"

	got := sanitizeIFlowText(text)
	if strings.Contains(got, "access-secret-value") {
		t.Fatalf("sanitizeIFlowText kept access token: %q", got)
	}
	if !strings.Contains(got, "model=gpt") {
		t.Fatalf("sanitizeIFlowText masked non-secret query parameter: %q", got)
	}
}
