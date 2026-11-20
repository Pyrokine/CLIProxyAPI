package qwen

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuditOAuthSuccessRequiresUsableAccessToken(t *testing.T) {
	for _, operation := range []string{"refresh", "device-poll"} {
		for _, payload := range []string{
			`{}`,
			`null`,
			`{"token_type":"Bearer","expires_in":3600}`,
			`{"access_token":"","refresh_token":"synthetic-refresh","expires_in":3600}`,
			`{"access_token":"   ","expires_in":3600}`,
			`{"error":"invalid_grant","error_description":"synthetic rejection"}`,
		} {
			t.Run(
				operation+"/"+payload, func(t *testing.T) {
					calls := 0
					auth := &Auth{
						httpClient: &http.Client{
							Transport: qwenRoundTripFunc(
								func(request *http.Request) (*http.Response, error) {
									calls++
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(payload)), Request: request,
									}, nil
								},
							),
						},
					}
					var token *OAuthTokenData
					var err error
					if operation == "refresh" {
						token, err = auth.RefreshTokens(t.Context(), "synthetic-refresh")
					} else {
						token, err = auth.PollForToken(t.Context(), "synthetic-device", "synthetic-verifier")
					}
					if err == nil || token != nil || calls != 1 {
						t.Fatalf("invalid OAuth success accepted: token=%#v error=%v calls=%d", token, err, calls)
					}
					if strings.Contains(err.Error(), "synthetic-refresh") || strings.Contains(
						err.Error(), "synthetic-verifier",
					) {
						t.Fatalf("OAuth rejection exposed credentials: %v", err)
					}
				},
			)
		}
		t.Run(
			operation+"/valid", func(t *testing.T) {
				payload := `{"access_token":"synthetic-access","refresh_token":"synthetic-rotated-` +
					`refresh","token_type":"Bearer","resource_url":"portal.qwen.ai","expire` +
					`s_in":3600}`
				auth := &Auth{
					httpClient: &http.Client{
						Transport: qwenRoundTripFunc(
							func(request *http.Request) (*http.Response, error) {
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(payload)), Request: request,
								}, nil
							},
						),
					},
				}
				var token *OAuthTokenData
				var err error
				if operation == "refresh" {
					token, err = auth.RefreshTokens(t.Context(), "synthetic-refresh")
				} else {
					token, err = auth.PollForToken(t.Context(), "synthetic-device", "synthetic-verifier")
				}
				if err != nil || token == nil || token.AccessToken != "synthetic-access" ||
					token.RefreshToken != "synthetic-rotated-refresh" ||
					token.TokenType != "Bearer" ||
					token.ResourceURL != "portal.qwen.ai" {
					t.Fatalf("valid OAuth success changed: token=%#v error=%v", token, err)
				}
				if expires, err := time.Parse(
					time.RFC3339, token.Expire,
				); err != nil || time.Until(expires) < 59*time.Minute {
					t.Fatalf("expiry=%q error=%v", token.Expire, err)
				}
			},
		)
	}
}
