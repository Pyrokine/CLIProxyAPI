package claude

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/misc"
	log "github.com/sirupsen/logrus"
)

func TestAuditOAuthSuccessRequiresAccessTokenBeforeProfile(t *testing.T) {
	for _, flow := range []string{"exchange", "refresh"} {
		for i, payload := range []string{
			`{}`, `null`, `{"refresh_token":"synthetic-new-refresh","expires_in":3600}`, `{"access_token":""}`,
			`{"access_token":"   "}`, `{"error":"invalid_grant"}`,
			`{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`,
		} {
			t.Run(
				fmt.Sprintf("%s/%d", flow, i), func(t *testing.T) {
					calls, profileCalls := 0, 0
					auth := &Auth{
						httpClient: &http.Client{
							Transport: roundTripFunc(
								func(req *http.Request) (*http.Response, error) {
									calls++
									body := payload
									switch req.URL.String() {
									case tokenURL:
									case profileURL:
										profileCalls++
										body = `{"account":{"uuid":"synthetic-account","email":` +
											`"synthetic@example.test"},"organization":{"uuid":` +
											`"synthetic-org"}}`
									case rolesURL:
										body = `{}`
									default:
										return nil, fmt.Errorf("unexpected OAuth route %s", req.URL)
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(body)), Request: req,
									}, nil
								},
							),
						},
					}
					var token *TokenData
					var err error
					if flow == "refresh" {
						token, err = auth.RefreshTokensWithRetry(
							t.Context(), fmt.Sprintf("synthetic-%s-%d", flow, i), 3,
						)
					} else {
						var bundle *AuthBundle
						bundle, err = auth.ExchangeCodeForTokens(
							t.Context(), "synthetic-code", "synthetic-state",
							&misc.PKCECodes{CodeVerifier: "synthetic-verifier"},
						)
						if bundle != nil {
							token = &bundle.TokenData
						}
					}
					if i < 6 {
						if err == nil || token != nil || calls != 1 || profileCalls != 0 {
							t.Fatalf(
								"invalid 200 reached profile/success: token=%#v error=%v calls=%d profile=%d",
								token,
								err,
								calls,
								profileCalls,
							)
						}
						if strings.Contains(err.Error(), "synthetic-new-refresh") || strings.Contains(
							err.Error(), payload,
						) {
							t.Fatalf("error exposed invalid response: %v", err)
						}
					} else if err !=
						nil ||
						token ==
							nil ||
						token.AccessToken !=
							"synthetic-new-access" ||
						token.RefreshToken !=
							"synthetic-new-refresh" ||
						token.AccountUUID !=
							"synthetic-account" ||
						token.OrganizationUUID !=
							"synthetic-org" ||
						profileCalls !=
							1 {
						t.Fatalf("valid control=%#v error=%v calls=%d profile=%d", token, err, calls, profileCalls)
					}
				},
			)
		}
	}
}

func TestAuditValidRefreshPreservesTokensWhenAdvisoryProfileFails(t *testing.T) {
	var output bytes.Buffer
	previous := log.StandardLogger().Out
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					body := `{"access_token":"synthetic-rotated-access","refresh_token":` +
						`"synthetic-rotated-refresh","expires_in":` +
						`3600}`
					status := http.StatusOK
					if req.URL.String() == profileURL {
						status = http.StatusServiceUnavailable
						body = `{}`
					} else if req.URL.String() != tokenURL {
						return nil, fmt.Errorf("unexpected OAuth route %s", req.URL)
					}
					return &http.Response{
						StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
						Request: req,
					}, nil
				},
			),
		},
	}
	token, err := auth.RefreshTokensWithRetry(t.Context(), "synthetic-advisory-profile", 3)
	if err != nil || token == nil || token.AccessToken != "synthetic-rotated-access" ||
		token.RefreshToken != "synthetic-rotated-refresh" {
		t.Fatalf("valid rotated material discarded: token=%#v error=%v", token, err)
	}
	if !strings.Contains(
		output.String(), "fetch Claude OAuth profile after refresh",
	) || !strings.Contains(output.String(), "503") {
		t.Fatalf("profile failure has no visible warning: %s", output.String())
	}
}
